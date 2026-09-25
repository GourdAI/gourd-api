//go:build unit

package service

// workbuddy_gateway_test.go WorkBuddy 网关集成单测：raw-CC 分流判定、CC 回退
// 发送分发、CC 入站转发（流式透传/非流式聚合）与 Responses 形状兼容转换。

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

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newWorkbuddyForwardGinContext(body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	return c, rec
}

// TestShouldForwardOpenAIResponsesViaRawChatCompletionsWorkbuddy 分流判定：
// workbuddy 上游只有 /v2/chat/completions，三条入站路径都必须走 raw-CC 回退。
func TestShouldForwardOpenAIResponsesViaRawChatCompletionsWorkbuddy(t *testing.T) {
	t.Parallel()
	account := &Account{ID: 901, Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey}
	require.True(t, shouldForwardOpenAIResponsesViaRawChatCompletions(account))

	// 非 APIKey 类型维持既有拒绝语义（workbuddy 实际恒为 APIKey）。
	oauth := &Account{ID: 902, Platform: PlatformWorkbuddy, Type: AccountTypeOAuth}
	require.False(t, shouldForwardOpenAIResponsesViaRawChatCompletions(oauth))
}

// TestSendCCUpstreamForAccountWorkbuddyRouting CC 回退发送分发：workbuddy 账号
// 必须路由到 workbuddy 专用发送管线（/v2/chat/completions + workbuddy 头）。
func TestSendCCUpstreamForAccountWorkbuddyRouting(t *testing.T) {
	t.Parallel()
	var (
		mu           sync.Mutex
		gotPath      string
		gotAuth      string
		gotCodeBuddy string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCodeBuddy = r.Header.Get("X-CodeBuddy-Request")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, workbuddyTestSSEBody)
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := workbuddyTestAccount(903, server.URL, map[string]any{"access_token": "tok-903", "uid": "u-903"})

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendCCUpstreamForAccount(context.Background(), c, account,
		[]byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"ping"}]}`), true)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, workbuddyChatPath, gotPath, "workbuddy 账号必须路由到 /v2/chat/completions")
	require.Equal(t, "Bearer tok-903", gotAuth)
	require.Equal(t, "1", gotCodeBuddy, "workbuddy 风控闸门头")
}

// TestSendCCUpstreamForAccountNonWorkbuddyKeepsCCFallback 非 workbuddy 账号维持
// 既有 CC 回退发送（resolveCCFallbackTarget + sendCCUpstreamRequest）。
func TestSendCCUpstreamForAccountNonWorkbuddyKeepsCCFallback(t *testing.T) {
	t.Parallel()
	var (
		mu      sync.Mutex
		gotPath string
		gotAuth string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"gpt-5.4","choices":[]}`)
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := &Account{
		ID: 904, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-cc-904", "base_url": server.URL},
	}

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendCCUpstreamForAccount(context.Background(), c, account, []byte(`{"model":"gpt-5.4"}`), true)
	require.NoError(t, err)
	defer resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "/v1/chat/completions", gotPath)
	require.Equal(t, "Bearer sk-cc-904", gotAuth)
}

// TestForwardWorkbuddyChatCompletionsNonStreamAggregation 非流式：上游 SSE 全流
// 聚合为 CC JSON 透传客户端（含 usage），并补齐 UpstreamEndpoint。
func TestForwardWorkbuddyChatCompletionsNonStreamAggregation(t *testing.T) {
	t.Parallel()
	var (
		mu      sync.Mutex
		gotPath string
		gotBody string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath = r.URL.Path
		gotBody = string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, workbuddyTestSSEBody)
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := workbuddyTestAccount(905, server.URL, map[string]any{"access_token": "tok-905", "uid": "u-905"})

	body := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"ping"}],"stream":false}`)
	c, rec := newWorkbuddyForwardGinContext(body)

	result, err := svc.forwardWorkbuddyChatCompletions(context.Background(), c, account, body, "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, rec.Code)

	got := rec.Body.Bytes()
	require.Equal(t, "chat.completion", gjson.GetBytes(got, "object").String())
	require.Equal(t, "pong", gjson.GetBytes(got, "choices.0.message.content").String())
	require.Equal(t, "stop", gjson.GetBytes(got, "choices.0.finish_reason").String())
	require.Equal(t, int64(5), gjson.GetBytes(got, "usage.total_tokens").Int(), "prompt 3 + completion 2 补齐 total")

	require.Equal(t, 3, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.False(t, result.Stream)
	require.Equal(t, workbuddyChatPath, result.UpstreamEndpoint)
	require.Equal(t, workbuddyChatPath, GetActualOpenAIUpstreamEndpoint(c))

	// 出站 body 已经过 workbuddy 协议变换（强制 stream、cache key 注入）。
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, workbuddyChatPath, gotPath)
	require.True(t, gjson.Get(gotBody, "stream").Bool())
	require.NotEmpty(t, gjson.Get(gotBody, "prompt_cache_key").String())
}

// TestForwardWorkbuddyChatCompletionsStreamPassthrough 流式：规范化 SSE 逐帧透传，
// 恰好一个 [DONE]。
func TestForwardWorkbuddyChatCompletionsStreamPassthrough(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, workbuddyTestSSEBody)
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := workbuddyTestAccount(906, server.URL, map[string]any{"access_token": "tok-906", "uid": "u-906"})

	body := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"ping"}],"stream":true}`)
	c, rec := newWorkbuddyForwardGinContext(body)

	result, err := svc.forwardWorkbuddyChatCompletions(context.Background(), c, account, body, "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)
	require.Equal(t, workbuddyChatPath, result.UpstreamEndpoint)

	got := rec.Body.String()
	require.Contains(t, got, `"content":"pong"`)
	require.Equal(t, 1, strings.Count(got, "data: [DONE]"), "恰好一个 [DONE]")
}

// TestForwardWorkbuddyChatCompletionsResponsesShapeConversion Responses 形状
// （input 数组、无 messages）入站：先转换为 CC 再出站。
func TestForwardWorkbuddyChatCompletionsResponsesShapeConversion(t *testing.T) {
	t.Parallel()
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, workbuddyTestSSEBody)
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := workbuddyTestAccount(907, server.URL, map[string]any{"access_token": "tok-907", "uid": "u-907"})

	body := []byte(`{"model":"glm-5.3","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello cursor"}]}]}`)
	c, rec := newWorkbuddyForwardGinContext(body)

	result, err := svc.forwardWorkbuddyChatCompletions(context.Background(), c, account, body, "")
	require.NoError(t, err)
	require.NotNil(t, result)

	// 上游收到的是转换后的 CC body：messages 存在、input 不再出现、模型名保留。
	require.Equal(t, "glm-5.3", gjson.GetBytes(gotBody, "model").String())
	require.True(t, gjson.GetBytes(gotBody, "messages").Exists())
	require.False(t, gjson.GetBytes(gotBody, "input").Exists())
	require.Equal(t, "user", gjson.GetBytes(gotBody, "messages.0.role").String())
	require.Contains(t, string(gotBody), "hello cursor")
	require.True(t, gjson.GetBytes(gotBody, "stream").Bool(), "workbuddy 上游强制流式")

	// 客户端收到聚合 JSON（转换链不变，仍是 CC 响应）。
	require.Equal(t, "pong", gjson.GetBytes(rec.Body.Bytes(), "choices.0.message.content").String())
}

// TestForwardWorkbuddyChatCompletionsMissingModel 缺 model 时 400。
func TestForwardWorkbuddyChatCompletionsMissingModel(t *testing.T) {
	t.Parallel()
	upstream := &workbuddyTestUpstream{client: http.DefaultClient}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := workbuddyTestAccount(908, "http://127.0.0.1:1", map[string]any{"access_token": "tok-908"})

	body := []byte(`{"messages":[{"role":"user","content":"ping"}],"stream":false}`)
	c, rec := newWorkbuddyForwardGinContext(body)

	result, err := svc.forwardWorkbuddyChatCompletions(context.Background(), c, account, body, "")
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_request_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "model is required", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
}

// TestTestWorkbuddyAccountConnectionSuccess 账号连接测试：经 workbuddy 协议头
// 发出请求并读取规范化 SSE，最终发出 test_complete 事件。
func TestTestWorkbuddyAccountConnectionSuccess(t *testing.T) {
	t.Parallel()
	var (
		mu      sync.Mutex
		gotPath string
		gotAuth string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, workbuddyTestSSEBody)
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          rawChatCompletionsTestConfig(),
		accountRepo:  &workbuddyTestAccountRepo{},
	}
	account := workbuddyTestAccount(909, server.URL, map[string]any{"access_token": "tok-909", "uid": "u-909"})

	c, rec := newWorkbuddyForwardGinContext(nil)
	require.NoError(t, svc.testWorkbuddyAccountConnection(c, account, "glm-5.3", "hi"))

	body := rec.Body.String()
	require.Contains(t, body, `"type":"test_start"`)
	require.Contains(t, body, `"text":"pong"`)
	require.Contains(t, body, `"type":"test_complete"`)
	require.NotContains(t, body, `"type":"error"`)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, workbuddyChatPath, gotPath)
	require.Equal(t, "Bearer tok-909", gotAuth)
}

// TestTestWorkbuddyAccountConnectionPreflightRefresh 账号测试的预刷新：
// access_token 临近过期 + refresh_token 存在 → 先刷新再测试，并写回凭据。
func TestTestWorkbuddyAccountConnectionPreflightRefresh(t *testing.T) {
	t.Parallel()
	var (
		mu          sync.Mutex
		refreshHits int
		chatAuth    string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case workbuddyRefreshPath:
			mu.Lock()
			refreshHits++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"data":{"accessToken":"tok-fresh","expiresIn":3600}}`)
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
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          rawChatCompletionsTestConfig(),
		accountRepo:  repo,
	}
	account := workbuddyTestAccount(910, server.URL, map[string]any{
		"access_token":  "tok-old",
		"refresh_token": "rt-910",
		"uid":           "u-910",
		"expires_at":    time.Now().Add(2 * time.Minute).Unix(),
	})

	c, rec := newWorkbuddyForwardGinContext(nil)
	require.NoError(t, svc.testWorkbuddyAccountConnection(c, account, "", ""))

	require.Contains(t, rec.Body.String(), `"type":"test_complete"`)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, refreshHits)
	require.Equal(t, "Bearer tok-fresh", chatAuth, "chat 出站使用刷新后的 token")
	require.Equal(t, 1, repo.calls(), "刷新写回凭据")
}
