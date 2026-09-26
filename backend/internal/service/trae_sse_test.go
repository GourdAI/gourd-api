//go:build unit

package service

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// trae_sse_test.go 覆盖 Trae 自定义 SSE → OpenAI CC 的归一层（业务纪律 12）：
// 上游从不发 data:[DONE]（由 normalizer 补且只补一个）、event:error 必须转成 CC error、
// response→content / reasoning_content→reasoning 两套字段名都兼容、tool_calls 续帧按 index 累加。

// sseEvent 一条上游事件（event 行 + data 行必须成对出现在同一空行分隔块内）。
func sseEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data
}

// sseStream 把若干事件拼成上游原始流文本。
func sseStream(parts ...string) string {
	var b strings.Builder
	for _, part := range parts {
		b.WriteString(part)
		b.WriteString("\n\n")
	}
	return b.String()
}

// sseFrames 把归一后的 SSE 文本切成 payload 列表（"[DONE]" 作为独立项保留）。
func sseFrames(t *testing.T, stream string) []string {
	t.Helper()
	var frames []string
	for _, block := range strings.Split(stream, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		require.True(t, strings.HasPrefix(block, "data: "), "规范化输出必须是 data: 前缀: %q", block)
		frames = append(frames, strings.TrimPrefix(block, "data: "))
	}
	return frames
}

func readAllTrae(t *testing.T, r io.Reader) string {
	t.Helper()
	raw, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(raw)
}

func decodeFrame(t *testing.T, payload string) map[string]any {
	t.Helper()
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &obj), "帧必须是合法 JSON: %s", payload)
	return obj
}

func frameDelta(t *testing.T, payload string) map[string]any {
	t.Helper()
	obj := decodeFrame(t, payload)
	choices, ok := obj["choices"].([]any)
	require.True(t, ok, "CC chunk 必须有 choices: %s", payload)
	require.NotEmpty(t, choices)
	delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
	if delta == nil {
		delta = map[string]any{}
	}
	return delta
}

func frameFinishReason(t *testing.T, payload string) string {
	t.Helper()
	obj := decodeFrame(t, payload)
	choices := obj["choices"].([]any)
	reason, _ := choices[0].(map[string]any)["finish_reason"].(string)
	return reason
}

// contentFrames 去掉末尾 [DONE] 后的 chunk payload。
func contentFrames(t *testing.T, out string) []string {
	t.Helper()
	frames := sseFrames(t, out)
	require.NotEmpty(t, frames)
	require.Equal(t, "[DONE]", frames[len(frames)-1], "[DONE] 必须是最后一帧")
	return frames[:len(frames)-1]
}

func countDone(out string) int {
	return strings.Count(out, "data: [DONE]")
}

func TestTraeSSEReaderNormalizesFullStream(t *testing.T) {
	t.Parallel()
	stream := sseStream(
		sseEvent("output", `{"id":"msg-1","model":"glm-5.2","response":"Hello"}`),
		sseEvent("output", `{"content":", world"}`),
		sseEvent("token_usage", `{"prompt_tokens":3,"completion_tokens":5}`),
		sseEvent("done", `{"finish_reason":"stop"}`),
	)
	out := readAllTrae(t, traeNewSSEReader(strings.NewReader(stream), traeStreamMeta{}))

	require.Equal(t, 1, countDone(out), "必须且只能补一个 [DONE]: %s", out)
	chunks := contentFrames(t, out)

	var content strings.Builder
	var usage map[string]any
	finishSeen := ""
	for _, frame := range chunks {
		obj := decodeFrame(t, frame)
		require.Equal(t, "chat.completion.chunk", obj["object"])
		delta := frameDelta(t, frame)
		if text, _ := delta["content"].(string); text != "" {
			content.WriteString(text)
		}
		if reason := frameFinishReason(t, frame); reason != "" {
			finishSeen = reason
		}
		if rawUsage, ok := obj["usage"].(map[string]any); ok && len(rawUsage) > 0 {
			usage = rawUsage
		}
	}
	require.Equal(t, "Hello, world", content.String(), "response 与 content 两套字段名都要认")
	require.Equal(t, "stop", finishSeen, "done 帧的 finish_reason 必须透出")
	require.NotNil(t, usage, "token_usage 帧必须转成 usage")
	require.EqualValues(t, 3, usage["prompt_tokens"])
	require.EqualValues(t, 5, usage["completion_tokens"])
	require.EqualValues(t, 8, usage["total_tokens"], "上游只给 prompt/completion 时 total_tokens 必须补全")

	// 元数据续传：首帧的 id/model 在全部帧里保持一致。
	first := decodeFrame(t, chunks[0])
	require.Equal(t, "msg-1", first["id"])
	require.Equal(t, "glm-5.2", first["model"])
	for _, frame := range chunks {
		obj := decodeFrame(t, frame)
		require.Equal(t, "msg-1", obj["id"], "id 必须在帧间续传")
		require.Equal(t, "glm-5.2", obj["model"])
	}
}

func TestTraeSSEReaderEmitsErrorChunkOnEventError(t *testing.T) {
	t.Parallel()
	// 上游 HTTP 200 + event:error：绝不能被当成"成功但空回复"。
	stream := sseStream(
		sseEvent("output", `{"response":"partial"}`),
		sseEvent("error", `{"code":4001,"message":"model not available","extra":{}}`),
	)
	out := readAllTrae(t, traeNewSSEReader(strings.NewReader(stream), traeStreamMeta{ID: "x", Model: "m"}))

	// 纪律 12 的硬要求：event:error 必须转为 CC error 帧（不能当成成功空回复）。
	var errObj map[string]any
	frames := sseFrames(t, out)
	for _, frame := range frames {
		obj := decodeFrame(t, frame)
		if raw, ok := obj["error"].(map[string]any); ok {
			errObj = raw
			break
		}
	}
	require.NotNil(t, errObj, "event:error 必须产出 CC error 帧: %s", out)
	require.Equal(t, "model not available (code=4001)", errObj["message"])
	require.Equal(t, "upstream_error", errObj["type"])
	require.Equal(t, "4001", errObj["code"], "业务码必须透传供上层判定 failover")

	// 修复后的硬约束（原断言为「记录现状（待修）」）：流内 error 不得再伪装成正常
	// 结束——不补 finish_reason=stop 也不补 [DONE]，否则上层截断检测会判成功，
	// 该次失败被记成功、按 0 token 出账、不换号也不落账号状态。
	require.NotContains(t, out, "data: [DONE]", "流内错误不得补 [DONE] 伪装成正常结束")
	require.NotContains(t, out, `"finish_reason":"stop"`)
}

// 流内 error 必须能上报给观察者做分级落状态（对齐 qoder 的 error hook）。
func TestTraeSSEReaderReportsBusinessErrorToHook(t *testing.T) {
	t.Parallel()
	var codes []string
	var messages []string
	reader := traeNewSSEReaderWithErrorHook(
		strings.NewReader(sseStream(
			sseEvent("output", `{"response":"hi"}`),
			sseEvent("error", `{"code":1005,"message":"credits exhausted"}`),
			sseEvent("error", `{"code":1005,"message":"credits exhausted"}`),
		)),
		traeStreamMeta{},
		func(code, message string) { codes = append(codes, code); messages = append(messages, message) },
	)
	out := readAllTrae(t, reader)
	// 同一错误只上报一次：落状态含多次 DB 往返，不得在热路径重复触发。
	require.Equal(t, []string{"1005"}, codes)
	require.Equal(t, "credits exhausted", messages[0])
	require.NotContains(t, out, "[DONE]")
}

// 无 hook 时行为与 traeNewSSEReader 完全等价。
func TestTraeSSEReaderWithoutHookEquivalent(t *testing.T) {
	t.Parallel()
	stream := sseStream(sseEvent("output", `{"response":"ok","finish_reason":"stop"}`))
	hooked := readAllTrae(t, traeNewSSEReaderWithErrorHook(strings.NewReader(stream), traeStreamMeta{}, nil))
	plain := readAllTrae(t, traeNewSSEReader(strings.NewReader(stream), traeStreamMeta{}))
	require.Equal(t, plain, hooked)
	require.Contains(t, hooked, "data: [DONE]")
}

func TestTraeSSEReaderErrorAsFirstFrameHasNoDone(t *testing.T) {
	t.Parallel()
	out := readAllTrae(t, traeNewSSEReader(strings.NewReader(sseStream(
		sseEvent("error", `{"code":1001,"message":"token expired"}`),
	)), traeStreamMeta{}))
	require.Contains(t, out, "token expired (code=1001)")
	require.NotContains(t, out, "[DONE]", "错误流不得伪装成正常结束")
}

func TestTraeSSEReaderAppendsFinishAndDoneWithoutDoneEvent(t *testing.T) {
	t.Parallel()
	out := readAllTrae(t, traeNewSSEReader(strings.NewReader(sseStream(
		sseEvent("output", `{"response":"only output"}`),
	)), traeStreamMeta{}))
	chunks := contentFrames(t, out)
	require.Len(t, chunks, 2, "内容帧 + 补的 finish 帧")
	require.Equal(t, "only output", func() string {
		d := frameDelta(t, chunks[0])
		s, _ := d["content"].(string)
		return s
	}())
	require.Equal(t, "stop", frameFinishReason(t, chunks[1]), "缺 done 时必须补 finish_reason=stop")
	require.Empty(t, frameDelta(t, chunks[1]), "补的结束帧 delta 应为空对象")
	require.Equal(t, 1, countDone(out))
}

func TestTraeSSEReaderEmptyStreamYieldsErrorFrame(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"",
		"   \n\n  ",
		sseStream(sseEvent("progress_notice", `{"stage":"queue"}`), sseEvent("metadata", "not-json")),
	} {
		out := readAllTrae(t, traeNewSSEReader(strings.NewReader(in), traeStreamMeta{}))
		require.Contains(t, out, "empty upstream stream", "输入=%q", in)
		require.NotContains(t, out, "[DONE]", "空流不得给出 [DONE] 成功语义")
		frames := sseFrames(t, out)
		require.Len(t, frames, 1)
		errObj := decodeFrame(t, frames[0])["error"].(map[string]any)
		require.Equal(t, "upstream_error", errObj["type"])
		require.Equal(t, "upstream_parse", errObj["code"])
	}

	// nil reader 同样不得 panic。
	out := readAllTrae(t, traeNewSSEReader(nil, traeStreamMeta{}))
	require.Contains(t, out, "empty upstream stream")
}

func TestTraeSSEReaderReasoningFieldAliases(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"reasoning_content", "reasoning", "thinking"} {
		out := readAllTrae(t, traeNewSSEReader(strings.NewReader(sseStream(
			sseEvent("output", `{"`+key+`":"deep thought"}`),
			sseEvent("done", `{"finish_reason":"stop"}`),
		)), traeStreamMeta{}))
		chunks := contentFrames(t, out)
		delta := frameDelta(t, chunks[0])
		// 归一后一律输出 OpenAI 的 reasoning_content 字段名。
		require.Equal(t, "deep thought", delta["reasoning_content"], "上游字段 %s", key)
		require.NotContains(t, delta, "reasoning")
		require.NotContains(t, delta, "thinking")
	}
}

func TestTraeSSEReaderToolCallsAccumulateByIndex(t *testing.T) {
	t.Parallel()
	out := readAllTrae(t, traeNewSSEReader(strings.NewReader(sseStream(
		sseEvent("output", `{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_time","arguments":""}}]}`),
		sseEvent("output", `{"tool_calls":[{"index":0,"function":{"arguments":"{\"tz\":\""}}]}`),
		sseEvent("output", `{"tool_calls":[{"index":0,"function":{"arguments":"Asia/Shanghai\"}"}}]}`),
		sseEvent("done", `{"finish_reason":"tool_calls"}`),
	)), traeStreamMeta{}))
	chunks := contentFrames(t, out)

	var accumulated strings.Builder
	var sawName, sawID string
	for _, frame := range chunks {
		delta := frameDelta(t, frame)
		calls, ok := delta["tool_calls"].([]any)
		if !ok || len(calls) == 0 {
			continue
		}
		call := calls[0].(map[string]any)
		require.EqualValues(t, 0, call["index"], "续帧必须原样保留 index 以便累加")
		require.Equal(t, "function", call["type"])
		fn, _ := call["function"].(map[string]any)
		if name, _ := fn["name"].(string); name != "" {
			sawName = name
		}
		if id, _ := call["id"].(string); id != "" {
			sawID = id
		}
		if args, _ := fn["arguments"].(string); args != "" {
			accumulated.WriteString(args)
		}
	}
	require.Equal(t, "get_time", sawName, "首帧带 name")
	require.Equal(t, "call_1", sawID)
	require.Equal(t, `{"tz":"Asia/Shanghai"}`, accumulated.String(), "续帧仅 arguments，按顺序累加成完整入参")
	require.Equal(t, "tool_calls", frameFinishReason(t, chunks[len(chunks)-1]))
}

func TestTraeSSEReaderLegacyFunctionCallShape(t *testing.T) {
	t.Parallel()
	out := readAllTrae(t, traeNewSSEReader(strings.NewReader(sseStream(
		sseEvent("output", `{"function_call":{"name":"lookup","arguments":"{}"}}`),
	)), traeStreamMeta{}))
	delta := frameDelta(t, contentFrames(t, out)[0])
	calls, ok := delta["tool_calls"].([]any)
	require.True(t, ok, "function_call 形态必须收敛为 tool_calls")
	call := calls[0].(map[string]any)
	require.EqualValues(t, 0, call["index"])
	require.Equal(t, "function", call["type"])
	fn := call["function"].(map[string]any)
	require.Equal(t, "lookup", fn["name"])
	require.Equal(t, "{}", fn["arguments"])
}

func TestTraeSSEReaderIgnoresObservabilityEvents(t *testing.T) {
	t.Parallel()
	out := readAllTrae(t, traeNewSSEReader(strings.NewReader(sseStream(
		sseEvent("request_wait_in_queue", `{"queue_position":3}`),
		sseEvent("queue_begin", `{"ts":1}`),
		sseEvent("queue_end", `{"ts":2}`),
		sseEvent("metadata", `not-json-at-all`),
		sseEvent("timing_cost", `{"cost":12}`),
		sseEvent("output", `{"response":"ok"}`),
		sseEvent("done", `{"finish_reason":"stop"}`),
	)), traeStreamMeta{}))
	chunks := contentFrames(t, out)
	require.Len(t, chunks, 2, "观测类事件不得产出 chunk: %s", out)
	delta := frameDelta(t, chunks[0])
	require.Equal(t, "ok", delta["content"])
	require.Equal(t, "stop", frameFinishReason(t, chunks[1]))
}

func TestTraeSSEReaderUsageOnDoneFrame(t *testing.T) {
	t.Parallel()
	// 上游把 usage 塞在 done 帧里的形态。
	out := readAllTrae(t, traeNewSSEReader(strings.NewReader(sseStream(
		sseEvent("output", `{"content":"x"}`),
		sseEvent("done", `{"finish_reason":"stop","usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`),
	)), traeStreamMeta{}))
	chunks := contentFrames(t, out)
	last := decodeFrame(t, chunks[len(chunks)-1])
	usage, ok := last["usage"].(map[string]any)
	require.True(t, ok, "done 帧携带的 usage 必须透出")
	require.EqualValues(t, 3, usage["total_tokens"])
	require.Equal(t, "stop", frameFinishReason(t, chunks[len(chunks)-1]))
}

func TestTraeSSEReaderIncrementalFinishReasonIsNull(t *testing.T) {
	t.Parallel()
	out := readAllTrae(t, traeNewSSEReader(strings.NewReader(sseStream(
		sseEvent("output", `{"response":"a"}`),
	)), traeStreamMeta{ID: "keep-id", Model: "keep-model"}))
	chunks := contentFrames(t, out)
	obj := decodeFrame(t, chunks[0])
	// CC 规范：增量帧 finish_reason 为 null。
	require.Nil(t, obj["choices"].([]any)[0].(map[string]any)["finish_reason"])
	require.Equal(t, "keep-id", obj["id"], "调用方提供的 meta 必须优先于占位")
	require.Equal(t, "keep-model", obj["model"])
}

func TestTraeAggregateSSERendersCompletion(t *testing.T) {
	t.Parallel()
	stream := sseStream(
		sseEvent("output", `{"id":"agg-1","model":"kimi-k2.5","reasoning_content":"think","response":"Hello"}`),
		sseEvent("output", `{"content":" world"}`),
		sseEvent("token_usage", `{"prompt_tokens":4,"completion_tokens":6}`),
		sseEvent("done", `{"finish_reason":"stop"}`),
	)
	raw, err := traeAggregateSSE(strings.NewReader(stream), traeStreamMeta{ID: "agg-1", Model: "kimi-k2.5"})
	require.NoError(t, err)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(raw, &resp))
	require.Equal(t, "chat.completion", resp["object"])
	// id/model 由调用方传入的 meta 决定（聚合路径不从帧里提取）。
	require.Equal(t, "agg-1", resp["id"])
	require.Equal(t, "kimi-k2.5", resp["model"])

	// 未传 meta 时落到稳定占位，保证客户端拿到非空 id。
	bareRaw, err := traeAggregateSSE(strings.NewReader(
		sseStream(sseEvent("output", `{"response":"hi"}`), sseEvent("done", `{"finish_reason":"stop"}`)),
	), traeStreamMeta{})
	require.NoError(t, err)
	var bare map[string]any
	require.NoError(t, json.Unmarshal(bareRaw, &bare))
	require.Equal(t, "trae", bare["model"], "未传 meta 时 model 回落占位 trae")
	require.Contains(t, bare["id"], "chatcmpl-trae-")

	choice := resp["choices"].([]any)[0].(map[string]any)
	require.Equal(t, "stop", choice["finish_reason"])
	message := choice["message"].(map[string]any)
	require.Equal(t, "assistant", message["role"])
	require.Equal(t, "Hello world", message["content"], "response/content 两套字段名聚合拼接")
	require.Equal(t, "think", message["reasoning_content"])

	usage := resp["usage"].(map[string]any)
	require.EqualValues(t, 4, usage["prompt_tokens"])
	require.EqualValues(t, 6, usage["completion_tokens"])
	require.EqualValues(t, 10, usage["total_tokens"])
}

func TestTraeAggregateSSEErrorAndEmpty(t *testing.T) {
	t.Parallel()
	_, err := traeAggregateSSE(strings.NewReader(sseStream(
		sseEvent("error", `{"code":1005,"message":"quota exhausted"}`),
	)), traeStreamMeta{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "quota exhausted")
	require.Contains(t, err.Error(), "code=1005")

	_, err = traeAggregateSSE(strings.NewReader(""), traeStreamMeta{})
	require.ErrorIs(t, err, errTraeEmptyStream)

	_, err = traeAggregateSSE(nil, traeStreamMeta{})
	require.ErrorIs(t, err, errTraeEmptyStream)

	_, err = traeAggregateSSE(strings.NewReader(sseStream(
		sseEvent("progress_notice", `{"x":1}`),
	)), traeStreamMeta{})
	require.ErrorIs(t, err, errTraeEmptyStream, "只有观测帧也算空流")
}

func TestTraeAggregateSSEToolCallsAndDefaultFinish(t *testing.T) {
	t.Parallel()
	raw, err := traeAggregateSSE(strings.NewReader(sseStream(
		sseEvent("output", `{"tool_calls":[{"index":0,"id":"c1","function":{"name":"shell","arguments":"{\"cmd\":"}}]}`),
		sseEvent("output", `{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}`),
	)), traeStreamMeta{})
	require.NoError(t, err)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(raw, &resp))
	choice := resp["choices"].([]any)[0].(map[string]any)
	require.Equal(t, "tool_calls", choice["finish_reason"], "有工具调用时缺省 finish_reason 必须归正为 tool_calls")
	message := choice["message"].(map[string]any)
	calls := message["tool_calls"].([]any)
	require.Len(t, calls, 1)
	call := calls[0].(map[string]any)
	require.Equal(t, "c1", call["id"])
	require.EqualValues(t, 0, call["index"])
	fn := call["function"].(map[string]any)
	require.Equal(t, "shell", fn["name"])
	require.Equal(t, `{"cmd":"ls"}`, fn["arguments"], "arguments 必须按 index 累加为完整 JSON")
	// 记录现状：带工具调用时正文保持空串（源码仅在"无正文且无工具调用"时置 nil）。
	require.Equal(t, "", message["content"])
}

func TestTraeAggregateSSEMultipleToolCallsKeepOrder(t *testing.T) {
	t.Parallel()
	raw, err := traeAggregateSSE(strings.NewReader(sseStream(
		sseEvent("output", `{"tool_calls":[{"index":0,"id":"c0","function":{"name":"a","arguments":"{}"}}]}`),
		sseEvent("output", `{"tool_calls":[{"index":1,"id":"c1","function":{"name":"b","arguments":"{}"}}]}`),
		sseEvent("done", `{"finish_reason":"tool_calls"}`),
	)), traeStreamMeta{})
	require.NoError(t, err)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(raw, &resp))
	message := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls := message["tool_calls"].([]any)
	require.Len(t, calls, 2)
	require.Equal(t, "a", calls[0].(map[string]any)["function"].(map[string]any)["name"])
	require.Equal(t, "b", calls[1].(map[string]any)["function"].(map[string]any)["name"])
}

func TestTraeAggregateSSEExplicitFinishReasonWins(t *testing.T) {
	t.Parallel()
	raw, err := traeAggregateSSE(strings.NewReader(sseStream(
		sseEvent("output", `{"response":"cut off","finish_reason":"length"}`),
	)), traeStreamMeta{})
	require.NoError(t, err)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(raw, &resp))
	require.Equal(t, "length", resp["choices"].([]any)[0].(map[string]any)["finish_reason"])
}

func TestTraeEnsureUsageTotal(t *testing.T) {
	t.Parallel()
	got := traeEnsureUsageTotal(map[string]any{"prompt_tokens": float64(7), "completion_tokens": float64(2)})
	require.EqualValues(t, 9, got["total_tokens"])
	// 已有 total_tokens 不得被改写。
	kept := traeEnsureUsageTotal(map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 99})
	require.EqualValues(t, 99, kept["total_tokens"])
	// 计数字段解析不出来时不凭空造 total。
	none := traeEnsureUsageTotal(map[string]any{"completion_tokens": "abc", "cached": 1})
	require.NotContains(t, none, "total_tokens")
	// 数字字符串也参与求和（上游存在字符串计数形态）。
	strNum := traeEnsureUsageTotal(map[string]any{"prompt_tokens": "5", "completion_tokens": "2"})
	require.EqualValues(t, 7, strNum["total_tokens"])
	// 不得修改入参 map。
	input := map[string]any{"prompt_tokens": 3}
	_ = traeEnsureUsageTotal(input)
	require.NotContains(t, input, "total_tokens")
}

func TestTraeStreamErrorFormatting(t *testing.T) {
	t.Parallel()
	require.Equal(t, "trae stream error", (*traeStreamError)(nil).Error())
	require.Equal(t, "plain message", (&traeStreamError{Message: "plain message"}).Error())
	require.Equal(t, "boom (code=1001)", (&traeStreamError{Code: "1001", Message: "boom"}).Error())
}
