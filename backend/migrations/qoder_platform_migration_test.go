package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQoderPlatformMigration(t *testing.T) {
	content, err := FS.ReadFile("240_add_qoder_platform.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "user_platform_quotas_platform_check")
	require.Contains(t, sql, "composite_model_routes_target_platform_check")
	require.Contains(t, sql, "channel_monitors_provider_check")
	require.Contains(t, sql, "channel_monitor_request_templates_provider_check")
	require.Contains(t, sql, "'qoder'")
	require.Contains(t, sql, "'workbuddy'")
	require.Contains(t, sql, "'opencode_go'")
	require.Contains(t, sql, "'minimax'")
	require.Contains(t, sql, "position('qoder' IN monitor_constraint_def) = 0")
	require.Contains(t, sql, "position('qoder' IN template_constraint_def) = 0")
	require.Contains(t, sql,
		"CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go', 'workbuddy', 'qoder'))")
	require.Contains(t, sql,
		"CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go', 'workbuddy', 'qoder'))")
	require.Contains(t, sql,
		"CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go', 'workbuddy', 'qoder'))")
}
