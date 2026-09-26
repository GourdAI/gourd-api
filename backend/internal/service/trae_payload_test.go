//go:build unit

package service

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// trae_payload_test.go 覆盖两条出站通道的报文形态（业务纪律 11 的前半段：候选序列）。
// 主通道 llm_utils_chat 保留 OpenAI 形态 + 两个路由字段；回退通道 ide/v1/chat 是
// TraeRequest 重建（user_input/chat_history/variables 字符串化 JSON）。

func decodeAttempt(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	require.NoError(t, json.Unmarshal(body, &obj))
	return obj
}

func traeCCBody(t *testing.T, raw string) []byte {
	t.Helper()
	// 校验测试样本本身是合法 JSON，避免把写错的夹具当成"源码行为"。
	var probe map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &probe))
	return []byte(raw)
}

func TestTraeChatAttemptsPrimaryChannelShape(t *testing.T) {
	t.Parallel()
	body := traeCCBody(t, `{
		"model": "glm-5.2",
		"messages": [{"role":"system","content":"sys"},{"role":"user","content":"hello"}],
		"tools": [{"type":"function","function":{"name":"get_time"}}],
		"tool_choice": "auto",
		"temperature": 0.3, "top_p": 0.9, "max_tokens": 128, "stop": ["\n\n"],
		"seed": 7, "n": 1, "stream": false,
		"reasoning_effort": "high", "service_tier": "priority"
	}`)
	attempts, err := traeChatAttempts(body, TraeCredentials{UID: "u1", AccessToken: "at"}, 7)
	require.NoError(t, err)
	// 入站带 tools：回退通道（ide/v1/chat 无字段承载工具定义）必须被掉除，
	// 否则 404 降级后会「回答正常但从不岬工具」。
	require.Len(t, attempts, 1, "声明工具的请求不得产出 ide 回退候选")
	require.Equal(t, traeChatPath, attempts[0].path)

	primary := decodeAttempt(t, attempts[0].body)
	// 路由字段：function 固定 + model 同步写入 config_name（上游按 config_name 选模型表）。
	require.Equal(t, traeChatFunction, primary["function"])
	require.Equal(t, "glm-5.2", primary["model"])
	require.Equal(t, "glm-5.2", primary["config_name"])
	// 该端点只出 SSE：非流式入站也必须被改写成 stream=true 并要求 usage 帧。
	require.Equal(t, true, primary["stream"])
	streamOptions, ok := primary["stream_options"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, streamOptions["include_usage"])

	// messages 原样保留（不做任何 Trae 侧重建）。
	messages, ok := primary["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 2)
	first := messages[0].(map[string]any)
	require.Equal(t, "system", first["role"])
	require.Equal(t, "sys", first["content"])
	require.Equal(t, "hello", messages[1].(map[string]any)["content"])

	// OpenAI 参数透传。
	require.Contains(t, primary, "tools")
	require.Equal(t, "auto", primary["tool_choice"])
	require.EqualValues(t, 0.3, primary["temperature"])
	require.EqualValues(t, 128, primary["max_tokens"])
	require.EqualValues(t, 7, primary["seed"])
	require.Len(t, primary["stop"].([]any), 1)

	// Trae 无这两个字段：透传会被判参数无效，必须剔除。
	require.NotContains(t, primary, "reasoning_effort")
	require.NotContains(t, primary, "service_tier")
}

func TestTraeChatAttemptsWithoutModelDropsConfigName(t *testing.T) {
	t.Parallel()
	body := traeCCBody(t, `{"messages":[{"role":"user","content":"hi"}]}`)
	attempts, err := traeChatAttempts(body, TraeCredentials{}, 7)
	require.NoError(t, err)
	primary := decodeAttempt(t, attempts[0].body)
	require.NotContains(t, primary, "config_name", "空模型不得写入 config_name（上游会当无效模型）")
	require.NotContains(t, primary, "model")
	require.Equal(t, traeChatFunction, primary["function"])
	require.Equal(t, true, primary["stream"])
}

func TestTraeChatAttemptsInvalidJSONReturnsError(t *testing.T) {
	t.Parallel()
	_, err := traeChatAttempts([]byte(`{not json`), TraeCredentials{}, 7)
	require.Error(t, err)

	// 空 messages 也要能出主端点（上游自行报错，不归本层判定）。
	attempts, err := traeChatAttempts(traeCCBody(t, `{"model":"glm-5"}`), TraeCredentials{}, 7)
	require.NoError(t, err)
	require.Len(t, attempts, 1, "无 user 输入时回退通道被静默省略")
	require.Equal(t, traeChatPath, attempts[0].path)
}

func TestTraeIDEPayloadShape(t *testing.T) {
	t.Parallel()
	body := traeCCBody(t, `{
		"model": "kimi-k2.5",
		"messages": [
			{"role":"system","content":"be terse"},
			{"role":"user","content":"first question"},
			{"role":"assistant","content":"first answer"},
			{"role":"user","content":"the real question"}
		]
	}`)
	attempts, err := traeChatAttempts(body, TraeCredentials{UID: "u9", AccessToken: "at9", IDEVersionCode: "20260820"}, 9)
	require.NoError(t, err)
	require.Len(t, attempts, 2)
	require.Equal(t, traeChatFallback, attempts[1].path)

	payload := decodeAttempt(t, attempts[1].body)
	// user_input 取最后一条 user，且该条不得再出现在 chat_history。
	require.Equal(t, "the real question", payload["user_input"])
	history, ok := payload["chat_history"].([]any)
	require.True(t, ok)
	require.Len(t, history, 3, "4 条消息里最后一条 user 移出历史")
	require.Equal(t, "system", history[0].(map[string]any)["role"])
	require.Equal(t, "first question", history[1].(map[string]any)["content"])
	require.Equal(t, "first answer", history[2].(map[string]any)["content"])
	for _, item := range history {
		require.NotEqual(t, "the real question", item.(map[string]any)["content"])
		require.Equal(t, payload["session_id"], item.(map[string]any)["session_id"], "历史条目 session_id 必须同源")
	}
	assistant := history[2].(map[string]any)
	require.Equal(t, "zh-cn", assistant["locale"], "官方客户端只给 assistant 轮次带 locale")
	require.NotContains(t, history[1].(map[string]any), "locale")
	require.Equal(t, "success", assistant["status"])

	// session_id 与 conversation_id 同源（多轮命中同一上下文）。
	require.Equal(t, payload["session_id"], payload["conversation_id"])
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{32}$`), payload["session_id"])

	// valid_turns 覆盖全部入站消息索引。
	turns, ok := payload["valid_turns"].([]any)
	require.True(t, ok)
	require.Len(t, turns, 4)
	require.EqualValues(t, 0, turns[0])
	require.EqualValues(t, 3, turns[3])
	require.EqualValues(t, 3, payload["current_turn"], "current_turn 指向最后一条 user 的下标")

	require.Equal(t, "general_qa_intent", payload["intent_name"])
	require.Equal(t, "kimi-k2.5", payload["model_name"])
	require.Equal(t, true, payload["is_preset"])
	require.NotNil(t, payload["multi_media"])
	require.Empty(t, payload["multi_media"].([]any))
	resolvers, ok := payload["context_resolvers"].([]any)
	require.True(t, ok)
	require.Len(t, resolvers, 2)

	// variables 必须是字符串化 JSON（对象形态上游报 9004）。
	rawVariables, ok := payload["variables"].(string)
	require.True(t, ok, "variables 必须是字符串")
	var variables map[string]any
	require.NoError(t, json.Unmarshal([]byte(rawVariables), &variables), "variables 必须是合法 JSON 字符串")
	require.Equal(t, "the real question", variables["input"])
	require.Equal(t, "the real question", variables["raw_input"])
	require.EqualValues(t, int64(20260820), variables["version_code"])
	require.Equal(t, payload["session_id"], variables["last_turn_session"])
	require.Equal(t, "zh-cn", variables["locale"])
	require.Equal(t, true, variables["use_filepath"])
	require.Equal(t, "Trae", variables["brand"])
	require.Equal(t, "windows", variables["system_type"])
	require.Equal(t, true, variables["badge_clickable"])
	currentTime, ok := variables["current_time"].(string)
	require.True(t, ok)
	require.Regexp(t, regexp.MustCompile(`^\d{8} \d{2}:\d{2}:\d{2}，星期[日一二三四五六]$`), currentTime,
		"current_time 必须形如 20260102 15:04:05，星期五（全角逗号 + 中文星期）")
}

func TestTraeIDECurrentTimeFormat(t *testing.T) {
	t.Parallel()
	// 固定时刻验证格式与星期映射（格式不符上游按参数错误处理）。
	require.Equal(t, "20260102 15:04:05，星期五", traeIDECurrentTime(
		time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)))
	require.Equal(t, "20260104 00:00:00，星期日", traeIDECurrentTime(
		time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)))
	require.Equal(t, "20260103 23:59:59，星期六", traeIDECurrentTime(
		time.Date(2026, 1, 3, 23, 59, 59, 0, time.UTC)))
}

func TestTraeIDEPayloadMultimodalContentFlattened(t *testing.T) {
	t.Parallel()
	body := traeCCBody(t, `{
		"model": "glm-5v-turbo",
		"messages": [
			{"role":"user","content":[
				{"type":"text","text":"look at this"},
				{"type":"image_url","image_url":{"url":"https://x/y.png"}},
				{"type":"text","text":"and explain"}
			]}
		]
	}`)
	attempts, err := traeChatAttempts(body, TraeCredentials{}, 3)
	require.NoError(t, err)
	require.Len(t, attempts, 2, "多模态消息拍平后仍有 user_input")

	payload := decodeAttempt(t, attempts[1].body)
	require.Equal(t, "look at this\nand explain", payload["user_input"], "content 数组按 \\n 拼接纯文本")

	// 主通道保留原始 messages 结构（OpenAI 形态，不拍平）。
	primary := decodeAttempt(t, attempts[0].body)
	raw := primary["messages"].([]any)[0].(map[string]any)["content"]
	parts, ok := raw.([]any)
	require.True(t, ok, "主通道不得篡改多模态 content")
	require.Len(t, parts, 3)
}

func TestTraeMessageTextContent(t *testing.T) {
	t.Parallel()
	require.Equal(t, "", traeMessageTextContent(nil))
	require.Equal(t, "", traeMessageTextContent(json.RawMessage(``)))
	require.Equal(t, "plain", traeMessageTextContent(json.RawMessage(`"plain"`)))
	require.Equal(t, "a\nb", traeMessageTextContent(json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)))
	// 空 text 分片被跳过。
	require.Equal(t, "only", traeMessageTextContent(json.RawMessage(`[{"type":"image_url"},{"type":"text","text":"only"}]`)))
	// 既非字符串也非数组 → 空串（不 panic）。
	require.Equal(t, "", traeMessageTextContent(json.RawMessage(`{"text":"obj"}`)))
	require.Equal(t, "", traeMessageTextContent(json.RawMessage(`123`)))
}

func TestTraeIDEPayloadSkippedWithoutUserMessage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{"只有 system", `{"model":"glm-5","messages":[{"role":"system","content":"sys only"}]}`},
		{"只有 assistant", `{"model":"glm-5","messages":[{"role":"assistant","content":"hi"}]}`},
		{"user 内容为空串", `{"model":"glm-5","messages":[{"role":"user","content":""}]}`},
		{"无 messages", `{"model":"glm-5"}`},
		{"user content 为空串", `{"messages":[{"role":"user","content":""}]}`},
		{"user content 为空白分片", `{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attempts, err := traeChatAttempts(traeCCBody(t, tc.body), TraeCredentials{}, 4)
			require.NoError(t, err)
			require.Len(t, attempts, 1, "ide 形态被跳过")
			require.Equal(t, traeChatPath, attempts[0].path, "主端点必须仍然产出")
		})
	}

	// role 大小写宽容：正常 User 消息仍进 ide 形态。
	attempts, err := traeChatAttempts(traeCCBody(t, `{"messages":[{"role":"User","content":"yolo"}]}`), TraeCredentials{}, 5)
	require.NoError(t, err)
	require.Len(t, attempts, 2)
	require.Equal(t, "yolo", decodeAttempt(t, attempts[1].body)["user_input"])

	// 记录现状：纯空白 user 内容不算"无输入"（源码只判空串），仍会产出回退报文。
	blankAttempts, err := traeChatAttempts(traeCCBody(t, `{"messages":[{"role":"USER","content":"   "}]}`), TraeCredentials{}, 6)
	require.NoError(t, err)
	require.Len(t, blankAttempts, 2)
	require.Equal(t, "   ", decodeAttempt(t, blankAttempts[1].body)["user_input"])
}

func TestTraeValidTurns(t *testing.T) {
	t.Parallel()
	require.Equal(t, []int{0, 1, 2}, traeValidTurns(3))
	require.Equal(t, []int{}, traeValidTurns(0))
	require.Equal(t, []int{}, traeValidTurns(-2), "负数按 0 处理，不得 panic")
}

func TestTraeStableConversationIDIsAccountScoped(t *testing.T) {
	t.Parallel()
	same := traeStableConversationID(7, "hello")
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{32}$`), same)
	require.Equal(t, same, traeStableConversationID(7, "hello"), "同账号同锚点必须复用同一 session_id")
	require.NotEqual(t, same, traeStableConversationID(7, "other question"))
	require.NotEqual(t, same, traeStableConversationID(8, "hello"),
		"不同账号绝不复用 session（防串话）")
}

// 会话 ID 必须只由「账号 + 首条 user 消息」决定：换票后的 access_token、以及建档后
// 才回填的 uid 都不得参与派生，否则同一段对话会被凭空断成两个会话。
func TestTraeSessionAnchorIgnoresRotatingCredentials(t *testing.T) {
	t.Parallel()
	firstTurn := traeCCBody(t, `{"model":"glm-5","messages":[
		{"role":"system","content":"sys"},{"role":"user","content":"opening question"}]}`)
	laterTurn := traeCCBody(t, `{"model":"glm-5","messages":[
		{"role":"system","content":"sys"},{"role":"user","content":"opening question"},
		{"role":"assistant","content":"an answer"},{"role":"user","content":"follow up"}]}`)

	first, err := buildTraeIDEPayload(parsedOf(t, firstTurn), TraeCredentials{UID: "u1", AccessToken: "at-old"}, "glm-5", 42)
	require.NoError(t, err)
	later, err := buildTraeIDEPayload(parsedOf(t, laterTurn), TraeCredentials{UID: "u1", AccessToken: "at-new"}, "glm-5", 42)
	require.NoError(t, err)

	sessionOf := func(raw []byte) string {
		return decodeAttempt(t, raw)["session_id"].(string)
	}
	require.Equal(t, sessionOf(first), sessionOf(later),
		"同一对话追加轮次与 access_token 轮换后，session_id 必须不变")
	require.Equal(t, "follow up", decodeAttempt(t, later)["user_input"], "user_input 仍取最后一条 user")
}

func TestTraeRequestDeclaresTools(t *testing.T) {
	t.Parallel()
	cases := []struct {
		body string
		want bool
	}{
		{`{"messages":[]}`, false},
		{`{"tools":[]}`, false},
		{`{"tools":[{"type":"function"}]}`, true},
		{`{"tool_choice":"required"}`, true},
		{`{"tool_choice":"none"}`, false},
		{`{"tool_choice":{"type":"function","function":{"name":"x"}}}`, true},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, traeRequestDeclaresTools([]byte(tc.body)), tc.body)
	}
}

func parsedOf(t *testing.T, body []byte) *traeParsedRequest {
	t.Helper()
	var parsed traeParsedRequest
	require.NoError(t, json.Unmarshal(body, &parsed))
	return &parsed
}

func TestTraeIDEPayloadVariablesAreStringifiedJSON(t *testing.T) {
	t.Parallel()
	raw, err := buildTraeIDEPayload(&traeParsedRequest{
		Model: "auto",
		Messages: []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}{
			{Role: "user", Content: json.RawMessage(`"q"`)},
			{Role: "assistant", Content: json.RawMessage(`"a"`)},
			{Role: "user", Content: json.RawMessage(`"q2"`)},
		},
	}, TraeCredentials{}, "auto", 7)
	require.NoError(t, err)
	payload := decodeAttempt(t, raw)
	// 多条 user 时仍取"最后一条"作 user_input，其余全部进历史。
	require.Equal(t, "q2", payload["user_input"])
	require.Len(t, payload["chat_history"].([]any), 2)
	require.True(t, strings.HasPrefix(payload["variables"].(string), "{"), "variables 必须是字符串化 JSON 对象")

	// 无 user 输入 → errTraeNoUserInput。
	_, err = buildTraeIDEPayload(&traeParsedRequest{Model: "auto"}, TraeCredentials{}, "auto", 7)
	require.ErrorIs(t, err, errTraeNoUserInput)
}
