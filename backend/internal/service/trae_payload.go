package service

// trae_payload.go Trae 出站请求体构造：把入站 OpenAI Chat Completions 请求变换为
// Trae 自研协议的形态，并给出候选端点尝试序列。
//
// 上游没有 OpenAI 兼容端点，已知两条对话通道（参考实现 dsh-trae-api 的"3 级端点回退"）：
//  1. /api/agent/v3/llm_utils_chat —— SOLO/新版 IDE 通道，body 基本就是 OpenAI 形态
//     （messages/tools/temperature/...）再加 function + config_name 两个路由字段；
//  2. /api/ide/v1/chat —— 老 IDE 标准对话通道，body 是 TraeRequest（user_input /
//     chat_history / variables(stringified JSON) / model_name / session_id ...），
//     与 OpenAI 形态完全不同，需要显式重建。
//
// 回退纪律：只有第 1 条返回 404（路由不存在）才尝试第 2 条；4xx 业务错误不回退，
// 否则等于对同一 prompt 重复扣积分。
//
// 工具调用纪律：入站带 tools / tool_choice 时**不得**产出回退通道——TraeRequest 报文
// 没有任何可承载工具定义的字段（见 traeIDERequest），一旦静默降级，客户端会以为工具
// 可用而模型永远不会调用，表现为「回答正常但从不出工具」，比直接报错难查得多。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// traeCatalogFunction 是 Trae 上游「可调模型表」的分片键：目录接口
// get_detail_param 用它拉哪张表，聊天接口 llm_utils_chat 就必须用同一个值发，
// 否则「拉到 A 表、用 B 表调用」会把必 4001 的模型开放给下游。
//
// 【为什么是 solo_agent 而不是 solo_work_lite】上游把可调用模型按 function 分片，
// 一个 config_name 只能通过「列出它的那个 function」调用；发错 function 得到的是
// HTTP 200 + 流内 code:4001「param is invalid」，而不是 4xx（2026-09-29 实测）。
// 同一张有效账号票逐 function 实测（版本画像 0.1.61/20260820）：
//   - solo_work_lite 可见表 30 条：qwen3.8-flash / glm-5.3-flash / glm-5.3-flashx /
//     kimi-k2.8-preview / Doubao-Seed-Code / step-5-preview 不在表内，发过去必 4001；
//   - solo_agent 可见表 36 条：上述模型全在，且实测**全部正常出流**；
//   - 两表 27 个交集模型用 solo_agent 调用全部出流，solo_work_lite 独有的
//     kimi-k2.6 / kimi-k2.7-code 用 solo_agent **同样能调** —— 切换零损失；
//   - solo_agent_lite / solo_work_remote / inline_chat 调 qwen3.8-flash 仍 4001。
//
// 第三方实现记载「llm_utils_chat 除 solo_work_lite 外一律 4001」「qwen3.8-flash
// 插件侧无可调用通道」（cpa-multi-plugins variant.go、dsh-connect-trae README）均
// 不成立：他们的 CN 目录只请求 solo_work_remote + solo_work_lite，从未在本仓这套
// 版本画像下试过 solo_agent。改这个值必须同时改两处消费点：聊天侧
// traeChatFunction 与目录侧 traeModelListBody。
const traeCatalogFunction = "solo_agent"

// traeChatFunction 主通道的 function 路由值。
//
// 与目录侧同源（见 traeCatalogFunction 的实测记录）：两侧一旦分头改动，
// 同步回来的模型表与实际可调用表就会不一致，用户会拿到必 4001 的模型。
const traeChatFunction = traeCatalogFunction

// errTraeNoUserInput ide/v1/chat 形态没有任何可用 user 输入。
var errTraeNoUserInput = errors.New("trae: no user input in request messages")

// traeAttempt 一次出站尝试：目标路径 + 已编码请求体。
type traeAttempt struct {
	path string
	body []byte
}

// traeParsedRequest 入站 CC 请求体的最小解析视图（两条通道共用）。
type traeParsedRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// traeChatAttempts 构造按序尝试的出站请求体。主通道必定产出；以下两种情形不产出
// 回退通道（宁可直接 404 报错，也不静默降级）：
//   - 入站声明了工具（tools 非空或 tool_choice 强制）——回退报文会丢掉工具定义；
//   - 回退报文本身构造失败（无可用 user 输入）。
//
// accountID 用于派生会话锚点（见 traeStableConversationID），必须传账号真实 ID；
// 无账号上下文（建号前探测）传 0。
func traeChatAttempts(ccBody []byte, creds TraeCredentials, accountID int64) ([]traeAttempt, error) {
	var parsed traeParsedRequest
	if err := json.Unmarshal(ccBody, &parsed); err != nil {
		return nil, err
	}
	model := strings.TrimSpace(parsed.Model)
	primary, err := buildTraeLLMUtilsChatBody(ccBody, creds, model)
	if err != nil {
		return nil, err
	}
	attempts := []traeAttempt{{path: traeChatPath, body: primary}}
	// 闸门看**归一后的出站报文**而不是入站原文：本层会把 `tool_choice:"none"` 连同
	// tools 一起摘掉（上游不允许「工具在手却禁用」），此时主通道请求已不再依赖工具，
	// ide/v1/chat 回退是安全的；若仍按入站原文判定，这类请求会在主通道 404 时白白
	// 失去回退机会（显式禁用工具 ≠ 依赖工具）。以 primary 为准能同时覆盖其它「归一
	// 后不再声明工具」的形态，且对真正带工具的请求结论不变。
	if traeRequestDeclaresTools(primary) {
		return attempts, nil
	}
	if fallback, ferr := buildTraeIDEPayload(&parsed, creds, model, accountID); ferr == nil {
		attempts = append(attempts, traeAttempt{path: traeChatFallback, body: fallback})
	}
	return attempts, nil
}

// traeRequestDeclaresTools 报告一份 CC 形态请求体是否依赖 function calling：tools 数组
// 非空，或 tool_choice 显式指定（"required"/具名工具即使无 tools 也是工具语义）。
//
// 调用方传的是归一后的 primary（见 traeChatAttempts），不是入站原文：归一层会取消
// 某些工具声明（none 连 tools 一起删），只有看输出才能判断回退报文会不会丢工具。
func traeRequestDeclaresTools(ccBody []byte) bool {
	if tools := gjson.GetBytes(ccBody, "tools"); tools.IsArray() && tools.Get("#").Int() > 0 {
		return true
	}
	if choice := gjson.GetBytes(ccBody, "tool_choice"); choice.Exists() {
		switch strings.TrimSpace(choice.String()) {
		case "", "none":
			return false
		}
		return true
	}
	return false
}

// buildTraeLLMUtilsChatBody 把 OpenAI CC 请求体归一为 llm_utils_chat 形态：保留
// OpenAI 字段（messages/tools/tool_choice/temperature/top_p/max_tokens/stop/seed/n），
// 把模型同时写入 model 与 config_name（上游按 config_name 选模型表），并强制
// stream=true（该端点只出 SSE，非流式由本服务读全流聚合）。
//
// 【为什么必须重写 messages，不能像早期版本那样原样透传】上游把消息体声明成
// `content []*idecopilot.LLMRawMessageContent`（切片），而 OpenAI Chat Completions 的
// `content` 允许是**裸字符串**。直接转发会被上游在 JSON 反序列化阶段拒掉，返回
// HTTP 400：cannot unmarshal string into Go struct field LLMRawMessage.messages.content
// of type []*idecopilot.LLMRawMessageContent —— 即「凡是发纯字符串 content 的客户端
// 全部不可用」，而这正是绝大多数客户端的默认形态。
//
// 同批归一的还有工具链路（见 normalizeTraeChatRequest 各助手函数）：上游的
// tool_calls 函数载荷键名是 function_call（不是 OpenAI 的 function）、tools[].function
// .parameters 是字符串（不是对象）、tool_choice 要字符串。三处不改的表现不是报错而是
// **静默不出工具**（工具名解析为空 → 模型收到一堆无名定义），比 400 更难定位。
func buildTraeLLMUtilsChatBody(ccBody []byte, creds TraeCredentials, model string) ([]byte, error) {
	// UseNumber：不带它时所有数字经 float64 往返，大整数（如 1e21 的 seed、token 上限）
	// 会被重写成科学计数法字面量，parameters 字符串化后上游按整数解析就会失败。
	decoder := json.NewDecoder(bytes.NewReader(ccBody))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	normalizeTraeChatRequest(payload)
	payload["function"] = traeChatFunction
	if model != "" {
		payload["model"] = model
		payload["config_name"] = model
	} else {
		delete(payload, "config_name")
	}
	payload["stream"] = true
	// 聚合非流式响应也要拿 usage：上游只在流内 token_usage 帧给出。
	payload["stream_options"] = map[string]any{"include_usage": true}
	// Trae 无该字段：推理行为由上游按 config_name 决定，透传会被判参数无效。
	delete(payload, "reasoning_effort")
	delete(payload, "service_tier")
	return json.Marshal(payload)
}

// normalizeTraeChatRequest 就地改写 payload 里的 messages / tools / tool_choice，
// 使其满足上游 llm_utils_chat 的报文契约。缺失字段一律不新建（上游对缺键的处理与
// 空值不同），无法识别的形态保守原样透传。
func normalizeTraeChatRequest(payload map[string]any) {
	if messages, ok := payload["messages"].([]any); ok {
		payload["messages"] = normalizeTraeChatMessages(messages)
	}
	if tools, ok := payload["tools"]; ok && tools != nil {
		payload["tools"] = normalizeTraeToolDefinitions(tools)
	}
	if choice, ok := payload["tool_choice"]; ok && choice != nil {
		switch normalized := normalizeTraeToolChoice(choice); normalized {
		case "":
			// 认不出来的形态（对象里没 function.name 等）：删掉比猜一个值安全，
			// 上游对无效 tool_choice 的处理是整请求拒绝。
			delete(payload, "tool_choice")
		case "none":
			// "none" 且仍带 tools 会被上游判参数冲突（工具在手却禁用），两者一起摘掉。
			delete(payload, "tool_choice")
			delete(payload, "tools")
		default:
			payload["tool_choice"] = normalized
		}
	}
}

// normalizeTraeChatMessages 把 messages 归一为上游可反序列化的形态，并清掉
// 无法配对的悬空条目。返回新切片（不改调用方入参的顶层结构，元素原地改）。
func normalizeTraeChatMessages(messages []any) []any {
	normalized := make([]any, 0, len(messages))
	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue // 非对象条目上游必然反序列化失败，丢弃优于整请求 400
		}
		role := strings.ToLower(strings.TrimSpace(traeMessageStringField(msg, "role")))
		if role == "" {
			continue
		}
		// 部分推理客户端（Codex 系）把系统提示发成 developer 角色，上游不认，实测
		// 表现为静默空流（比 400 更难查）→ 归一为 system。
		if role == "developer" {
			role = "system"
		}
		msg["role"] = role
		// 消息级 name 无任何一手证据表明上游接受（多模态规范里它属于工具名冗余字段），
		// 而多余键在上游严格解码下会变成整请求拒绝，直接摘掉。
		delete(msg, "name")

		content, hasContent := msg["content"]
		if hasContent && content != nil {
			msg["content"] = normalizeTraeMessageContent(content)
		}
		// 否则保持缺失/null：纯 tool_calls 的 assistant 轮次本就无 content，
		// 上游 []*T 对 null 反序列化为 nil 切片是合法形态；补一个空文本块反而会出错。

		if calls, ok := msg["tool_calls"]; ok && calls != nil {
			if kept := normalizeTraeToolCalls(calls); len(kept) > 0 {
				msg["tool_calls"] = kept
			} else {
				delete(msg, "tool_calls")
				if !hasContent || content == nil {
					// 全被剔且无正文 → 空占位消息，留着会让上游空流；整条丢掉。
					continue
				}
			}
		}
		normalized = append(normalized, msg)
	}
	return dropOrphanTraeToolResults(normalized)
}

// dropOrphanTraeToolResults 剔除 tool_call_id 无法配对到前序 assistant tool_calls 的
// role=tool 消息。上游按 id 关联工具结果，悬空引用会让流**静默返回空**（HTTP 200、
// 零 token、无任何错误帧），客户端只看到「模型没回答」；本仓入站裁剪历史时极易产生
// 这种悬空（assistant 轮被裁掉、tool 结果还在）。
func dropOrphanTraeToolResults(messages []any) []any {
	known := make(map[string]struct{}, len(messages))
	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		calls, ok := msg["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, raw := range calls {
			if call, ok := raw.(map[string]any); ok {
				if id := strings.TrimSpace(traeMessageStringField(call, "id")); id != "" {
					known[id] = struct{}{}
				}
			}
		}
	}
	if len(known) == 0 && !hasTraeToolMessage(messages) {
		// 快路：既无工具调用也无工具结果（绝大多数普通对话）时不重建切片。
		return messages
	}
	out := make([]any, 0, len(messages))
	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		if strings.EqualFold(traeMessageStringField(msg, "role"), "tool") {
			id := strings.TrimSpace(traeMessageStringField(msg, "tool_call_id"))
			if _, matched := known[id]; id == "" || !matched {
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

func hasTraeToolMessage(messages []any) bool {
	for _, item := range messages {
		if msg, ok := item.(map[string]any); ok && strings.EqualFold(traeMessageStringField(msg, "role"), "tool") {
			return true
		}
	}
	return false
}

// normalizeTraeMessageContent 把 content 归一为内容数组：
//   - 裸字符串 → [{"type":"text","text":s}]（上游只接受切片形态）；
//   - 已是数组 → 保留结构（多模态 image_url 等 part 原样透传），仅把数组内的
//     裸字符串元素补成文本块；
//   - 其它类型（数字/对象）→ 原样返回，交给上游按自己的契约拒绝，本层不臆造。
func normalizeTraeMessageContent(value any) any {
	switch content := value.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": content}}
	case []any:
		parts := make([]any, 0, len(content))
		for _, part := range content {
			if text, ok := part.(string); ok {
				parts = append(parts, map[string]any{"type": "text", "text": text})
				continue
			}
			parts = append(parts, part)
		}
		return parts
	default:
		return value
	}
}

// normalizeTraeToolCalls 把 OpenAI 的 tool_calls 改写为上游形态：函数载荷键名
// function → function_call，并剔除无函数名的条目（上游 FunctionCall.Name 必填，
// 留空会让整请求被判参数错误）。
func normalizeTraeToolCalls(raw any) []any {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	kept := make([]any, 0, len(list))
	for _, item := range list {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := call["function"].(map[string]any); ok {
			if _, exists := call["function_call"]; !exists {
				call["function_call"] = fn
			}
			delete(call, "function")
		}
		fn, ok := call["function_call"].(map[string]any)
		if !ok {
			continue
		}
		if name := strings.TrimSpace(traeMessageStringField(fn, "name")); name == "" {
			continue
		}
		kept = append(kept, call)
	}
	return kept
}

// normalizeTraeToolDefinitions 把 tools[].function.parameters 从 JSON 对象序列化成
// 字符串（上游该字段是 string 类型，OpenAI 标准是 object）；已是字符串的条目不动。
func normalizeTraeToolDefinitions(raw any) any {
	list, ok := raw.([]any)
	if !ok {
		return raw
	}
	for _, item := range list {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		params, exists := fn["parameters"]
		if !exists || params == nil {
			continue
		}
		if _, isString := params.(string); isString {
			continue
		}
		if encoded, err := json.Marshal(params); err == nil {
			fn["parameters"] = string(encoded)
		}
	}
	return list
}

// normalizeTraeToolChoice 把 tool_choice 归一为上游接受的字符串：
// auto / required / none / 具体工具名。认不出返回空串（调用方删键）。
func normalizeTraeToolChoice(raw any) string {
	switch choice := raw.(type) {
	case string:
		switch trimmed := strings.TrimSpace(choice); trimmed {
		case "auto", "required", "none":
			return trimmed
		case "":
			return ""
		default:
			// OpenAI 允许直接给工具名；上游同样按字符串名匹配，保留。
			return trimmed
		}
	case map[string]any:
		if fn, ok := choice["function"].(map[string]any); ok {
			if name := strings.TrimSpace(traeMessageStringField(fn, "name")); name != "" {
				return name
			}
		}
		switch strings.TrimSpace(traeMessageStringField(choice, "type")) {
		case "required":
			return "required"
		case "none":
			return "none"
		case "auto", "":
			return "auto"
		}
		return ""
	default:
		return ""
	}
}

// traeMessageStringField 取对象里的字符串字段（非字符串/缺失返回空串）。
func traeMessageStringField(obj map[string]any, key string) string {
	if obj == nil {
		return ""
	}
	value, ok := obj[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

// traeIDEMessages ide/v1/chat 的 chat_history 条目（对齐官方客户端字段集）。
type traeIDEMessages struct {
	Role      string `json:"role"`
	SessionID string `json:"session_id"`
	Locale    string `json:"locale,omitempty"`
	Content   string `json:"content"`
	Status    string `json:"status"`
}

// traeIDEVariables ide/v1/chat 的 variables 内层结构（stringified JSON）。逐字段
// 对齐官方客户端实值：缺字段上游报 9004（参数错误）。
type traeIDEVariables struct {
	Language               string `json:"language"`
	Locale                 string `json:"locale"`
	Input                  string `json:"input"`
	VersionCode            int64  `json:"version_code"`
	IsInlineChat           bool   `json:"is_inline_chat"`
	IsCommand              bool   `json:"is_command"`
	RawInput               string `json:"raw_input"`
	Problem                string `json:"problem"`
	CurrentFilename        string `json:"current_filename"`
	IsSelectCodeBeforeChat bool   `json:"is_select_code_before_chat"`
	LastSelectTime         int64  `json:"last_select_time"`
	LastTurnSession        string `json:"last_turn_session"`
	HashWorkspace          bool   `json:"hash_workspace"`
	HashFile               int    `json:"hash_file"`
	HashCode               int    `json:"hash_code"`
	UseFilepath            bool   `json:"use_filepath"`
	CurrentTime            string `json:"current_time"`
	BadgeClickable         bool   `json:"badge_clickable"`
	WorkspacePath          string `json:"workspace_path"`
	Brand                  string `json:"brand"`
	SystemType             string `json:"system_type"`
}

// traeIDERequest /api/ide/v1/chat 的 TraeRequest 报文（字段名对齐官方客户端）。
type traeIDERequest struct {
	UserInput                  string            `json:"user_input"`
	IntentName                 string            `json:"intent_name"`
	Variables                  string            `json:"variables"`
	ContextResolvers           []traeIDEContext  `json:"context_resolvers"`
	GenerateSuggestedQuestions bool              `json:"generate_suggested_questions"`
	ChatHistory                []traeIDEMessages `json:"chat_history"`
	SessionID                  string            `json:"session_id"`
	ConversationID             string            `json:"conversation_id"`
	CurrentTurn                int               `json:"current_turn"`
	ValidTurns                 []int             `json:"valid_turns"`
	MultiMedia                 []any             `json:"multi_media"`
	ModelName                  string            `json:"model_name"`
	IsPreset                   bool              `json:"is_preset"`
	Provider                   string            `json:"provider"`
}

// traeIDEContext context_resolvers 条目（variables 同样是字符串化 JSON）。
type traeIDEContext struct {
	ResolverID string `json:"resolver_id"`
	Variables  string `json:"variables"`
}

// buildTraeIDEPayload 把入站请求重建为 ide/v1/chat 的 TraeRequest：最后一条 user
// 消息作 user_input，其余消息进 chat_history；session_id 与 conversation_id 同源。
//
// 会话锚点取**首条** user 消息（见 traeSessionAnchor），因此同一段对话的多轮请求
// 天然命中同一 session_id；这里不再用当前轮的 userInput 作种子（那样每轮都变，
// 上游侧会话缓存永远不命中）。
func buildTraeIDEPayload(parsed *traeParsedRequest, creds TraeCredentials, model string, accountID int64) ([]byte, error) {
	userInput := ""
	userIdx := -1
	for i := len(parsed.Messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(parsed.Messages[i].Role), "user") {
			userInput = traeMessageTextContent(parsed.Messages[i].Content)
			userIdx = i
			break
		}
	}
	if userInput == "" {
		return nil, errTraeNoUserInput
	}
	versionCode, _ := strconv.ParseInt(traeEffective(creds.IDEVersionCode, defaultTraeIDEVersionCode), 10, 64)
	sessionID := traeStableConversationID(accountID, traeSessionAnchor(parsed))
	variables := traeIDEVariables{
		Locale:          "zh-cn",
		Input:           userInput,
		VersionCode:     versionCode,
		RawInput:        userInput,
		LastTurnSession: sessionID,
		UseFilepath:     true,
		CurrentTime:     traeIDECurrentTime(time.Now()),
		BadgeClickable:  true,
		WorkspacePath:   "/home/trae/workspace",
		Brand:           "Trae",
		SystemType:      traeEffective(creds.DeviceType, defaultTraeDeviceType),
	}
	variablesJSON, err := json.Marshal(variables)
	if err != nil {
		return nil, err
	}
	history := make([]traeIDEMessages, 0, len(parsed.Messages))
	for i, msg := range parsed.Messages {
		if i == userIdx {
			continue // 最后一条 user 作为 user_input，不进历史
		}
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		item := traeIDEMessages{
			Role:      role,
			SessionID: sessionID,
			Content:   traeMessageTextContent(msg.Content),
			Status:    "success",
		}
		// 官方客户端只给 assistant 轮次带 locale。
		if !strings.EqualFold(role, "user") && !strings.EqualFold(role, "system") {
			item.Locale = "zh-cn"
		}
		history = append(history, item)
	}
	currentTurn := userIdx
	if currentTurn < 0 {
		currentTurn = len(parsed.Messages) - 1
	}
	body := traeIDERequest{
		UserInput:  userInput,
		IntentName: "general_qa_intent",
		Variables:  string(variablesJSON),
		ContextResolvers: []traeIDEContext{
			{ResolverID: "project-labels", Variables: `{"labels":""}`},
			{ResolverID: "terminal_context", Variables: `{"terminal_context":[]}`},
		},
		ChatHistory:    history,
		SessionID:      sessionID,
		ConversationID: sessionID,
		CurrentTurn:    currentTurn,
		ValidTurns:     traeValidTurns(len(parsed.Messages)),
		MultiMedia:     []any{},
		ModelName:      model,
		IsPreset:       true,
	}
	return json.Marshal(body)
}

// traeValidTurns 生成 [0, n) 的轮次索引（上游按 valid_turns 决定参与上下文的历史）。
func traeValidTurns(n int) []int {
	if n < 0 {
		n = 0
	}
	turns := make([]int, n)
	for i := range turns {
		turns[i] = i
	}
	return turns
}

// traeMessageTextContent 把 OpenAI 的 string | content-part 数组形态消息内容拍平为
// 纯文本（Trae 两条通道都只接受字符串内容）。
func traeMessageTextContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		collected := make([]string, 0, len(parts))
		for _, part := range parts {
			if part.Text != "" {
				collected = append(collected, part.Text)
			}
		}
		return strings.Join(collected, "\n")
	}
	return ""
}

// traeIDECurrentTime 官方客户端的 current_time 格式：`20260102 15:04:05，星期二`
// （全角逗号 + 中文星期）。格式不符上游按参数错误处理。
func traeIDECurrentTime(now time.Time) string {
	// 按 rune 切片：每个星期词占 3 个 rune（9 字节），用字节下标会切出半个词。
	const weekdays = "星期日星期一星期二星期三星期四星期五星期六"
	runes := []rune(weekdays)
	idx := int(now.Weekday()) * 3
	if idx+3 > len(runes) {
		idx = 0
	}
	return now.Format("20060102 15:04:05") + "，" + string(runes[idx:idx+3])
}

// traeSessionAnchor 会话锚点：**首条** user 消息文本。一段对话在尾部追加 messages 时
// 首条不变，故跨轮稳定（与仓库内 buildStableSessionSeed 同一口径，见
// gateway_claude_oauth_body.go）。取不到 user 消息时退回全量首条消息，保证锚点非空。
func traeSessionAnchor(parsed *traeParsedRequest) string {
	for _, msg := range parsed.Messages {
		if strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			if text := traeMessageTextContent(msg.Content); text != "" {
				return text
			}
		}
	}
	if len(parsed.Messages) > 0 {
		return traeMessageTextContent(parsed.Messages[0].Content)
	}
	return ""
}

// traeStableConversationID 由「账号 ID + 会话锚点」派生稳定会话 ID：同一对话的多轮
// 请求命中同一 session_id 上游才走上下文；账号 ID 参与派生，故不同账号绝不复用（防串话）。
//
// 刻意**不**纳入 access_token：token 每次换票都会轮换（见 trae_token.go），拿它做种子
// 会让同一段对话在换票后凭空断成两个会话。也不纳入 uid：uid 是建档后惰性回填的，
// 会在回填前后各派生出一个不同 ID。账号 ID 在一行记录的生命周期内恒定。
func traeStableConversationID(accountID int64, anchor string) string {
	return traeHashID("trae-session:" + strconv.FormatInt(accountID, 10) + "|" + anchor)
}

// traeHashID 稳定 32 位 hex 摘要（用于 session_id 等需要确定性的标识）。
func traeHashID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:16])
}
