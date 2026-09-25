//go:build unit

package service

// qoder_model_normalize_test.go Qoder 模型 key 归一化、「模型不一致」豁免与
// 出站 accept 头的回归测试。
//
// 背景（2026-09-22 排查坐实）：
//  1. 账号映射里写了展示名 qwen3.8-flash 时，出站原样发送，上游静默回落
//     （credits=0）——必须归一为官方 key（qfmodel）。
//  2. 上游 chat 端点只接受 accept: text/event-stream；application/json 会直接
//     500 Internal Server Error（实测复现），非流式也必须以 SSE accept 出站。
//  3. 上游响应 model 恒为 "auto"（协议占位值，发任何 key 都如此）→「模型不一致」
//     审计对 Qoder 系统性误报，发送值为官方 key 时应豁免。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNormalizeQoderModelKey 展示名/别名/官方 key 三方归一。
func TestNormalizeQoderModelKey(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"qwen3.8-flash", "qfmodel"},
		{"Qwen3.8-Flash", "qfmodel"},
		{"QWEN3.8-FLASH", "qfmodel"},
		{"  qwen3.8-flash  ", "qfmodel"},
		{"qwen3.8-max", "qmodel_38max"},
		{"qwen3.7-max", "qmodel_latest"},
		{"qwen3.7-plus", "qmodel"},
		{"qwen3.7-flash", "q37fmodel"},
		{"deepseek-v4-pro", "dmodel"},
		{"deepseek-flash", "dfmodel"},
		{"deepseek-v4-flash", "dfmodel"},
		{"glm-5.3", "gmodel"},
		{"glm-5.3-flash", "gfmodel"},
		{"glm-5.2", "gm51model"},
		{"kimi-k3", "kmodel_latest"},
		{"kimi-k2.8-preview", "kmodel"},
		{"kimi-k2.8", "kmodel"},
		{"minimax-m2.7", "mmodel"},
		// 官方 key 直通（含大小写归一）。
		{"qfmodel", "qfmodel"},
		{"QFModel", "qfmodel"},
		{"auto", "auto"},
		{"qmodel_38max", "qmodel_38max"},
		// 未知值原样返回（宽容，交由上游判定）。
		{"unknown-custom-model", "unknown-custom-model"},
		{"", ""},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, normalizeQoderModelKey(tc.in), "normalizeQoderModelKey(%q)", tc.in)
	}
}

// TestIsQoderAutoResponseModel auto 占位值判定（大小写与空白不敏感）。
func TestIsQoderAutoResponseModel(t *testing.T) {
	t.Parallel()
	require.True(t, isQoderAutoResponseModel("auto"))
	require.True(t, isQoderAutoResponseModel("Auto"))
	require.True(t, isQoderAutoResponseModel("AUTO"))
	require.True(t, isQoderAutoResponseModel(" auto "))
	require.False(t, isQoderAutoResponseModel("qfmodel"))
	require.False(t, isQoderAutoResponseModel(""))
}

// TestUpstreamModelMismatchForAccountQoder Qoder auto 豁免矩阵：
//   - 发送官方 key 且响应 auto → 非 nil 且 false（豁免）；
//   - 发送非官方 key（历史坏配置）→ 仍标 true（可审计）；
//   - 响应非 auto → 走通用审计逻辑；
//   - 非 Qoder 账号 → 无豁免；
//   - 响应为空 → nil（三态语义不变）。
func TestUpstreamModelMismatchForAccountQoder(t *testing.T) {
	t.Parallel()
	qoder := &Account{ID: 1, Platform: PlatformQoder}
	openai := &Account{ID: 2, Platform: PlatformOpenAI}

	check := func(account *Account, sent, resp string, wantNil, wantMismatch bool) {
		t.Helper()
		got := upstreamModelMismatchForAccount(account, sent, resp)
		if wantNil {
			require.Nil(t, got, "sent=%q resp=%q", sent, resp)
			return
		}
		require.NotNil(t, got, "sent=%q resp=%q", sent, resp)
		require.Equal(t, wantMismatch, *got, "sent=%q resp=%q", sent, resp)
	}

	check(qoder, "qfmodel", "auto", false, false)
	check(qoder, "QFModel", "Auto", false, false)
	check(qoder, "auto", "auto", false, false)
	check(qoder, "qwen3.8-flash", "auto", false, true) // 非官方 key 不豁免
	check(qoder, "qfmodel", "some-other-model", false, true)
	check(qoder, "qfmodel", "", true, false)
	check(qoder, "", "auto", false, true)
	check(openai, "qfmodel", "auto", false, true) // 非 Qoder 平台无豁免
	check(nil, "qfmodel", "auto", false, true)
}

// TestNormalizeOpenAIModelForUpstreamQoder forwardQoderChatCompletions 的出站
// 归一链路：展示名 → 官方 key。
func TestNormalizeOpenAIModelForUpstreamQoder(t *testing.T) {
	t.Parallel()
	account := &Account{ID: 1, Platform: PlatformQoder}
	require.Equal(t, "qfmodel", normalizeOpenAIModelForUpstream(account, "qwen3.8-flash"))
	require.Equal(t, "qfmodel", normalizeOpenAIModelForUpstream(account, "qfmodel"))
	require.Equal(t, "auto", normalizeOpenAIModelForUpstream(account, "auto"))
}

// TestQoderResolveUpstreamModelKeyNormalizesAlias 发送侧解析链：
// body 模型为展示名、账号无映射时也归一为官方 key；映射命中优先且幂等。
func TestQoderResolveUpstreamModelKeyNormalizesAlias(t *testing.T) {
	t.Parallel()
	noMapping := &Account{ID: 1, Platform: PlatformQoder, Credentials: map[string]any{}}
	require.Equal(t, "qfmodel", qoderResolveUpstreamModelKey(noMapping, []byte(`{"model":"qwen3.8-flash"}`)))
	require.Equal(t, "auto", qoderResolveUpstreamModelKey(noMapping, []byte(`{"model":"auto"}`)))
	require.Equal(t, "auto", qoderResolveUpstreamModelKey(noMapping, []byte(`{}`)))

	withMapping := &Account{ID: 2, Platform: PlatformQoder, Credentials: map[string]any{
		"model_mapping": map[string]any{"qwen3.8-flash": "qfmodel", "qfmodel": "qfmodel"},
	}}
	require.Equal(t, "qfmodel", qoderResolveUpstreamModelKey(withMapping, []byte(`{"model":"qwen3.8-flash"}`)))
	require.Equal(t, "qfmodel", qoderResolveUpstreamModelKey(withMapping, []byte(`{"model":"qfmodel"}`)))
}

// TestQoderUpstreamAcceptAlwaysEventStream 出站 accept 头不变量：
// 无论客户端流式与否，上游 accept 恒为 text/event-stream
// （application/json 实测被上游直接 500）。
func TestQoderUpstreamAcceptAlwaysEventStream(t *testing.T) {
	t.Parallel()
	var (
		mu      sync.Mutex
		accepts []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		accepts = append(accepts, r.Header.Get("Accept"))
		mu.Unlock()
		inner := `{"id":"chat-a","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
		env := map[string]any{"headers": map[string]any{}, "body": inner, "statusCodeValue": 200}
		raw, _ := jsonMarshalQoder(env)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
		_, _ = w.Write([]byte("\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := &Account{ID: 810, Platform: PlatformQoder, Credentials: map[string]any{
		"access_token": "dt-accept", "uid": "u-810", "base_url": server.URL,
	}}
	c := newWorkbuddyTestGinContext()

	// 流式请求。
	resp, err := svc.sendQoderUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`), true)
	require.NoError(t, err)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// 非流式请求（聚合路径）。
	resp2, err := svc.sendQoderUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`), false)
	require.NoError(t, err)
	_, _ = io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, accepts, 2)
	require.Equal(t, "text/event-stream", accepts[0], "流式请求 accept")
	require.Equal(t, "text/event-stream", accepts[1], "非流式请求同样必须 SSE accept 上游")
}
