package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func itemIDTestStr(s string) *string { return &s }

// 回归测试：response.completed 的 output 项必须复用流内已宣告的 item id。
// 旧实现在 chatOutput() 里对 reasoning / function_call / custom_tool_call /
// tool_search_call 换发新 id，客户端按 item id 合并「流事件 + completed」时会
// 把同一 call_id 存成两份 function_call，下一轮回放即触发上游 11148
// tool_call_sequence_broken（同 assistant 内重复 tool_call id）。
func TestFinalizeChatCompletionsResponsesStream_CompletedOutputReusesStreamItemIDs(t *testing.T) {
	state := NewChatCompletionsToResponsesStreamState("deepseek-test")

	var events []ResponsesStreamEvent
	events = append(events, ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{
		Choices: []ChatChunkChoice{{Delta: ChatDelta{ReasoningContent: itemIDTestStr("hmm")}}},
	}, state)...)
	events = append(events, ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{
		Choices: []ChatChunkChoice{{Delta: ChatDelta{Content: itemIDTestStr("checking")}}},
	}, state)...)
	i0, i1 := 0, 1
	events = append(events, ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{
		Choices: []ChatChunkChoice{{Delta: ChatDelta{ToolCalls: []ChatToolCall{
			{Index: &i0, ID: "call_a", Function: ChatFunctionCall{Name: "get_weather", Arguments: `{"city":"bj"}`}},
			{Index: &i1, ID: "call_b", Function: ChatFunctionCall{Name: "get_time", Arguments: `{"city":"bj"}`}},
		}}}},
	}, state)...)
	events = append(events, FinalizeChatCompletionsResponsesStream(state)...)

	streamItemIDByCall := map[string]string{}
	streamReasoningID := ""
	streamMessageID := ""
	for _, e := range events {
		if e.Type == "response.output_item.done" && e.Item != nil {
			switch e.Item.Type {
			case "function_call":
				streamItemIDByCall[e.Item.CallID] = e.Item.ID
			case "reasoning":
				streamReasoningID = e.Item.ID
			case "message":
				streamMessageID = e.Item.ID
			}
		}
	}
	require.Len(t, streamItemIDByCall, 2, "流内应宣告两个 function_call 项")
	require.NotEmpty(t, streamReasoningID)
	require.NotEmpty(t, streamMessageID)

	final := events[len(events)-1]
	require.Equal(t, "response.completed", final.Type)
	require.NotNil(t, final.Response)

	seen := map[string]int{}
	for _, item := range final.Response.Output {
		switch item.Type {
		case "function_call":
			assert.Equal(t, streamItemIDByCall[item.CallID], item.ID,
				"completed 的 function_call item id 必须与流内 output_item.done 一致")
			seen[item.CallID]++
		case "reasoning":
			assert.Equal(t, streamReasoningID, item.ID,
				"completed 的 reasoning item id 必须与流内一致")
		case "message":
			assert.Equal(t, streamMessageID, item.ID)
		}
	}
	assert.Equal(t, 1, seen["call_a"], "completed 内同一 call_id 只能出现一次")
	assert.Equal(t, 1, seen["call_b"], "completed 内同一 call_id 只能出现一次")
}

// 回归测试：入站历史里同一条 assistant 消息内重复的 tool_call id 只保留一次，
// 让被旧版网关污染过的客户端历史能够自愈（上游对重复 id 必拒 11148）。
func TestNormalizeChatMessages_DeduplicatesRepeatedToolCallIDs(t *testing.T) {
	dup := ChatToolCall{ID: "call_dup", Type: "function", Function: ChatFunctionCall{Name: "a", Arguments: `{}`}}
	other := ChatToolCall{ID: "call_ok", Type: "function", Function: ChatFunctionCall{Name: "b", Arguments: `{}`}}
	msgs := []ChatMessage{
		{Role: "user", Content: json.RawMessage(`"hi"`)},
		{Role: "assistant", ToolCalls: []ChatToolCall{dup, other, dup}},
		{Role: "tool", ToolCallID: "call_dup", Content: json.RawMessage(`"r1"`)},
		{Role: "tool", ToolCallID: "call_ok", Content: json.RawMessage(`"r2"`)},
	}

	out := normalizeChatMessages(msgs)

	require.Len(t, out, 4)
	require.Len(t, out[1].ToolCalls, 2, "重复 id 应去重为一次")
	assert.Equal(t, "call_dup", out[1].ToolCalls[0].ID)
	assert.Equal(t, "call_ok", out[1].ToolCalls[1].ID)
	assert.Equal(t, "tool", out[2].Role)
	assert.Equal(t, "call_dup", out[2].ToolCallID)
	assert.Equal(t, "tool", out[3].Role)
	assert.Equal(t, "call_ok", out[3].ToolCallID)
}
