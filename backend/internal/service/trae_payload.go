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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// traeChatFunction 主通道的 function 路由值（SOLO 免费对话通道）。
const traeChatFunction = "solo_work_lite"

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
	if traeRequestDeclaresTools(ccBody) {
		return attempts, nil
	}
	if fallback, ferr := buildTraeIDEPayload(&parsed, creds, model, accountID); ferr == nil {
		attempts = append(attempts, traeAttempt{path: traeChatFallback, body: fallback})
	}
	return attempts, nil
}

// traeRequestDeclaresTools 报告入站 CC 请求是否依赖 function calling：tools 数组非空，
// 或 tool_choice 显式指定（"required"/具名工具即使无 tools 也是工具语义）。
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
func buildTraeLLMUtilsChatBody(ccBody []byte, creds TraeCredentials, model string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(ccBody, &payload); err != nil {
		return nil, err
	}
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
