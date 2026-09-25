package repository

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 防漂移：候选 SQL 的平台白名单必须与 service.IsUpstreamBillingProbeIdentity
// 完全一致。两边任何一侧增删平台而忘了同步，本测试即失败。
func TestUpstreamBillingProbePlatformsSQLMirrorsServiceWhitelist(t *testing.T) {
	quoted := strings.Split(upstreamBillingProbePlatformsSQL, ", ")
	require.NotEmpty(t, quoted)

	sqlPlatforms := make(map[string]struct{}, len(quoted))
	for _, item := range quoted {
		require.True(t, strings.HasPrefix(item, "'") && strings.HasSuffix(item, "'"), "platform literal must be single-quoted: %s", item)
		sqlPlatforms[strings.Trim(item, "'")] = struct{}{}
	}

	// SQL 名单内的每个平台都必须被服务层认定为探测合格。
	for platform := range sqlPlatforms {
		require.True(t, service.IsUpstreamBillingProbeIdentity(platform, service.AccountTypeAPIKey),
			"platform %q is in the SQL whitelist but not probe-eligible in service", platform)
	}

	// 反向：任何被服务层认定为合格的平台都必须在 SQL 名单内（否则它永远探测不到）。
	for _, platform := range []string{
		service.PlatformOpenAI, service.PlatformAnthropic, service.PlatformGemini,
		service.PlatformAntigravity, service.PlatformGrok, service.PlatformKimi,
		service.PlatformZhipu, service.PlatformDeepseek, service.PlatformMiniMax,
		service.PlatformOpenCodeGo,
	} {
		_, ok := sqlPlatforms[platform]
		require.True(t, ok, "probe-eligible platform %q is missing from the SQL whitelist", platform)
	}

	// WorkBuddy 已被移出资格名单，SQL 名单必须同步排除。
	_, workbuddyInSQL := sqlPlatforms[service.PlatformWorkbuddy]
	require.False(t, workbuddyInSQL, "workbuddy must not appear in the probe candidates SQL whitelist")
}

func TestUpstreamBillingProbeExtraIsSchedulerNeutral(t *testing.T) {
	require.True(t, isSchedulerNeutralExtraKey("upstream_billing_probe"))
	require.True(t, isSchedulerNeutralExtraKey("upstream_billing_probe_enabled"))
	require.False(t, shouldEnqueueSchedulerOutboxForExtraUpdates(map[string]any{
		"upstream_billing_probe":         map[string]any{"status": "ok"},
		"upstream_billing_probe_enabled": true,
	}))
}
