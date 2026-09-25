package service

// openai_gateway_tool_pairing_audit.go WorkBuddy 上游 11148（tool_call_sequence_broken）
// 配对审计诊断。
//
// 背景：上游对工具调用历史的校验规则（2026-09-21 实测坐实，cmd/wbpair 直连）为
// 「每个 tool_call 恰好一条结果、tool_call_id 非空且唯一、结果紧跟其 assistant」，
// 违反即 400 code=11148。客户端（Codex / workbuddy-test 等）回放历史时可能重编
// item id、丢弃 function_call_output 或打乱顺序，网关桥接层虽有一层
// normalizeChatMessages 修复，但修复发生在出站之前，无法覆盖「出站后仍不合规」
// 的残余形态。本文件在上游返回 11148 时对出站 body 做一次只读审计，把具体断裂点
// （缺哪条结果 / 哪条孤儿 / 哪里不紧邻 / 哪个 id 缺失或重复）打进日志，供定位。
//
// 审计是纯只读的：不改写 body、不影响转发与错误响应，仅在命中 11148 时记一条 WARN。
// 出站 body 支持两种格式：chat completions（messages 数组，含紧邻性检查）与
// responses（input 数组，仅对称性/孤儿/id 检查——responses 格式里 call 与 output
// 允许同轮内分离排列，紧邻性由上游转换后保证）。

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// isWorkbuddyToolSequenceBrokenError 判定上游错误体是否为 11148 工具序列断裂。
// WorkBuddy 错误信封形如 {"code":11148,...,"extError":{"code":"tool_call_sequence_broken"}}，
// 双信号任一命中即认定（数字码优先，字符串码兜底）。
func isWorkbuddyToolSequenceBrokenError(upstreamBody []byte) bool {
	if len(upstreamBody) == 0 {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(upstreamBody, &obj); err != nil {
		return false
	}
	if rawJSONNumber(obj["code"]) == 11148 {
		return true
	}
	var ext map[string]json.RawMessage
	if err := json.Unmarshal(obj["extError"], &ext); err == nil {
		if strings.EqualFold(rawJSONString(ext["code"]), "tool_call_sequence_broken") {
			return true
		}
	}
	return false
}

// auditWorkbuddyToolPairing 审计出站 body 的工具配对不变量，返回人类可读的违规
// 描述列表；空列表表示满足全部不变量。
//
// chat 格式（messages）检查项（与上游实测判定规则一一对应）：
//  1. assistant 的每个 tool_call 必须恰好一条 role=tool 结果（数量对称）；
//  2. role=tool 必须带非空 tool_call_id 且能对应到某个 assistant tool_call（无孤儿）；
//  3. tool_call.id 不得为空（空 id 出站 JSON 里键会整体消失）；
//  4. 同一请求内 tool_call id 不得重复；
//  5. tool 结果与它的 assistant 之间不得插入其他角色消息（紧邻性）。
//
// responses 格式（input）检查项：对称性、孤儿 output、空/重复 call_id
// （不查紧邻性，见文件头注释）。
func auditWorkbuddyToolPairing(outboundBody []byte) []string {
	if len(outboundBody) == 0 {
		return []string{"no outbound body captured for audit"}
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(outboundBody, &obj); err != nil {
		return []string{"outbound body is not valid JSON"}
	}
	if raw, ok := obj["messages"]; ok {
		var messages []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &messages); err == nil {
			return auditChatMessagesPairing(messages)
		}
		return []string{"messages present but not an array"}
	}
	if raw, ok := obj["input"]; ok {
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &items); err == nil {
			return auditResponsesInputPairing(items)
		}
		return []string{"input present but not an array"}
	}
	return []string{"outbound body has neither messages nor input array"}
}

// auditChatMessagesPairing 审计 chat completions 出站 messages 的配对不变量。
func auditChatMessagesPairing(messages []map[string]json.RawMessage) []string {
	var problems []string

	type callRef struct {
		msgIndex int
		callID   string
		name     string
	}
	callOwner := make(map[string]int) // call_id → 所在 assistant 的 messages 下标
	seenCallIDs := make(map[string]int)
	callCountByCallID := make(map[string]int)
	resultsByCallID := make(map[string]int)
	firstCallByCallID := make(map[string]callRef)
	var uniqueCallOrder []string

	for i, m := range messages {
		switch rawJSONString(m["role"]) {
		case "assistant":
			var toolCalls []map[string]json.RawMessage
			if err := json.Unmarshal(m["tool_calls"], &toolCalls); err != nil || len(toolCalls) == 0 {
				continue
			}
			for _, tc := range toolCalls {
				var fn map[string]json.RawMessage
				_ = json.Unmarshal(tc["function"], &fn)
				id := rawJSONString(tc["id"])
				name := rawJSONString(fn["name"])
				if id == "" {
					problems = append(problems, fmt.Sprintf("messages[%d] assistant tool_call has empty id (name=%q)", i, name))
				} else {
					if prev, dup := seenCallIDs[id]; dup {
						problems = append(problems, fmt.Sprintf("duplicate tool_call id %q (messages[%d] and messages[%d])", id, prev, i))
					} else {
						seenCallIDs[id] = i
						callOwner[id] = i
						uniqueCallOrder = append(uniqueCallOrder, id)
						firstCallByCallID[id] = callRef{msgIndex: i, callID: id, name: name}
					}
					callCountByCallID[id]++
				}
			}
		case "tool":
			id := rawJSONString(m["tool_call_id"])
			if id == "" {
				problems = append(problems, fmt.Sprintf("messages[%d] role=tool has no tool_call_id key (orphan result)", i))
				continue
			}
			owner, known := callOwner[id]
			if !known {
				problems = append(problems, fmt.Sprintf("messages[%d] role=tool tool_call_id=%q has no matching assistant tool_call (orphan result)", i, id))
				continue
			}
			resultsByCallID[id]++
			// 紧邻性：结果必须紧跟其 assistant（允许同一 assistant 的连续结果）。
			if prev := previousNonToolIndex(messages, i); prev != owner {
				problems = append(problems, fmt.Sprintf("messages[%d] tool result for %q is not adjacent to its assistant (messages[%d]); intervening message at messages[%d]", i, id, owner, prev))
			}
		}
	}

	// 对称性：每个唯一 tool_call id 的实例数必须等于结果数（重复 id 时按实例数比对）。
	for _, id := range uniqueCallOrder {
		first := firstCallByCallID[id]
		want := callCountByCallID[id]
		got := resultsByCallID[id]
		if got != want {
			problems = append(problems, fmt.Sprintf("assistant tool_call %q (%s) first at messages[%d]: %d call instance(s) but %d tool result(s) (expected equal)", id, first.name, first.msgIndex, want, got))
		}
	}
	return problems
}

// auditResponsesInputPairing 审计 responses 格式 input 数组的 call/output 对称性。
func auditResponsesInputPairing(items []map[string]json.RawMessage) []string {
	var problems []string
	callTypes := map[string]bool{"function_call": true, "custom_tool_call": true, "tool_search_call": true}
	outputTypes := map[string]bool{"function_call_output": true, "custom_tool_call_output": true, "tool_search_output": true}

	type callRef struct {
		index  int
		callID string
		typ    string
	}
	var calls []callRef
	seenCallIDs := make(map[string]int)
	resultsByCallID := make(map[string]int)
	callOwner := make(map[string]int)

	for i, item := range items {
		typ := rawJSONString(item["type"])
		switch {
		case callTypes[typ]:
			id := rawJSONString(item["call_id"])
			if id == "" {
				problems = append(problems, fmt.Sprintf("input[%d] %s has empty call_id", i, typ))
			} else {
				if prev, dup := seenCallIDs[id]; dup {
					problems = append(problems, fmt.Sprintf("duplicate call_id %q (input[%d] and input[%d])", id, prev, i))
				}
				seenCallIDs[id] = i
				callOwner[id] = i
			}
			calls = append(calls, callRef{index: i, callID: id, typ: typ})
		case outputTypes[typ]:
			id := rawJSONString(item["call_id"])
			if id == "" {
				problems = append(problems, fmt.Sprintf("input[%d] %s has empty call_id (orphan output)", i, typ))
				continue
			}
			if _, known := callOwner[id]; !known {
				problems = append(problems, fmt.Sprintf("input[%d] %s call_id=%q has no matching call item (orphan output)", i, typ, id))
				continue
			}
			resultsByCallID[id]++
		}
	}
	for _, c := range calls {
		if c.callID == "" {
			continue
		}
		switch resultsByCallID[c.callID] {
		case 0:
			problems = append(problems, fmt.Sprintf("input[%d] %s call_id=%q has NO matching output item", c.index, c.typ, c.callID))
		case 1:
			// ok
		default:
			problems = append(problems, fmt.Sprintf("input[%d] %s call_id=%q has %d output items (expected exactly 1)", c.index, c.typ, c.callID, resultsByCallID[c.callID]))
		}
	}
	return problems
}

// previousNonToolIndex 返回 i 之前最近一条非 tool 角色消息的下标；全为 tool 时返回 -1。
func previousNonToolIndex(messages []map[string]json.RawMessage, i int) int {
	for j := i - 1; j >= 0; j-- {
		if rawJSONString(messages[j]["role"]) != "tool" {
			return j
		}
	}
	return -1
}

// logWorkbuddyToolPairingAudit 在上游返回 11148 时打印出站配对审计。
// 只读诊断：不修改任何请求/响应状态。
func logWorkbuddyToolPairingAudit(upstreamBody, outboundBody []byte, accountID int64, platform string) {
	if !isWorkbuddyToolSequenceBrokenError(upstreamBody) {
		return
	}
	problems := auditWorkbuddyToolPairing(outboundBody)
	if len(problems) == 0 {
		logger.L().Warn("workbuddy 11148 pairing audit: outbound history satisfies all pairing invariants; breakage likely introduced by client history replay beyond call/output symmetry",
			zap.Int64("account_id", accountID),
			zap.String("platform", platform),
		)
		return
	}
	fields := []zap.Field{
		zap.Int64("account_id", accountID),
		zap.String("platform", platform),
		zap.Int("problem_count", len(problems)),
	}
	for i, p := range problems {
		if i >= 20 {
			fields = append(fields, zap.String("problem_truncated", "more problems omitted"))
			break
		}
		fields = append(fields, zap.String(fmt.Sprintf("problem_%d", i), p))
	}
	logger.L().Warn("workbuddy 11148 pairing audit: outbound tool-call history violates upstream invariants", fields...)
}

// rawJSONString 取 JSON 字符串字段值；缺失/非字符串/null 均返回 ""。
func rawJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// rawJSONNumber 取 JSON 数字字段值；缺失/非数字返回 0。
func rawJSONNumber(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0
	}
	return f
}
