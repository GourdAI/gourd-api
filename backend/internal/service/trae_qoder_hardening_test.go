//go:build unit

package service

// trae_qoder_hardening_test.go Trae / Qoder 防封对齐的语义单测。
//
// 本次把两家从「WorkBuddy 已改造、Trae/Qoder 仍是旧模型」补齐，需要钉死四条：
//  1. 两家出站默认带 TLS 指纹（未表态 → DoWithTLS），与 WorkBuddy 完全同口径；
//  2. 账号显式 enable_tls_fingerprint=false 仍优先（平台默认不得吞掉管理员意图）；
//  3. 平台开关 tls_fingerprint_enabled=false 时回到纯 opt-in（零行为变更的阀）；
//  4. Qoder 活动域（签到/余额/领取）必须显式设 UA，不得漏出 net/http 自动补的
//     Go 运行时默认值——那是自动化特征，且与同账号 chat 域的显式值不一致。
//
// 反向护栏要求：把 traeDo/qoderDo 改回 doUpstreamWithOptionalTLSConcurrency、
// 或删掉 applyQoderCampaignHeaders 里的 UA 设置，相应用例必须 FAIL。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

// traeQoderTLSDoCalls 用 tlsDispatchUpstreamStub 统计一次出站走了哪条路径。
//
// 参数说明：
//   - platform 决定构造哪个平台的服务；
//   - cfg 用于控制平台开关；nil 时服务取「平台默认开启」语义；
//   - extra 是账号 Extra，用于表达「未表态 / 显式开 / 显式关」。
func traeQoderTLSDoCalls(t *testing.T, platform string, cfg *config.Config, extra map[string]any) (doTLS int, plainDo int) {
	t.Helper()
	local := &tlsDispatchUpstreamStub{}
	profileService := newTLSDispatchProfileService()
	account := &Account{ID: 9, Platform: platform, Type: AccountTypeOAuth, Extra: extra}

	switch platform {
	case PlatformTrae:
		svc := &TraeCreditsService{httpUpstream: local, cfg: cfg}
		svc.SetTLSFingerprintProfileService(profileService)
		_, err := svc.traeDo(tlsDispatchRequest(), "", account)
		require.NoError(t, err)
	case PlatformQoder:
		svc := &QoderCreditsService{httpUpstream: local, cfg: cfg}
		svc.SetTLSFingerprintProfileService(profileService)
		_, err := svc.qoderDo(tlsDispatchRequest(), "", account)
		require.NoError(t, err)
	default:
		t.Fatalf("unsupported platform %q", platform)
	}
	return local.doTLSCalls, local.doCalls
}

// TestTraeQoderPlatformDefaultTLSStates 两家在「平台默认开启」下的三态语义。
//
// cfg 为 nil 时走服务内建的 platformDefault=true（与 WorkBuddy 的
// workbuddyDo / workbuddyPlatformDefaultTLS 的 cfg==nil 口径一致）。
func TestTraeQoderPlatformDefaultTLSStates(t *testing.T) {
	cases := []struct {
		name        string
		platform    string
		extra       map[string]any
		wantDoTLS   int
		wantPlainDo int
	}{
		{"Trae 未表态 → 平台默认开启", PlatformTrae, nil, 1, 0},
		{"Trae 显式 true", PlatformTrae, map[string]any{"enable_tls_fingerprint": true}, 1, 0},
		{"Trae 显式 false 优先", PlatformTrae, map[string]any{"enable_tls_fingerprint": false}, 0, 1},
		{"Trae 非 bool 视为未表态", PlatformTrae, map[string]any{"enable_tls_fingerprint": "true"}, 1, 0},
		{"Qoder 未表态 → 平台默认开启", PlatformQoder, nil, 1, 0},
		{"Qoder 显式 true", PlatformQoder, map[string]any{"enable_tls_fingerprint": true}, 1, 0},
		{"Qoder 显式 false 优先", PlatformQoder, map[string]any{"enable_tls_fingerprint": false}, 0, 1},
		{"Qoder 非 bool 视为未表态", PlatformQoder, map[string]any{"enable_tls_fingerprint": "true"}, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doTLS, plainDo := traeQoderTLSDoCalls(t, tc.platform, nil, tc.extra)
			require.Equal(t, tc.wantDoTLS, doTLS, "DoWithTLS 调用次数")
			require.Equal(t, tc.wantPlainDo, plainDo, "普通 Do 调用次数")
		})
	}
}

// TestTraeQoderPlatformDefaultTLSCanBeDisabled 平台开关关闭时回到纯 opt-in。
//
// 这是「零行为变更」的阀：管理员显式关掉平台默认后，未表态账号必须走普通 Do。
func TestTraeQoderPlatformDefaultTLSCanBeDisabled(t *testing.T) {
	off := &config.Config{}
	off.Gateway.Trae.TLSFingerprintEnabled = false
	off.Gateway.Qoder.TLSFingerprintEnabled = false

	doTLS, plainDo := traeQoderTLSDoCalls(t, PlatformTrae, off, nil)
	require.Zero(t, doTLS, "Trae 平台开关关闭后未表态账号不得走 DoWithTLS")
	require.Equal(t, 1, plainDo)

	doTLS, plainDo = traeQoderTLSDoCalls(t, PlatformQoder, off, nil)
	require.Zero(t, doTLS, "Qoder 平台开关关闭后未表态账号不得走 DoWithTLS")
	require.Equal(t, 1, plainDo)

	// 开关关闭时，显式启用的账号仍应走 DoWithTLS（opt-in 语义未被破坏）。
	on := map[string]any{"enable_tls_fingerprint": true}
	doTLS, plainDo = traeQoderTLSDoCalls(t, PlatformTrae, off, on)
	require.Equal(t, 1, doTLS, "显式启用不受平台开关影响")
	require.Zero(t, plainDo)

	doTLS, plainDo = traeQoderTLSDoCalls(t, PlatformQoder, off, on)
	require.Equal(t, 1, doTLS, "显式启用不受平台开关影响")
	require.Zero(t, plainDo)
}

// TestTraeQoderPlatformDefaultTLSOnByDefault 平台开关默认开启（配置缺省值）。
//
// 与 config.setDefaults 里 gateway.{trae,qoder}.tls_fingerprint_enabled=true 对应；
// 这里锁的是「零值 Config 之外，显式 true 也必须生效」，防止有人把默认值写成 false
// 却忘了改服务侧代码。
func TestTraeQoderPlatformDefaultTLSOnByDefault(t *testing.T) {
	on := &config.Config{}
	on.Gateway.Trae.TLSFingerprintEnabled = true
	on.Gateway.Qoder.TLSFingerprintEnabled = true

	doTLS, plainDo := traeQoderTLSDoCalls(t, PlatformTrae, on, nil)
	require.Equal(t, 1, doTLS)
	require.Zero(t, plainDo)

	doTLS, plainDo = traeQoderTLSDoCalls(t, PlatformQoder, on, nil)
	require.Equal(t, 1, doTLS)
	require.Zero(t, plainDo)
}

// TestQoderCampaignHeadersSetExplicitUserAgent Qoder 活动域必须显式设 UA。
//
// 缺陷形态：applyQoderCampaignHeaders 原先不设 User-Agent，net/http 会按协议自动
// 补 Go-http-client/1.1（HTTP/1.1）或 2.0（HTTP/2），与同账号 chat 域显式声明的
// 2.0 不一致；「漏 Go 运行时默认 UA」本身就是自动化特征。
//
// 本次按用户确认：Go-http-client/2.0 是上游路由约定值，故各域统一为该常量。
func TestQoderCampaignHeadersSetExplicitUserAgent(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://qoder.example/sash/api/v1/me/campaigns", nil)
	applyQoderCampaignHeaders(req, "tok-1")

	require.Equal(t, qoderUpstreamUserAgent, req.Header.Get("user-agent"),
		"活动域必须显式设 UA，且与 chat 域同源常量")
	require.Equal(t, "Go-http-client/2.0", qoderUpstreamUserAgent,
		"该值是上游路由约定，不得随意改动")
	require.NotEmpty(t, req.Header.Get("cosy-clienttype"), "活动域身份头不得缺失")
	require.Equal(t, "application/json", req.Header.Get("content-type"))
}

// TestQoderChatHeadersAndCampaignShareUserAgent Qoder 各域 UA 必须同源。
//
// 三处出站（chat / jobToken / openapi 活动）此前是字面量重复，容易出现
// 「改了 chat 忘了活动」。现统一引用 qoderUpstreamUserAgent 常量，本用例锁住：
// chat 域实际发出的 UA 与常量一致（常量被改则这里立刻 FAIL）。
func TestQoderChatHeadersAndCampaignShareUserAgent(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://qoder.example/algo/api/v2/service/pro/sse/agent_chat_generation", nil)
	creds := QoderCredentials{UID: "u-1", AccessToken: "dt-tok"}
	meta := &QoderBodyMeta{RequestID: "req-1", ModelKey: "auto"}
	require.NoError(t, applyQoderChatHeaders(req, creds, "dt-tok", meta, []byte(`{}`),
		"https://qoder.example/algo/api/v2/service/pro/sse/agent_chat_generation", "text/event-stream"))

	require.Equal(t, qoderUpstreamUserAgent, req.Header.Get("user-agent"),
		"chat 域与活动域必须引用同一常量")
}

// TestTraeBillingAndChatHeadersSetUserAgent Trae 各域 UA 均已显式设置（防回退）。
//
// Trae 的 UA 是两套（聊天域 Trae/<ver>、UG 域 VSCode <ver> (TRAE...)），
// 都不是 Go 运行时默认值；这里锁住「既非空也不是 Go 默认值」。
func TestTraeBillingAndChatHeadersSetUserAgent(t *testing.T) {
	creds := TraeCredentials{Realm: "cn", AccessToken: "at-1", IDEVersion: "1.2.3"}

	billing := httptest.NewRequest(http.MethodPost, "https://trae.example/trae/api/v2/ug/checkin_credits/status", nil)
	applyTraeBillingHeaders(billing, creds, "1234567890123456")
	require.NotEmpty(t, billing.Header.Get("User-Agent"))
	require.NotContains(t, billing.Header.Get("User-Agent"), "Go-http-client",
		"UG 域不得漏出 Go 运行时默认 UA")
	require.Contains(t, billing.Header.Get("User-Agent"), "VSCode")

	chat := httptest.NewRequest(http.MethodPost, "https://trae.example/api/v1/chat", nil)
	applyTraeChatHeaders(chat, creds, &Account{ID: 1, Platform: PlatformTrae})
	require.NotEmpty(t, chat.Header.Get("User-Agent"))
	require.NotContains(t, chat.Header.Get("User-Agent"), "Go-http-client",
		"聊天域不得漏出 Go 运行时默认 UA")
	require.Contains(t, chat.Header.Get("User-Agent"), "Trae/")
}

// TestTraeQoderSkipPredicates 两家「当日已完成」谓词的边界。
//
// 与 WorkBuddy 的 workbuddyCheckinAlreadyDoneToday 同口径：
//   - ok / already → 跳过；
//   - fail → 必须重试（兜底窗口的本意）；
//   - 跨日/跨轮 → 必须重跑；
//   - 快照缺失 → 不跳过（宁可多试，不可漏领）。
func TestTraeQoderSkipPredicates(t *testing.T) {
	traeToday := timezone.Now().Format("2006-01-02")
	qoderToday := qoderCampaignRoundDate(time.Now())

	cases := []struct {
		name    string
		account *Account
		want    bool
		check   func(*Account) bool
	}{
		{"Trae nil", nil, false, traeCheckinAlreadyDoneToday},
		{"Trae 无 Extra", &Account{}, false, traeCheckinAlreadyDoneToday},
		{"Trae 当日 ok", &Account{Extra: map[string]any{traeCheckinExtraKey: map[string]any{"date": traeToday, "status": TraeCheckinStatusOK}}}, true, traeCheckinAlreadyDoneToday},
		{"Trae 当日 already", &Account{Extra: map[string]any{traeCheckinExtraKey: map[string]any{"date": traeToday, "status": TraeCheckinStatusAlready}}}, true, traeCheckinAlreadyDoneToday},
		{"Trae 当日 fail 必须重试", &Account{Extra: map[string]any{traeCheckinExtraKey: map[string]any{"date": traeToday, "status": TraeCheckinStatusFail}}}, false, traeCheckinAlreadyDoneToday},
		{"Trae 跨日必须重签", &Account{Extra: map[string]any{traeCheckinExtraKey: map[string]any{"date": "2000-01-01", "status": TraeCheckinStatusOK}}}, false, traeCheckinAlreadyDoneToday},

		{"Qoder nil", nil, false, qoderCheckinAlreadyDoneToday},
		{"Qoder 无 Extra", &Account{}, false, qoderCheckinAlreadyDoneToday},
		{"Qoder 本轮 ok", &Account{Extra: map[string]any{qoderCheckinExtraKey: map[string]any{"round": qoderToday, "status": QoderCheckinStatusOK}}}, true, qoderCheckinAlreadyDoneToday},
		{"Qoder 本轮 already", &Account{Extra: map[string]any{qoderCheckinExtraKey: map[string]any{"round": qoderToday, "status": QoderCheckinStatusAlready}}}, true, qoderCheckinAlreadyDoneToday},
		{"Qoder 本轮 fail 必须重试", &Account{Extra: map[string]any{qoderCheckinExtraKey: map[string]any{"round": qoderToday, "status": QoderCheckinStatusFail}}}, false, qoderCheckinAlreadyDoneToday},
		{"Qoder 跨轮必须重领", &Account{Extra: map[string]any{qoderCheckinExtraKey: map[string]any{"round": "2000-01-01", "status": QoderCheckinStatusOK}}}, false, qoderCheckinAlreadyDoneToday},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.check(tc.account))
		})
	}
}

// TestSpreadRunnerSharedAcrossPlatforms 公共原语对三家平台候选都按同一节奏推进。
//
// 这条用例防的是「某个平台偷偷绕过 runner 直连旧循环」：若 Trae/Qoder 的 runOnce
// 不再走 runner，本次的反向护栏用例会 FAIL；本用例额外确认 runner 本身对任何
// 账号列表（不限平台）语义一致，避免将来给 runner 加平台分支时出现差异节奏。
func TestSpreadRunnerSharedAcrossPlatforms(t *testing.T) {
	spreadZeroWaits(t)
	var executed []string
	accounts := []*Account{
		{ID: 1, Platform: PlatformTrae},
		{ID: 2, Platform: PlatformQoder},
		{ID: 3, Platform: PlatformWorkbuddy},
	}
	runner := spreadRunner{label: "checkin"}
	stats := runner.run(context.Background(), nil, accounts, nil,
		func(_ context.Context, a *Account) spreadRunOutcome {
			executed = append(executed, a.Platform)
			return spreadRunOK
		})
	require.Equal(t, []string{PlatformTrae, PlatformQoder, PlatformWorkbuddy}, executed,
		"runner 必须按传入顺序逐个推进，不得做任何平台分支")
	require.Equal(t, 3, stats.Attempted)
	require.Equal(t, 3, stats.OK)
}
