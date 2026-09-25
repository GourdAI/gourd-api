//go:build unit

package service

// workbuddy_payload_g17_test.go 锁死 G17 不变式：指纹净化绝不能把
// tool_calls[].function.arguments 从合法 JSON 改成非法 JSON。
//
// 背景：旧实现把 arguments 当纯文本整串替换，剥离/改写层会跨 JSON token 改写
// （实测把数字字面量插进连字符、截断引号内片段），产出非法 JSON。上游实测容忍
// 非法 arguments（不触发 11148），但模型侧读到的是坏参数。修复后改为
// 「解析 → 只净化字符串叶子 → 重新序列化」。

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// wbG17Fingerprint 被上游逐字拦截的裸数字指纹；净化产物是插入连字符的
// wbG17Cleaned（可读性保留，逐字匹配失效）。
const (
	wbG17Fingerprint = "11128"
	wbG17Cleaned     = "11-128"
)

// TestSanitizeWorkbuddyToolCallArgumentsKeepsValidJSON 每条用例输入都必须是合法
// JSON 且含指纹，输出必须仍是合法 JSON，且非字符串节点零改动。
func TestSanitizeWorkbuddyToolCallArgumentsKeepsValidJSON(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, in string }{
		{
			// 旧实现：整串文本替换会把 -11128 里的数字字面量插进连字符 → 非法 JSON。
			name: "number adjacent to fingerprint",
			in:   `{"command":"grep -11128"}`,
		},
		{
			name: "nested object and array",
			in:   `{"cmd":"11128","opts":{"depth":3},"files":["a","11128"]}`,
		},
		{
			// float64 往返会写成 1.2345678901234568e+29；UseNumber 必须保住字面量。
			name: "large integer stays literal",
			in:   `{"n":123456789012345678901234567890,"s":"11128"}`,
		},
		{
			name: "boolean and null untouched",
			in:   `{"ok":true,"nil":null,"s":"11128"}`,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.True(t, json.Valid([]byte(tc.in)), "precondition: input must be valid JSON")
			out := sanitizeWorkbuddyToolCallArguments(tc.in)
			require.True(t, json.Valid([]byte(out)), "output must stay valid JSON, got %q", out)
			require.NotContains(t, out, wbG17Fingerprint)

			// 树形结构必须保持：键集、数组长度、非字符串叶子（数字/布尔/null）
			// 逐字不变；字符串叶子允许被净化（这正是修复的目的）。
			var got, want any
			require.NoError(t, json.Unmarshal([]byte(out), &got))
			require.NoError(t, json.Unmarshal([]byte(tc.in), &want))
			assertSameJSONShape(t, want, got, "")
		})
	}
}

// assertSameJSONShape 递归比对：容器类型与键集/长度必须一致，非字符串叶子必须相等，
// 字符串叶子只要求类型一致（内容允许被净化）。
func assertSameJSONShape(t *testing.T, want, got any, path string) {
	t.Helper()
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		require.True(t, ok, "%s: expected object, got %T", path, got)
		require.Equal(t, len(w), len(g), "%s: key set size changed", path)
		for k, wv := range w {
			gv, ok := g[k]
			require.True(t, ok, "%s: key %q must survive", path, k)
			assertSameJSONShape(t, wv, gv, path+"."+k)
		}
	case []any:
		g, ok := got.([]any)
		require.True(t, ok, "%s: expected array, got %T", path, got)
		require.Equal(t, len(w), len(g), "%s: array length changed", path)
		for i := range w {
			assertSameJSONShape(t, w[i], g[i], fmt.Sprintf("%s[%d]", path, i))
		}
	case string:
		_, ok := got.(string)
		require.True(t, ok, "%s: expected string leaf, got %T", path, got)
	default:
		// 数字/布尔/null：必须逐字相等（浮点往返与 kv 段插入在这里最容易露馅）。
		require.Equal(t, want, got, "%s: non-string leaf must be untouched", path)
	}
}

// TestSanitizeWorkbuddyToolCallArgumentsPassthrough 无指纹与非法 JSON 输入保持原样。
func TestSanitizeWorkbuddyToolCallArgumentsPassthrough(t *testing.T) {
	t.Parallel()
	// 无指纹：零改动（快速路径）。
	require.Equal(t, `{"cmd":"ls"}`, sanitizeWorkbuddyToolCallArguments(`{"cmd":"ls"}`))
	// 非法 JSON（截断）：任何文本级改写只会更坏，原样放行。
	truncated := `{"cmd":"grep - 11128`
	require.Equal(t, truncated, sanitizeWorkbuddyToolCallArguments(truncated))
}

// TestSanitizeWorkbuddyToolCallArgumentsRewritesStringLeaves 字符串叶子仍被净化，
// 且首尾空白保留（不套用 prose 的 TrimSpace）。对象键名不参与净化。
func TestSanitizeWorkbuddyToolCallArgumentsRewritesStringLeaves(t *testing.T) {
	t.Parallel()
	out := sanitizeWorkbuddyToolCallArguments(`{"a":" 11128 ","b":"plain"}`)
	require.True(t, json.Valid([]byte(out)))
	require.NotContains(t, out, wbG17Fingerprint)
	var m map[string]string
	require.NoError(t, json.Unmarshal([]byte(out), &m))
	require.Equal(t, "plain", m["b"])                 // 无指纹叶子零改动
	require.Contains(t, m["a"], wbG17Cleaned)         // 指纹已被改写为带连字符形态
	require.NotContains(t, m["a"], wbG17Fingerprint)
	require.True(t, len(m["a"]) > len(wbG17Cleaned))  // 首尾空白保留
}
