//go:build unit

package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// qoder_payload_test.go 请求体构造单测：模板占位符替换、system 置前、developer 归一、
// tool 配对保留、max_tokens 注入、stream 强制、business/chat_context/model_config 注入。

// qoderUnmarshalMessages 从构造出的 body 提取 messages 为 map 切片。
func qoderUnmarshalMessages(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var obj map[string]any
	require.NoError(t, json.Unmarshal(body, &obj))
	msgs, ok := obj["messages"].([]any)
	require.True(t, ok, "messages 必须是数组")
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		require.True(t, ok)
		out = append(out, mm)
	}
	return out
}

func TestBuildQoderBodyTemplatePlaceholdersAndBasics(t *testing.T) {
	t.Parallel()
	cc := []byte(`{"model":"auto","stream":false,"messages":[{"role":"user","content":"你好 Qoder"}]}`)
	body, meta, err := BuildQoderBody(cc, "auto", "personal_standard")
	require.NoError(t, err)
	require.NotNil(t, meta)

	var obj map[string]any
	require.NoError(t, json.Unmarshal(body, &obj), "替换占位符后必须是合法 JSON")

	// 占位符全部被替换：整份 body 无残留 token。
	require.NotContains(t, string(body), "{UUID")
	require.NotContains(t, string(body), "{TIME1}")

	// 主键与 stream。
	require.Equal(t, meta.RequestID, obj["request_id"])
	require.Equal(t, meta.RequestID, obj["chat_record_id"], "request_id = chat_record_id")
	require.NotEqual(t, obj["request_id"], obj["request_set_id"], "request_set_id 独立")
	require.NotEqual(t, obj["request_id"], obj["session_id"], "session_id 独立")
	require.Equal(t, true, obj["stream"], "stream 强制 true")
	require.Equal(t, "personal_standard", obj["aliyun_user_type"])

	// UUID 形态（36 位带横线）。
	reqID, ok := obj["request_id"].(string)
	require.True(t, ok)
	require.Len(t, reqID, 36)
	require.Contains(t, reqID, "-")

	// business 注入。
	biz, ok := obj["business"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, meta.BusinessID, biz["id"])
	require.NotEqual(t, reqID, biz["id"])
	beginAt, ok := biz["begin_at"].(float64)
	require.True(t, ok)
	require.Greater(t, beginAt, float64(1600000000000), "begin_at 是 unix 毫秒")
	require.Equal(t, "你好 Qoder", biz["name"], "business.name = 最后一条 user 消息")

	// chat_context 注入。
	ctxObj, ok := obj["chat_context"].(map[string]any)
	require.True(t, ok)
	textObj, ok := ctxObj["text"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "你好 Qoder", textObj["text"])
	extra, ok := ctxObj["extra"].(map[string]any)
	require.True(t, ok)
	oc, ok := extra["originalContent"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "你好 Qoder", oc["text"])
	mc, ok := extra["modelConfig"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "auto", mc["key"])

	// model_config 注入。
	topMC, ok := obj["model_config"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "auto", topMC["key"])
}

func TestBuildQoderBodySystemFirstAndDeveloperNormalize(t *testing.T) {
	t.Parallel()
	cc := []byte(`{"model":"auto","messages":[
		{"role":"system","content":"sys-1"},
		{"role":"developer","content":"dev-as-sys"},
		{"role":"user","content":"q1"},
		{"role":"assistant","content":"a1"},
		{"role":"user","content":"q2"}
	]}`)
	body, _, err := BuildQoderBody(cc, "auto", "")
	require.NoError(t, err)
	msgs := qoderUnmarshalMessages(t, body)
	require.GreaterOrEqual(t, len(msgs), 5)
	require.Equal(t, "system", msgs[0]["role"])
	require.Equal(t, "sys-1", msgs[0]["content"])
	require.Equal(t, "system", msgs[1]["role"], "developer 归一为 system")
	require.Equal(t, "dev-as-sys", msgs[1]["content"])

	// user 形态：最后一条 user 消息为 prompt（q2）。
	var userTexts []string
	for _, m := range msgs {
		if m["role"] == "user" {
			require.Equal(t, "", m["content"], "user content 置空")
			contents, ok := m["contents"].([]any)
			require.True(t, ok, "user 带 contents 数组")
			require.Len(t, contents, 1)
			part, ok := contents[0].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "text", part["type"])
			userTexts = append(userTexts, part["text"].(string))
			require.Contains(t, m, "response_meta")
			require.Contains(t, m, "reasoning_content_signature")
		}
	}
	require.Equal(t, []string{"q1", "q2"}, userTexts)
}

func TestBuildQoderBodyFallbackTemplateSystem(t *testing.T) {
	t.Parallel()
	cc := []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	body, _, err := BuildQoderBody(cc, "auto", "")
	require.NoError(t, err)
	msgs := qoderUnmarshalMessages(t, body)
	require.GreaterOrEqual(t, len(msgs), 2, "模板 system 兜底 + user")
	require.Equal(t, "system", msgs[0]["role"])
	sysContent, _ := msgs[0]["content"].(string)
	require.Contains(t, sysContent, "Qoder", "模板自带 Qoder system prompt")
}

func TestBuildQoderBodyToolPairingPreserved(t *testing.T) {
	t.Parallel()
	cc := []byte(`{"model":"auto","messages":[
		{"role":"user","content":"list files"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"ls","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call-1","content":"a.txt\nb.txt"},
		{"role":"assistant","content":"done"}
	]}`)
	body, _, err := BuildQoderBody(cc, "auto", "")
	require.NoError(t, err)
	msgs := qoderUnmarshalMessages(t, body)
	var sawToolCall, sawToolResult bool
	for _, m := range msgs {
		if m["role"] == "assistant" {
			if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
				sawToolCall = true
				tc, ok := tcs[0].(map[string]any)
				require.True(t, ok)
				require.Equal(t, "call-1", tc["id"], "tool_calls 原样保留")
			}
		}
		if m["role"] == "tool" {
			sawToolResult = true
			require.Equal(t, "call-1", m["tool_call_id"])
			require.Equal(t, "a.txt\nb.txt", m["content"])
		}
	}
	require.True(t, sawToolCall, "assistant.tool_calls 保留")
	require.True(t, sawToolResult, "tool 结果消息保留")
}

func TestBuildQoderBodyToolsAndMaxTokensAndMultimodal(t *testing.T) {
	t.Parallel()
	cc := []byte(`{"model":"qmodel","max_tokens":2048,"messages":[
		{"role":"user","content":[{"type":"text","text":"part1"},{"type":"image_url","image_url":{"url":"https://x/y.png"}},{"type":"text","text":" part2"}]}
	],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`)
	body, meta, err := BuildQoderBody(cc, "qmodel", "")
	require.NoError(t, err)
	require.Equal(t, "qmodel", meta.ModelKey)
	var obj map[string]any
	require.NoError(t, json.Unmarshal(body, &obj))
	// max_tokens 注入 parameters。
	params, ok := obj["parameters"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(2048), params["max_tokens"])
	// tools 原样透传。
	tools, ok := obj["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 1)
	// 多模态 text 拼接。
	msgs := qoderUnmarshalMessages(t, body)
	var userText string
	for _, m := range msgs {
		if m["role"] == "user" {
			contents := m["contents"].([]any)
			part := contents[0].(map[string]any)
			userText, _ = part["text"].(string)
		}
	}
	require.Equal(t, "part1 part2", userText, "多模态 text part 按序拼接，image 不进文本")
	require.Equal(t, "part1 part2", meta.Prompt)
}

func TestBuildQoderBodyBusinessNameTruncated(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("长", 50)
	cc := []byte(`{"model":"auto","messages":[{"role":"user","content":"` + long + `"}]}`)
	body, meta, err := BuildQoderBody(cc, "auto", "")
	require.NoError(t, err)
	require.Len(t, []rune(meta.Prompt), 50)
	var obj map[string]any
	require.NoError(t, json.Unmarshal(body, &obj))
	biz := obj["business"].(map[string]any)
	require.Equal(t, 30, len([]rune(biz["name"].(string))), "business.name 截 30 字符")
}

func TestBuildQoderBodyUnmarshalableCC(t *testing.T) {
	t.Parallel()
	_, _, err := BuildQoderBody([]byte(`not-json`), "auto", "")
	require.Error(t, err, "非法 CC body 必须报错而非静默")
}
