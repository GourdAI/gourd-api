//go:build unit

package service

// qoder_identity_heal_test.go 回归测试：COSY 身份 uid 自愈（105 Login expired 根因修复）。
//
// 背景（2026-09-22 实测差分实验坐实）：userinfo 响应键为 id/username（初版误按
// userId 解析致 uid 恒空）；uid 缺失时 COSY 会话/cosy-user 回落伪值 "cred:<token>"，
// 上游 chat 网关恒返 {"code":"105","message":"Login expired"}（HTTP 200 信封包 403），
// 而真实 uid 则 200 正常流。修复 = 解析真值键 + 出站/测试路径身份自愈回填并持久化。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// qoderHealTestUpstreamServer 起一个假 Qoder 上游：GET userinfo 回身份真值、
// POST chat 回 SSE 信封流；记录 userinfo 命中次数与 chat 请求头。
func qoderHealTestUpstreamServer(t *testing.T, userinfoStatus int, userinfoBody string, uid string) (*httptest.Server, *struct {
	mu            sync.Mutex
	userinfoHits  int
	chatCosyUsers []string
}) {
	t.Helper()
	rec := &struct {
		mu            sync.Mutex
		userinfoHits  int
		chatCosyUsers []string
	}{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/userinfo" {
			rec.mu.Lock()
			rec.userinfoHits++
			rec.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(userinfoStatus)
			_, _ = w.Write([]byte(userinfoBody))
			return
		}
		if strings.Contains(r.URL.Path, "agent_chat_generation") {
			rec.mu.Lock()
			rec.chatCosyUsers = append(rec.chatCosyUsers, r.Header.Get("Cosy-User"))
			rec.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			inner := `{"id":"chat-h","choices":[{"index":0,"delta":{"role":"assistant","content":"pong"},"finish_reason":null}]}`
			env := map[string]any{"headers": map[string]any{}, "body": inner, "statusCodeValue": 200}
			raw, _ := json.Marshal(env)
			_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
			_, _ = w.Write([]byte("\n\ndata: [DONE]\n\n"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	return server, rec
}

const qoderRealUserinfoBody = `{"id":"uid-real-1","name":"login@example.com","username":"nick-1","avatar":"","source":"sso.aliyun","organization_id":"","organization_name":""}`

// qoderHealStubFetcher 返回固定身份的 userinfo fetcher stub（模拟官方 userinfo
// 响应解析结果；err 非 nil 时模拟拉取失败）。
func qoderHealStubFetcher(calls *int, mu *sync.Mutex, err error) qoderUserinfoFetcher {
	return func(ctx context.Context, token, userinfoURL string) (*qoderUserinfoResponse, error) {
		if mu != nil {
			mu.Lock()
			*calls++
			mu.Unlock()
		} else {
			*calls++
		}
		if err != nil {
			return nil, err
		}
		return &qoderUserinfoResponse{ID: "uid-real-1", Name: "login@example.com", Username: "nick-1"}, nil
	}
}

// TestQoderHealIdentityBackfillsUIDAndPersists 核心回归：uid 缺失时出站自愈——
// 拉 userinfo 回填 uid/nickname、持久化到仓库、且 chat 请求 cosy-user 为真实 uid。
func TestQoderHealIdentityBackfillsUIDAndPersists(t *testing.T) {
	t.Parallel()
	server, rec := qoderHealTestUpstreamServer(t, http.StatusOK, qoderRealUserinfoBody, "uid-real-1")
	upstream := &workbuddyTestUpstream{client: server.Client()}
	repo := &workbuddyTestAccountRepo{}
	svc := newWorkbuddyTestService(upstream, repo)
	var healCalls int
	svc.qoderUserinfoFetch = qoderHealStubFetcher(&healCalls, nil, nil)
	account := &Account{
		ID:          901,
		Platform:    PlatformQoder,
		Credentials: map[string]any{"access_token": "dt-heal", "base_url": server.URL},
	}

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendQoderUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"auto","messages":[{"role":"user","content":"ping"}]}`), true)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// userinfo 被拉取一次，chat 的 cosy-user 为真实 uid（而非 cred: 伪值）。
	rec.mu.Lock()
	require.Equal(t, []string{"uid-real-1"}, rec.chatCosyUsers, "cosy-user 必须为真实 uid")
	rec.mu.Unlock()
	require.Equal(t, 1, healCalls, "uid 缺失必须拉一次 userinfo")

	// 凭据写回：uid + nickname（username 优先）。
	require.Equal(t, 1, repo.calls(), "自愈必须持久化凭据")
	require.Equal(t, "uid-real-1", repo.lastCredentials["uid"])
	require.Equal(t, "nick-1", repo.lastCredentials["nickname"])
}

// TestQoderHealIdentitySkipsWhenUIDPresent 反向保护：uid 已存在时零额外请求、零写回。
func TestQoderHealIdentitySkipsWhenUIDPresent(t *testing.T) {
	t.Parallel()
	server, rec := qoderHealTestUpstreamServer(t, http.StatusOK, qoderRealUserinfoBody, "uid-real-1")
	upstream := &workbuddyTestUpstream{client: server.Client()}
	repo := &workbuddyTestAccountRepo{}
	svc := newWorkbuddyTestService(upstream, repo)
	var skipCalls int
	svc.qoderUserinfoFetch = qoderHealStubFetcher(&skipCalls, nil, nil)
	account := &Account{
		ID:          902,
		Platform:    PlatformQoder,
		Credentials: map[string]any{"access_token": "dt-ok", "uid": "u-fixed", "base_url": server.URL},
	}

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendQoderUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"auto","messages":[{"role":"user","content":"ping"}]}`), true)
	require.NoError(t, err)
	defer resp.Body.Close()

	rec.mu.Lock()
	require.Equal(t, 0, rec.userinfoHits, "uid 已存在不得拉 userinfo")
	require.Equal(t, []string{"u-fixed"}, rec.chatCosyUsers)
	rec.mu.Unlock()
	require.Equal(t, 0, repo.calls(), "uid 已存在不得写回凭据")
	require.Equal(t, 0, skipCalls, "uid 已存在不得调 fetcher")
}

// TestQoderHealIdentityUserinfoFailureNonBlocking 自愈失败不阻断主流程：
// userinfo 500 时仍正常发 chat（cosy-user 回落伪值，与修复前行为一致）。
func TestQoderHealIdentityUserinfoFailureNonBlocking(t *testing.T) {
	t.Parallel()
	server, rec := qoderHealTestUpstreamServer(t, http.StatusInternalServerError, `{"error":"boom"}`, "")
	upstream := &workbuddyTestUpstream{client: server.Client()}
	repo := &workbuddyTestAccountRepo{}
	svc := newWorkbuddyTestService(upstream, repo)
	var failCalls int
	svc.qoderUserinfoFetch = qoderHealStubFetcher(&failCalls, nil, errors.New("userinfo unavailable"))
	account := &Account{
		ID:          903,
		Platform:    PlatformQoder,
		Credentials: map[string]any{"access_token": "dt-heal-fail", "base_url": server.URL},
	}

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendQoderUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"auto","messages":[{"role":"user","content":"ping"}]}`), true)
	require.NoError(t, err, "userinfo 失败不得阻断出站")
	defer resp.Body.Close()

	rec.mu.Lock()
	require.Equal(t, []string{qoderFingerprintSeed("", "dt-heal-fail")}, rec.chatCosyUsers, "自愈失败回落伪值（与修复前一致）")
	rec.mu.Unlock()
	require.Equal(t, 1, failCalls)
	require.Equal(t, 0, repo.calls(), "自愈失败不得写回")
}

// TestQoderUserinfoResponseParsesRealKeys 契约回归：userinfo 响应键为 id/username
// （初版误写 userId 致 uid 恒空——105 根因），真值样本必须解析出 uid 与昵称。
func TestQoderUserinfoResponseParsesRealKeys(t *testing.T) {
	t.Parallel()
	var ui qoderUserinfoResponse
	require.NoError(t, json.Unmarshal([]byte(qoderRealUserinfoBody), &ui))
	require.Equal(t, "uid-real-1", ui.ID, "uid 键为 id（非 userId）")
	require.Equal(t, "login@example.com", ui.Name)
	require.Equal(t, "nick-1", ui.Username)
	require.Equal(t, "nick-1", firstNonEmptyQoder(strings.TrimSpace(ui.Username), strings.TrimSpace(ui.Name)), "昵称 username 优先")
}

// qoderHealRecordingRepo 记录 SetError 调用的仓库 stub（测试路径错误标记断言用）。
type qoderHealRecordingRepo struct {
	workbuddyTestAccountRepo
	mu            sync.Mutex
	setErrorCalls int
	lastErrorMsg  string
}

func (r *qoderHealRecordingRepo) SetError(ctx context.Context, id int64, errorMsg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setErrorCalls++
	r.lastErrorMsg = errorMsg
	return nil
}

// TestProcessQoderTestStreamMarksAccountOnErrorFrame 测试路径可见性回归：
// SSE error 帧（105 等）必须标记账号错误态，管理页可见失败原因。
func TestProcessQoderTestStreamMarksAccountOnErrorFrame(t *testing.T) {
	t.Parallel()
	repo := &qoderHealRecordingRepo{}
	svc := &AccountTestService{accountRepo: repo}
	account := &Account{ID: 904, Platform: PlatformQoder, Credentials: map[string]any{"access_token": "dt-x"}}

	c := newWorkbuddyTestGinContext()
	body := strings.NewReader("data: {\"error\":{\"code\":\"105\",\"message\":\"Login expired\"}}\n\n")
	err := svc.processQoderTestStream(c, account, body)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Login expired")

	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Equal(t, 1, repo.setErrorCalls, "error 帧必须标记账号错误态")
	require.Contains(t, repo.lastErrorMsg, "Login expired")
}

// TestQoderAccountTestConnectionHealsUID 端到端回归：账号连接测试路径同样自愈——
// uid 缺失时先拉 userinfo 回填，chat 请求 cosy-user 为真实 uid，凭据持久化。
func TestQoderAccountTestConnectionHealsUID(t *testing.T) {
	t.Parallel()
	server, rec := qoderHealTestUpstreamServer(t, http.StatusOK, qoderRealUserinfoBody, "uid-real-1")
	repo := &qoderHealRecordingRepo{}
	svc := &AccountTestService{
		accountRepo:  repo,
		httpUpstream: &workbuddyTestUpstream{client: server.Client()},
		cfg: &config.Config{Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
		}},
	}
	var testHealCalls int
	svc.qoderUserinfoFetch = qoderHealStubFetcher(&testHealCalls, nil, nil)
	account := &Account{
		ID:          905,
		Platform:    PlatformQoder,
		Credentials: map[string]any{"access_token": "dt-test-heal", "base_url": server.URL},
	}

	c := newWorkbuddyTestGinContext()
	require.NoError(t, svc.testQoderAccountConnection(c, account, "", "hi"))

	rec.mu.Lock()
	require.Equal(t, []string{"uid-real-1"}, rec.chatCosyUsers)
	rec.mu.Unlock()
	require.Equal(t, 1, testHealCalls, "测试路径 uid 缺失必须自愈")
	require.Equal(t, 1, repo.calls(), "测试路径自愈必须持久化")
	require.Equal(t, "uid-real-1", repo.lastCredentials["uid"])
}
