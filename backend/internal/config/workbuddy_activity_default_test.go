package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// workbuddy_activity_default_test.go 钉住「对话活跃上报」的默认关闭语义（2026-11-01
// 由 true 翻为 false）在**两条独立生效路径**上同时成立。
//
// 为什么需要两条断言而不是一条 SetDefault 检查：
//
//	viper.SetDefault("gateway.workbuddy.activity_enabled", false)
//
// 只覆盖「运行环境里没有 config.yaml」的情形。而部署里更常见的形态是
// copy config.example.yaml → config.yaml：此时显式键会直接**覆盖**默认值。
// 本次自查正是在示例文件里发现它仍写着 `activity_enabled: true` —— 也就是说
// 上一轮改动在「按官方示例部署」的实例上完全没生效。单改 SetDefault 是不够的，
// 示例模板必须同步；本用例就是把「模板不得回到 true」这条钉成硬护栏。
//
// 反向护栏（验证本用例真的会响）：把 deploy/config.example.yaml 的
// activity_enabled 改回 true，下面第二条必须 FAIL。

// TestLoadDefaultWorkbuddyActivityDisabled 无任何配置文件时的 viper 默认值。
func TestLoadDefaultWorkbuddyActivityDisabled(t *testing.T) {
	resetViperWithJWTSecret(t)

	cfg, err := Load()
	require.NoError(t, err)
	require.False(t, cfg.Gateway.Workbuddy.ActivityEnabled,
		"活跃上报默认关：每日向 /v2/report 送凭空捏造的 chat_request_send 事件，"+
			"是 2026-11-01 WorkBuddy 国内站 11140 账号封禁排查中风险最高的可疑源")

	// 其余三项默认不得被这次翻转连带改动（管理员显式开启时不必同时改多项）。
	require.True(t, cfg.Gateway.Workbuddy.CheckinEnabled)
	require.True(t, cfg.Gateway.Workbuddy.TravelEnabled)
	require.Equal(t, []int{10}, cfg.Gateway.Workbuddy.ActivityHours)
	require.Equal(t, 5, cfg.Gateway.Workbuddy.ActivityReportCount)
}

// TestExampleConfigWorkbuddyActivityDisabled 示例模板本身不得把该任务打开。
func TestExampleConfigWorkbuddyActivityDisabled(t *testing.T) {
	// 测试工作目录是 internal/config，模板在仓库根的 deploy/ 下（repository 包已有
	// 以 filepath.Join("..","..","migrations",...) 读仓内文件的先例，此处同风格）。
	path := filepath.Join("..", "..", "..", "deploy", "config.example.yaml")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("示例配置模板不在预期位置（%s），跳过", path)
	}
	require.NoError(t, err, "读取 deploy/config.example.yaml 失败")

	var doc struct {
		Gateway struct {
			Workbuddy struct {
				ActivityEnabled *bool `yaml:"activity_enabled"`
			} `yaml:"workbuddy"`
		} `yaml:"gateway"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &doc), "deploy/config.example.yaml 解析失败")

	// 键不存在也接受（那会落到 viper 默认 false）；存在则必须是 false。
	if doc.Gateway.Workbuddy.ActivityEnabled != nil {
		require.False(t, *doc.Gateway.Workbuddy.ActivityEnabled,
			"示例模板把 activity_enabled 写成 true 会覆盖 viper 默认值，"+
				"使「默认关」在所有按模板部署的实例上失效")
	}

	// 顺带钉住注释里写了开关的理由，避免后来者只看到一行 false 又顺手改回 true。
	require.Contains(t, string(raw), "activity_enabled",
		"模板必须提到该键（附默认关的理由），不得静默删键")
	require.False(t, strings.Contains(string(raw), "activity_enabled: true"),
		"模板内不得以任何形式出现 activity_enabled: true 的示例值")
}
