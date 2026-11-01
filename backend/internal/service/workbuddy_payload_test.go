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

// TestPrepareWorkbuddyBodyNoPromptCacheKey 2026-11-01 规格翻转：网关不再注入
// prompt_cache_key（旧实现注入 "wb2a-<uid8>-<hash>"，前缀字面自报 workbuddy2api）。
//
// 旧用例钉的是「必须注入且跳账号不同值」——那是把网关特征写成了契约，对本缺陷全绿。
// 现在断言方向翻转：默认不得出现该字段；但客户端显式带的原值必须透传。
func TestPrepareWorkbuddyBodyNoPromptCacheKey(t *testing.T) {
	t.Parallel()
	body := []byte(`{"conversation_id":"conv-1","messages":[]}`)

	out := PrepareWorkbuddyBody(body, WorkbuddyCredentials{UID: "user-aaaa1111bbbb"})
	require.False(t, gjson.GetBytes(out, "prompt_cache_key").Exists(),
		"网关不得自造 prompt_cache_key（修复前恒注入 wb2a- 前缀值）")
	require.NotContains(t, string(out), "wb2a", "出站 body 不得残留 workbuddy2api 项目盐串")

	// 两个不同账号发同一 body：除账号相关字段外不得引入额外差异键。
	outB := PrepareWorkbuddyBody(body, WorkbuddyCredentials{UID: "user-bbbb2222cccc"})
	require.False(t, gjson.GetBytes(outB, "prompt_cache_key").Exists())

	// 客户端显式带 key → 原值透传（那是客户端自己的声明，不是网关伪造）。
	preset := PrepareWorkbuddyBody([]byte(`{"prompt_cache_key":"client-key","conversation_id":"conv-1","messages":[]}`), WorkbuddyCredentials{UID: "u"})
	require.Equal(t, "client-key", gjson.GetBytes(preset, "prompt_cache_key").String())
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

// TestPrepareWorkbuddyBodySanitizeToggle 2026-11-01 默认翻转：指纹脱敏（逐字改写
// Claude Code / Codex 模板句以绕过上游子串审核）不再默认开启，改看账号开关
// credentials.sanitize_fingerprints。
//
// 旧用例的「默认开启」分支把规避审核写成了默认行为；上游已把这类行为定性为
// 会导致账号级限制（code 11140），因此默认必须不变。开关本身保留（能力不删），
// 并验证其改写行为仍完整可用。
func TestPrepareWorkbuddyBodySanitizeToggle(t *testing.T) {
	t.Parallel()
	body := []byte(`{"messages":[{"role":"user","content":"error code 11128 appeared"}]}`)

	// 默认（开关未开）：**不改写**，内容逐字透传。
	out := PrepareWorkbuddyBody(body, WorkbuddyCredentials{UID: "u-1"})
	require.Contains(t, gjson.GetBytes(out, "messages.0.content").String(), "11128",
		"默认不得改写上游审核目标（修复前默认会把 11128 改成 11-128）")
	require.NotContains(t, gjson.GetBytes(out, "messages.0.content").String(), "11-128")
	// 协议兼容变换仍生效（与脱敏解耦）。
	require.True(t, gjson.GetBytes(out, "stream").Bool())

	// 账号显式开启后，脱敏行为与旧实现一致。
	on := PrepareWorkbuddyBodyOpt(body, WorkbuddyCredentials{UID: "u-1"}, true)
	require.Contains(t, gjson.GetBytes(on, "messages.0.content").String(), "11-128")
	require.NotContains(t, gjson.GetBytes(on, "messages.0.content").String(), "11128")

	// 开关从凭据读取：sanitize_fingerprints=true 走 PrepareWorkbuddyBody 即生效。
	account := &Account{ID: 1, Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"sanitize_fingerprints": true}}
	creds := account.GetWorkbuddyCredentials()
	require.True(t, creds.SanitizeFingerprints)
	flagged := PrepareWorkbuddyBody(body, creds)
	require.Contains(t, gjson.GetBytes(flagged, "messages.0.content").String(), "11-128")

	// 模板句最小改写（开关开启时）。
	body2 := []byte(`{"messages":[{"role":"user","content":"You are Claude Code, Anthropic's official CLI for Claude."}]}`)
	out2 := PrepareWorkbuddyBodyOpt(body2, WorkbuddyCredentials{UID: "u-1"}, true)
	require.Contains(t, gjson.GetBytes(out2, "messages.0.content").String(), "official CLI tool for Claude")
	// 默认关时逐字不动。
	out2off := PrepareWorkbuddyBody(body2, WorkbuddyCredentials{UID: "u-1"})
	require.Contains(t, gjson.GetBytes(out2off, "messages.0.content").String(), "You are Claude Code, Anthropic's official CLI for Claude.")

	// tool_calls arguments 与 reasoning_content 同样净化（仅开启时）。
	body3 := []byte(`{"messages":[{"role":"assistant","content":null,"reasoning_content":"see 11128","tool_calls":[{"id":"c1","type":"function","function":{"name":"a","arguments":"{\"cmd\":\"11128\"}"}}]},{"role":"tool","tool_call_id":"c1","content":"ok"}]}`)
	out3 := PrepareWorkbuddyBodyOpt(body3, WorkbuddyCredentials{UID: "u-1"}, true)
	require.NotContains(t, gjson.GetBytes(out3, "messages.0.reasoning_content").String(), "11128")
	require.NotContains(t, gjson.GetBytes(out3, "messages.0.tool_calls.0.function.arguments").String(), "11128")
}

// TestBuildWorkbuddyCacheKeyEmptyUID 已删除：随 buildWorkbuddyCacheKey 一同移除
//（网关不再自造 prompt_cache_key，见 workbuddy_payload.go 的删除说明）。
