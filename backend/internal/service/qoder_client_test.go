//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// qoder_client_test.go 出站发送单测（httptest 假上游）：请求体被 Encode 可解码、
// COSY 签名头存在且形态正确、x-model-key 头正确、凭据缺失报错、非流式聚合。

// TestSendQoderUpstreamRequestStreamingEncodeAndHeaders 验证：
// 请求体被 COSY Encode（可 Decode 回 JSON）、签名头形态 Bearer COSY.x.y、
// x-model-key 头正确、SSE 规范化输出恰好一个 [DONE]。
func TestSendQoderUpstreamRequestStreamingEncodeAndHeaders(t *testing.T) {
	t.Parallel()
	var (
		mu        sync.Mutex
		gotPath   string
		gotQuery  string
		gotBody   string
		gotAuth   string
		gotKey    string
		gotSource string
		gotUA     string
		gotPolicy string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotBody = string(body)
		gotAuth = r.Header.Get("Authorization")
		gotKey = r.Header.Get("X-Model-Key")
		gotSource = r.Header.Get("X-Model-Source")
		gotUA = r.Header.Get("User-Agent")
		gotPolicy = r.Header.Get("Cosy-Data-Policy")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		inner := `{"id":"chat-1","choices":[{"index":0,"delta":{"role":"assistant","content":"pong"},"finish_reason":null}]}`
		env := map[string]any{"headers": map[string]any{}, "body": inner, "statusCodeValue": 200}
		raw, _ := jsonMarshalQoder(env)
		_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
		_, _ = w.Write([]byte("\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := &Account{
		ID:          801,
		Platform:    PlatformQoder,
		Credentials: map[string]any{"access_token": "dt-tok-1", "uid": "u-801", "base_url": server.URL},
	}

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendQoderUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"auto","messages":[{"role":"user","content":"ping"}]}`), true)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out := string(raw)
	require.Contains(t, out, `"content":"pong"`)
	require.Equal(t, 1, strings.Count(out, "data: [DONE]"), "恰好一个 [DONE]")

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, qoderChatPath, gotPath, "路径为 /algo/api/v2/service/pro/sse/agent_chat_generation")
	require.Contains(t, gotQuery, "FetchKeys=llm_model_result")
	require.Contains(t, gotQuery, "AgentId=agent_common")
	require.Contains(t, gotQuery, "Encode=1")

	// 请求体被 COSY Encode：Decode 回 JSON 且 stream=true。
	decoded, decErr := QoderCosyDecode(gotBody)
	require.NoError(t, decErr, "上游 body 必须是 COSY 编码形态")
	require.True(t, gjson.Get(string(decoded), "stream").Bool(), "解码后 stream=true")
	require.NotEmpty(t, gjson.Get(string(decoded), "request_id").String())
	require.NotContains(t, string(decoded), "{UUID")

	// 签名头形态与模型头。
	require.True(t, strings.HasPrefix(gotAuth, "Bearer COSY."), "Authorization 形态: %s", gotAuth)
	parts := strings.SplitN(strings.TrimPrefix(gotAuth, "Bearer COSY."), ".", 2)
	require.Len(t, parts, 2)
	require.Len(t, parts[1], 32, "签名段 32 hex")
	require.Equal(t, "auto", gotKey, "x-model-key 为映射后模型")
	require.Equal(t, "system", gotSource)
	require.Equal(t, "Go-http-client/2.0", gotUA)
	require.Equal(t, "agree", gotPolicy)
}

// TestSendQoderUpstreamRequestNonStreamAggregation 非流式：聚合为 CC JSON 假响应。
func TestSendQoderUpstreamRequestNonStreamAggregation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner1 := `{"id":"chat-n","model":"auto","choices":[{"index":0,"delta":{"role":"assistant","content":"he"},"finish_reason":null}]}`
		inner2 := `{"id":"chat-n","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`
		env1 := map[string]any{"headers": map[string]any{}, "body": inner1, "statusCodeValue": 200}
		env2 := map[string]any{"headers": map[string]any{}, "body": inner2, "statusCodeValue": 200}
		raw1, _ := jsonMarshalQoder(env1)
		raw2, _ := jsonMarshalQoder(env2)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + string(raw1) + "\n\n"))
		_, _ = w.Write([]byte("\n\n"))
		_, _ = w.Write([]byte("data: " + string(raw2) + "\n\n"))
		_, _ = w.Write([]byte("\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := &Account{
		ID:          802,
		Platform:    PlatformQoder,
		Credentials: map[string]any{"access_token": "dt-tok-2", "uid": "u-802", "base_url": server.URL},
	}

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendQoderUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"auto","messages":[{"role":"user","content":"ping"}]}`), false)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "chat.completion", gjson.GetBytes(raw, "object").String())
	require.Equal(t, "hello", gjson.GetBytes(raw, "choices.0.message.content").String())
	require.Equal(t, int64(8), gjson.GetBytes(raw, "usage.total_tokens").Int(), "usage 补 total")
}

// TestSendQoderUpstreamRequestMissingCredentials 凭据缺失报错。
func TestSendQoderUpstreamRequestMissingCredentials(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := &Account{ID: 803, Platform: PlatformQoder, Credentials: map[string]any{}}

	c := newWorkbuddyTestGinContext()
	_, err := svc.sendQoderUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"auto","messages":[]}`), true)
	require.Error(t, err, "无 dt- 也无 PAT/刷新令牌必须报错")
	// 非 qoder 账号同样拒绝。
	other := &Account{ID: 804, Platform: PlatformOpenAI, Credentials: map[string]any{"access_token": "x"}}
	_, err = svc.sendQoderUpstreamRequest(context.Background(), c, other,
		[]byte(`{"model":"auto"}`), true)
	require.Error(t, err)
}

// TestSendQoderUpstreamRequestPreflightJobTokenExchange PAT 凭据先 jobToken 交换再发 chat。
func TestSendQoderUpstreamRequestPreflightJobTokenExchange(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var jobTokenHits int
	var chatAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/algo/api/v3/user/jobToken":
			mu.Lock()
			jobTokenHits++
			mu.Unlock()
			// 请求体必须是 COSY 编码（可解码回 inner payload）。
			body, _ := io.ReadAll(r.Body)
			decoded, derr := QoderCosyDecode(string(body))
			require.NoError(t, derr)
			inner := gjson.Get(string(decoded), "payload").String()
			require.NotEmpty(t, inner, "内层 payload 必须存在")
			require.Contains(t, inner, "personalToken")
			require.Contains(t, inner, "pt-tok")
			require.Equal(t, "cosy", r.Header.Get("Appcode"))
			require.Equal(t, "v2", r.Header.Get("Login-Version"))
			require.NotEmpty(t, r.Header.Get("Signature"))
			require.NotEmpty(t, r.Header.Get("Date"))
			// 响应为 Encode 编码的 JSON。
			respInner := map[string]any{
				"name":               "tester",
				"id":                 "uid-9",
				"userType":           "personal_standard",
				"securityOauthToken": "sec-fresh",
				"refreshToken":       "drt-fresh",
			}
			raw, _ := jsonMarshalQoder(respInner)
			_, _ = w.Write([]byte(QoderCosyEncode(raw)))
		case qoderChatPath:
			mu.Lock()
			chatAuth = r.Header.Get("Authorization")
			mu.Unlock()
			inner := `{"id":"chat-x","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`
			env := map[string]any{"headers": map[string]any{}, "body": inner, "statusCodeValue": 200}
			raw, _ := jsonMarshalQoder(env)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
			_, _ = w.Write([]byte("\n\ndata: [DONE]\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	repo := &workbuddyTestAccountRepo{}
	svc := newWorkbuddyTestService(upstream, repo)
	account := &Account{
		ID:       805,
		Platform: PlatformQoder,
		Credentials: map[string]any{
			"personal_token": "pt-tok",
			"uid":            "u-805",
			"base_url":       server.URL,
		},
	}

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendQoderUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"auto","messages":[{"role":"user","content":"ping"}]}`), true)
	require.NoError(t, err)
	defer resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, jobTokenHits, "PAT 预交换一次")
	require.True(t, strings.HasPrefix(chatAuth, "Bearer COSY."), "chat 用 COSY 签名头")
	// 交换写回凭据。
	require.Equal(t, "sec-fresh", account.GetQoderCredentials().AccessToken)
	require.Equal(t, "sec-fresh", repo.lastCredentials["access_token"])
	require.Equal(t, 1, repo.calls())
}

// TestSendQoderUpstreamRequestPATExchangeFailsNoFallback 交换失败且无可用 token → 报错。
func TestSendQoderUpstreamRequestPATExchangeFailsNoFallback(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == qoderJobTokenPath {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"pat rejected"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := &Account{
		ID:          806,
		Platform:    PlatformQoder,
		Credentials: map[string]any{"personal_token": "pt-bad", "base_url": server.URL},
	}

	c := newWorkbuddyTestGinContext()
	_, err := svc.sendQoderUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"auto","messages":[]}`), true)
	require.Error(t, err, "交换失败且无任何可用 token 必须报错")
}

// jsonMarshalQoder 测试辅助：marshal JSON（失败 panic 由测试失败呈现）。
func jsonMarshalQoder(v any) ([]byte, error) {
	return json.Marshal(v)
}
