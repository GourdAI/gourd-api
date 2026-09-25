//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// workbuddy_payload_test.go PrepareWorkbuddyBody 变换管线的单测：
// 强制 stream、max_tokens 翻译、tool_choice 归一、developer→system、
// tool 配对修复、deepseek thinking 注入、prompt_cache_key 隔离性、
// global console system 兜底与 sanitize 开关。

func TestPrepareWorkbuddyBodyForcesStream(t *testing.T) {
	t.Parallel()
	creds := WorkbuddyCredentials{UID: "u-1", Realm: "cn"}
	out := PrepareWorkbuddyBody([]byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`), creds)
	require.True(t, gjson.GetBytes(out, "stream").Bool(), "stream 必须被强制为 true")
	require.True(t, gjson.GetBytes(out, "stream_options.include_usage").Bool())

	// 显式 stream:false 也被覆盖；显式 stream_options 不覆盖。
	out2 := PrepareWorkbuddyBody([]byte(`{"stream":false,"stream_options":{"include_usage":false}}`), creds)
	require.True(t, gjson.GetBytes(out2, "stream").Bool())
	require.False(t, gjson.GetBytes(out2, "stream_options.include_usage").Bool(), "显式 stream_options 原样保留")

	// 不可解析 body：原样返回（不二次错误化）。
	raw := []byte(`not-json`)
	require.Equal(t, raw, PrepareWorkbuddyBody(raw, creds))
	require.Nil(t, PrepareWorkbuddyBody(nil, creds))
}

func TestPrepareWorkbuddyBodyMaxCompletionTokens(t *testing.T) {
	t.Parallel()
	creds := WorkbuddyCredentials{UID: "u-1"}

	out := PrepareWorkbuddyBody([]byte(`{"max_completion_tokens":123}`), creds)
	require.Equal(t, int64(123), gjson.GetBytes(out, "max_tokens").Int())
	require.False(t, gjson.GetBytes(out, "max_completion_tokens").Exists(), "别名一律删除")

	// 显式 max_tokens 优先：别名只删不译。
	out = PrepareWorkbuddyBody([]byte(`{"max_completion_tokens":5,"max_tokens":9}`), creds)
	require.Equal(t, int64(9), gjson.GetBytes(out, "max_tokens").Int())

	// 0/null/负数/非数值不翻译。
	for _, body := range []string{
		`{"max_completion_tokens":0}`,
		`{"max_completion_tokens":null}`,
		`{"max_completion_tokens":-5}`,
		`{"max_completion_tokens":"abc"}`,
	} {
		out = PrepareWorkbuddyBody([]byte(body), creds)
		require.False(t, gjson.GetBytes(out, "max_tokens").Exists(), "body=%s", body)
		require.False(t, gjson.GetBytes(out, "max_completion_tokens").Exists(), "body=%s", body)
	}
}

func TestPrepareWorkbuddyBodyToolChoiceNormalization(t *testing.T) {
	t.Parallel()
	creds := WorkbuddyCredentials{UID: "u-1"}

	// "none" → 删 tool_choice + 删 tools/functions。
	out := PrepareWorkbuddyBody([]byte(`{"tool_choice":"none","tools":[{"type":"function"}],"functions":[{"name":"x"}]}`), creds)
	require.False(t, gjson.GetBytes(out, "tool_choice").Exists())
	require.False(t, gjson.GetBytes(out, "tools").Exists())
	require.False(t, gjson.GetBytes(out, "functions").Exists())

	// 对象形式 none 同上。
	out = PrepareWorkbuddyBody([]byte(`{"tool_choice":{"type":"none"},"tools":[{"type":"function"}]}`), creds)
	require.False(t, gjson.GetBytes(out, "tool_choice").Exists())
	require.False(t, gjson.GetBytes(out, "tools").Exists())

	// auto/required 对象 → 字符串。
	out = PrepareWorkbuddyBody([]byte(`{"tool_choice":{"type":"auto"}}`), creds)
	require.Equal(t, "auto", gjson.GetBytes(out, "tool_choice").String())
	out = PrepareWorkbuddyBody([]byte(`{"tool_choice":{"type":"required"}}`), creds)
	require.Equal(t, "required", gjson.GetBytes(out, "tool_choice").String())

	// function 对象 → 工具名字符串。
	out = PrepareWorkbuddyBody([]byte(`{"tool_choice":{"type":"function","function":{"name":"bash"}}}`), creds)
	require.Equal(t, "bash", gjson.GetBytes(out, "tool_choice").String())

	// function 无名字 → "auto"。
	out = PrepareWorkbuddyBody([]byte(`{"tool_choice":{"type":"function","function":{}}}`), creds)
	require.Equal(t, "auto", gjson.GetBytes(out, "tool_choice").String())

	// 未知对象/非标量 → 删除。
	out = PrepareWorkbuddyBody([]byte(`{"tool_choice":{"type":"weird"}}`), creds)
	require.False(t, gjson.GetBytes(out, "tool_choice").Exists())
	out = PrepareWorkbuddyBody([]byte(`{"tool_choice":42}`), creds)
	require.False(t, gjson.GetBytes(out, "tool_choice").Exists())

	// 字符串 "auto" 原样保留。
	out = PrepareWorkbuddyBody([]byte(`{"tool_choice":"auto"}`), creds)
	require.Equal(t, "auto", gjson.GetBytes(out, "tool_choice").String())
}

func TestPrepareWorkbuddyBodyDeveloperRole(t *testing.T) {
	t.Parallel()
	creds := WorkbuddyCredentials{UID: "u-1"}
	out := PrepareWorkbuddyBody([]byte(`{"messages":[{"role":"developer","content":"sys"},{"role":"user","content":"hi"}]}`), creds)
	require.Equal(t, "system", gjson.GetBytes(out, "messages.0.role").String())
	require.Equal(t, "user", gjson.GetBytes(out, "messages.1.role").String())
}

func TestPrepareWorkbuddyBodyToolPairingRepair(t *testing.T) {
	t.Parallel()
	creds := WorkbuddyCredentials{UID: "u-1"}

	// 孤儿 tool_call（无结果）与孤儿 tool 结果（无调用）都被剔除；成对保留。
	body := `{"messages":[
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"a","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"b","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"r1"},
		{"role":"assistant","tool_calls":[{"id":"c3","type":"function","function":{"name":"c","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c9","content":"orphan"},
		{"role":"user","content":"next"}
	]}`
	out := PrepareWorkbuddyBody([]byte(body), creds)
	// 第一条 assistant：c1 保留、c2（无结果）剔除。
	require.Equal(t, int64(1), gjson.GetBytes(out, "messages.0.tool_calls.#").Int())
	require.Equal(t, "c1", gjson.GetBytes(out, "messages.0.tool_calls.0.id").String())
	// c1 结果保留；c9 孤儿结果整条删除；c3 无结果 → 整个 tool_calls 键删除。
	roles := []string{}
	for _, m := range gjson.GetBytes(out, "messages").Array() {
		roles = append(roles, m.Get("role").String())
	}
	require.Equal(t, []string{"assistant", "tool", "assistant", "user"}, roles)
	require.False(t, gjson.GetBytes(out, "messages.2.tool_calls").Exists())
	require.Equal(t, "c1", gjson.GetBytes(out, "messages.1.tool_call_id").String())
}

func TestPrepareWorkbuddyBodyToolResultRepack(t *testing.T) {
	t.Parallel()
	creds := WorkbuddyCredentials{UID: "u-1"}
	// 插在同批结果中间的非 tool 消息后移（Codex image_resize_notice 形态）。
	body := `{"messages":[
		{"role":"assistant","tool_calls":[{"id":"c0","type":"function","function":{"name":"a","arguments":"{}"}},{"id":"c1","type":"function","function":{"name":"b","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c0","content":"r0"},
		{"role":"developer","content":"<image_resize_notice>"},
		{"role":"tool","tool_call_id":"c1","content":"r1"}
	]}`
	out := PrepareWorkbuddyBody([]byte(body), creds)
	roles := []string{}
	ids := []string{}
	for _, m := range gjson.GetBytes(out, "messages").Array() {
		roles = append(roles, m.Get("role").String())
		ids = append(ids, m.Get("tool_call_id").String())
	}
	require.Equal(t, []string{"assistant", "tool", "tool", "system"}, roles)
	require.Equal(t, []string{"", "c0", "c1", ""}, ids)
}

func TestPrepareWorkbuddyBodyDeepSeekThinking(t *testing.T) {
	t.Parallel()
	creds := WorkbuddyCredentials{UID: "u-1"}

	// deepseek 无 thinking → 注入 enabled + 默认档 high。
	out := PrepareWorkbuddyBody([]byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`), creds)
	require.Equal(t, "enabled", gjson.GetBytes(out, "thinking.type").String())
	require.Equal(t, "high", gjson.GetBytes(out, "reasoning_effort").String())

	// 显式 reasoning_effort → 不覆盖、不降级（无模型目录，透传）。
	out = PrepareWorkbuddyBody([]byte(`{"model":"deepseek-v4-flash","reasoning_effort":"low"}`), creds)
	require.Equal(t, "low", gjson.GetBytes(out, "reasoning_effort").String())
	require.Equal(t, "enabled", gjson.GetBytes(out, "thinking.type").String())

	// thinking disabled → 尊重并删 effort。
	out = PrepareWorkbuddyBody([]byte(`{"model":"deepseek-v4-pro","thinking":{"type":"disabled"},"reasoning_effort":"high"}`), creds)
	require.Equal(t, "disabled", gjson.GetBytes(out, "thinking.type").String())
	require.False(t, gjson.GetBytes(out, "reasoning_effort").Exists())

	// 非 deepseek 模型 → 零改动。
	out = PrepareWorkbuddyBody([]byte(`{"model":"glm-5.3"}`), creds)
	require.False(t, gjson.GetBytes(out, "thinking").Exists())
	require.False(t, gjson.GetBytes(out, "reasoning_effort").Exists())
}

func TestPrepareWorkbuddyBodyReasoningContentBackfill(t *testing.T) {
	t.Parallel()
	creds := WorkbuddyCredentials{UID: "u-1"}
	body := `{"model":"deepseek-v4-pro","messages":[
		{"role":"assistant","content":"a1","reasoning":"r1"},
		{"role":"assistant","content":"a2"},
		{"role":"user","content":"hi"}
	]}`
	out := PrepareWorkbuddyBody([]byte(body), creds)
	require.Equal(t, "r1", gjson.GetBytes(out, "messages.0.reasoning_content").String())
	require.Equal(t, "", gjson.GetBytes(out, "messages.1.reasoning_content").String(), "无痕迹 assistant 补空串")
	require.False(t, gjson.GetBytes(out, "messages.2.reasoning_content").Exists(), "非 assistant 不补")
}

func TestPrepareWorkbuddyBodyPromptCacheKeyIsolation(t *testing.T) {
	t.Parallel()
	body := []byte(`{"conversation_id":"conv-1","messages":[]}`)

	credsA := WorkbuddyCredentials{UID: "user-aaaa1111bbbb"}
	credsB := WorkbuddyCredentials{UID: "user-bbbb2222cccc"}
	outA := PrepareWorkbuddyBody(body, credsA)
	outB := PrepareWorkbuddyBody(body, credsB)
	keyA := gjson.GetBytes(outA, "prompt_cache_key").String()
	keyB := gjson.GetBytes(outB, "prompt_cache_key").String()

	require.Equal(t, buildWorkbuddyCacheKey("user-aaaa1111bbbb", "conv-1"), keyA)
	require.True(t, len(keyA) > 0 && keyA[:5] == "wb2a-", "格式 wb2a-<uid8>-<convHex>")
	require.Contains(t, keyA, "-user-aaa-", "uid8 为 uid 前 8 位")
	require.NotEqual(t, keyA, keyB, "跨账号必须隔离（uid 参与哈希）")

	// 同 uid 同会话稳定；不同会话不同。
	require.Equal(t, keyA, gjson.GetBytes(PrepareWorkbuddyBody(body, credsA), "prompt_cache_key").String())
	outOther := PrepareWorkbuddyBody([]byte(`{"conversation_id":"conv-2","messages":[]}`), credsA)
	require.NotEqual(t, keyA, gjson.GetBytes(outOther, "prompt_cache_key").String())

	// body 已带 key → 原值保留。
	outPreset := PrepareWorkbuddyBody([]byte(`{"prompt_cache_key":"preset-key","conversation_id":"conv-1"}`), credsA)
	require.Equal(t, "preset-key", gjson.GetBytes(outPreset, "prompt_cache_key").String())

	// camel 形态 conversationId 同样生效。
	outCamel := PrepareWorkbuddyBody([]byte(`{"conversationId":"conv-1"}`), credsA)
	require.Equal(t, keyA, gjson.GetBytes(outCamel, "prompt_cache_key").String())
}

func TestPrepareWorkbuddyBodyGlobalConsoleSystem(t *testing.T) {
	t.Parallel()
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	// global：首条非 system → 前置兜底 system。
	out := PrepareWorkbuddyBody(body, WorkbuddyCredentials{UID: "u-1", Realm: "global"})
	require.Equal(t, "system", gjson.GetBytes(out, "messages.0.role").String())
	require.Equal(t, "You are a helpful assistant.", gjson.GetBytes(out, "messages.0.content").String())
	require.Equal(t, "user", gjson.GetBytes(out, "messages.1.role").String())

	// global 且首条已是 system → 不重复注入。
	out = PrepareWorkbuddyBody([]byte(`{"messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`), WorkbuddyCredentials{UID: "u-1", Realm: "global"})
	require.Equal(t, int64(2), gjson.GetBytes(out, "messages.#").Int())

	// CN：不注入。
	out = PrepareWorkbuddyBody(body, WorkbuddyCredentials{UID: "u-1", Realm: "cn"})
	require.Equal(t, "user", gjson.GetBytes(out, "messages.0.role").String())
}

func TestPrepareWorkbuddyBodySanitizeToggle(t *testing.T) {
	t.Parallel()
	creds := WorkbuddyCredentials{UID: "u-1"}
	body := []byte(`{"messages":[{"role":"user","content":"error code 11128 appeared"}]}`)

	// 默认开启脱敏：11128 → 11-128。
	out := PrepareWorkbuddyBody(body, creds)
	require.Contains(t, gjson.GetBytes(out, "messages.0.content").String(), "11-128")
	require.NotContains(t, gjson.GetBytes(out, "messages.0.content").String(), "11128")

	// 关闭脱敏：原样保留（协议兼容变换仍生效）。
	out = PrepareWorkbuddyBodyOpt(body, creds, false)
	require.Contains(t, gjson.GetBytes(out, "messages.0.content").String(), "11128")

	// 模板句最小改写。
	body2 := []byte(`{"messages":[{"role":"user","content":"You are Claude Code, Anthropic's official CLI for Claude."}]}`)
	out = PrepareWorkbuddyBody(body2, creds)
	require.Contains(t, gjson.GetBytes(out, "messages.0.content").String(), "official CLI tool for Claude")

	// tool_calls arguments 与 reasoning_content 同样净化。
	body3 := []byte(`{"messages":[{"role":"assistant","content":null,"reasoning_content":"see 11128","tool_calls":[{"id":"c1","type":"function","function":{"name":"a","arguments":"{\"cmd\":\"11128\"}"}}]},{"role":"tool","tool_call_id":"c1","content":"ok"}]}`)
	out = PrepareWorkbuddyBody(body3, creds)
	require.NotContains(t, gjson.GetBytes(out, "messages.0.reasoning_content").String(), "11128")
	require.NotContains(t, gjson.GetBytes(out, "messages.0.tool_calls.0.function.arguments").String(), "11128")
}

func TestBuildWorkbuddyCacheKeyEmptyUID(t *testing.T) {
	t.Parallel()
	key := buildWorkbuddyCacheKey("", "conv")
	require.Contains(t, key, "wb2a--")
	require.NotEqual(t, key, buildWorkbuddyCacheKey("", "other"))
}
