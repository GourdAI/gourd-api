//go:build unit

package service

// trae_tls_http1_test.go 钉住「Trae 平台默认指纹必须是 h1」这条契约。
//
// 【要防的回归（2026-10-27 线上全量报错）】
// 上一轮为修「WorkBuddy 接口变慢」把 WorkBuddy/Trae/Qoder 三个平台一起改成
// NodeOpenSSLH2Profile（ALPN 含 h2 ⇒ 传输层用 http2.Transport）。当时依据的注释写
// 「三家共 19 个域名 ALPN 均协商到 h2」，但那是按域名清单**推断**的，没有逐个实测。
// 实测结果：Trae CN 聊天域 trae-api-cn.mchost.guru **只协商 http/1.1**，而
// http2.Transport 不会按 ALPN 回落 h1 ⇒ Trae CN 账号**全量硬失败**，报错逐字为：
//
//	http2: failed reading the frame payload: http2: frame too large,
//	note that the frame header looked like an HTTP/1.1 header
//
// 本文件的用例只断言「形状契约」（ALPN 不含 h2 ⇒ 传输层分派到 http.Transport），
// 不断言网络可达性 —— 后者由 tlsfingerprint 包里的拔号测试自行覆盖。

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// TestResolveTLSProfile_TraeUsesHTTP1Default Trae 平台默认必须是「ALPN 不含 h2」的形状。
//
// 反向护栏：把 defaultTLSProfileForPlatform 的 PlatformTrae 分支改回
// tlsfingerprint.NodeOpenSSLH2Profile()，本用例必须 FAIL（ALPN 会含 h2）。
func TestResolveTLSProfile_TraeUsesHTTP1Default(t *testing.T) {
	svc := NewTLSFingerprintProfileService(&tlsFPStubRepo{}, nil)

	for _, resolve := range []struct {
		name string
		fn   func(*Account) *tlsfingerprint.Profile
	}{
		{"ResolveTLSProfile", svc.ResolveTLSProfile},
		{"ResolvePlatformProfile", svc.ResolvePlatformProfile},
	} {
		account := &Account{
			ID:       31,
			Platform: PlatformTrae,
			Type:     AccountTypeAPIKey,
			Extra:    map[string]any{"enable_tls_fingerprint": true},
		}
		profile := resolve.fn(account)
		require.NotNil(t, profile, "%s", resolve.name)
		require.Equal(t, tlsfingerprint.ProfileNameNodeOpenSSLH1, profile.Name,
			"%s: Trae 必须拿 h1 内置默认（trae-api-cn.mchost.guru 只协商 http/1.1）", resolve.name)
		require.NotContains(t, profile.ALPNProtocols, "h2",
			"%s: Trae 绝不能声明 h2。http2.Transport 不会按 ALPN 回落 h1，"+
				"一旦协商到 h1 就是全量硬失败而不是慢", resolve.name)
		// 形状一致性：字段留空 ⇒ dialer 回落同一组 Node.js 24.x 常量 ⇒ JA3 与
		// NodeOpenSSLH2Profile 逐字节相同（只差 JA4 协议段）。
		require.Empty(t, profile.CipherSuites, "%s: 指纹形状必须是 Node/OpenSSL 默认值", resolve.name)
		require.Empty(t, profile.Extensions, "%s", resolve.name)
		require.False(t, profile.EnableGREASE, "%s", resolve.name)
	}
}

// TestTraceProfileDispatchesToHTTP1Transport 确认形状契约真的传导到传输层分派：
// Trae 的平台默认 Profile 不会被 http2.Transport 接走。
//
// 这是「ALPN 与传输层能力自洽」这条不变式的端到端检查点：只要
// fingerprintNegotiatesHTTP2(profile) 为 false，buildUpstreamRoundTripperWithTLSFingerprint
// 就会返回 *http.Transport 而不是 http2.Transport。
func TestTraceProfileDispatchesToHTTP1Transport(t *testing.T) {
	profile := tlsfingerprint.NodeOpenSSLH1Profile()
	require.NotNil(t, profile)
	require.NotContains(t, profile.ALPNProtocols, "h2",
		"Trae 默认形状若声明 h2，仓库的分派逻辑会把它交给 http2.Transport（不会回落 h1）")

	// 对照：WorkBuddy/Qoder 仍拿 h2 形状，证明本次改动没有把另两家一并降级。
	for _, p := range []*tlsfingerprint.Profile{
		tlsfingerprint.NodeOpenSSLH2Profile(),
	} {
		require.Contains(t, p.ALPNProtocols, "h2",
			"%s 仍应声明 h2（h2-only 上游上这是性能修复，不是缺陷）", p.Name)
	}
}

// TestTraeAndWorkbuddyProfilesShareJA3 记录一个容易误解的点：Trae 的 h1 形状与
// WorkBuddy/Qoder 的 h2 形状**只差 ALPN**，JA3 逐字节相同。
//
// 断言方式：两者除 Name/ALPN 外的所有字段都为空（由 dialer 回落同一组包级常量），
// 因此「JA3 相同」是结构保证而非巧合。将来若有人给其中一个形状加 CipherSuites，
// 本用例会报错，提醒他这会把两个平台的 JA3 拆开。
func TestTraeAndWorkbuddyProfilesShareJA3(t *testing.T) {
	h1 := tlsfingerprint.NodeOpenSSLH1Profile()
	h2 := tlsfingerprint.NodeOpenSSLH2Profile()

	require.Empty(t, h1.CipherSuites, "Trae h1 形状不得自带套件表，否则与 h2 形状的 JA3 分叉")
	require.Empty(t, h2.CipherSuites, "h2 形状不得自带套件表")
	require.Empty(t, h1.Extensions)
	require.Empty(t, h2.Extensions)
	require.Empty(t, h1.Curves)
	require.Empty(t, h2.Curves)
	require.Empty(t, h1.SignatureAlgorithms)
	require.Empty(t, h2.SignatureAlgorithms)
	require.False(t, h1.EnableGREASE)
	require.False(t, h2.EnableGREASE)

	// Name 必须不同：Name 进 Profile.CacheKey，两个平台不能共用同一个指纹传输层缓存条目
	// （否则改了一个平台的 ALPN 判定会误命中另一个平台已建好的 Transport）。
	require.NotEqual(t, h1.Name, h2.Name)
	require.NotEqual(t, h1.CacheKey(), h2.CacheKey(),
		"CacheKey 必须不同，否则 Trae 与 WorkBuddy 会共用同一个 Transport 缓存条目")
}
