//go:build unit

package service

// platform_default_tls_parity_test.go 平台默认 TLS 指纹的「三条路径一致性」单测。
//
// 本轮 code review 发现的结构性缺口：平台默认开启的判定原本分散在四处
// （workbuddyDo / workbuddyPlatformDefaultTLS / AccountTestService / 以及 cfg==nil
// 各自为政），于是出现了这样一条真实缺口——
//
//	Trae / Qoder 的 credits/活动域（traeDo / qoderDo）已默认带指纹（Node 形状），
//	但聊天转发主链路（trae_client.go / qoder_client.go → doOpenAIUpstream）与
//	账号测试路径（resolveTLSProfileForAccount）仍只认 WorkBuddy ⇒ 默认发
//	Go 标准库 ClientHello。
//
// 后果是同一个账号：辅助面带 Node 指纹、聊天主链路发 Go 指纹 —— 比统一不伪装
// 更容易被 JA3/JA4 关联识别。修复方式是把判定收敛为唯一实现
// platformDefaultTLSFor(account, cfg)，本用例锁住「三条路径必须同口径」。
//
// 反向护栏要求：把 resolveOpenAIUpstreamTLSProfile 或 resolveTLSProfileForAccount
// 里的 platformDefaultTLSFor(...) 改回 false / 改回只认 WorkBuddy，相应用例必须 FAIL。

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// platformDefaultTLSPlatforms 适用「平台默认开启」的三家平台。
var platformDefaultTLSPlatforms = []string{PlatformWorkbuddy, PlatformTrae, PlatformQoder}

// TestPlatformDefaultTLSForIsSingleSource 判定函数本身的三平台语义（cfg 存在 / nil / 开关关）。
func TestPlatformDefaultTLSForIsSingleSource(t *testing.T) {
	on := &config.Config{}
	on.Gateway.Workbuddy.TLSFingerprintEnabled = true
	on.Gateway.Trae.TLSFingerprintEnabled = true
	on.Gateway.Qoder.TLSFingerprintEnabled = true

	for _, platform := range platformDefaultTLSPlatforms {
		acc := &Account{Platform: platform}
		require.True(t, platformDefaultTLSFor(acc, on), "%s 开关为 true 时必须平台默认开启", platform)
		// cfg 为 nil：平台默认语义不依赖配置存在性（与三家 credits 服务同口径）。
		require.True(t, platformDefaultTLSFor(acc, nil), "%s 在 cfg==nil 时仍应平台默认开启", platform)
	}

	// 三平台之外的平台必须恒 false —— 这是「零行为变更」的真正闸门。
	for _, platform := range []string{PlatformOpenAI, PlatformGemini, PlatformGrok, PlatformAnthropic} {
		acc := &Account{Platform: platform}
		require.False(t, platformDefaultTLSFor(acc, on), "%s 不属于三平台，必须保持 opt-in", platform)
		require.False(t, platformDefaultTLSFor(acc, nil), "%s 不属于三平台，cfg==nil 也不得开启", platform)
	}

	// nil 账号不得 panic（调用点都在热路径上）。
	require.False(t, platformDefaultTLSFor(nil, on))

	// 平台开关关闭 → 各自回到 opt-in，且不影响另外两家。
	for _, platform := range platformDefaultTLSPlatforms {
		off := &config.Config{}
		off.Gateway.Workbuddy.TLSFingerprintEnabled = true
		off.Gateway.Trae.TLSFingerprintEnabled = true
		off.Gateway.Qoder.TLSFingerprintEnabled = true
		switch platform {
		case PlatformWorkbuddy:
			off.Gateway.Workbuddy.TLSFingerprintEnabled = false
		case PlatformTrae:
			off.Gateway.Trae.TLSFingerprintEnabled = false
		case PlatformQoder:
			off.Gateway.Qoder.TLSFingerprintEnabled = false
		}
		require.False(t, platformDefaultTLSFor(&Account{Platform: platform}, off),
			"%s 平台开关关闭后必须回到 opt-in", platform)
	}
}

// TestChatPathUsesPlatformDefaultTLS 聊天转发主链路（doOpenAIUpstream 的 profile 解析）
// 必须对三平台默认给出 Profile。
//
// 这条是本轮最关键的回归护栏：改造前 Trae/Qoder 在这里返回 nil，导致聊天流量
// 走普通 Do（Go 标准库 ClientHello），而同一账号的签到/余额请求已带 Node 形状。
func TestChatPathUsesPlatformDefaultTLS(t *testing.T) {
	profileService := newTLSDispatchProfileService()
	for _, platform := range platformDefaultTLSPlatforms {
		acc := &Account{ID: 41, Platform: platform, Type: AccountTypeOAuth}

		// cfg==nil：平台默认仍生效。
		svc := &OpenAIGatewayService{cfg: nil, tlsFPProfileService: profileService}
		require.NotNil(t, svc.resolveOpenAIUpstreamTLSProfile(acc),
			"%s 聊天主链路在 cfg==nil 时必须带指纹（与 credits 域同口径）", platform)

		// 显式关闭优先。
		off := &Account{ID: 42, Platform: platform, Type: AccountTypeOAuth,
			Extra: map[string]any{"enable_tls_fingerprint": false}}
		require.Nil(t, svc.resolveOpenAIUpstreamTLSProfile(off),
			"%s 账号显式关闭必须压过平台默认", platform)
	}
}

// TestAccountTestPathMatchesChatPath 账号测试路径与聊天主链路必须同口径。
//
// 缺口形态：测试请求不带指纹但真实请求带 → 「测试通过、真实请求被识别」，
// 是排查上最容易被误导的一类问题。
func TestAccountTestPathMatchesChatPath(t *testing.T) {
	profileService := newTLSDispatchProfileService()
	for _, platform := range platformDefaultTLSPlatforms {
		acc := &Account{ID: 51, Platform: platform, Type: AccountTypeOAuth}

		chat := (&OpenAIGatewayService{cfg: nil, tlsFPProfileService: profileService}).resolveOpenAIUpstreamTLSProfile(acc)
		test := (&AccountTestService{cfg: nil, tlsFPProfileService: profileService}).resolveTLSProfileForAccount(acc)

		require.NotNil(t, chat, "%s 聊天主链路必须带指纹", platform)
		require.NotNil(t, test, "%s 账号测试路径必须与聊天主链路一致地带指纹", platform)

		// 显式关闭时两边也必须一致地都不带。
		off := &Account{ID: 52, Platform: platform, Type: AccountTypeOAuth,
			Extra: map[string]any{"enable_tls_fingerprint": false}}
		require.Nil(t, (&OpenAIGatewayService{cfg: nil, tlsFPProfileService: profileService}).resolveOpenAIUpstreamTLSProfile(off))
		require.Nil(t, (&AccountTestService{cfg: nil, tlsFPProfileService: profileService}).resolveTLSProfileForAccount(off),
			"%s 测试路径也必须尊重显式关闭", platform)
	}
}

// TestAuxiliaryDomainMatchesChatPath 辅助面（traeDo / qoderDo / workbuddyDo）与聊天主链路同口径。
//
// 三家 credits 服务本轮已改为调用 platformDefaultTLSFor。本用例确认它们与聊天域
// 对同一账号、同一配置给出**相同**的「带不带指纹」结论——这正是消除身份矛盾的定义。
func TestAuxiliaryDomainMatchesChatPath(t *testing.T) {
	profileService := newTLSDispatchProfileService()
	chatSvc := &OpenAIGatewayService{cfg: nil, tlsFPProfileService: profileService}

	for _, platform := range platformDefaultTLSPlatforms {
		acc := &Account{ID: 61, Platform: platform, Type: AccountTypeOAuth, Concurrency: 1}
		chatHasProfile := chatSvc.resolveOpenAIUpstreamTLSProfile(acc) != nil

		local := &tlsDispatchUpstreamStub{}
		var doTLS, plainDo int
		switch platform {
		case PlatformWorkbuddy:
			svc := &WorkBuddyCreditsService{httpUpstream: local, cfg: nil}
			svc.SetTLSFingerprintProfileService(profileService)
			_, err := svc.workbuddyDo(tlsDispatchRequest(), "", acc)
			require.NoError(t, err)
		case PlatformTrae:
			svc := &TraeCreditsService{httpUpstream: local, cfg: nil}
			svc.SetTLSFingerprintProfileService(profileService)
			_, err := svc.traeDo(tlsDispatchRequest(), "", acc)
			require.NoError(t, err)
		case PlatformQoder:
			svc := &QoderCreditsService{httpUpstream: local, cfg: nil}
			svc.SetTLSFingerprintProfileService(profileService)
			_, err := svc.qoderDo(tlsDispatchRequest(), "", acc)
			require.NoError(t, err)
		}
		doTLS, plainDo = local.doTLSCalls, local.doCalls

		require.True(t, chatHasProfile, "%s 聊天主链路应带指纹（前置条件）", platform)
		require.Equal(t, 1, doTLS, "%s 辅助面必须走 DoWithTLS", platform)
		require.Zero(t, plainDo, "%s 辅助面不得走普通 Do", platform)
	}
}

// TestOpenAIPluginTransportChatPathIntegration 端到端：doOpenAIUpstream 真的调用 DoWithTLS。
//
// 前面的用例只验证 profile 解析结果，这条验证**接线**——防止有人把
// resolveOpenAIUpstreamTLSProfile 修好但 doOpenAIUpstream 仍然漏用（或反过来）。
func TestOpenAIPluginTransportChatPathIntegration(t *testing.T) {
	profileService := newTLSDispatchProfileService()
	for _, platform := range platformDefaultTLSPlatforms {
		local := &tlsDispatchUpstreamStub{}
		svc := &OpenAIGatewayService{cfg: nil, httpUpstream: local, tlsFPProfileService: profileService}
		req := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/chat", nil)
		acc := &Account{ID: 71, Platform: platform, Type: AccountTypeOAuth}

		_, err := svc.doOpenAIUpstream(req, "", acc)
		require.NoError(t, err)
		require.Equal(t, 1, local.doTLSCalls, "%s 聊天转发出站必须走 DoWithTLS（接线不得断开）", platform)
		require.Zero(t, local.doCalls)
	}
}
