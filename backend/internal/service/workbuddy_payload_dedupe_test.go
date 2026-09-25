//go:build unit

package service

// workbuddy_payload_dedupe_test.go 锁死 WorkBuddy 出站 body 的「工具调用配对」
// 不变式——这是上游 11148（tool_call_sequence_broken）的判定面：
//
//  1. 同一条 assistant 消息内 tool_call id 必须唯一；
//  2. 每个 tool_call 必须有且仅有一条 tool 结果（数量对称）；
//  3. 每条 tool 结果必须有对应的 tool_call（无孤儿）。
//
// 背景：上游实测对「同一 assistant 内两个相同 tool_call id」必返 400 code=11148
// （displayMsg「工具调用记录不完整，请重新发起对话。」），症状就是「只能对话发
// 一条消息」——带工具的第二轮开始整条会话报废。apicompat 的 responses→chat 桥
// 已做去重，但 WorkBuddy 原生 CC 入站走的是 workbuddy_payload.go 自己的
// cleanupWorkbuddyOrphanToolCalls，那里只做「是否有配对」的对称裁剪，不去重
// 重复 id：两个相同 id 都有结果 → keepCalls 命中两次 → 两条原样出站 → 11148。

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// assertWorkbuddyToolPairingInvariants 对出站 body 做全线配对不变式审计。
func assertWorkbuddyToolPairingInvariants(t *testing.T, body []byte) {
	t.Helper()
	msgs := gjson.GetBytes(body, "messages").Array()
	seenCall := map[string]int{}
	seenResult := map[string]int{}
	for _, m := range msgs {
		switch m.Get("role").String() {
		case "assistant":
			for _, tc := range m.Get("tool_calls").Array() {
				id := tc.Get("id").String()
				require.NotEmpty(t, id, "tool_call id 不得为空")
				seenCall[id]++
				require.Equal(t, 1, seenCall[id],
					"tool_call id %q 在出站 body 中出现 %d 次：上游对重复 id 必拒 11148", id, seenCall[id])
			}
		case "tool":
			id := m.Get("tool_call_id").String()
			require.NotEmpty(t, id, "tool 结果不得缺 tool_call_id")
			seenResult[id]++
			require.Equal(t, 1, seenResult[id],
				"tool 结果 %q 出现 %d 次：调用与结果数量不对称，上游判 11148", id, seenResult[id])
		}
	}
	for id := range seenCall {
		require.Equal(t, 1, seenResult[id], "tool_call %q 缺少配对结果", id)
	}
	for id := range seenResult {
		require.Equal(t, 1, seenCall[id], "tool 结果 %q 是孤儿（无对应 tool_call）", id)
	}
}

// TestPrepareWorkbuddyBodyDedupesRepeatedToolCallIDs 同一条 assistant 内重复的
// tool_call id 必须去重为一次（被旧版网关 completed 换发 item id 污染过的客户端
// 历史），并保持「一调用一结果」对称。
func TestPrepareWorkbuddyBodyDedupesRepeatedToolCallIDs(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call-1","type":"function","function":{"name":"Read","arguments":"{\"p\":\"a\"}"}},
			{"id":"call-1","type":"function","function":{"name":"Read","arguments":"{\"p\":\"a\"}"}}
		]},
		{"role":"tool","tool_call_id":"call-1","content":"ok"}
	]}`)
	out := PrepareWorkbuddyBody(body, WorkbuddyCredentials{UID: "u-1", Realm: "cn"})

	calls := gjson.GetBytes(out, "messages.1.tool_calls").Array()
	require.Len(t, calls, 1, "重复 tool_call id 必须去重为一次")
	require.Equal(t, "call-1", calls[0].Get("id").String())
	assertWorkbuddyToolPairingInvariants(t, out)
}

// TestPrepareWorkbuddyBodyDropsDuplicateToolResults 同一 tool_call_id 的多条
// 结果只保留一条（旧版网关污染历史里「一调用多结果」同样触发 11148）。
func TestPrepareWorkbuddyBodyDropsDuplicateToolResults(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call-1","type":"function","function":{"name":"Bash","arguments":"{}"}}
		]},
		{"role":"tool","tool_call_id":"call-1","content":"first"},
		{"role":"tool","tool_call_id":"call-1","content":"second"}
	]}`)
	out := PrepareWorkbuddyBody(body, WorkbuddyCredentials{UID: "u-1", Realm: "cn"})
	assertWorkbuddyToolPairingInvariants(t, out)
}

// TestPrepareWorkbuddyBodyPreservesHealthyParallelToolCalls 合法并行工具调用
// （不同 id、各自一条结果）必须整段保留——去重绝不能误伤正确配对。
func TestPrepareWorkbuddyBodyPreservesHealthyParallelToolCalls(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call-1","type":"function","function":{"name":"Read","arguments":"{}"}},
			{"id":"call-2","type":"function","function":{"name":"Bash","arguments":"{}"}}
		]},
		{"role":"tool","tool_call_id":"call-1","content":"a"},
		{"role":"tool","tool_call_id":"call-2","content":"b"}
	]}`)
	out := PrepareWorkbuddyBody(body, WorkbuddyCredentials{UID: "u-1", Realm: "cn"})
	require.Len(t, gjson.GetBytes(out, "messages.1.tool_calls").Array(), 2, "合法并行调用不得被裁剪")
	assertWorkbuddyToolPairingInvariants(t, out)
}

// TestPrepareWorkbuddyBodyToolPairingMultiTurnReplay 多轮工具历史回放（两轮
// call+result 交替）必须原样通过：去重逻辑不得把跨轮的合法历史改坏。
func TestPrepareWorkbuddyBodyToolPairingMultiTurnReplay(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call-1","type":"function","function":{"name":"Read","arguments":"{}"}}
		]},
		{"role":"tool","tool_call_id":"call-1","content":"a"},
		{"role":"assistant","content":"done"},
		{"role":"user","content":"again"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call-2","type":"function","function":{"name":"Read","arguments":"{}"}}
		]},
		{"role":"tool","tool_call_id":"call-2","content":"b"}
	]}`)
	out := PrepareWorkbuddyBody(body, WorkbuddyCredentials{UID: "u-1", Realm: "cn"})
	assertWorkbuddyToolPairingInvariants(t, out)
}
