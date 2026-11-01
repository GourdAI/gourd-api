//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// workbuddy_tasks_activity_gate_test.go 活跃上报定时任务的启用门控。
//
// 【为什么要单独一条】该任务每日向 billing 域 POST /v2/report 送 N 条**凭空捏造的
// chat_request_send 事件**，是 2026-11-01 WorkBuddy 国内站 11140 账号封禁排查中风险
// 最高的可疑源；默认值已从 true 翻为 false，且门控函数刻意把 nil 配置也归入「关」。
//
// 两个易被静默破坏的点，本文件各钉一条：
//  1. nil cfg 不得放行 —— 旧写法 `cfg == nil || Enabled` 会让「没配置」等于「开着」，
//     正好在最没人盯着的环境里把可疑流量放出去；
//  2. 手动入口不受本开关影响 —— 关掉定时任务不等于取消能力，管理员仍可单号触发。

func TestWorkbuddyActivityTaskEnabledDefaultsToOff(t *testing.T) {
	t.Parallel()
	// nil 配置 = 关（默认翻转的核心语义；反向护栏：把实现改回
	// `cfg == nil || ...` 本条必须 FAIL）。
	require.False(t, workbuddyActivityTaskEnabled(nil), "nil 配置不得启动活跃上报")

	off := &config.Config{}
	require.False(t, workbuddyActivityTaskEnabled(off), "零值配置不得启动")

	on := &config.Config{}
	on.Gateway.Workbuddy.ActivityEnabled = true
	require.True(t, workbuddyActivityTaskEnabled(on), "显式开启必须启动")
}

func TestWorkbuddyTasksHoursStillDefaultsWhenConfigured(t *testing.T) {
	t.Parallel()
	// 小时列表缺省值不因本次开关翻转而改变：管理员显式开启后仍拿到默认 [10]，
	// 不必同时改两项才能恢复旧行为。
	require.Equal(t, []int{10}, workbuddyTasksHours(nil, "activity"))
	require.Equal(t, []int{9, 21}, workbuddyTasksHours(nil, "travel"))
}
