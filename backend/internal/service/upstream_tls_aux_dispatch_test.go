//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// 防封加固续轮的行为锁。
//
// 修复前的事实（file:line 已取证）：Trae/Qoder/WorkBuddy 三家的「签到 / 余额 /
// 换票 / 批量任务」出站一律直调 HTTPUpstream.Do —— 账号被 pacer 限速，但交出的
// ClientHello 仍是普通 Go TLS，与面板上"该账号已启用 TLS 指纹"互相矛盾。
// 聊天主链路早已吃指纹（trae_client.go:130 / qoder_client.go:179 /
// workbuddy_client.go:148），所以真实形态是"同一账号两种 TLS 身份"。
//
// 本文件钉住三条：
//  1. 账号启用指纹 ⇒ 这些辅助域出站必须走 DoWithTLS 并带上解析出的 Profile；
//  2. 未注入 profileService ⇒ 必须退回普通 Do（老部署零行为变更）；
//  3. 账号未启用指纹 ⇒ 仍走普通 Do（不得凭空给未开启的账号换 ClientHello）；
//     且连接池并发度保持历史口径 maxInt(account.Concurrency, 1)。
//
// 与 http_upstream_pacer_test.go 同一动机：只测分派函数自身不够，必须证明**调用点真的接上了**。

// tlsFPRecordingUpstream 记录每次出站走 Do 还是 DoWithTLS，并把请求真正投递出去。
type tlsFPRecordingUpstream struct {
	mu       sync.Mutex
	client   *http.Client
	calls    []tlsFPCall
	profiles []*tlsfingerprint.Profile
}

type tlsFPCall struct {
	method      string // "Do" / "DoWithTLS"
	path        string
	accountID   int64
	concurrency int
	hasProfile  bool
}

func (u *tlsFPRecordingUpstream) Do(req *http.Request, _ string, accountID int64, concurrency int) (*http.Response, error) {
	u.mu.Lock()
	u.calls = append(u.calls, tlsFPCall{method: "Do", path: req.URL.Path, accountID: accountID, concurrency: concurrency})
	u.mu.Unlock()
	return u.send(req)
}

func (u *tlsFPRecordingUpstream) DoWithTLS(req *http.Request, _ string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.mu.Lock()
	u.calls = append(u.calls, tlsFPCall{
		method: "DoWithTLS", path: req.URL.Path, accountID: accountID,
		concurrency: concurrency, hasProfile: profile != nil,
	})
	u.profiles = append(u.profiles, profile)
	u.mu.Unlock()
	return u.send(req)
}

func (u *tlsFPRecordingUpstream) send(req *http.Request) (*http.Response, error) {
	u.mu.Lock()
	client := u.client
	u.mu.Unlock()
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(req)
}

func (u *tlsFPRecordingUpstream) snapshot() []tlsFPCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]tlsFPCall(nil), u.calls...)
}

func (u *tlsFPRecordingUpstream) firstProfile(t *testing.T) *tlsfingerprint.Profile {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	require.NotEmpty(t, u.profiles, "expected at least one DoWithTLS call")
	return u.profiles[0]
}

// tlsFPStubProfileService 用有模板的桩仓储构造（复用 tls_fingerprint_profile_service_test
// 的 tlsFPStubRepo / nodeCipherTemplate，模板 ID=77、Name="Manual Node Template"）。
func tlsFPStubProfileService() *TLSFingerprintProfileService {
	return NewTLSFingerprintProfileService(
		&tlsFPStubRepo{profiles: []*model.TLSFingerprintProfile{nodeCipherTemplate()}},
		nil,
	)
}

// markTLSFingerprintEnabled 把账号置为「启用 TLS 指纹 + 显式绑定模板 77」。
// 注意 GetTLSFingerprintProfileID 认 int64/int/float64，这里用 int64。
func markTLSFingerprintEnabled(account *Account) *Account {
	if account.Extra == nil {
		account.Extra = map[string]any{}
	}
	account.Extra["enable_tls_fingerprint"] = true
	account.Extra["tls_fingerprint_profile_id"] = int64(77)
	return account
}

func requireAllOutboundUseTLSProfile(t *testing.T, calls []tlsFPCall) {
	t.Helper()
	require.NotEmpty(t, calls, "expected outbound calls to be recorded")
	for _, c := range calls {
		require.Equal(t, "DoWithTLS", c.method,
			"account has TLS fingerprint enabled; path=%s must not go through plain Do", c.path)
		require.True(t, c.hasProfile, "DoWithTLS must receive a non-nil profile, path=%s", c.path)
	}
}

func requireAllOutboundPlainDo(t *testing.T, calls []tlsFPCall) {
	t.Helper()
	require.NotEmpty(t, calls, "expected outbound calls to be recorded")
	for _, c := range calls {
		require.Equal(t, "Do", c.method,
			"expected plain Do (degraded / not enabled), path=%s got %s", c.path, c.method)
	}
}

// --- Trae UG 域（签到状态 + 权益包余额）：trae_credits_service.go 原 :779 ---

func TestTraeCreditsOutboundUsesTLSFingerprintDispatch(t *testing.T) {
	future := time.Now().AddDate(0, 0, 20).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case traeCheckinStatusPath:
			writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 100))
		case traeEntUsagePath:
			writeTraeJSON(w, http.StatusOK, traePacksBody(traePack("plan_monthly_9001", 2000, 500, future)))
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404,"message":"no route"}`)
		}
	}))
	defer server.Close()

	upstream := &tlsFPRecordingUpstream{client: server.Client()}
	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(upstream, repo)
	svc.SetTLSFingerprintProfileService(tlsFPStubProfileService())

	account := markTLSFingerprintEnabled(traeCreditsAccount(41, server.URL, nil))
	account.Concurrency = 0 // 历史口径 maxInt(Concurrency,1) ⇒ 必须仍是 1
	repo.accountsByID = map[int64]*Account{41: account}

	result, err := svc.QueryCredits(context.Background(), 41)
	require.NoError(t, err)
	require.True(t, result.Success, "error: %s", result.Error)

	calls := upstream.snapshot()
	requireAllOutboundUseTLSProfile(t, calls)
	require.Equal(t, int64(41), calls[0].accountID, "dispatch must keep passing the real account ID")
	require.Equal(t, 1, calls[0].concurrency, "maxInt(account.Concurrency,1) semantics must not change")
	require.Equal(t, "Manual Node Template", upstream.firstProfile(t).Name,
		"explicitly bound template must be the one handed to DoWithTLS")
}

// 反向护栏一：服务未注入 profileService 时必须退回普通 Do（零行为变更）。
func TestTraeCreditsOutboundFallsBackToPlainDoWithoutProfileService(t *testing.T) {
	future := time.Now().AddDate(0, 0, 20).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case traeCheckinStatusPath:
			writeTraeJSON(w, http.StatusOK, traeStatusBody(true, true, 0))
		case traeEntUsagePath:
			writeTraeJSON(w, http.StatusOK, traePacksBody(traePack("plan_monthly_9002", 2000, 500, future)))
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404,"message":"no route"}`)
		}
	}))
	defer server.Close()

	upstream := &tlsFPRecordingUpstream{client: server.Client()}
	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(upstream, repo) // 不调 SetTLSFingerprintProfileService

	account := markTLSFingerprintEnabled(traeCreditsAccount(42, server.URL, nil))
	repo.accountsByID = map[int64]*Account{42: account}

	result, err := svc.QueryCredits(context.Background(), 42)
	require.NoError(t, err)
	require.True(t, result.Success, "error: %s", result.Error)

	requireAllOutboundPlainDo(t, upstream.snapshot())
}

// 反向护栏二：账号未启用指纹时不得凭空换 ClientHello。
func TestTraeCreditsOutboundKeepsPlainDoWhenFingerprintDisabled(t *testing.T) {
	future := time.Now().AddDate(0, 0, 20).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case traeCheckinStatusPath:
			writeTraeJSON(w, http.StatusOK, traeStatusBody(true, true, 0))
		case traeEntUsagePath:
			writeTraeJSON(w, http.StatusOK, traePacksBody(traePack("plan_monthly_9003", 2000, 500, future)))
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404,"message":"no route"}`)
		}
	}))
	defer server.Close()

	upstream := &tlsFPRecordingUpstream{client: server.Client()}
	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(upstream, repo)
	svc.SetTLSFingerprintProfileService(tlsFPStubProfileService())

	account := traeCreditsAccount(43, server.URL, nil) // 无 enable_tls_fingerprint
	repo.accountsByID = map[int64]*Account{43: account}

	_, err := svc.QueryCredits(context.Background(), 43)
	require.NoError(t, err)

	requireAllOutboundPlainDo(t, upstream.snapshot())
}

// --- WorkBuddy billing 域：workbuddy_credits_service.go 原 :622 ---

func TestWorkBuddyBillingOutboundUsesTLSFingerprintDispatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(workbuddyResourceBody))
	}))
	defer server.Close()

	upstream := &tlsFPRecordingUpstream{client: server.Client()}
	repo := &workbuddyCreditsFakeRepo{}
	svc := newWorkbuddyCreditsTestService(upstream, repo)
	svc.SetTLSFingerprintProfileService(tlsFPStubProfileService())

	account := markTLSFingerprintEnabled(
		workbuddyCreditsAccount(7, server.URL, map[string]any{
			"access_token": "at-1", "uid": "u-1", "realm": "cn",
		}))
	account.Concurrency = 0
	repo.accountsByID = map[int64]*Account{7: account}

	result, err := svc.QueryCredits(context.Background(), 7)
	require.NoError(t, err)
	require.True(t, result.Success, "error: %s", result.Error)

	calls := upstream.snapshot()
	requireAllOutboundUseTLSProfile(t, calls)
	require.Contains(t, calls[0].path, "get-user-resource")
	require.Equal(t, 1, calls[0].concurrency, "maxInt(account.Concurrency,1) semantics must not change")
}

// --- WorkBuddy 批量任务面：workbuddy_tasks_service.go 原 :573（复用 credits 传输层） ---

func TestWorkBuddyTasksOutboundUsesTLSFingerprintDispatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer server.Close()

	upstream := &tlsFPRecordingUpstream{client: server.Client()}
	repo := &workbuddyCreditsFakeRepo{}
	credits := newWorkbuddyCreditsTestService(upstream, repo)
	credits.SetTLSFingerprintProfileService(tlsFPStubProfileService())
	cfg := &config.Config{Security: config.SecurityConfig{
		URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
	}}
	tasks := NewWorkBuddyTasksService(credits, repo, cfg)

	account := markTLSFingerprintEnabled(
		workbuddyCreditsAccount(9, server.URL, map[string]any{
			"access_token": "at-9", "uid": "u-9", "realm": "cn",
		}))

	// 直接打生产函数 workbuddyTasksSend（十余个任务动作共用的唯一出站点）。
	_, err := tasks.workbuddyTasksSend(context.Background(), account, http.MethodPost,
		server.URL+"/v2/billing/meter/task-action", nil)
	require.NoError(t, err)

	calls := upstream.snapshot()
	requireAllOutboundUseTLSProfile(t, calls)
	require.Equal(t, int64(9), calls[0].accountID)
}

// --- Qoder openapi 域：qoder_campaign_service.go 原 :839 ---

func TestQoderCampaignOutboundUsesTLSFingerprintDispatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/quota/usage":
			_, _ = w.Write([]byte(`{"total":100,"used":10,"remaining":90}`))
		default:
			_, _ = w.Write([]byte(`{"campaigns":[]}`))
		}
	}))
	defer server.Close()

	upstream := &tlsFPRecordingUpstream{client: server.Client()}
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL)
	svc.SetTLSFingerprintProfileService(tlsFPStubProfileService())

	account := markTLSFingerprintEnabled(qoderCreditsTestAccount(11, nil))
	account.Concurrency = 0
	repo.accountsByID = map[int64]*Account{11: account}

	_, err := svc.QueryCredits(context.Background(), 11)
	require.NoError(t, err)

	calls := upstream.snapshot()
	requireAllOutboundUseTLSProfile(t, calls)
	require.Equal(t, 1, calls[0].concurrency, "maxInt(account.Concurrency,1) semantics must not change")
}

// 反向护栏：未注入 profileService ⇒ Qoder openapi 域退回普通 Do。
func TestQoderCampaignOutboundFallsBackWithoutProfileService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/quota/usage":
			_, _ = w.Write([]byte(`{"total":100,"used":10,"remaining":90}`))
		default:
			_, _ = w.Write([]byte(`{"campaigns":[]}`))
		}
	}))
	defer server.Close()

	upstream := &tlsFPRecordingUpstream{client: server.Client()}
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL) // 不注入

	account := markTLSFingerprintEnabled(qoderCreditsTestAccount(12, nil))
	repo.accountsByID = map[int64]*Account{12: account}

	_, err := svc.QueryCredits(context.Background(), 12)
	require.NoError(t, err)

	requireAllOutboundPlainDo(t, upstream.snapshot())
}
