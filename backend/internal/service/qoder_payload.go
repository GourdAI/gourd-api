package service

// qoder_payload.go Qoder 出站请求体构造管线：
//   1. go:embed 模板（qoderassets.BasepromptJSON）读原始文本；
//   2. 占位符替换：{UUID1}~{UUID5} 各自独立新 UUID、{TIME1} unix 毫秒；
//   3. json.Unmarshal 为 map（替换前整份文件不是合法 JSON）；
//   4. 入站 CC messages → Qoder messages 转换（system 置前、user/assistant/tool
//      按 Qoder 形态重建、多模态 text 拼接、tools 透传、max_tokens 注入 parameters）；
//   5. 业务字段再加工：request_id/chat_record_id/business/chat_context/model_config。
//
// 上游恒为流式：body["stream"] 强制 true（与 workbuddy 的 PrepareWorkbuddyBody 同语义，
// 非流式请求由发送层聚合 SSE 为单个 CC JSON）。

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/qoderassets"
	"github.com/google/uuid"
)

// qoderNowTime 返回当前时间（独立函数便于测试控制时间源）。
func qoderNowTime() time.Time { return time.Now() }

// qoderTemplateReplacer 为每次构造请求生成：5 个独立 UUID + unix 毫秒时间戳。
func qoderTemplateReplacer() (*strings.Replacer, [5]string) {
	ids := [5]string{}
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	now := fmt.Sprintf("%d", qoderNowUnixMilli())
	return strings.NewReplacer(
		"{UUID1}", ids[0],
		"{UUID2}", ids[1],
		"{UUID3}", ids[2],
		"{UUID4}", ids[3],
		"{UUID5}", ids[4],
		"{TIME1}", now,
	), ids
}

// BuildQoderBody 由入站 CC 请求体构造 Qoder 上游请求体。
// modelKey 是映射后的 Qoder 模型 key（写 model_config.key 与 x-model-key 头由
// 调用方从返回值提取）；userType 为身份 UserType（写 aliyun_user_type）。
// 模板加载/占位符替换失败时返回错误（模板是协议根基，坏模板绝不降级原样发送）。
func BuildQoderBody(ccBody []byte, modelKey, userType string) ([]byte, *QoderBodyMeta, error) {
	replacer, ids := qoderTemplateReplacer()
	filled := replacer.Replace(string(qoderassetsBaseprompt()))
	var body map[string]any
	if err := json.Unmarshal([]byte(filled), &body); err != nil {
		return nil, nil, fmt.Errorf("qoder payload: parse baseprompt template: %w", err)
	}

	// 1) 主键：request_id = chat_record_id = ids[0]；request_set_id/session_id 各自独立。
	body["request_id"] = ids[0]
	body["chat_record_id"] = ids[0]
	body["request_set_id"] = ids[1]
	body["session_id"] = ids[3]
	// 上游只支持流式。
	body["stream"] = true
	if userType != "" {
		body["aliyun_user_type"] = userType
	}

	// 2) 入站消息转换。
	msgs, prompt, err := qoderConvertMessages(ccBody, body)
	if err != nil {
		return nil, nil, err
	}
	body["messages"] = msgs

	// 3) chat_context：text 与 extra.originalContent 承载最后一条 user 消息（prompt）。
	if ctx, ok := body["chat_context"].(map[string]any); ok {
		if text, ok := ctx["text"].(map[string]any); ok {
			text["text"] = prompt
		}
		if extra, ok := ctx["extra"].(map[string]any); ok {
			if oc, ok := extra["originalContent"].(map[string]any); ok {
				oc["text"] = prompt
			}
			// model_config 副本（chat_context.extra.modelConfig.key 同步映射后模型）。
			if mc, ok := extra["modelConfig"].(map[string]any); ok {
				mc["key"] = modelKey
			}
		}
	}

	// 4) model_config：key = 映射后模型 key，展示字段取静态目录。
	displayName := qoderModelDisplayName(modelKey)
	if mc, ok := body["model_config"].(map[string]any); ok {
		mc["key"] = modelKey
		mc["display_name"] = displayName
	}

	// 5) business：新 id、begin_at=unix 毫秒、name=最后一条 user 消息截 30 字符。
	if biz, ok := body["business"].(map[string]any); ok {
		biz["id"] = ids[4]
		biz["begin_at"] = qoderNowUnixMilli()
		biz["name"] = qoderTruncateRunes(prompt, 30)
	}

	// 6) tools / max_tokens / stream 注入。
	if err := qoderInjectToolsAndParams(ccBody, body); err != nil {
		return nil, nil, err
	}

	out, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("qoder payload: marshal body: %w", err)
	}
	return out, &QoderBodyMeta{
		RequestID:   ids[0],
		SessionID:   ids[3],
		BusinessID:  ids[4],
		ModelKey:    modelKey,
		DisplayName: displayName,
		Prompt:      prompt,
	}, nil
}

// QoderBodyMeta 是 BuildQoderBody 的产物元数据（客户端据此填签名 requestId
// 与 x-model-key 头）。
type QoderBodyMeta struct {
	RequestID   string
	SessionID   string
	BusinessID  string
	ModelKey    string
	DisplayName string
	Prompt      string
}

// qoderassetsBaseprompt 返回内嵌模板原始字节（独立函数便于测试打桩说明来源）。
func qoderassetsBaseprompt() []byte {
	return qoderassets.BasepromptJSON
}

// qoderNowUnixMilli 当前 unix 毫秒时间戳。
func qoderNowUnixMilli() int64 {
	return qoderNowTime().UnixMilli()
}

// qoderTruncateRunes 按字符数截断（中文名不劈成乱码）。
func qoderTruncateRunes(s string, n int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[:n])
}

// qoderCleanupOrphanToolCalls 剔除无法配对的 tool_call 与 tool 结果（所有模型，
// 恒生效）。语义与 cleanupWorkbuddyOrphanToolCalls 完全一致：
//   - assistant.tool_calls 按 keepCalls 对称裁剪：只留有结果配对的调用；
//     同一条 assistant 内重复 id 只保留首次出现；
//   - role:tool 每个 id 只保留首条结果，孤儿结果整条删除；
//   - 无任何工具流量 → 原 slice 原样返回，changed=false（零分配零改动）。
//
// 必要性：客户端（Codex/Claude Code 等）在工具执行失败或被中断时会把 assistant
// 的 tool_calls 持久化进会话历史却写不回结果消息；这段坏历史会被每次请求原样
// 重放，上游随即拒绝对之后每条用户消息服务——症状正是「只能对话，工具全废、
// 且之后普通对话也报错」。宁可丢一轮工具上下文，也要保住整条会话。
// 只要存在合法配对就整段保留，绝不吞掉正确配对。
func qoderCleanupOrphanToolCalls(messages []any) ([]any, bool) {
	if len(messages) == 0 {
		return messages, false
	}
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	hasTraffic := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch msg["role"] {
		case "tool":
			if id, ok := msg["tool_call_id"].(string); ok && id != "" {
				resultIDs[id] = true
				hasTraffic = true
			}
		case "assistant":
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, tci := range tcs {
					tc, ok := tci.(map[string]any)
					if !ok {
						continue
					}
					if id, ok := tc["id"].(string); ok && id != "" {
						callIDs[id] = true
						hasTraffic = true
					}
				}
			}
		}
	}
	if !hasTraffic {
		return messages, false
	}
	keepCalls := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keepCalls[id] = true
		}
	}
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		tcs, ok := msg["tool_calls"].([]any)
		if !ok || len(tcs) == 0 {
			continue
		}
		keptCalls := make([]any, 0, len(tcs))
		seenInAssistant := make(map[string]bool, len(tcs))
		for _, tci := range tcs {
			tc, ok := tci.(map[string]any)
			if !ok {
				continue
			}
			id, _ := tc["id"].(string)
			if !keepCalls[id] {
				continue
			}
			if seenInAssistant[id] {
				continue
			}
			seenInAssistant[id] = true
			keptCalls = append(keptCalls, tc)
		}
		if len(keptCalls) == len(tcs) {
			continue
		}
		changed = true
		if len(keptCalls) == 0 {
			delete(msg, "tool_calls")
			continue
		}
		msg["tool_calls"] = keptCalls
	}
	kept := make([]any, 0, len(messages))
	seenResult := make(map[string]bool, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		if role, _ := msg["role"].(string); role == "tool" {
			id, _ := msg["tool_call_id"].(string)
			if !keepCalls[id] {
				changed = true
				continue
			}
			if seenResult[id] {
				changed = true
				continue
			}
			seenResult[id] = true
		}
		kept = append(kept, m)
	}
	if !changed {
		return messages, false
	}
	return kept, true
}

// qoderConvertMessages 把入站 CC messages 转换为 Qoder messages 形态。
// 规则：
//   - system 消息置于最前（入站无 system 时保留模板自带 system）；
//   - developer 归一为 system；
//   - user → {role:"user", content:"", contents:[{type:"text",text:...}],
//     response_meta:{id:"",usage:全零}, reasoning_content_signature:""}；
//   - assistant → {role:"assistant", content:<文本>, tool_calls? , 同样带
//     response_meta / reasoning_content_signature}；
//   - tool → {role:"tool", tool_call_id, content}；
//   - 多模态 text part 拼接。
//
// 返回转换后的消息数组与最后一条 user 消息文本（prompt，供 chat_context/business 用）。
func qoderConvertMessages(ccBody []byte, template map[string]any) ([]any, string, error) {
	var cc struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(ccBody, &cc); err != nil {
		return nil, "", fmt.Errorf("qoder payload: parse CC request: %w", err)
	}

	// 模板自带 messages（首条为 system prompt）：入站无 system 时兜底保留。
	var templateSystem any
	if tplMsgs, ok := template["messages"].([]any); ok && len(tplMsgs) > 0 {
		if m, ok := tplMsgs[0].(map[string]any); ok {
			if role, _ := m["role"].(string); role == "system" {
				// 深拷贝避免两次构造共享引用。
				raw, _ := json.Marshal(m)
				var copy map[string]any
				_ = json.Unmarshal(raw, &copy)
				templateSystem = copy
			}
		}
	}

	// 工具配对自愈：先按「调用↔结果」对称裁剪并去重，再做角色形态重建。
	// 必须在角色重建之前——重建后 tool_calls 会被包进 Qoder 形态，裁剪逻辑
	// 需要读原始 CC 形态的 id 字段。
	ccMessages := make([]any, 0, len(cc.Messages))
	for _, raw := range cc.Messages {
		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err != nil {
			return nil, "", fmt.Errorf("qoder payload: parse message: %w", err)
		}
		ccMessages = append(ccMessages, msg)
	}
	ccMessages, _ = qoderCleanupOrphanToolCalls(ccMessages)

	out := make([]any, 0, len(ccMessages)+1)
	systemMsgs := make([]any, 0, 2)
	prompt := ""
	for _, m := range ccMessages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "system", "developer":
			systemMsgs = append(systemMsgs, map[string]any{"role": "system", "content": qoderContentText(msg["content"])})
		case "user":
			text := qoderContentText(msg["content"])
			prompt = text
			out = append(out, map[string]any{
				"role":    "user",
				"content": "",
				"contents": []any{
					map[string]any{"type": "text", "text": text},
				},
				"response_meta":               qoderZeroResponseMeta(),
				"reasoning_content_signature": "",
			})
		case "assistant":
			m := map[string]any{
				"role":                        "assistant",
				"content":                     qoderContentText(msg["content"]),
				"response_meta":               qoderZeroResponseMeta(),
				"reasoning_content_signature": "",
			}
			if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
				m["tool_calls"] = tcs
			}
			// assistant.historyReasoningContent 历史思维链回填（上游多轮一致性）。
			if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
				m["historyReasoningContent"] = rc
			}
			out = append(out, m)
		case "tool":
			// tool_call_id 必须非空：上游按 id 归并调用与结果，空/缺失 id 会让整段
			// 工具上下文无法配对（表现为工具结果被忽略或上游报错）。配对裁剪已保证
			// 有结果的调用 id 非空，这里再做一道防御：空 id 直接丢弃该条结果。
			callID, _ := msg["tool_call_id"].(string)
			if strings.TrimSpace(callID) == "" {
				continue
			}
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      qoderContentText(msg["content"]),
			})
		default:
			// 未知角色跳过（对坏输入宽容，不把整单请求错误化）。
		}
	}
	if len(systemMsgs) == 0 && templateSystem != nil {
		systemMsgs = append(systemMsgs, templateSystem)
	}
	if len(systemMsgs) > 0 {
		out = append(append([]any{}, systemMsgs...), out...)
	}
	return out, prompt, nil
}

// qoderZeroResponseMeta 构造全零 usage 的 response_meta（模板 user 消息的既有形态）。
func qoderZeroResponseMeta() map[string]any {
	return map[string]any{
		"id": "",
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
			"completion_tokens_details": map[string]any{
				"reasoning_tokens": 0,
			},
			"prompt_tokens_details": map[string]any{
				"cached_tokens": 0,
			},
		},
	}
}

// qoderContentText 提取消息 content 文本：字符串原样；多模态数组拼接全部 text part。
func qoderContentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			// 只拼接 text part；image_url 等 part 不进文本（上游 imageUrls 由模板承载）。
			if t, ok := m["type"].(string); ok && t == "text" {
				if txt, ok := m["text"].(string); ok {
					b.WriteString(txt)
				}
				continue
			}
			// 无 type 键但带 text 的 part 也兼容。
			if t, has := m["type"]; !has || t == nil {
				if txt, ok := m["text"].(string); ok {
					b.WriteString(txt)
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

// qoderInjectToolsAndParams 注入 tools（原样透传）与 max_tokens（入 parameters）。
func qoderInjectToolsAndParams(ccBody []byte, body map[string]any) error {
	var cc struct {
		Tools     []any `json:"tools"`
		MaxTokens int64 `json:"max_tokens"`
	}
	if err := json.Unmarshal(ccBody, &cc); err != nil {
		return fmt.Errorf("qoder payload: parse CC request tools: %w", err)
	}
	if len(cc.Tools) > 0 {
		body["tools"] = cc.Tools
	} else {
		// 客户端未声明任何工具：必须剔除模板自带的 14 个 Qoder IDE 内置工具
		// （Bash/Read/Write/Glob/Grep/…）。代理侧无法执行这些工具，一旦把它们
		// 暴露给模型，模型会调用 Bash/Read 等客户端根本不认识的工具，客户端无法
		// 回填结果 → 持续产出孤儿 tool_call，表现为「工具全废且之后普通对话也
		// 一轮轮退化」。宁可让这一轮退化成纯对话，也不要让模型追着幻影工具跑。
		delete(body, "tools")
	}
	if cc.MaxTokens > 0 {
		params, ok := body["parameters"].(map[string]any)
		if !ok {
			params = map[string]any{}
			body["parameters"] = params
		}
		params["max_tokens"] = cc.MaxTokens
	}
	return nil
}
