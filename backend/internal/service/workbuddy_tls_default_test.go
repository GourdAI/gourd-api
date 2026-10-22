//go:build unit

package service

// workbuddy_tls_default_test.go WorkBuddy「平台默认开启 TLS 指纹」的语义单测。
//
// 这次改造把 WorkBuddy 从「账号级 opt-in」改成「平台默认开启」，需要钉死三条边界：
//  1. 未表态 → 走 DoWithTLS（这是本次要修的核心缺陷：Go 标准库 ClientHello）；
//  2. 显式 false → 走普通 Do（管理员意图优先于平台默认）；
//  3. 其它平台完全不变（platformDefault=false 时行为与改造前一致）。
//
// 另钉一点：ExtrA 里非 bool 值（如字符串 "true"）不算表态，不得因平台默认而"顺手启用"。

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestWorkbuddyPlatformDefaultTLSStates 三态语义（未表态/显式开/显式关）。
func TestWorkbuddyPlatformDefaultTLSStates(t *testing.T) {
	profileService := newTLSDispatchProfileService()

	cases := []struct {
		name        string
		extra       map[string]any
		wantDoTLS   int
		wantPlainDo int
	}{
		{"未表态 → 平台默认开启", nil, 1, 0},
		{"显式 true", map[string]any{"enable_tls_fingerprint": true}, 1, 0},
		{"显式 false 优先于平台默认", map[string]any{"enable_tls_fingerprint": false}, 0, 1},
		{"非 bool 视为未表态", map[string]any{"enable_tls_fingerprint": "true"}, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local := &tlsDispatchUpstreamStub{}
			account := &Account{ID: 7, Platform: PlatformWorkbuddy, Type: AccountTypeOAuth, Extra: tc.extra}
			_, err := doUpstreamWithPlatformDefaultTLS(local, profileService, tlsDispatchRequest(), "", account, 1, true)
			require.NoError(t, err)
			require.Equal(t, tc.wantDoTLS, local.doTLSCalls, "DoWithTLS 调用次数")
			require.Equal(t, tc.wantPlainDo, local.doCalls, "普通 Do 调用次数")
		})
	}
}

// TestWorkbuddyPlatformDefaultTLSOff 平台开关关闭时回到 opt-in 语义。
func TestWorkbuddyPlatformDefaultTLSOff(t *testing.T) {
	profileService := newTLSDispatchProfileService()
	local := &tlsDispatchUpstreamStub{}
	account := &Account{ID: 8, Platform: PlatformWorkbuddy, Type: AccountTypeOAuth}

	// platformDefault=false + 未表态 → 不伪装（改造前的 WorkBuddy 行为）
	_, err := doUpstreamWithPlatformDefaultTLS(local, profileService, tlsDispatchRequest(), "", account, 1, false)
	require.NoError(t, err)
	require.Equal(t, 1, local.doCalls)
	require.Zero(t, local.doTLSCalls)

	// platformDefault=false + 显式 true → 仍然伪装
	enabled := &tlsDispatchUpstreamStub{}
	accountOn := &Account{ID: 9, Platform: PlatformWorkbuddy, Type: AccountTypeOAuth,
		Extra: map[string]any{"enable_tls_fingerprint": true}}
	_, err = doUpstreamWithPlatformDefaultTLS(enabled, profileService, tlsDispatchRequest(), "", accountOn, 1, false)
	require.NoError(t, err)
	require.Equal(t, 1, enabled.doTLSCalls)
}

// TestOtherPlatformsUnaffectedByWorkbuddyDefault 平台默认不得泄漏到其它平台。
func TestOtherPlatformsUnaffectedByWorkbuddyDefault(t *testing.T) {
	profileService := newTLSDispatchProfileService()
	for _, platform := range []string{PlatformAnthropic, PlatformGemini, PlatformOpenAI, PlatformGrok, PlatformAntigravity, PlatformTrae, PlatformQoder} {
		local := &tlsDispatchUpstreamStub{}
		account := &Account{ID: 11, Platform: platform, Type: AccountTypeOAuth}
		// platformDefault=false 是其它平台的调用口径（resolveOpenAIUpstreamTLSProfile 里
		// 只有 WorkBuddy 才给 true）。
		_, err := doUpstreamWithPlatformDefaultTLS(local, profileService, tlsDispatchRequest(), "", account, 1, false)
		require.NoError(t, err)
		require.Equal(t, 1, local.doCalls, "platform=%s 必须保持 opt-in", platform)
		require.Zero(t, local.doTLSCalls, "platform=%s", platform)
	}
}

// TestResolveOpenAIUpstreamTLSProfileWorkbuddyDefault
// WorkBuddy 账号在配置默认（true）下解析出非 nil Profile；显式关闭时 nil。
func TestResolveOpenAIUpstreamTLSProfileWorkbuddyDefault(t *testing.T) {
	profileService := newTLSDispatchProfileService()

	cfg := &config.Config{}
	cfg.Gateway.Workbuddy.TLSFingerprintEnabled = true
	svc := &OpenAIGatewayService{cfg: cfg, tlsFPProfileService: profileService}

	wb := &Account{ID: 21, Platform: PlatformWorkbuddy, Type: AccountTypeOAuth}
	require.NotNil(t, svc.resolveOpenAIUpstreamTLSProfile(wb), "WorkBuddy 未表态 + 平台默认开 → 必须给出 Profile")

	wbOff := &Account{ID: 22, Platform: PlatformWorkbuddy, Type: AccountTypeOAuth,
		Extra: map[string]any{"enable_tls_fingerprint": false}}
	require.Nil(t, svc.resolveOpenAIUpstreamTLSProfile(wbOff), "显式关闭必须压过平台默认")

	// 非 WorkBuddy 账号即使平台开关为 true 也不得被默认启用。
	other := &Account{ID: 23, Platform: PlatformGemini, Type: AccountTypeOAuth}
	require.Nil(t, svc.resolveOpenAIUpstreamTLSProfile(other), "其它平台必须保持 opt-in")

	// 平台开关关闭 → WorkBuddy 回到 opt-in。
	cfg.Gateway.Workbuddy.TLSFingerprintEnabled = false
	require.Nil(t, svc.resolveOpenAIUpstreamTLSProfile(wb), "开关关闭后不得伪装")
}

// TestResolveOpenAIUpstreamTLSProfileNilConfig KeepsLegacy
// 无配置（单测常见）时保持历史 opt-in 语义，不隐式开启。
func TestResolveOpenAIUpstreamTLSProfileNilConfig(t *testing.T) {
	profileService := newTLSDispatchProfileService()
	svc := &OpenAIGatewayService{cfg: nil, tlsFPProfileService: profileService}
	wb := &Account{ID: 31, Platform: PlatformWorkbuddy, Type: AccountTypeOAuth}
	require.Nil(t, svc.resolveOpenAIUpstreamTLSProfile(wb),
		"cfg 为 nil 时必须保持 opt-in（否则大批既有单测会因为意外启用而行为漂移）")
}

// TestTLSFingerprintExtraState 三态读取本身（未设置 / 显式开 / 显式关 / 非 bool）。
func TestTLSFingerprintExtraState(t *testing.T) {
	enabled, explicit := (&Account{}).tlsFingerprintExtraState()
	require.False(t, explicit)
	require.False(t, enabled)

	enabled, explicit = (&Account{Extra: map[string]any{"enable_tls_fingerprint": true}}).tlsFingerprintExtraState()
	require.True(t, explicit)
	require.True(t, enabled)

	enabled, explicit = (&Account{Extra: map[string]any{"enable_tls_fingerprint": false}}).tlsFingerprintExtraState()
	require.True(t, explicit)
	require.False(t, enabled)

	enabled, explicit = (&Account{Extra: map[string]any{"enable_tls_fingerprint": "true"}}).tlsFingerprintExtraState()
	require.False(t, explicit, "非 bool 不是有效声明")
	require.False(t, enabled)

	require.False(t, (*Account)(nil).IsTLSFingerprintEnabled(), "nil 账号必须安全")
}
