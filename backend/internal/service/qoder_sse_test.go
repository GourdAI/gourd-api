//go:build unit

package service

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// qoder_sse_test.go 上游 SSE 解析单测：正常帧、信封 418 错误帧、业务 code 错误、
// usage 同帧合并、[DONE] 归一化、空流、非流式聚合。

// qoderSSEEnvelope 辅助：构造一帧上游信封 data 行（body 为内层 JSON 字符串原样）。
func qoderSSEEnvelope(innerJSON string, status int) string {
	env := map[string]any{
		"headers":         map[string]any{},
		"body":            innerJSON,
		"statusCodeValue": status,
	}
	raw, _ := json.Marshal(env)
	return "data: " + string(raw)
}

func TestQoderParseDataFrameNormal(t *testing.T) {
	t.Parallel()
	inner := `{"id":"chat-1","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`
	data := qoderParseDataFrame(qoderSSEEnvelope(inner, 200)[len("data: "):])
	require.NotNil(t, data)
	require.Nil(t, data.BusinessErr)
	require.NotNil(t, data.InnerObject)
	require.Equal(t, "chat-1", data.InnerObject["id"])
}

func TestQoderParseDataFrameEnvelope418(t *testing.T) {
	t.Parallel()
	payload := qoderSSEEnvelope("quota exceeded", 418)[len("data: "):]
	data := qoderParseDataFrame(payload)
	require.NotNil(t, data)
	require.NotNil(t, data.BusinessErr)
	require.Equal(t, "418", data.BusinessErr.Code)
	require.Equal(t, "quota exceeded", data.BusinessErr.Message)
}

func TestQoderParseDataFrameBusinessCodeError(t *testing.T) {
	t.Parallel()
	inner := `{"code":"115","message":"rate limited"}`
	data := qoderParseDataFrame(qoderSSEEnvelope(inner, 200)[len("data: "):])
	require.NotNil(t, data)
	require.NotNil(t, data.BusinessErr)
	require.Equal(t, "115", data.BusinessErr.Code)
	require.Equal(t, "rate limited", data.BusinessErr.Message)
	// code "0" 不是错误。
	inner0 := `{"code":"0","choices":[]}`
	data0 := qoderParseDataFrame(qoderSSEEnvelope(inner0, 200)[len("data: "):])
	require.NotNil(t, data0)
	require.Nil(t, data0.BusinessErr)
}

func TestQoderSSEReaderNormalStream(t *testing.T) {
	t.Parallel()
	inner1 := `{"id":"chat-1","model":"auto","choices":[{"index":0,"delta":{"role":"assistant","content":"he"},"finish_reason":null}]}`
	inner2 := `{"id":"chat-1","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":null}]}`
	upstream := qoderSSEEnvelope(inner1, 200) + "\n\n" +
		qoderSSEEnvelope(inner2, 200) + "\n\n" +
		"data: [DONE]\n\n"
	reader := newQoderSSEReader(strings.NewReader(upstream), "")
	raw := readAllQoder(t, reader)
	out := string(raw)
	// 恰好一个 [DONE]。
	require.Equal(t, 1, strings.Count(out, "data: [DONE]"))
	// 规范化为标准 CC chunk：id 续传、object 补齐。
	require.Contains(t, out, `"object":"chat.completion.chunk"`)
	require.Contains(t, out, `"content":"he"`)
	require.Contains(t, out, `"content":"llo"`)
	require.Equal(t, 2, strings.Count(out, `"id":"chat-1"`), "id 续传首帧真实 id")
}

func TestQoderSSEReaderUsageSameFrameMerged(t *testing.T) {
	t.Parallel()
	// usage 与 choices 同帧：合并透传不丢弃。
	inner := `{"id":"chat-1","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`
	upstream := qoderSSEEnvelope(inner, 200) + "\n\n" + "data: [DONE]\n\n"
	out := string(readAllQoder(t, newQoderSSEReader(strings.NewReader(upstream), "")))
	require.Contains(t, out, `"usage"`)
	require.Contains(t, out, `"prompt_tokens":7`)
	require.Contains(t, out, `"completion_tokens":3`)
}

func TestQoderSSEReaderErrorFrames(t *testing.T) {
	t.Parallel()
	// 信封 418 + 业务 code 错误帧都转 CC error 形态。
	upstream := qoderSSEEnvelope("quota exceeded", 418) + "\n\n" +
		qoderSSEEnvelope(`{"code":"115","message":"blocked"}`, 200) + "\n\n" +
		"data: [DONE]\n\n"
	out := string(readAllQoder(t, newQoderSSEReader(strings.NewReader(upstream), "")))
	require.Contains(t, out, `"error"`)
	require.Contains(t, out, `"code":"418"`)
	require.Contains(t, out, `"code":"115"`)
	require.Equal(t, 1, strings.Count(out, "data: [DONE]"))
}

func TestQoderSSEReaderEmptyStream(t *testing.T) {
	t.Parallel()
	// 只有注释行/空行的空流：补一帧 error + [DONE]。
	out := string(readAllQoder(t, newQoderSSEReader(strings.NewReader(": keep-alive\n\n"), "")))
	require.Contains(t, out, `"error"`)
	require.Equal(t, 1, strings.Count(out, "data: [DONE]"))
}

func TestAggregateQoderSSEFull(t *testing.T) {
	t.Parallel()
	inner1 := `{"id":"chat-agg","model":"auto","choices":[{"index":0,"delta":{"role":"assistant","content":"he"},"finish_reason":null}]}`
	inner2 := `{"id":"chat-agg","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`
	upstream := qoderSSEEnvelope(inner1, 200) + "\n\n" +
		qoderSSEEnvelope(inner2, 200) + "\n\n" +
		"data: [DONE]\n\n"
	out, usage, err := aggregateQoderSSE(strings.NewReader(upstream), "")
	require.NoError(t, err)
	require.NotNil(t, usage)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(out, &resp))
	require.Equal(t, "chat.completion", resp["object"])
	choices, _ := resp["choices"].([]any)
	c0, _ := choices[0].(map[string]any)
	msg, _ := c0["message"].(map[string]any)
	require.Equal(t, "hello", msg["content"])
	require.Equal(t, "stop", c0["finish_reason"])
	u, _ := resp["usage"].(map[string]any)
	require.Equal(t, float64(10), u["total_tokens"], "usage 补 total")
}

func TestAggregateQoderSSEEnvelopeError(t *testing.T) {
	t.Parallel()
	upstream := qoderSSEEnvelope("quota exceeded", 418) + "\n\n" + "data: [DONE]\n\n"
	_, _, err := aggregateQoderSSE(strings.NewReader(upstream), "")
	require.Error(t, err)
	require.True(t, isQoderUpstreamEnvelopeError(err), "信封错误必须结构化: %v", err)
	var env *errQoderUpstreamEnvelope
	require.ErrorAs(t, err, &env)
	require.Equal(t, "418", env.Code)
	require.Equal(t, "quota exceeded", env.Message)
}

func TestAggregateQoderSSEBusinessError(t *testing.T) {
	t.Parallel()
	upstream := qoderSSEEnvelope(`{"code":"115","message":"blocked"}`, 200) + "\n\n" + "data: [DONE]\n\n"
	_, _, err := aggregateQoderSSE(strings.NewReader(upstream), "")
	require.Error(t, err)
	var env *errQoderUpstreamEnvelope
	require.ErrorAs(t, err, &env)
	require.Equal(t, "115", env.Code)
}

func TestAggregateQoderSSEEmptyStream(t *testing.T) {
	t.Parallel()
	_, _, err := aggregateQoderSSE(strings.NewReader("data: [DONE]\n\n"), "")
	require.Error(t, err)
	require.True(t, qoderIsEmptyStreamError(err))
}

func TestAggregateQoderSSEToolCalls(t *testing.T) {
	t.Parallel()
	inner1 := `{"id":"chat-tool","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"ls","arguments":""}}]},"finish_reason":null}]}`
	inner2 := `{"id":"chat-tool","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\".\"}"}}]},"finish_reason":"tool_calls"}]}`
	upstream := qoderSSEEnvelope(inner1, 200) + "\n\n" + qoderSSEEnvelope(inner2, 200) + "\n\n" + "data: [DONE]\n\n"
	out, _, err := aggregateQoderSSE(strings.NewReader(upstream), "")
	require.NoError(t, err)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(out, &resp))
	choices, _ := resp["choices"].([]any)
	c0, _ := choices[0].(map[string]any)
	msg, _ := c0["message"].(map[string]any)
	tcs, ok := msg["tool_calls"].([]any)
	require.True(t, ok, "tool_calls 按分片聚合")
	require.Len(t, tcs, 1)
	tc, _ := tcs[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	require.Equal(t, "ls", fn["name"])
	require.Equal(t, `{"path":"."}`, fn["arguments"])
	require.Equal(t, "tool_calls", c0["finish_reason"])
}

// TestQoderResponseModelRewriteStream 流式响应 model 回写（P1）：上游恒报
// model:"auto"（协议占位），客户端必须看到实际出站的官方 key。
func TestQoderResponseModelRewriteStream(t *testing.T) {
	t.Parallel()
	inner := `{"id":"chat-m","model":"auto","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`
	upstream := qoderSSEEnvelope(inner, 200) + "\n\n" + "data: [DONE]\n\n"

	out := string(readAllQoder(t, newQoderSSEReader(strings.NewReader(upstream), "qfmodel")))
	require.Contains(t, out, `"model":"qfmodel"`, "出站 key 回写进响应帧")
	require.NotContains(t, out, `"model":"auto"`, "上游占位值不得泄露给客户端")

	// 同帧多帧（id 续传的后续帧无 model）也必须被补齐为出站 key。
	inner2 := `{"id":"chat-m","model":"auto","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`
	multi := qoderSSEEnvelope(inner2, 200) + "\n\n" + qoderSSEEnvelope(inner2, 200) + "\n\n" + "data: [DONE]\n\n"
	out2 := string(readAllQoder(t, newQoderSSEReader(strings.NewReader(multi), "dmodel")))
	require.Equal(t, 2, strings.Count(out2, `"model":"dmodel"`), "每一帧都回写")
}

// TestQoderResponseModelRewriteAggregate 非流式聚合响应同样回写（P1）。
func TestQoderResponseModelRewriteAggregate(t *testing.T) {
	t.Parallel()
	inner := `{"id":"chat-a","model":"auto","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`
	upstream := qoderSSEEnvelope(inner, 200) + "\n\n" + "data: [DONE]\n\n"

	out, _, err := aggregateQoderSSE(strings.NewReader(upstream), "qfmodel")
	require.NoError(t, err)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(out, &resp))
	require.Equal(t, "qfmodel", resp["model"], "聚合响应 model = 实际出站 key")
}

// TestQoderResponseModelNoRewriteForUnknownKey 非官方 key 不回写（P1 与审计口径
// 联动）：保留上游 "auto"，使「模型不一致」审计仍能发现历史坏配置。
func TestQoderResponseModelNoRewriteForUnknownKey(t *testing.T) {
	t.Parallel()
	inner := `{"id":"chat-u","model":"auto","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`
	upstream := qoderSSEEnvelope(inner, 200) + "\n\n" + "data: [DONE]\n\n"

	out := string(readAllQoder(t, newQoderSSEReader(strings.NewReader(upstream), "qwen3.8-flash")))
	require.Contains(t, out, `"model":"auto"`, "非官方 key 不改写响应声明")

	outAgg, _, err := aggregateQoderSSE(strings.NewReader(upstream), "some-unknown-model")
	require.NoError(t, err)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(outAgg, &resp))
	require.Equal(t, "auto", resp["model"])
}

// TestQoderResponseModelHelperMatrix qoderResponseModel 纯函数矩阵。
func TestQoderResponseModelHelperMatrix(t *testing.T) {
	t.Parallel()
	require.Equal(t, "qfmodel", qoderResponseModel("qfmodel", "auto"), "官方 key 覆盖占位值")
	require.Equal(t, "qfmodel", qoderResponseModel(" QFModel ", "auto"), "大小写/空白归一")
	require.Equal(t, "kmodel", qoderResponseModel("kmodel", nil), "上游无声明时仍回写官方 key")
	require.Equal(t, "auto", qoderResponseModel("auto", "auto"), "auto 也是官方 key（智能路由），回写同值")
	require.Equal(t, "qfmodel", qoderResponseModel("qfmodel", nil), "nil 声明也回写")
	// 非官方 key：不改写，保留上游声明供审计。
	require.Equal(t, "auto", qoderResponseModel("qwen3.8-flash", "auto"))
	require.Equal(t, "weird-model", qoderResponseModel("weird-model", "weird-model"), "上游真实声明原样保留")
	// sentModel 为空 + 上游无声明 → 占位 "qoder"。
	require.Equal(t, "qoder", qoderResponseModel("", ""))
	require.Equal(t, "qoder", qoderResponseModel("", nil))
	require.Equal(t, "mmodel", qoderResponseModel("", "mmodel"))
}

// TestAggregateQoderSSENestedModelOnlyUpstreamSentEmpty 回写链路空 sentModel 时的
// 向后兼容：透传上游值。
func TestAggregateQoderSSENestedModelOnlyUpstreamSentEmpty(t *testing.T) {
	t.Parallel()
	inner := `{"id":"chat-e","model":"auto","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`
	upstream := qoderSSEEnvelope(inner, 200) + "\n\n" + "data: [DONE]\n\n"
	out, _, err := aggregateQoderSSE(strings.NewReader(upstream), "")
	require.NoError(t, err)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(out, &resp))
	require.Equal(t, "auto", resp["model"], "sentModel 为空时透传上游声明")
}

// readAllQoder 测试辅助：读满整个 reader。
func readAllQoder(t *testing.T, r io.Reader) []byte {
	t.Helper()
	raw, err := io.ReadAll(r)
	require.NoError(t, err)
	return raw
}
