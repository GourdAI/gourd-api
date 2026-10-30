//go:build unit

// 本文件锁住「带 TLS 指纹的出站必须能跑 HTTP/2」这条不变量。
//
// 背景（2026-10-25 排查「WorkBuddy 接口调用比之前慢很多」）：
//
//	e37fae9 把 WorkBuddy/Trae/Qoder 三平台的 TLS 指纹改为默认开启后，出站从
//	「Go 标准库 HTTP/2」退化成「utls HTTP/1.1」。单看握手并不慢，但账号隔离下
//	resolvePoolSettings 把 maxConnsPerHost 夹成 account.Concurrency（线上常见 1），
//	h1 不能多路复用 ⇒ 同账号并发请求全部串行排队，且每个排队请求再付一次完整握手。
//	真实上游 copilot.tencent.com 实测 12 并发：改造前 105ms → 改造后 779ms（7.4×）。
//
// 修复：Profile 的 ALPN 含 h2 时，传输层改用 http2.Transport 并复用同一个指纹拨号函数
// （buildUpstreamRoundTripperWithTLSFingerprint）。实测回到 104~125ms，且仍协商 HTTP/2.0。
//
// 反向护栏要求：把 buildUpstreamRoundTripperWithTLSFingerprint 里的 h2 分派改成
// `false &&`，TestFingerprintHTTP2Dispatch 必须 FAIL；把三平台分支改回
// NodeDefaultProfile，TestThreePlatformDefaultsAdvertiseH2 必须 FAIL。
package repository

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// TestFingerprintHTTP2Dispatch 锁住分派规则：「ALPN 含 h2」且「上游 profile 为 OpenAI」
// 才切 http2.Transport。两个条件各自职责不同，必须分别反向验证。
func TestFingerprintHTTP2Dispatch(t *testing.T) {
	settings := defaultPoolSettings(nil)
	chat := service.HTTPUpstreamProfileOpenAI
	aux := service.HTTPUpstreamProfileDefault

	t.Run("聊天主链路 + ALPN 含 h2 时返回 http2.Transport", func(t *testing.T) {
		tr, err := buildUpstreamRoundTripperWithTLSFingerprint(settings, nil, tlsfingerprint.NodeOpenSSLH2Profile(), chat)
		require.NoError(t, err)
		_, isH2 := tr.(*http2.Transport)
		require.True(t, isH2, "声明 h2 的 Profile 必须拿 http2.Transport，实得 %T", tr)
	})

	t.Run("非 OpenAI 画像保持 http.Transport", func(t *testing.T) {
		// http2.Transport 没有 ResponseHeaderTimeout 字段。本分支保护的是**其它平台**
		// （Claude/OpenAI/Grok 等走 Default 或 Grok 画像的出站）：它们的响应头超时
		// 语义必须零变更。
		// 【注意，不要误以】三平台（WorkBuddy/Trae/Qoder）的签到/余额/任务类辅助面
		// **不走本分支** —— 它们也注入 HTTPUpstreamProfileOpenAI（见
		// workbuddy_tasks_service.go:589 等），因此同样被切到 h2；那些请求的超时由
		// 各自的 context.WithTimeout 兜底。
		tr, err := buildUpstreamRoundTripperWithTLSFingerprint(settings, nil, tlsfingerprint.NodeOpenSSLH2Profile(), aux)
		require.NoError(t, err)
		_, isH1 := tr.(*http.Transport)
		require.True(t, isH1, "非 OpenAI profile 不得被切到 http2.Transport（会丢响应头超时），实得 %T", tr)
	})

	t.Run("ALPN 不含 h2 时仍是 http.Transport", func(t *testing.T) {
		// Claude 形状（ALPN 空 ⇒ 回落 http/1.1）必须零行为变更
		tr, err := buildUpstreamRoundTripperWithTLSFingerprint(settings, nil, tlsfingerprint.NodeDefaultProfile(), chat)
		require.NoError(t, err)
		_, isH1 := tr.(*http.Transport)
		require.True(t, isH1, "未声明 h2 的 Profile 必须保持 *http.Transport，实得 %T", tr)
	})

	t.Run("Trae 的 h1 形状必须拿 http.Transport（不得进 http2.Transport）", func(t *testing.T) {
		// 【为何单独一句（2026-10-27 线上全量报错）】Trae CN 聊天域
		// trae-api-cn.mchost.guru 实测只协商 HTTP/1.1，而 http2.Transport 不会按
		// ALPN 回落 h1 ⇒ 一旦被分派到 h2 就是全量硬失败（报错形如
		// "frame too large, note that the frame header looked like an HTTP/1.1 header"）。
		// 这条断言把“形状”与“传输层选型”钉在一起，防止将来有人把 Trae 改回 h2 形状
		// 而只跑 service 层用例没发现。
		tr, err := buildUpstreamRoundTripperWithTLSFingerprint(settings, nil, tlsfingerprint.NodeOpenSSLH1Profile(), chat)
		require.NoError(t, err)
		_, isH1 := tr.(*http.Transport)
		require.True(t, isH1, "Trae 的 h1 形状必须走 http.Transport，实得 %T（http2 不会回落 h1）", tr)
	})

	t.Run("OpenAI rustls 形状未被误升 h2", func(t *testing.T) {
		// CodexRustlsProfile 的 ALPN 是 ["http/1.1"]（有意偏离，见 profiles.go 说明）
		tr, err := buildUpstreamRoundTripperWithTLSFingerprint(settings, nil, tlsfingerprint.CodexRustlsProfile(), chat)
		require.NoError(t, err)
		_, isH1 := tr.(*http.Transport)
		require.True(t, isH1, "rustls 形状声明 h1，必须仍是 *http.Transport，实得 %T", tr)
	})

	t.Run("socks5 代理保持原 http.Transport 路径", func(t *testing.T) {
		// 本次修复刻意不触碰代理隧道语义（socks5/https 仍走 h1 指纹路径）
		proxyURL, err := url.Parse("socks5h://127.0.0.1:1080")
		require.NoError(t, err)
		tr, err := buildUpstreamRoundTripperWithTLSFingerprint(settings, proxyURL, tlsfingerprint.NodeOpenSSLH2Profile(), chat)
		require.NoError(t, err)
		_, isH1 := tr.(*http.Transport)
		require.True(t, isH1, "socks5 代理不得被切到 http2.Transport，实得 %T", tr)
	})

	t.Run("http CONNECT 代理仍可走 h2", func(t *testing.T) {
		proxyURL, err := url.Parse("http://127.0.0.1:3128")
		require.NoError(t, err)
		tr, err := buildUpstreamRoundTripperWithTLSFingerprint(settings, proxyURL, tlsfingerprint.NodeOpenSSLH2Profile(), chat)
		require.NoError(t, err)
		_, isH2 := tr.(*http2.Transport)
		require.True(t, isH2, "http CONNECT 代理下指纹拨号已自带隧道，应保留多路复用，实得 %T", tr)
	})
}

// TestFingerprintNegotiatesHTTP2IsALPNSourced 判据必须只看 Profile.ALPNProtocols，
// 否则「ALPN 声称 h2 但传输层只会 h1」这种 chimera 会重新出现。
func TestFingerprintNegotiatesHTTP2IsALPNSourced(t *testing.T) {
	require.False(t, fingerprintNegotiatesHTTP2(nil), "nil profile 不得启用 h2")
	require.False(t, fingerprintNegotiatesHTTP2(&tlsfingerprint.Profile{Name: "x"}), "ALPN 空 ⇒ 回落 h1")
	require.False(t, fingerprintNegotiatesHTTP2(&tlsfingerprint.Profile{ALPNProtocols: []string{"http/1.1"}}))
	require.True(t, fingerprintNegotiatesHTTP2(&tlsfingerprint.Profile{ALPNProtocols: []string{"h2", "http/1.1"}}))
	require.True(t, fingerprintNegotiatesHTTP2(&tlsfingerprint.Profile{ALPNProtocols: []string{"H2"}}),
		"ALPN 比较必须大小写不敏感（上游/模板可能写 H2）")
	require.True(t, fingerprintNegotiatesHTTP2(&tlsfingerprint.Profile{ALPNProtocols: []string{" h2 "}}),
		"ALPN 比较必须容忍空白")
}

// TestNodeOpenSSLH2ProfileKeepsNodeShape 钉住「只改 ALPN、不改握手形状」：
// ALPN 不进 JA3 串，因此除协议段外的指纹必须与 NodeDefaultProfile 字节级一致。
// 若有人顺手补了 CipherSuites/Extensions，本用例会当场抓住 —— 那会让三平台账号的
// JA3 突然变化，属于防封侧的高风险改动，必须显式评审。
func TestNodeOpenSSLH2ProfileKeepsNodeShape(t *testing.T) {
	plain := tlsfingerprint.NodeDefaultProfile()
	h2 := tlsfingerprint.NodeOpenSSLH2Profile()

	require.Empty(t, h2.CipherSuites)
	require.Empty(t, h2.Curves)
	require.Empty(t, h2.SignatureAlgorithms)
	require.Empty(t, h2.SupportedVersions)
	require.Empty(t, h2.KeyShareGroups)
	require.Empty(t, h2.PSKModes)
	require.Empty(t, h2.Extensions)
	require.False(t, h2.EnableGREASE)
	require.Equal(t, plain.EnableGREASE, h2.EnableGREASE)

	// CacheKey 必须不同：否则两种形状会在客户端缓存里互相命中（chimera）。
	require.NotEqual(t, plain.CacheKey(), h2.CacheKey(),
		"ALPN 差异必须体现在 CacheKey 上，否则换 profile 后缓存仍命中旧 Transport")
}
