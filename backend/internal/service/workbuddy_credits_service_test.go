//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// workbuddy_credits_service_test.go 积分查询与签到的单测（httptest 假上游）：
// 积分聚合口径、双域路径、信封解析、签到幂等/失败分类、token 预刷新、快照落库。

// workbuddyCreditsFakeRepo 记录 extra 落库的账号仓库假实现。
type workbuddyCreditsFakeRepo struct {
	mockAccountRepoForGemini
	mu          sync.Mutex
	extraCalls  int
	lastExtraID int64
	lastExtra   map[string]any
}

func (r *workbuddyCreditsFakeRepo) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.extraCalls++
	r.lastExtraID = id
	r.lastExtra = make(map[string]any, len(updates))
	for k, v := range updates {
		r.lastExtra[k] = v
	}
	return nil
}

func (r *workbuddyCreditsFakeRepo) snapshot() (int, int64, map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make(map[string]any, len(r.lastExtra))
	for k, v := range r.lastExtra {
		cp[k] = v
	}
	return r.extraCalls, r.lastExtraID, cp
}

func newWorkbuddyCreditsTestService(upstream HTTPUpstream, repo AccountRepository) *WorkBuddyCreditsService {
	return &WorkBuddyCreditsService{
		accountRepo:  repo,
		httpUpstream: upstream,
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
			},
		},
	}
}

func workbuddyCreditsAccount(id int64, serverURL string, creds map[string]any) *Account {
	if creds == nil {
		creds = map[string]any{}
	}
	creds["base_url"] = serverURL
	creds["billing_base_url"] = serverURL
	return &Account{ID: id, Platform: PlatformWorkbuddy, Credentials: creds}
}

const workbuddyResourceBody = `{
  "code": 0, "msg": "ok",
  "data": {"Response": {"Data": {"TotalDosage": 10000, "Accounts": [
    {"PackageName": "基础包", "CycleEndTime": "2027-02-14 00:00:00",
     "CapacitySize": 5000, "CapacityRemain": 1200, "CapacityUsed": 3800,
     "CycleCapacitySize": 5000, "CycleCapacityRemain": 1200, "CycleCapacityUsed": 3800},
    {"PackageName": "奖励包", "CycleEndTime": "2027-03-01 00:00:00",
     "CapacitySize": 500, "CapacityRemain": 300, "CapacityUsed": 200,
     "CycleCapacitySize": 500, "CycleCapacityRemain": 300, "CycleCapacityUsed": 200}
  ]}}}
}`

func TestWorkBuddyQueryCreditsAggregatesPackagesAndPersistsSnapshot(t *testing.T) {
	t.Parallel()
	var (
		mu        sync.Mutex
		gotPaths  []string
		gotAuth   string
		gotUserID string
		gotUA     string
		gotBody   string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPaths = append(gotPaths, r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		gotUserID = r.Header.Get("X-User-Id")
		gotUA = r.Header.Get("User-Agent")
		gotBody = string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(workbuddyResourceBody))
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyCreditsTestService(upstream, repo)
	account := workbuddyCreditsAccount(7, server.URL, map[string]any{
		"access_token": "at-1", "uid": "u-1", "realm": "cn",
	})
	repo.accountsByID = map[int64]*Account{7: account}

	result, err := svc.QueryCredits(context.Background(), 7)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success, "error: %s", result.Error)
	// remain = 1200 + 300 = 1500；size = 5000 + 500 = 5500（TotalDosage 10000 更大 → 取 10000）
	require.EqualValues(t, 1500, result.Remain)
	require.EqualValues(t, 10000, result.Size)
	require.EqualValues(t, 10000-1500, result.Used)
	require.Equal(t, 2, result.Packs)
	require.Len(t, result.Packages, 2)
	require.Equal(t, "基础包", result.Packages[0].PackageName)
	require.Equal(t, "cn", result.Realm)

	mu.Lock()
	require.Equal(t, []string{"/v2/billing/meter/get-user-resource"}, gotPaths)
	require.Equal(t, "Bearer at-1", gotAuth)
	require.Equal(t, "u-1", gotUserID)
	require.Equal(t, "WorkBuddy/"+workbuddyClientVersion, gotUA, "billing 域 UA 单段覆写")
	require.Contains(t, gotBody, `"ProductCode":"p_tcaca"`)
	require.Contains(t, gotBody, `"Status":[0,3]`)
	mu.Unlock()

	// 快照落库断言。
	calls, id, extra := repo.snapshot()
	require.Equal(t, 1, calls)
	require.EqualValues(t, 7, id)
	snap, ok := extra["workbuddy_credits"].(WorkBuddyCreditsSnapshot)
	require.True(t, ok, "extra 应写入 workbuddy_credits 快照")
	require.EqualValues(t, 1500, snap.Remain)
	require.Equal(t, "cn", snap.Realm)
	require.Len(t, snap.Packages, 2)
}

func TestWorkBuddyQueryCreditsGlobalPathFallbackOn404(t *testing.T) {
	t.Parallel()
	var (
		mu       sync.Mutex
		attempts []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts = append(attempts, r.URL.Path)
		mu.Unlock()
		// 首个候选路径 404 → 应回落 /v2 变体并成功。
		if r.URL.Path == "/billing/meter/get-user-resource" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"msg":"not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(workbuddyResourceBody))
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyCreditsTestService(upstream, repo)
	account := workbuddyCreditsAccount(8, server.URL, map[string]any{
		"access_token": "at-g", "uid": "u-g", "realm": "global",
	})
	repo.accountsByID = map[int64]*Account{8: account}

	result, err := svc.QueryCredits(context.Background(), 8)
	require.NoError(t, err)
	require.True(t, result.Success, "error: %s", result.Error)
	require.Equal(t, "global", result.Realm)
	mu.Lock()
	require.Equal(t, []string{"/billing/meter/get-user-resource", "/v2/billing/meter/get-user-resource"}, attempts)
	mu.Unlock()
}

func TestWorkBuddyQueryCreditsCNUsesV2PathOnly(t *testing.T) {
	t.Parallel()
	var paths []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(workbuddyResourceBody))
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	svc := newWorkbuddyCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := workbuddyCreditsAccount(9, server.URL, map[string]any{"access_token": "at", "realm": "cn"})
	repo.accountsByID = map[int64]*Account{9: account}
	result, err := svc.QueryCredits(context.Background(), 9)
	require.NoError(t, err)
	require.True(t, result.Success, "error: %s", result.Error)
	mu.Lock()
	require.Equal(t, []string{"/v2/billing/meter/get-user-resource"}, paths)
	mu.Unlock()
}

func TestWorkBuddyQueryCreditsUpstreamBusinessErrorIsResultNotGoError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// HTTP 200 + 业务 code != 0：应回填 result.Error（success=false），而不是 Go error。
		_, _ = w.Write([]byte(`{"code":110,"msg":"今日额度已用尽"}`))
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	svc := newWorkbuddyCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := workbuddyCreditsAccount(10, server.URL, map[string]any{"access_token": "at", "realm": "cn"})
	repo.accountsByID = map[int64]*Account{10: account}

	result, err := svc.QueryCredits(context.Background(), 10)
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Error, "110")
	require.Contains(t, result.Error, "今日额度已用尽")
	calls, _, _ := repo.snapshot()
	require.Equal(t, 0, calls, "失败查询不落快照")
}

func TestWorkBuddyCheckinSuccessUpdatesSnapshotAndCredits(t *testing.T) {
	t.Parallel()
	var (
		mu           sync.Mutex
		checkinPaths []string
		checkinBody  string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			checkinPaths = append(checkinPaths, r.URL.Path)
			checkinBody = string(body)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case "/v2/billing/meter/get-user-resource":
			_, _ = w.Write([]byte(workbuddyResourceBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	svc := newWorkbuddyCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := workbuddyCreditsAccount(11, server.URL, map[string]any{"access_token": "at", "realm": "cn"})
	repo.accountsByID = map[int64]*Account{11: account}

	result, err := svc.Checkin(context.Background(), 11)
	require.NoError(t, err)
	require.Equal(t, WorkBuddyCheckinStatusOK, result.Status)
	require.True(t, result.Success)
	require.NotNil(t, result.Credits)
	require.EqualValues(t, 1500, *result.Credits)
	mu.Lock()
	require.Equal(t, []string{"/v2/billing/meter/daily-checkin"}, checkinPaths)
	require.Equal(t, "{}", checkinBody)
	mu.Unlock()

	calls, _, extra := repo.snapshot()
	require.Equal(t, 2, calls, "签到 + 积分刷新各写一次快照")
	snap, ok := extra["workbuddy_checkin"].(WorkBuddyCheckinSnapshot)
	require.True(t, ok)
	require.Equal(t, WorkBuddyCheckinStatusOK, snap.Status)
	require.NotNil(t, snap.Credits)
	require.EqualValues(t, 1500, *snap.Credits)
}

func TestWorkBuddyCheckinAlreadyIsSuccess(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/billing/meter/daily-checkin" {
			// 已签到：业务码 14001，HTTP 200（枚举信/文案两形态之一）。
			_, _ = w.Write([]byte(`{"code":14001,"msg":"今天已签到"}`))
			return
		}
		_, _ = w.Write([]byte(workbuddyResourceBody))
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	svc := newWorkbuddyCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := workbuddyCreditsAccount(12, server.URL, map[string]any{"access_token": "at", "realm": "cn"})
	repo.accountsByID = map[int64]*Account{12: account}

	result, err := svc.Checkin(context.Background(), 12)
	require.NoError(t, err)
	require.Equal(t, WorkBuddyCheckinStatusAlready, result.Status)
	require.True(t, result.Success, "already 是幂等成功语义")
	require.Empty(t, result.Detail, "「已签到」不填 detail，避免误读成失败")
	_, _, extra := repo.snapshot()
	snap, ok := extra["workbuddy_checkin"].(WorkBuddyCheckinSnapshot)
	require.True(t, ok)
	require.Equal(t, WorkBuddyCheckinStatusAlready, snap.Status)
}

func TestWorkBuddyCheckinHTTP400AlreadyTextIsIdempotent(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/billing/meter/daily-checkin" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":10001,"msg":"今天已签到"}`))
			return
		}
		_, _ = w.Write([]byte(workbuddyResourceBody))
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	svc := newWorkbuddyCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := workbuddyCreditsAccount(13, server.URL, map[string]any{"access_token": "at", "realm": "cn"})
	repo.accountsByID = map[int64]*Account{13: account}

	result, err := svc.Checkin(context.Background(), 13)
	require.NoError(t, err)
	require.Equal(t, WorkBuddyCheckinStatusAlready, result.Status)
	require.True(t, result.Success)
}

func TestWorkBuddyCheckinTransportErrorIsFailNotAlready(t *testing.T) {
	t.Parallel()
	// 连接级错误（服务器立即关闭）不得被记成 already。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	svc := newWorkbuddyCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := workbuddyCreditsAccount(14, server.URL, map[string]any{"access_token": "at", "realm": "cn"})
	repo.accountsByID = map[int64]*Account{14: account}

	result, err := svc.Checkin(context.Background(), 14)
	require.NoError(t, err)
	require.Equal(t, WorkBuddyCheckinStatusFail, result.Status)
	require.False(t, result.Success)
	require.NotEmpty(t, result.Detail)
}

func TestWorkBuddyCheckinGlobalRealmSkippedWithoutUpstreamCall(t *testing.T) {
	t.Parallel()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	svc := newWorkbuddyCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := workbuddyCreditsAccount(15, server.URL, map[string]any{"access_token": "at", "realm": "global"})
	repo.accountsByID = map[int64]*Account{15: account}

	result, err := svc.Checkin(context.Background(), 15)
	require.NoError(t, err)
	require.Equal(t, WorkBuddyCheckinStatusSkipped, result.Status)
	require.Contains(t, result.Detail, "global")
	require.EqualValues(t, 0, atomic.LoadInt32(&calls), "global 账号不发任何上游请求（不逐事件风控）")
	// skipped 不落快照。
	calls2, _, _ := repo.snapshot()
	require.Equal(t, 0, calls2)
}

func TestWorkBuddyCheckinPreflightRefreshWritesCredentials(t *testing.T) {
	t.Parallel()
	var (
		mu        sync.Mutex
		refreshOK bool
		checkinAt time.Time
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/auth/token/refresh":
			mu.Lock()
			refreshOK = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":3600,"domain":"www.codebuddy.cn"}}`))
		case "/v2/billing/meter/daily-checkin":
			mu.Lock()
			checkinAt = time.Now()
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		default:
			_, _ = w.Write([]byte(workbuddyResourceBody))
		}
	}))
	defer server.Close()

	repo := &workbuddyTestAccountRepo{}
	repo.accountsByID = map[int64]*Account{}
	svc := newWorkbuddyCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	// expires_at 已过期 → 触发预刷新；billing_base_url 缺失 → 用 base_url 走 billing 域。
	account := workbuddyCreditsAccount(16, server.URL, map[string]any{
		"access_token": "at-old", "refresh_token": "rt-old",
		"expires_at": time.Now().Add(-time.Hour).Unix(), "realm": "cn",
	})
	repo.accountsByID[16] = account

	result, err := svc.Checkin(context.Background(), 16)
	require.NoError(t, err)
	require.Equal(t, WorkBuddyCheckinStatusOK, result.Status)
	mu.Lock()
	require.True(t, refreshOK, "过期 token 应先预刷新")
	require.False(t, checkinAt.IsZero())
	mu.Unlock()
	// 凭据写回断言：access_token/refresh_token 已更新。
	require.Equal(t, "at-new", account.Credentials["access_token"])
	require.Equal(t, "rt-new", account.Credentials["refresh_token"])
	require.Greater(t, repo.calls(), 0)
}

func TestWorkBuddyCheckin401RefreshesAndRetriesOnce(t *testing.T) {
	t.Parallel()
	var (
		mu           sync.Mutex
		checkinCalls int
		refreshCalls int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/auth/token/refresh":
			mu.Lock()
			refreshCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"at-new","expiresIn":3600}}`))
		case "/v2/billing/meter/daily-checkin":
			mu.Lock()
			checkinCalls++
			n := checkinCalls
			mu.Unlock()
			if n == 1 {
				// 首次 401 → 应刷新 token 后重试一次成功。
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":401,"msg":"unauthorized"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		default:
			_, _ = w.Write([]byte(workbuddyResourceBody))
		}
	}))
	defer server.Close()

	repo := &workbuddyTestAccountRepo{}
	repo.accountsByID = map[int64]*Account{}
	svc := newWorkbuddyCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := workbuddyCreditsAccount(17, server.URL, map[string]any{
		"access_token": "at", "refresh_token": "rt", "realm": "cn",
	})
	repo.accountsByID[17] = account

	result, err := svc.Checkin(context.Background(), 17)
	require.NoError(t, err)
	require.Equal(t, WorkBuddyCheckinStatusOK, result.Status)
	mu.Lock()
	require.Equal(t, 2, checkinCalls, "401 后应重试一次（共两次签到调用）")
	require.Equal(t, 1, refreshCalls, "401 触发一次刷新")
	mu.Unlock()
	require.Equal(t, "at-new", account.Credentials["access_token"])
}

func TestWorkBuddyCreditsRejectNonWorkbuddyAccount(t *testing.T) {
	t.Parallel()
	repo := &workbuddyCreditsFakeRepo{}
	repo.accountsByID = map[int64]*Account{
		20: {ID: 20, Platform: PlatformOpenAI, Credentials: map[string]any{}},
	}
	svc := newWorkbuddyCreditsTestService(&workbuddyTestUpstream{client: http.DefaultClient}, repo)
	_, err := svc.QueryCredits(context.Background(), 20)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a workbuddy account")
}

func TestWorkBuddyAggregateResourceClampsDirtyData(t *testing.T) {
	t.Parallel()
	// CycleRemain 超过 Size 的脏数据应被钳制；used 取 CycleUsed 与 size-remain 的较大者。
	// 150 钳到 100（used=0）后，CycleUsed=30 > 0 → used=30、remain=100-30=70（与参考实现同口径）。
	resp := &workbuddyUserResourceResp{}
	require.NoError(t, json.Unmarshal([]byte(`{"Response":{"Data":{"TotalDosage":0,"Accounts":[{
		"CycleCapacitySize":100,"CycleCapacityRemain":150,"CycleCapacityUsed":30
	}]}}}`), resp))
	remain, used, size, packs, packages := workbuddyAggregateResource(resp)
	require.EqualValues(t, 70, remain, "remain 钳制后又按 CycleUsed 修正")
	require.EqualValues(t, 30, used)
	require.EqualValues(t, 100, size)
	require.Equal(t, 1, packs)
	require.Len(t, packages, 1)
}

func TestWorkBuddyPackageRemainUsedFallsBackToCapacityFields(t *testing.T) {
	t.Parallel()
	// 无 Cycle 字段时回退 Capacity 三字段；used == 0 且 size > remain 时推导 used。
	pkg := workbuddyResourcePackage{CapacitySize: 800, CapacityRemain: 300}
	remain, used, size := workbuddyPackageRemainUsed(pkg)
	require.EqualValues(t, 300, remain)
	require.EqualValues(t, 500, used)
	require.EqualValues(t, 800, size)
}

// workbuddyEmptyResourceBody 企业成员账号在个人 billing 资源池的真实上游形态：
// HTTP 200 + code=0 + msg=OK，但 Accounts=null、TotalDosage=0（2026-09-24 实测抓包）。
const workbuddyEmptyResourceBody = `{
  "code": 0, "msg": "OK",
  "data": {"Response": {"Data": {"TotalCount": 0, "TotalDosage": 0, "Accounts": null}, "RequestId": ""}}
}`

func TestWorkBuddyQueryCreditsEnterpriseEmptyPoolIsNotApplicable(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(workbuddyEmptyResourceBody))
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	svc := newWorkbuddyCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := workbuddyCreditsAccount(31, server.URL, map[string]any{
		"access_token": "at", "uid": "u-31", "realm": "cn", "enterprise_id": "fyl2hqjv2nsw",
	})
	repo.accountsByID = map[int64]*Account{31: account}

	result, err := svc.QueryCredits(context.Background(), 31)
	require.NoError(t, err)
	require.True(t, result.Success, "error: %s", result.Error)
	require.True(t, result.Enterprise)
	require.True(t, result.NotApplicable, "企业号空池应标 not_applicable，而非当作余额 0")
	require.EqualValues(t, 0, result.Remain)
	require.Equal(t, 0, result.Packs)

	// 快照需携带该标记，否则列表页免探测渲染时会退回到「剩余 0」。
	_, _, extra := repo.snapshot()
	snap, ok := extra["workbuddy_credits"].(WorkBuddyCreditsSnapshot)
	require.True(t, ok)
	require.True(t, snap.NotApplicable, "快照应写入 not_applicable")
	require.True(t, snap.Enterprise)
}

func TestWorkBuddyQueryCreditsPersonalEmptyPoolIsNotMarkedNotApplicable(t *testing.T) {
	t.Parallel()
	// 个人号真用完（空池）不得被误标为企业不适用，否则用户会以为还能领。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(workbuddyEmptyResourceBody))
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	svc := newWorkbuddyCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := workbuddyCreditsAccount(32, server.URL, map[string]any{
		"access_token": "at", "uid": "u-32", "realm": "cn",
	})
	repo.accountsByID = map[int64]*Account{32: account}

	result, err := svc.QueryCredits(context.Background(), 32)
	require.NoError(t, err)
	require.True(t, result.Success)
	require.False(t, result.Enterprise)
	require.False(t, result.NotApplicable, "非企业账号不标不适用")
}

func TestWorkBuddyQueryCreditsEnterpriseWithPackagesIsNotNotApplicable(t *testing.T) {
	t.Parallel()
	// 企业账号若确实拿到套餐（上游行为变化/企业买了独立资源包），应正常展示余额。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(workbuddyResourceBody))
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	svc := newWorkbuddyCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := workbuddyCreditsAccount(33, server.URL, map[string]any{
		"access_token": "at", "uid": "u-33", "realm": "cn", "enterprise_id": "ent-x",
	})
	repo.accountsByID = map[int64]*Account{33: account}

	result, err := svc.QueryCredits(context.Background(), 33)
	require.NoError(t, err)
	require.True(t, result.Enterprise)
	require.False(t, result.NotApplicable, "有套餐就按真实余额展示")
	require.EqualValues(t, 1500, result.Remain)
}

func TestWorkBuddyCheckinEnterpriseSkippedWithoutUpstreamCall(t *testing.T) {
	t.Parallel()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	svc := newWorkbuddyCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := workbuddyCreditsAccount(34, server.URL, map[string]any{
		"access_token": "at", "uid": "u-34", "realm": "cn", "enterprise_id": "ent-y",
	})
	repo.accountsByID = map[int64]*Account{34: account}

	result, err := svc.Checkin(context.Background(), 34)
	require.NoError(t, err)
	require.Equal(t, WorkBuddyCheckinStatusSkipped, result.Status)
	require.Contains(t, result.Detail, "enterprise")
	require.EqualValues(t, 0, atomic.LoadInt32(&calls), "企业号不发 daily-checkin（避免无意义请求）")

	calls2, _, _ := repo.snapshot()
	require.Equal(t, 0, calls2, "skipped 不落快照")
}

func TestWorkBuddyCheckinSnapshotMergeKeepsSuccessOverLaterFailure(t *testing.T) {
	t.Parallel()
	today := time.Now().Format("2006-01-02")
	existing := &WorkBuddyCheckinSnapshot{Date: today, Status: WorkBuddyCheckinStatusOK}
	next := WorkBuddyCheckinSnapshot{Date: today, Status: WorkBuddyCheckinStatusFail}
	require.True(t, shouldKeepWorkbuddyCheckinSnapshot(existing, next), "当日已成功后失败重试不得覆盖")

	// 反过来（先失败后成功）应允许覆盖。
	okNext := WorkBuddyCheckinSnapshot{Date: today, Status: WorkBuddyCheckinStatusOK}
	require.False(t, shouldKeepWorkbuddyCheckinSnapshot(&WorkBuddyCheckinSnapshot{Date: today, Status: WorkBuddyCheckinStatusFail}, okNext))

	// 跨日重置：不保留。
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	require.False(t, shouldKeepWorkbuddyCheckinSnapshot(&WorkBuddyCheckinSnapshot{Date: yesterday, Status: WorkBuddyCheckinStatusOK}, next))
}

func TestNextWorkbuddyCheckinFirePicksEarliestFutureHour(t *testing.T) {
	t.Parallel()
	hours := []int{9, 21}
	// 今天 8:00 → 下一次 9:00。
	at := time.Date(2026, 9, 23, 8, 0, 0, 0, time.Local)
	next := nextWorkbuddyCheckinFire(at, hours)
	require.Equal(t, time.Date(2026, 9, 23, 9, 0, 0, 0, time.Local), next)
	// 今天 9:30 → 下一次 21:00。
	at = time.Date(2026, 9, 23, 9, 30, 0, 0, time.Local)
	next = nextWorkbuddyCheckinFire(at, hours)
	require.Equal(t, time.Date(2026, 9, 23, 21, 0, 0, 0, time.Local), next)
	// 今天 22:00 → 明天 9:00。
	at = time.Date(2026, 9, 23, 22, 0, 0, 0, time.Local)
	next = nextWorkbuddyCheckinFire(at, hours)
	require.Equal(t, time.Date(2026, 9, 24, 9, 0, 0, 0, time.Local), next)
}

func TestWorkbuddyCheckinHoursNormalization(t *testing.T) {
	t.Parallel()
	// 缺省配置 → 默认 [9,21]。
	require.Equal(t, []int{9, 21}, workbuddyCheckinHours(&config.Config{}))
	// 非法小时丢弃、去重、排序。
	cfg := &config.Config{}
	cfg.Gateway.Workbuddy.CheckinHours = []int{21, 5, 21, 99, -1}
	require.Equal(t, []int{5, 21}, workbuddyCheckinHours(cfg))
	// 全非法 → nil（视为关闭）。
	cfg.Gateway.Workbuddy.CheckinHours = []int{42}
	require.Nil(t, workbuddyCheckinHours(cfg))
}
