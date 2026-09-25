//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// workbuddy_client_test.go 出站发送与 token 刷新的单测（httptest 假上游）：
// 发送路径与头断言、401→刷新→重试、非流式聚合、刷新写回与并发单次刷新。

// workbuddyTestUpstream 以真实 HTTP 客户端执行请求的 HTTPUpstream 假实现，
// 记录每次出站请求供断言（真实打到 httptest 上游）。
type workbuddyTestUpstream struct {
	mu       sync.Mutex
	client   *http.Client
	requests []*http.Request
	bodies   []string
}

func (u *workbuddyTestUpstream) Do(req *http.Request, proxyURL string, accountID int64, concurrency int) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	u.mu.Lock()
	u.requests = append(u.requests, req)
	u.bodies = append(u.bodies, string(body))
	u.mu.Unlock()
	return u.client.Do(req)
}

func (u *workbuddyTestUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func (u *workbuddyTestUpstream) snapshot() ([]*http.Request, []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]*http.Request(nil), u.requests...), append([]string(nil), u.bodies...)
}

// workbuddyTestAccountRepo 记录凭据写回的账号仓库假实现。
type workbuddyTestAccountRepo struct {
	mockAccountRepoForGemini
	mu              sync.Mutex
	updateCalls     int
	lastCredentials map[string]any
}

func (r *workbuddyTestAccountRepo) UpdateCredentials(ctx context.Context, id int64, credentials map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updateCalls++
	r.lastCredentials = shallowCopyMap(credentials)
	if acc, ok := r.accountsByID[id]; ok && acc != nil {
		acc.Credentials = shallowCopyMap(credentials)
	}
	return nil
}

func (r *workbuddyTestAccountRepo) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.updateCalls
}

func newWorkbuddyTestService(upstream HTTPUpstream, repo AccountRepository) *OpenAIGatewayService {
	return &OpenAIGatewayService{
		accountRepo:  repo,
		httpUpstream: upstream,
		cfg: &config.Config{
			Security: config.SecurityConfig{
				// 测试用 httptest（http://127.0.0.1）：关闭白名单并允许 http。
				URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
			},
		},
	}
}

func newWorkbuddyTestGinContext() *gin.Context {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c
}

func workbuddyTestAccount(id int64, serverURL string, creds map[string]any) *Account {
	if creds == nil {
		creds = map[string]any{}
	}
	creds["base_url"] = serverURL
	return &Account{ID: id, Platform: PlatformWorkbuddy, Credentials: creds}
}

const workbuddyTestSSEBody = `data: {"id":"chat-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"pong"},"finish_reason":null}]}` + "\n\n" +
	`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}` + "\n\n" +
	"data: [DONE]\n\n"

func TestSendWorkbuddyUpstreamRequestStreamingPathAndHeaders(t *testing.T) {
	t.Parallel()
	var (
		mu       sync.Mutex
		gotPath  string
		gotAuth  string
		gotUA    string
		gotReqID string
		gotBody  string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotReqID = r.Header.Get("X-CodeBuddy-Request")
		gotBody = string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, workbuddyTestSSEBody)
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	repo := &workbuddyTestAccountRepo{}
	svc := newWorkbuddyTestService(upstream, repo)
	account := workbuddyTestAccount(701, server.URL, map[string]any{
		"access_token": "tok-1",
		"uid":          "u-701",
		"realm":        "cn",
	})

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendWorkbuddyUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"ping"}]}`), true)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"content":"pong"`)
	require.Equal(t, 1, strings.Count(string(raw), "data: [DONE]"), "恰好一个 [DONE]")

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, workbuddyChatPath, gotPath, "路径为 /v2/chat/completions")
	require.Equal(t, "Bearer tok-1", gotAuth)
	require.Equal(t, workbuddyUAFor("cn"), gotUA)
	require.Equal(t, "1", gotReqID)
	require.True(t, gjson.Get(gotBody, "stream").Bool(), "上游 body 被强制 stream:true")
	require.True(t, gjson.Get(gotBody, "stream_options.include_usage").Bool())
	require.NotEmpty(t, gjson.Get(gotBody, "prompt_cache_key").String(), "注入了 prompt_cache_key")

	reqs, _ := upstream.snapshot()
	require.Len(t, reqs, 1)
	require.Equal(t, "application/json, text/event-stream", reqs[0].Header.Get("Accept"))
	require.Equal(t, "https://www.codebuddy.cn", reqs[0].Header.Get("Origin"))
	require.True(t, workbuddyValidTraceID(reqs[0].Header.Get("X-Conversation-Request-ID")))
	require.Equal(t, reqs[0].Header.Get("X-Conversation-Message-ID"), reqs[0].Header.Get("X-Request-ID"))
	require.Equal(t, 0, repo.calls(), "无刷新场景不写凭据")
}

func TestSendWorkbuddyUpstreamRequest401RefreshRetry(t *testing.T) {
	t.Parallel()
	var (
		mu          sync.Mutex
		chatAuths   []string
		refreshHits int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case workbuddyRefreshPath:
			mu.Lock()
			refreshHits++
			mu.Unlock()
			require.Equal(t, "rt-1", r.Header.Get("X-Refresh-Token"))
			require.Equal(t, "plugin", r.Header.Get("X-Auth-Refresh-Source"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"accessToken":"tok-2","refreshToken":"rt-2","expiresIn":3600,"domain":"copilot.tencent.com"}}`)
		case workbuddyChatPath:
			auth := r.Header.Get("Authorization")
			mu.Lock()
			chatAuths = append(chatAuths, auth)
			mu.Unlock()
			if auth == "Bearer tok-2" {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, workbuddyTestSSEBody)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"token expired","code":"401"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	repo := &workbuddyTestAccountRepo{}
	svc := newWorkbuddyTestService(upstream, repo)
	// expires_at 在未来：预刷新不触发，首次 chat 以旧 token 出站 → 401 → 刷新 → 重试。
	account := workbuddyTestAccount(702, server.URL, map[string]any{
		"access_token":  "tok-1",
		"refresh_token": "rt-1",
		"uid":           "u-702",
		"expires_at":    time.Now().Add(time.Hour).Unix(),
	})

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendWorkbuddyUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"ping"}]}`), true)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"Bearer tok-1", "Bearer tok-2"}, chatAuths, "首次旧 token、重试新 token")
	require.Equal(t, 1, refreshHits, "401 后刷新一次")
	// 刷新写回：内存凭据与仓库调用。
	require.Equal(t, "tok-2", account.GetWorkbuddyCredentials().AccessToken)
	require.Equal(t, "rt-2", account.GetWorkbuddyCredentials().RefreshToken)
	require.Equal(t, 1, repo.calls())
	require.Equal(t, "tok-2", repo.lastCredentials["access_token"])
}

func TestSendWorkbuddyUpstreamRequestPreflightRefresh(t *testing.T) {
	t.Parallel()
	var refreshHits int
	var chatAuth string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case workbuddyRefreshPath:
			mu.Lock()
			refreshHits++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"data":{"accessToken":"tok-fresh","expiresIn":7200}}`)
		case workbuddyChatPath:
			mu.Lock()
			chatAuth = r.Header.Get("Authorization")
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, workbuddyTestSSEBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	repo := &workbuddyTestAccountRepo{}
	svc := newWorkbuddyTestService(upstream, repo)
	// access_token 已临近过期（10min skew 内）→ 预刷新。
	account := workbuddyTestAccount(703, server.URL, map[string]any{
		"access_token":  "tok-old",
		"refresh_token": "rt-703",
		"expires_at":    time.Now().Add(2 * time.Minute).Unix(),
	})

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendWorkbuddyUpstreamRequest(context.Background(), c, account, []byte(`{"model":"glm-5.3"}`), true)
	require.NoError(t, err)
	defer resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, refreshHits)
	require.Equal(t, "Bearer tok-fresh", chatAuth, "chat 出站使用刷新后的 token")
}

func TestSendWorkbuddyUpstreamRequestNonStreamAggregation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, workbuddyChatPath, r.URL.Path)
		require.True(t, gjson.Get(readBody(t, r), "stream").Bool())
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, workbuddyTestSSEBody)
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := workbuddyTestAccount(704, server.URL, map[string]any{"access_token": "tok-704", "uid": "u-704"})

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendWorkbuddyUpstreamRequest(context.Background(), c, account, []byte(`{"model":"glm-5.3"}`), false)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "chat.completion", gjson.GetBytes(raw, "object").String())
	require.Equal(t, "pong", gjson.GetBytes(raw, "choices.0.message.content").String())
	require.Equal(t, "stop", gjson.GetBytes(raw, "choices.0.finish_reason").String())
	require.Equal(t, int64(5), gjson.GetBytes(raw, "usage.total_tokens").Int(), "prompt 3 + completion 2 补齐 total")
}

func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	return string(body)
}

// TestSendWorkbuddyUpstreamRequest401RetryNonStreamAggregation 401→刷新→重试成功后，
// 非流式路径仍必须把整流体聚合为 CC JSON（早期实现在重试成功后直接 return，
// 把原始 SSE 文本当作 JSON 回给客户端——回归锁定）。
func TestSendWorkbuddyUpstreamRequest401RetryNonStreamAggregation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case workbuddyRefreshPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"accessToken":"tok-2","refreshToken":"rt-2","expiresIn":3600}}`)
		case workbuddyChatPath:
			if r.Header.Get("Authorization") != "Bearer tok-2" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"token expired","code":"401"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, workbuddyTestSSEBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := workbuddyTestAccount(709, server.URL, map[string]any{
		"access_token":  "tok-1",
		"refresh_token": "rt-709",
		"uid":           "u-709",
		"expires_at":    time.Now().Add(time.Hour).Unix(),
	})

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendWorkbuddyUpstreamRequest(context.Background(), c, account, []byte(`{"model":"glm-5.3"}`), false)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "chat.completion", gjson.GetBytes(raw, "object").String(), "重试成功后仍聚合为 CC JSON")
	require.Equal(t, "pong", gjson.GetBytes(raw, "choices.0.message.content").String())
}

func TestRefreshWorkbuddyTokenWritesBack(t *testing.T) {
	t.Parallel()
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, workbuddyRefreshPath, r.URL.Path)
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"accessToken":"tok-9","refreshToken":"rt-9","expiresIn":7200,"domain":"www.workbuddy.ai"}}`)
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	repo := &workbuddyTestAccountRepo{}
	svc := newWorkbuddyTestService(upstream, repo)
	account := workbuddyTestAccount(705, server.URL, map[string]any{
		"access_token":  "tok-old",
		"refresh_token": "rt-old",
		"uid":           "u-705",
		"realm":         "global",
	})

	require.NoError(t, svc.refreshWorkbuddyToken(context.Background(), account))

	creds := account.GetWorkbuddyCredentials()
	require.Equal(t, "tok-9", creds.AccessToken)
	require.Equal(t, "rt-9", creds.RefreshToken)
	require.Equal(t, "www.workbuddy.ai", creds.Domain)
	require.Greater(t, creds.ExpiresAt, time.Now().Add(time.Hour).Unix(), "expires_at = now + expiresIn")
	require.Equal(t, 1, repo.calls())
	require.Equal(t, "tok-9", repo.lastCredentials["access_token"])
	require.Equal(t, "rt-9", repo.lastCredentials["refresh_token"])

	require.Equal(t, "rt-old", gotHeaders.Get("X-Refresh-Token"))
	require.Equal(t, "plugin", gotHeaders.Get("X-Auth-Refresh-Source"))
	require.Equal(t, workbuddyUAFor("global"), gotHeaders.Get("User-Agent"))
	require.Equal(t, "1", gotHeaders.Get("X-CodeBuddy-Request"))
}

func TestRefreshWorkbuddyTokenErrorCode(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":40001,"msg":"refresh token expired"}`)
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	repo := &workbuddyTestAccountRepo{}
	svc := newWorkbuddyTestService(upstream, repo)
	account := workbuddyTestAccount(706, server.URL, map[string]any{"access_token": "tok", "refresh_token": "rt-706"})

	err := svc.refreshWorkbuddyToken(context.Background(), account)
	require.Error(t, err)
	require.Contains(t, err.Error(), "code=40001")
	require.Contains(t, err.Error(), "refresh token expired")
	require.Equal(t, 0, repo.calls(), "失败不写库")
	require.Equal(t, "tok", account.GetWorkbuddyCredentials().AccessToken, "失败不改内存凭据")
}

func TestRefreshWorkbuddyTokenConcurrentSingleRefresh(t *testing.T) {
	t.Parallel()
	var (
		mu          sync.Mutex
		refreshHits int
	)
	refreshStarted := make(chan struct{}, 4)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		refreshHits++
		mu.Unlock()
		refreshStarted <- struct{}{}
		<-release // 阻塞至测试放行：保证第二个 goroutine 在锁外捕获旧 token 快照
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"accessToken":"tok-2","refreshToken":"rt-2","expiresIn":3600}}`)
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	repo := &workbuddyTestAccountRepo{}
	svc := newWorkbuddyTestService(upstream, repo)
	account := workbuddyTestAccount(707, server.URL, map[string]any{"access_token": "tok-1", "refresh_token": "rt-707"})

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs[0] = svc.refreshWorkbuddyToken(context.Background(), account)
	}()
	<-refreshStarted // 第一个刷新已进入网络 I/O（持锁）
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs[1] = svc.refreshWorkbuddyToken(context.Background(), account)
	}()
	time.Sleep(50 * time.Millisecond) // 第二个 goroutine 完成快照捕获并阻塞在锁上
	close(release)
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, refreshHits, "同一账号并发刷新只做一次")
	require.Equal(t, "tok-2", account.GetWorkbuddyCredentials().AccessToken)
	require.Equal(t, 1, repo.calls())
}

func TestSendWorkbuddyUpstreamRequestTransportError(t *testing.T) {
	t.Parallel()
	// 上游不可达（端口关闭）：传输层错误经 handleOpenAIUpstreamTransportError 归一。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	serverURL := server.URL
	server.Close() // 立刻关闭：连接拒绝

	upstream := &workbuddyTestUpstream{client: http.DefaultClient}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := workbuddyTestAccount(708, serverURL, map[string]any{"access_token": "tok-708"})

	c := newWorkbuddyTestGinContext()
	_, err := svc.sendWorkbuddyUpstreamRequest(context.Background(), c, account, []byte(`{"model":"glm-5.3"}`), true)
	require.Error(t, err)
}
