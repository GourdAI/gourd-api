//go:build unit

package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const traeGuardrailNotice = "[System reminder: the current model does not support images. " +
	"Content filtered. Please inform user to switch to a multimodal model or try alternative approach.]"

func TestFlattenUpstreamSnippetCollapsesNewlines(t *testing.T) {
	got := flattenUpstreamSnippet("  line one\n\tline  two\r\nline three  ")
	require.Equal(t, "line one line two line three", got)
	require.Empty(t, flattenUpstreamSnippet("   \n\t "))
}

func TestIsUpstreamGuardrailNotice(t *testing.T) {
	t.Run("bracketed system reminder is a notice", func(t *testing.T) {
		require.True(t, isUpstreamGuardrailNotice(traeGuardrailNotice))
		require.True(t, isUpstreamGuardrailNotice("\n "+traeGuardrailNotice+"\n"))
	})

	t.Run("tagged system reminder is a notice", func(t *testing.T) {
		require.True(t, isUpstreamGuardrailNotice("<system-reminder>\nPlease switch to a multimodal model.\n</system-reminder>"))
	})

	t.Run("real upstream errors are not notices", func(t *testing.T) {
		// 非包裹形态 / 缺护栏指令句 / 真实报错正文，都不得被误判。
		require.False(t, isUpstreamGuardrailNotice(`{"error":{"message":"invalid api key"}}`))
		require.False(t, isUpstreamGuardrailNotice("[not-found] model does not support images"))
		require.False(t, isUpstreamGuardrailNotice("<system-reminder>Today is 2026-09-27.</system-reminder>"))
		require.False(t, isUpstreamGuardrailNotice("rate limit exceeded, please retry later"))
		require.False(t, isUpstreamGuardrailNotice(""))
	})
}

// traeTruncateForError / qoderTruncateReason 是上游片段进入管理员可见信息的必经之路，
// 必须保证多行护栏提示不会把 toast / 表格撑成一段文章。
func TestTruncateHelpersFlattenMultiLineUpstreamSnippets(t *testing.T) {
	body := `{"error":{"message":"` + strings.ReplaceAll(traeGuardrailNotice, " ", "  ") + `"}}`

	traeOut := traeTruncateForError(body)
	require.NotContains(t, traeOut, "\n")
	require.NotContains(t, traeOut, "  ")
	require.LessOrEqual(t, len([]rune(traeOut)), 203) // 200 + "..."

	qoderOut := qoderTruncateReason(traeGuardrailNotice, 200)
	require.NotContains(t, qoderOut, "\n")
	require.Contains(t, qoderOut, "does not support images")
}

func TestResponsesProbeBodyIsGuardrailNotice(t *testing.T) {
	noticeOnly, err := json.Marshal(map[string]any{
		"output": []any{
			map[string]any{
				"type":    "message",
				"content": []any{map[string]any{"type": "output_text", "text": traeGuardrailNotice}},
			},
		},
	})
	require.NoError(t, err)
	require.True(t, responsesProbeBodyIsGuardrailNotice(noticeOnly))

	// error.message 形态（非 Responses 形状的兼容网关）。
	errBody, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": traeGuardrailNotice},
	})
	require.NoError(t, err)
	require.True(t, responsesProbeBodyIsGuardrailNotice(errBody))

	// 正常工具调用回复不得被误判。
	withCall, err := json.Marshal(map[string]any{
		"output": []any{
			map[string]any{
				"type":    "message",
				"content": []any{map[string]any{"type": "output_text", "text": "Sure, calling the tool now."}},
			},
			map[string]any{"type": "function_call", "name": "probe_ping"},
		},
	})
	require.NoError(t, err)
	require.False(t, responsesProbeBodyIsGuardrailNotice(withCall))
	require.True(t, responsesProbeBodyHasFunctionCall(withCall))

	require.False(t, responsesProbeBodyIsGuardrailNotice(nil))
}
