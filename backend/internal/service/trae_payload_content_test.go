//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// trae_payload_content_test.go 上游 llm_utils_chat 报文契约回归。
//
// 立项起因（线上真实故障）：我们把 OpenAI 形态的**裸字符串 content** 原样转发，
// 而上游把该字段声明成切片，于是所有纯文本请求都被 JSON 反序列化拒掉：
//
//	HTTP 400 cannot unmarshal string into Go struct field
//	  LLMRawMessage.messages.content of type []*idecopilot.LLMRawMessageContent
//
// 下列用例逐条钉住归一规则。断言全部针对「出站字节里的形态」，而不是内部函数，
// 这样任何一处回退成原样透传都会被立刻抓到。

func traePrimary(t *testing.T, raw string) map[string]any {
	t.Helper()
	attempts, err := traeChatAttempts(traeCCBody(t, raw), TraeCredentials{AccessToken: "at"}, 1)
	require.NoError(t, err)
	require.NotEmpty(t, attempts)
	require.Equal(t, traeChatPath, attempts[0].path)
	return decodeAttempt(t, attempts[0].body)
}

func traePrimaryFirstMessage(t *testing.T, raw string) map[string]any {
	t.Helper()
	primary := traePrimary(t, raw)
	messages, ok := primary["messages"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, messages)
	return messages[0].(map[string]any)
}

// 纯文本 content 必须数组化，且对**所有角色**生效（上游声明与角色无关）。
// tool 角度的夹具带已配对的 tool_call_id（否则会被悬空剔除逻辑先掉光）。
func TestTraePrimaryContentStringIsArrayified(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ role, body string }{
		{"system", `{"model":"glm-5.2","messages":[{"role":"system","content":"plain text"}]}`},
		{"user", `{"model":"glm-5.2","messages":[{"role":"user","content":"plain text"}]}`},
		{"assistant", `{"model":"glm-5.2","messages":[{"role":"assistant","content":"plain text"}]}`},
		{"tool", traePairedToolBody(t)},
	} {
		primary := traePrimary(t, tc.body)
		var msg map[string]any
		for _, item := range primary["messages"].([]any) {
			if candidate := item.(map[string]any); candidate["role"] == tc.role {
				msg = candidate
			}
		}
		require.NotNil(t, msg, "%s 消息不应被丢弃", tc.role)
		require.Equal(t, []any{map[string]any{"type": "text", "text": "plain text"}}, msg["content"],
			"%s 的字符串 content 必须数组化为 {type:\"text\",text:...}，否则上游 400", tc.role)
	}
}

func traePairedToolBody(t *testing.T) string {
	t.Helper()
	return `{"model":"glm-5.2","messages":[
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"plain text"}]}`
}

// 空字符串也要数组化成单元素（不是丢键、也不是数组化失败）：上游对 null 与 []
// 的处理不同，丢内容比多发一个空文本块更糟。
func TestTraePrimaryContentEmptyStringKeptAsBlock(t *testing.T) {
	t.Parallel()
	msg := traePrimaryFirstMessage(t, `{"model":"glm-5","messages":[{"role":"user","content":""}]}`)
	require.Equal(t, []any{map[string]any{"type": "text", "text": ""}}, msg["content"])
}

// content 缺失 / null 时必须保持缺失或 null（纯 tool_calls 的 assistant 轮次），
// 不得凭空造出一个空文本块——上游会把无意义块当有效输入。
func TestTraePrimaryNullContentNotArrayified(t *testing.T) {
	t.Parallel()
	absent := traePrimary(t, `{"model":"glm-5","messages":[
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`)
	msg := absent["messages"].([]any)[0].(map[string]any)
	require.NotContains(t, msg, "content", "无 content 的 assistant 轮次不得被补上空文本块")

	nullMsg := traePrimaryFirstMessage(t, `{"model":"glm-5","messages":[{"role":"assistant","content":null}]}`)
	require.Nil(t, nullMsg["content"], "null content 原样保留（上游 []*T 反序列化为 nil 是合法的）")
}

// 已是数组的 content 原样透传（多模态 image_url 不能被降级成纯文本）。
func TestTraePrimaryContentArrayPassesThrough(t *testing.T) {
	t.Parallel()
	msg := traePrimaryFirstMessage(t, `{"model":"glm-5v","messages":[{"role":"user","content":[
		{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`)
	parts, ok := msg["content"].([]any)
	require.True(t, ok)
	require.Len(t, parts, 2, "多模态 part 数量不得被篡改")
	require.Equal(t, "image_url", parts[1].(map[string]any)["type"], "图片 part 必须原样送达上游")
}

// 数组里混入裸字符串元素时补成文本块（上游元素是指针切片，裸字符串同样 400）。
func TestTraePrimaryContentStringElementsInsideArray(t *testing.T) {
	t.Parallel()
	msg := traePrimaryFirstMessage(t, `{"model":"glm-5","messages":[{"role":"user","content":["a","b"]}]}`)
	require.Equal(t, []any{
		map[string]any{"type": "text", "text": "a"},
		map[string]any{"type": "text", "text": "b"},
	}, msg["content"])
}

// developer 角色上游不认（实测静默空流，比 400 更难查）→ 归一为 system。
func TestTraePrimaryDeveloperRoleNormalized(t *testing.T) {
	t.Parallel()
	msg := traePrimaryFirstMessage(t, `{"model":"glm-5","messages":[{"role":"developer","content":"sys"}]}`)
	require.Equal(t, "system", msg["role"], "developer 必须归一为 system")
}

// 消息级 name 无一手证据被接受，多余键在严格解码下会整请求拒绝 → 摘掉。
func TestTraePrimaryDropsMessageName(t *testing.T) {
	t.Parallel()
	primary := traePrimary(t, `{"model":"glm-5","messages":[
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","name":"get_time","content":"ok"}]}`)
	last := primary["messages"].([]any)[1].(map[string]any)
	require.Equal(t, "tool", last["role"])
	require.NotContains(t, last, "name")
}

// tool_calls 的函数载荷键名上游是 function_call（不是 OpenAI 的 function）。
// 不改名的表现不是报错而是工具名解析为空 → 模型永远不出工具。
func TestTraePrimaryToolCallsRenamedToFunctionCall(t *testing.T) {
	t.Parallel()
	primary := traePrimary(t, `{"model":"glm-5","messages":[
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"p\":\"a\"}"}},
			{"id":"c2","type":"function","function":{"arguments":"{}"}}]}]}`)
	msg := primary["messages"].([]any)[0].(map[string]any)
	calls, ok := msg["tool_calls"].([]any)
	require.True(t, ok)
	require.Len(t, calls, 1, "无函数名的 tool_call 必须剔除（上游 FunctionCall.Name 必填）")
	call := calls[0].(map[string]any)
	require.NotContains(t, call, "function", "OpenAI 的 function 键必须消失")
	fn, ok := call["function_call"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "read_file", fn["name"])
	require.Equal(t, `{"p":"a"}`, fn["arguments"])
	require.Equal(t, "c1", call["id"], "id/type 原样保留供上游配对")
}

// 全被剔除且无正文的 assistant 占位消息整条丢弃（留着会让上游空流）。
func TestTraePrimaryDropsEmptyToolCallPlaceholder(t *testing.T) {
	t.Parallel()
	primary := traePrimary(t, `{"model":"glm-5","messages":[
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function"}]},
		{"role":"user","content":"hi"}]}`)
	messages := primary["messages"].([]any)
	require.Len(t, messages, 1, "空 tool_calls 占位消息必须整条丢弃")
	require.Equal(t, "user", messages[0].(map[string]any)["role"])
}

// tools[].function.parameters 上游是 string 类型（OpenAI 标准是 object）→ 必须字符串化。
func TestTraePrimaryToolParametersStringified(t *testing.T) {
	t.Parallel()
	primary := traePrimary(t, `{"model":"glm-5","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","required":["x"]}}}]}`)
	fn := primary["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	params, ok := fn["parameters"].(string)
	require.True(t, ok, "parameters 必须是 JSON 字符串，实型 %T", fn["parameters"])
	var round map[string]any
	require.NoError(t, json.Unmarshal([]byte(params), &round))
	require.Equal(t, "object", round["type"], "字符串化不得丢内容")

	// 已是字符串的不重复编码（双重字符串化会让上游解出转义地狱）。
	again := traePrimary(t, `{"model":"glm-5","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f","parameters":"{\"type\":\"object\"}"}}]}`)
	require.Equal(t, `{"type":"object"}`,
		again["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"])
}

// tool_choice 对象形态归一为具体工具名字符串。
func TestTraePrimaryToolChoiceFlattened(t *testing.T) {
	t.Parallel()
	primary := traePrimary(t, `{"model":"glm-5","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f"}}],
		"tool_choice":{"type":"function","function":{"name":"f"}}}`)
	require.Equal(t, "f", primary["tool_choice"], "指定工具形态要降成裸名字字符串")

	byType := traePrimary(t, `{"model":"glm-5","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"required"}}`)
	require.Equal(t, "required", byType["tool_choice"])
}

// tool_choice=none 且带 tools 会被上游判参数冲突，两者一起摘掉。
func TestTraePrimaryToolChoiceNoneDropsTools(t *testing.T) {
	t.Parallel()
	primary := traePrimary(t, `{"model":"glm-5","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"none"}`)
	require.NotContains(t, primary, "tool_choice")
	require.NotContains(t, primary, "tools", "禁用工具时不得仍把定义发给上游")
}

// 无法识别的 tool_choice 形态（对象里既无 name 也无可认 type）删键而非猜值。
func TestTraePrimaryUnrecognizedToolChoiceDropped(t *testing.T) {
	t.Parallel()
	primary := traePrimary(t, `{"model":"glm-5","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"weird"}}`)
	require.NotContains(t, primary, "tool_choice")
}

// 悬空 tool 结果（tool_call_id 找不到前序调用）会让上游**静默返回空流**：
// HTTP 200、零 token、无错误帧，客户端只看到「模型没回答」，必须在出站前剔除。
func TestTraePrimaryDropsOrphanToolResults(t *testing.T) {
	t.Parallel()
	primary := traePrimary(t, `{"model":"glm-5","messages":[
		{"role":"user","content":"hi"},
		{"role":"tool","tool_call_id":"missing","content":"stale result"},
		{"role":"tool","content":"no id at all"},
		{"role":"user","content":"again"}]}`)
	messages := primary["messages"].([]any)
	require.Len(t, messages, 2, "两条悬空 tool 结果都必须剔除")
	for _, item := range messages {
		require.NotEqual(t, "tool", item.(map[string]any)["role"])
	}
}

// 正常配对的 tool 结果必须保留（剔多了等于把工具执行结果吞掉，模型会重复调用）。
func TestTraePrimaryKeepsMatchedToolResults(t *testing.T) {
	t.Parallel()
	primary := traePrimary(t, `{"model":"glm-5","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"result"}]}`)
	messages := primary["messages"].([]any)
	require.Len(t, messages, 3, "配对的 tool 结果不得被误删")
	last := messages[2].(map[string]any)
	require.Equal(t, "tool", last["role"])
	require.Equal(t, []any{map[string]any{"type": "text", "text": "result"}}, last["content"])
}

// 大整数不得被 float64 往返改写成科学计数法（早期版本无 UseNumber 会产 1e+21）。
// 断言看**出站字节**而不是解回来的 map：map 会再次把数字读成 float64，无法证伪。
func TestTraePrimaryPreservesLargeIntegerLiterals(t *testing.T) {
	t.Parallel()
	attempts, err := traeChatAttempts(traeCCBody(t,
		`{"model":"glm-5","messages":[{"role":"user","content":"hi"}],"seed":12345678901234567890}`),
		TraeCredentials{AccessToken: "at"}, 1)
	require.NoError(t, err)
	require.NotContains(t, string(attempts[0].body), "1.2345678901234567e+19",
		"数字经 float64 往返会被重写成浮点字面量，上游按 int64 解析 seed 就会失败")
	require.Contains(t, string(attempts[0].body), "12345678901234567890")
}
