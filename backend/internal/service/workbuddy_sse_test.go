//go:build unit

package service

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// workbuddy_sse_test.go SSE 规范化读取器与聚合器的单测：
// 噪声帧剔除、tool name 收敛、id 续传、[DONE] 兜底、空流 error 帧、
// 聚合的 content/tool_calls 合并、usage 补 total 与截断参数丢弃。

// workbuddyReadAllSSE 读尽规范化流并拆出各 data 帧 payload。
func workbuddyReadAllSSE(t *testing.T, input string) []string {
	t.Helper()
	raw, err := io.ReadAll(newWorkbuddySSEReader(strings.NewReader(input)))
	require.NoError(t, err)
	var frames []string
	for _, line := range strings.Split(string(raw), "\n") {
		if payload, ok := extractOpenAISSEDataLine(line); ok {
			frames = append(frames, strings.TrimSpace(payload))
		}
	}
	return frames
}

func workbuddyCountDone(frames []string) int {
	n := 0
	for _, f := range frames {
		if f == "[DONE]" {
			n++
		}
	}
	return n
}

func TestWorkbuddySSEReaderNormalizesFrames(t *testing.T) {
	t.Parallel()
	input := ": keepalive\n\n" +
		`data: {"id":"chat-1","object":"chat.completion.chunk","system_fingerprint":"fp_1","noise_field":1,"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":""}]}` + "\n\n" +
		"\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}` + "\n\n" +
		"data: [DONE]\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"after-done-should-be-ignored"}}]}` + "\n\n"

	frames := workbuddyReadAllSSE(t, input)
	require.Len(t, frames, 3, "2 数据帧 + 1 [DONE]")
	require.Equal(t, 1, workbuddyCountDone(frames), "恰好一个 [DONE]")

	f0 := frames[0]
	require.Equal(t, "chat-1", gjson.Get(f0, "id").String())
	require.Equal(t, "chat.completion.chunk", gjson.Get(f0, "object").String())
	require.Equal(t, "fp_1", gjson.Get(f0, "system_fingerprint").String())
	require.False(t, gjson.Get(f0, "noise_field").Exists(), "顶层未知字段剔除")
	require.Equal(t, "assistant", gjson.Get(f0, "choices.0.delta.role").String())
	require.False(t, gjson.Get(f0, "choices.0.delta.content").Exists(), "空 content 噪声剔除")
	require.Equal(t, gjson.Null, gjson.Get(f0, "choices.0.finish_reason").Type, "finish_reason 空串转 null")
	require.Equal(t, gjson.Null, gjson.Get(f0, "usage").Type, "usage 缺失补 null")

	f1 := frames[1]
	require.Equal(t, "chat-1", gjson.Get(f1, "id").String(), "id 续传首帧真实 id")
	require.Equal(t, "你好", gjson.Get(f1, "choices.0.delta.content").String())
}

func TestWorkbuddySSEReaderToolCallNameStripping(t *testing.T) {
	t.Parallel()
	input := `data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":""}}]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"","arguments":"{\"a\""}}]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}` + "\n\n" +
		"data: [DONE]\n\n"

	frames := workbuddyReadAllSSE(t, input)
	require.Len(t, frames, 4)
	require.Equal(t, "bash", gjson.Get(frames[0], "choices.0.delta.tool_calls.0.function.name").String())
	require.False(t, gjson.Get(frames[1], "choices.0.delta.tool_calls.0.function.name").Exists(), "后续分片 name 键删除")
	require.Equal(t, `{"a"`, gjson.Get(frames[1], "choices.0.delta.tool_calls.0.function.arguments").String())
	require.False(t, gjson.Get(frames[2], "choices.0.delta.tool_calls.0.function.name").Exists())
	// id/type/arguments 原样透传（只动 function.name 键）。
	require.Equal(t, "call_1", gjson.Get(frames[1], "choices.0.delta.tool_calls.0.id").String())
	require.Equal(t, "function", gjson.Get(frames[1], "choices.0.delta.tool_calls.0.type").String())
}

func TestWorkbuddySSEReaderErrorFramePassthrough(t *testing.T) {
	t.Parallel()
	input := `data: {"error":{"message":"rate limited","code":"6004"}}` + "\n\n" + "data: [DONE]\n\n"
	frames := workbuddyReadAllSSE(t, input)
	require.Len(t, frames, 2)
	require.Equal(t, "rate limited", gjson.Get(frames[0], "error.message").String(), "error 帧原样透传")
	require.Equal(t, "6004", gjson.Get(frames[0], "error.code").String())
	require.Equal(t, 1, workbuddyCountDone(frames))
}

func TestWorkbuddySSEReaderEmptyStreamFallback(t *testing.T) {
	t.Parallel()
	// 只有注释/空行：输出一帧 error + [DONE]，不 panic。
	frames := workbuddyReadAllSSE(t, ": ping\n\n\n: pong\n")
	require.Len(t, frames, 2)
	require.Equal(t, "empty upstream stream", gjson.Get(frames[0], "error.message").String())
	require.Equal(t, "upstream_parse", gjson.Get(frames[0], "error.code").String())
	require.Equal(t, "[DONE]", frames[1])
}

func TestWorkbuddySSEReaderEnsuresSingleDone(t *testing.T) {
	t.Parallel()
	// 上游漏发 [DONE]（EOF 收尾）：网关兜底补一个。
	input := `data: {"id":"x","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n"
	frames := workbuddyReadAllSSE(t, input)
	require.Len(t, frames, 2)
	require.Equal(t, 1, workbuddyCountDone(frames))
}

func TestWorkbuddySSEReaderNilInput(t *testing.T) {
	t.Parallel()
	raw, err := io.ReadAll(newWorkbuddySSEReader(nil))
	require.NoError(t, err)
	require.Contains(t, string(raw), "empty upstream stream")
	require.Contains(t, string(raw), "[DONE]")
}

func TestAggregateWorkbuddySSE(t *testing.T) {
	t.Parallel()
	input := `data: {"id":"chat-agg","model":"glm-5.3","created":123,"choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"Hel"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"lo"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"a\""}}]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_cache_hit_tokens":7}}` + "\n\n" +
		"data: [DONE]\n\n"

	out, usage, err := aggregateWorkbuddySSE(strings.NewReader(input))
	require.NoError(t, err)
	require.Equal(t, "chat.completion", gjson.GetBytes(out, "object").String())
	require.Equal(t, "chat-agg", gjson.GetBytes(out, "id").String())
	require.Equal(t, "glm-5.3", gjson.GetBytes(out, "model").String())
	require.Equal(t, int64(123), gjson.GetBytes(out, "created").Int())
	require.Equal(t, "assistant", gjson.GetBytes(out, "choices.0.message.role").String())
	require.Equal(t, "Hello", gjson.GetBytes(out, "choices.0.message.content").String())
	require.Equal(t, "tool_calls", gjson.GetBytes(out, "choices.0.finish_reason").String())
	// tool_calls 按 index 合并：id/name 首片、arguments 拼接。
	require.Equal(t, int64(1), gjson.GetBytes(out, "choices.0.message.tool_calls.#").Int())
	require.Equal(t, "call_1", gjson.GetBytes(out, "choices.0.message.tool_calls.0.id").String())
	require.Equal(t, "bash", gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.name").String())
	require.Equal(t, `{"a":1}`, gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments").String())
	// usage：原字段保留 + 补 total；缓存命中映射到 CacheReadInputTokens。
	require.Equal(t, int64(15), gjson.GetBytes(out, "usage.total_tokens").Int())
	require.Equal(t, int64(7), gjson.GetBytes(out, "usage.prompt_cache_hit_tokens").Int(), "上游原字段保留")
	require.NotNil(t, usage)
	require.Equal(t, 10, usage.InputTokens)
	require.Equal(t, 5, usage.OutputTokens)
	require.Equal(t, 7, usage.CacheReadInputTokens)
}

func TestWorkbuddySSEReaderUsageCacheMapping(t *testing.T) {
	t.Parallel()
	// usage 帧：prompt_cache_hit_tokens 映射为标准嵌套字段（供计费识别缓存命中），
	// 原字段保留不删；已有 prompt_tokens_details 时不覆盖（上游标准字段优先）。
	input := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_cache_hit_tokens":7}}` + "\n\n" +
		"data: [DONE]\n\n"
	frames := workbuddyReadAllSSE(t, input)
	require.Len(t, frames, 3)
	require.Equal(t, int64(7), gjson.Get(frames[1], "usage.prompt_tokens_details.cached_tokens").Int(), "命中映射为标准字段")
	require.Equal(t, int64(7), gjson.Get(frames[1], "usage.prompt_cache_hit_tokens").Int(), "上游原字段保留")

	// 已有标准嵌套字段：不覆盖。
	input2 := `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_cache_hit_tokens":7,"prompt_tokens_details":{"cached_tokens":3}}}` + "\n\n" +
		"data: [DONE]\n\n"
	frames2 := workbuddyReadAllSSE(t, input2)
	require.Equal(t, int64(3), gjson.Get(frames2[0], "usage.prompt_tokens_details.cached_tokens").Int(), "标准字段优先")

	// 命中为 0：不新增嵌套字段（零改动）。
	input3 := `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_cache_hit_tokens":0}}` + "\n\n" +
		"data: [DONE]\n\n"
	frames3 := workbuddyReadAllSSE(t, input3)
	require.False(t, gjson.Get(frames3[0], "usage.prompt_tokens_details").Exists())
}

func TestAggregateWorkbuddySSETruncatedToolCalls(t *testing.T) {
	t.Parallel()
	// finish_reason=="length"：残缺 arguments 丢弃，完整参数保留。
	input := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"a","arguments":"{\"x\":"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c2","type":"function","function":{"name":"b","arguments":"{\"y\":2}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	out, _, err := aggregateWorkbuddySSE(strings.NewReader(input))
	require.NoError(t, err)
	require.Equal(t, int64(1), gjson.GetBytes(out, "choices.0.message.tool_calls.#").Int())
	require.Equal(t, "c2", gjson.GetBytes(out, "choices.0.message.tool_calls.0.id").String())

	// 无 [DONE]（EOF 截断）：残缺参数同样丢弃。
	inputNoDone := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"a","arguments":"{\"x\":"}}]}}]}` + "\n"
	out, _, err = aggregateWorkbuddySSE(strings.NewReader(inputNoDone))
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(out, "choices.0.message.tool_calls").Exists(), "EOF 截断的残缺参数被丢弃")
}

func TestAggregateWorkbuddySSEReasoningAndMessageFallback(t *testing.T) {
	t.Parallel()
	// reasoning_content 合并 + message（非 delta）形态兼容。
	input := `data: {"choices":[{"index":0,"delta":{"reasoning_content":"think-1 "}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"message":{"role":"assistant","content":"final"}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	out, _, err := aggregateWorkbuddySSE(strings.NewReader(input))
	require.NoError(t, err)
	require.Equal(t, "think-1 ", gjson.GetBytes(out, "choices.0.message.reasoning_content").String())
	require.Equal(t, "final", gjson.GetBytes(out, "choices.0.message.content").String())
}

func TestAggregateWorkbuddySSEEmptyStream(t *testing.T) {
	t.Parallel()
	_, _, err := aggregateWorkbuddySSE(strings.NewReader(": ping\n\n"))
	require.Error(t, err)
	require.True(t, workbuddyIsEmptyStreamError(err), "空流返回哨兵错误")

	_, _, err = aggregateWorkbuddySSE(nil)
	require.True(t, workbuddyIsEmptyStreamError(err))
}

func TestWorkbuddyIsTruncatedArguments(t *testing.T) {
	t.Parallel()
	require.False(t, workbuddyIsTruncatedArguments(""), "空串是合法无参工具")
	require.False(t, workbuddyIsTruncatedArguments("   "))
	require.False(t, workbuddyIsTruncatedArguments(`{"a":1}`))
	require.False(t, workbuddyIsTruncatedArguments(`null`))
	require.False(t, workbuddyIsTruncatedArguments(`[1,2]`))
	require.True(t, workbuddyIsTruncatedArguments(`{"a":`))
}
