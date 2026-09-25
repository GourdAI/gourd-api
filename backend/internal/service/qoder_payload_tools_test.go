//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// qoder_payload_tools_test.go Qoder 出站工具链路的不变量测试。
//
// 背景（与 WorkBuddy 11148 同一类缺陷）：客户端在工具执行失败/被中断时会把
// assistant 的 tool_calls 持久化进会话历史却不写回结果。这段坏历史会被每次
// 请求原样重放，上游随即拒绝对之后每条消息服务 —— 症状正是「只能对话，工具
// 全废，且之后连普通对话也受影响」。
//
// 本文件锁定两条不变量：
//   1. 出站消息满足「每个 tool_call 恰好一条结果、id 非空且唯一」（配对+去重）；
//   2. 合法流量（并行调用、多轮回放）零改动，绝不能被自愈逻辑误伤。

// qoderTemplateFilledJSON 返回占位符替换后的模板 JSON（不跑转换管线），用于断言
// 模板本身的形态（如内置 IDE 工具）确实存在，避免测试在测空气。
func qoderTemplateFilledJSON(t *testing.T) string {
	t.Helper()
	replacer, _ := qoderTemplateReplacer()
	return replacer.Replace(string(qoderassetsBaseprompt()))
}

// qoderFindAssistantWithToolCalls 返回第一条带 tool_calls 的 assistant 消息。
func qoderFindAssistantWithToolCalls(t *testing.T, msgs []map[string]any) map[string]any {
	t.Helper()
	for _, m := range msgs {
		if m["role"] != "assistant" {
			continue
		}
		if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
			return m
		}
	}
	return nil
}

// TestBuildQoderBodyDropsOrphanToolCalls assistant 带 tool_calls 但没有对应
// tool 结果（工具执行失败被客户端持久化）→ 整批剔除，不得留下无结果的 tool_call。
func TestBuildQoderBodyDropsOrphanToolCalls(t *testing.T) {
	t.Parallel()
	cc := []byte(`{"model":"auto","messages":[
		{"role":"user","content":"list files"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"ls","arguments":"{}"}}]},
		{"role":"user","content":"thanks"}
	]}`)
	body, _, err := BuildQoderBody(cc, "auto", "")
	require.NoError(t, err)
	msgs := qoderUnmarshalMessages(t, body)
	require.Nil(t, qoderFindAssistantWithToolCalls(t, msgs), "孤儿 tool_call 必须被剔除")
}

// TestBuildQoderBodyDropsDuplicateToolCallIDs 同一 assistant 内两个相同
// tool_call id（网关换发 item id 污染过的历史）→ 只保留首次出现；同一 id 的
// 多条结果同样只保留首条。
func TestBuildQoderBodyDropsDuplicateToolCallIDs(t *testing.T) {
	t.Parallel()
	cc := []byte(`{"model":"auto","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":"","tool_calls":[
			{"id":"call-1","type":"function","function":{"name":"ls","arguments":"{}"}},
			{"id":"call-1","type":"function","function":{"name":"ls","arguments":"{}"}}
		]},
		{"role":"tool","tool_call_id":"call-1","content":"first"},
		{"role":"tool","tool_call_id":"call-1","content":"second"}
	]}`)
	body, _, err := BuildQoderBody(cc, "auto", "")
	require.NoError(t, err)
	msgs := qoderUnmarshalMessages(t, body)

	assistant := qoderFindAssistantWithToolCalls(t, msgs)
	require.NotNil(t, assistant)
	require.Len(t, assistant["tool_calls"], 1, "重复 id 只保留首次出现")

	results := 0
	for _, m := range msgs {
		if m["role"] == "tool" {
			results++
			require.Equal(t, "first", m["content"], "同 id 多结果只保留首条")
		}
	}
	require.Equal(t, 1, results, "重复结果被去重")
}

// TestBuildQoderBodyPreservesValidParallelToolCalls 合法并行调用（两个不同 id，
// 各有一条结果）必须原样保留 —— 自愈逻辑不得误伤。
func TestBuildQoderBodyPreservesValidParallelToolCalls(t *testing.T) {
	t.Parallel()
	cc := []byte(`{"model":"auto","messages":[
		{"role":"user","content":"read two files"},
		{"role":"assistant","content":"","tool_calls":[
			{"id":"call-1","type":"function","function":{"name":"read","arguments":"{\"p\":\"a\"}"}},
			{"id":"call-2","type":"function","function":{"name":"read","arguments":"{\"p\":\"b\"}"}}
		]},
		{"role":"tool","tool_call_id":"call-1","content":"A"},
		{"role":"tool","tool_call_id":"call-2","content":"B"},
		{"role":"user","content":"summarize"}
	]}`)
	body, _, err := BuildQoderBody(cc, "auto", "")
	require.NoError(t, err)
	msgs := qoderUnmarshalMessages(t, body)

	assistant := qoderFindAssistantWithToolCalls(t, msgs)
	require.NotNil(t, assistant)
	require.Len(t, assistant["tool_calls"], 2, "合法并行调用零改动")

	ids := map[string]bool{}
	for _, m := range msgs {
		if m["role"] == "tool" {
			id, _ := m["tool_call_id"].(string)
			require.NotEmpty(t, id, "tool 结果不得缺 tool_call_id")
			ids[id] = true
		}
	}
	require.Equal(t, map[string]bool{"call-1": true, "call-2": true}, ids)
}

// TestBuildQoderBodyPreservesMultiRoundToolReplay 两轮工具历史回放（call+result
// 交替出现两次）必须完整保留，不得被去重逻辑当成重复 id 砍掉。
func TestBuildQoderBodyPreservesMultiRoundToolReplay(t *testing.T) {
	t.Parallel()
	cc := []byte(`{"model":"auto","messages":[
		{"role":"user","content":"q1"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"ls","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call-1","content":"r1"},
		{"role":"user","content":"q2"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call-2","type":"function","function":{"name":"ls","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call-2","content":"r2"}
	]}`)
	body, _, err := BuildQoderBody(cc, "auto", "")
	require.NoError(t, err)
	msgs := qoderUnmarshalMessages(t, body)

	calls, results := 0, map[string]bool{}
	for _, m := range msgs {
		switch m["role"] {
		case "assistant":
			if tcs, ok := m["tool_calls"].([]any); ok {
				calls += len(tcs)
			}
		case "tool":
			id, _ := m["tool_call_id"].(string)
			results[id] = true
		}
	}
	require.Equal(t, 2, calls, "两轮调用全部保留")
	require.Equal(t, map[string]bool{"call-1": true, "call-2": true}, results, "两轮结果全部保留")
}

// TestBuildQoderBodyDropsToolResultWithoutCallID 缺 tool_call_id 的 tool 结果
// 无法与任何调用配对 → 丢弃（上游按 id 归并，空 id 会让整段上下文错配）。
func TestBuildQoderBodyDropsToolResultWithoutCallID(t *testing.T) {
	t.Parallel()
	cc := []byte(`{"model":"auto","messages":[
		{"role":"user","content":"q"},
		{"role":"tool","content":"orphan without id"}
	]}`)
	body, _, err := BuildQoderBody(cc, "auto", "")
	require.NoError(t, err)
	msgs := qoderUnmarshalMessages(t, body)
	for _, m := range msgs {
		require.NotEqual(t, "tool", m["role"], "无 id 的 tool 结果被丢弃")
	}
}

// TestBuildQoderBodyToolsClientOverridesTemplate 客户端显式声明 tools 时以客户端
// 为准（整份替换，避免模板内置 IDE 工具与客户端工具重名）。
func TestBuildQoderBodyToolsClientOverridesTemplate(t *testing.T) {
	t.Parallel()
	cc := []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"my_tool","parameters":{"type":"object"}}}]}`)
	body, _, err := BuildQoderBody(cc, "auto", "")
	require.NoError(t, err)
	var obj map[string]any
	require.NoError(t, json.Unmarshal(body, &obj))
	tools, ok := obj["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 1, "客户端 tools 整份替换模板内置工具")
}

// TestBuildQoderBodyToolsStrippedWhenClientDeclaresNone 客户端未声明 tools 时，
// 模板自带的 14 个 Qoder IDE 内置工具必须剔除：代理无法执行它们，暴露出去会让
// 模型追着幻影工具跑，客户端无法回填结果。
func TestBuildQoderBodyToolsStrippedWhenClientDeclaresNone(t *testing.T) {
	t.Parallel()
	cc := []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	// 前置断言：模板本身确实带内置工具（保证这条测试不是在测空气）。
	tpl := map[string]any{}
	filled := qoderTemplateFilledJSON(t)
	require.NoError(t, json.Unmarshal([]byte(filled), &tpl))
	tplTools, _ := tpl["tools"].([]any)
	require.NotEmpty(t, tplTools, "模板应内置 IDE 工具，否则本测试无意义")

	body, _, err := BuildQoderBody(cc, "auto", "")
	require.NoError(t, err)
	var obj map[string]any
	require.NoError(t, json.Unmarshal(body, &obj))
	_, exists := obj["tools"]
	require.False(t, exists, "客户端无 tools 时不得暴露模板内置 IDE 工具")
}
