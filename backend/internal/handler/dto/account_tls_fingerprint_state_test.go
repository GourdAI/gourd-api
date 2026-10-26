package dto

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// withTLSPlatformCfg 临时注入平台级配置，测试结束自动复原（包级变量，避免用例间串味）。
func withTLSPlatformCfg(t *testing.T, cfg *config.Config) {
	t.Helper()
	prev := tlsFingerprintPlatformCfg
	tlsFingerprintPlatformCfg = cfg
	t.Cleanup(func() { tlsFingerprintPlatformCfg = prev })
}

// TestTLSFingerprintState_ThreeStates 三态判定必须与出站链路 resolvePlatformDefaultTLSProfile 一致。
//
// 这是本次修复的核心不变式：旧实现只有 IsTLSFingerprintEnabled() 这个二值视图，
// 「未表态（跟随平台默认、实际开启）」与「显式关闭」在 API 响应里完全无法区分，
// 于是管理台把 WorkBuddy/Trae/Qoder 的开关永远显示成 OFF，而出站实际一直在伪装。
func TestTLSFingerprintState_ThreeStates(t *testing.T) {
	withTLSPlatformCfg(t, nil) // cfg=nil 时三平台仍默认开启（platformDefaultTLSFor 的既定语义）

	wb := func(extra map[string]any) *service.Account {
		return &service.Account{ID: 1, Platform: service.PlatformWorkbuddy, Type: service.AccountTypeOAuth, Extra: extra}
	}

	state, platformDefault, effective := TLSFingerprintState(wb(nil))
	require.Equal(t, TLSFingerprintStateInherit, state, "未表态必须是 inherit")
	require.True(t, platformDefault, "WorkBuddy 平台默认必须为 true")
	require.True(t, effective, "未表态 + 平台默认开 → 实际在伪装（旧实现此处误报未伪装）")

	state, platformDefault, effective = TLSFingerprintState(wb(map[string]any{"enable_tls_fingerprint": true}))
	require.Equal(t, TLSFingerprintStateExplicitOn, state)
	require.True(t, platformDefault)
	require.True(t, effective)

	state, platformDefault, effective = TLSFingerprintState(wb(map[string]any{"enable_tls_fingerprint": false}))
	require.Equal(t, TLSFingerprintStateExplicitOff, state, "显式 false 必须是 explicit_off，不能与 inherit 混同")
	require.True(t, platformDefault, "平台默认值本身不受账号表态影响")
	require.False(t, effective, "显式关闭必须压过平台默认")

	// 非 bool 值（脏数据）按未表态处理，与 tlsFingerprintExtraState 的历史判定一致。
	state, _, effective = TLSFingerprintState(wb(map[string]any{"enable_tls_fingerprint": "true"}))
	require.Equal(t, TLSFingerprintStateInherit, state)
	require.True(t, effective)

	// 非默认开启平台：未表态 → 不伪装。
	state, platformDefault, effective = TLSFingerprintState(
		&service.Account{ID: 2, Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth})
	require.Equal(t, TLSFingerprintStateInherit, state)
	require.False(t, platformDefault, "Anthropic 仍是 opt-in")
	require.False(t, effective)
}

// TestTLSFingerprintState_PlatformConfigFalse 平台开关显式关闭时，inherit 账号不得报「已伪装」。
func TestTLSFingerprintState_PlatformConfigFalse(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.Workbuddy.TLSFingerprintEnabled = false
	cfg.Gateway.Trae.TLSFingerprintEnabled = false
	cfg.Gateway.Qoder.TLSFingerprintEnabled = false
	withTLSPlatformCfg(t, cfg)

	for _, platform := range []string{service.PlatformWorkbuddy, service.PlatformTrae, service.PlatformQoder} {
		state, platformDefault, effective := TLSFingerprintState(
			&service.Account{ID: 3, Platform: platform, Type: service.AccountTypeOAuth})
		require.Equal(t, TLSFingerprintStateInherit, state, platform)
		require.False(t, platformDefault, platform+" 配置关闭后平台默认必须为 false")
		require.False(t, effective, platform+" 配置关闭后不得伪装")
	}

	// 配置关闭时，账号显式 true 仍然生效（账号表态优先于平台默认的方向不变）。
	state, _, effective := TLSFingerprintState(&service.Account{
		ID: 4, Platform: service.PlatformWorkbuddy, Type: service.AccountTypeOAuth,
		Extra: map[string]any{"enable_tls_fingerprint": true},
	})
	require.Equal(t, TLSFingerprintStateExplicitOn, state)
	require.True(t, effective)
}

// TestAccountFromServiceShallow_TLSFingerprintEchoesEffective 验证 DTO 把三态与生效值都透出。
//
// 反向护栏关注点：explicit_off 必须下发 **false 而不是不下发**。旧实现只在开启时下发，
// 前端因此无法区分「未表态」与「已关闭」，也就无法把开关渲染成真正的 OFF。
func TestAccountFromServiceShallow_TLSFingerprintEchoesEffective(t *testing.T) {
	withTLSPlatformCfg(t, nil)

	inherit := AccountFromServiceShallow(
		&service.Account{ID: 5, Platform: service.PlatformWorkbuddy, Type: service.AccountTypeOAuth})
	require.Nil(t, inherit.EnableTLSFingerprint, "inherit 保持不下发（兼容旧报文形态）")
	require.Equal(t, TLSFingerprintStateInherit, inherit.TLSFingerprintState)
	require.True(t, inherit.TLSFingerprintPlatformDefault)
	require.True(t, inherit.TLSFingerprintEffective, "未表态的 WorkBuddy 账号必须报「实际已伪装」")

	off := AccountFromServiceShallow(&service.Account{
		ID: 6, Platform: service.PlatformWorkbuddy, Type: service.AccountTypeOAuth,
		Extra: map[string]any{"enable_tls_fingerprint": false},
	})
	require.NotNil(t, off.EnableTLSFingerprint, "显式关闭必须下发 false，否则前端无法区分 inherit 与 off")
	require.False(t, *off.EnableTLSFingerprint)
	require.Equal(t, TLSFingerprintStateExplicitOff, off.TLSFingerprintState)
	require.False(t, off.TLSFingerprintEffective)

	on := AccountFromServiceShallow(&service.Account{
		ID: 7, Platform: service.PlatformWorkbuddy, Type: service.AccountTypeOAuth,
		Extra: map[string]any{"enable_tls_fingerprint": true},
	})
	require.NotNil(t, on.EnableTLSFingerprint)
	require.True(t, *on.EnableTLSFingerprint)
	require.Equal(t, TLSFingerprintStateExplicitOn, on.TLSFingerprintState)
	require.True(t, on.TLSFingerprintEffective)
}

// TestAccountListItemFromAccount_CarriesTLSState 列表投影不得丢掉三态字段。
//
// 列表页是管理员判断「哪些账号在伪装」的主视图，若只在详情接口透出，列表仍会显示 OFF。
func TestAccountListItemFromAccount_CarriesTLSState(t *testing.T) {
	withTLSPlatformCfg(t, nil)

	full := AccountFromServiceShallow(&service.Account{
		ID: 8, Platform: service.PlatformWorkbuddy, Type: service.AccountTypeOAuth,
		Extra: map[string]any{"enable_tls_fingerprint": false},
	})
	item := AccountListItemFromAccount(full)
	require.NotNil(t, item)
	require.Equal(t, TLSFingerprintStateExplicitOff, item.TLSFingerprintState)
	require.False(t, item.TLSFingerprintEffective)
	require.True(t, item.TLSFingerprintPlatformDefault)
	require.NotNil(t, item.EnableTLSFingerprint)
	require.False(t, *item.EnableTLSFingerprint)
}

// TestTLSFingerprintPlatformDefaults 平台默认值查询端点的数据源。
func TestTLSFingerprintPlatformDefaults(t *testing.T) {
	withTLSPlatformCfg(t, nil)

	defaults := TLSFingerprintPlatformDefaults()
	require.True(t, defaults[service.PlatformWorkbuddy])
	require.True(t, defaults[service.PlatformTrae])
	require.True(t, defaults[service.PlatformQoder])
	require.NotContains(t, defaults, service.PlatformAnthropic, "opt-in 平台不必预告")

	cfg := &config.Config{}
	cfg.Gateway.Workbuddy.TLSFingerprintEnabled = false
	cfg.Gateway.Trae.TLSFingerprintEnabled = false
	cfg.Gateway.Qoder.TLSFingerprintEnabled = false
	withTLSPlatformCfg(t, cfg)

	defaults = TLSFingerprintPlatformDefaults()
	require.False(t, defaults[service.PlatformWorkbuddy])
	require.False(t, defaults[service.PlatformTrae])
	require.False(t, defaults[service.PlatformQoder])
}

// TestTLSFingerprintState_NilAccount nil 账号必须安全（mapper 上游有 nil 分支，但本函数也要自守）。
func TestTLSFingerprintState_NilAccount(t *testing.T) {
	withTLSPlatformCfg(t, nil)
	state, _, effective := TLSFingerprintState(nil)
	require.Equal(t, TLSFingerprintStateInherit, state)
	require.False(t, effective)
}
