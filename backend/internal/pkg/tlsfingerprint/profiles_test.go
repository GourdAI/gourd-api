//go:build unit

package tlsfingerprint

import (
	"testing"

	utls "github.com/refraction-networking/utls"
)

// Codex 内置默认指纹必须真正携带 rustls 特征：一旦哪天有人把它改回空 Profile
// （等价于 Node.js 默认），这些断言会立刻失败。

// TestCodexRustlsProfileMatchesRustlsShape 校验 rustls 与 Node.js/OpenSSL 的三个决定性差异。
func TestCodexRustlsProfileMatchesRustlsShape(t *testing.T) {
	p := CodexRustlsProfile()

	if p.Name != ProfileNameCodexRustls {
		t.Fatalf("profile name: got %q", p.Name)
	}

	// 1. TLS1.3 顺序必须是 AES-256 优先（rustls），而非 Node.js 的 1301 优先。
	if len(p.CipherSuites) < 3 || p.CipherSuites[0] != 0x1302 || p.CipherSuites[1] != 0x1301 || p.CipherSuites[2] != 0x1303 {
		t.Errorf("tls1.3 cipher order must be 1302,1301,1303 (rustls), got %#x", p.CipherSuites)
	}

	// 2. rustls 只支持 AEAD：不得出现 TLS_RSA_* 非 PFS 套件与 ECDHE-CBC 套件。
	forbidden := map[uint16]string{
		0x009c: "TLS_RSA_WITH_AES_128_GCM_SHA256",
		0x009d: "TLS_RSA_WITH_AES_256_GCM_SHA384",
		0x002f: "TLS_RSA_WITH_AES_128_CBC_SHA",
		0x0035: "TLS_RSA_WITH_AES_256_CBC_SHA",
		0xc009: "TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA",
		0xc013: "TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA",
		0xc00a: "TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA",
		0xc014: "TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA",
	}
	for _, suite := range p.CipherSuites {
		if name, bad := forbidden[suite]; bad {
			t.Errorf("cipher %#x (%s) is not a rustls suite", suite, name)
		}
	}

	// 3. rustls 恒定在套件表尾部追加 SCSV(0x00ff)，这是它与 OpenSSL 最强的判别信号。
	if last := p.CipherSuites[len(p.CipherSuites)-1]; last != 0x00ff {
		t.Errorf("last cipher suite must be TLS_EMPTY_RENEGOTIATION_INFO_SCSV(0x00ff), got %#x", last)
	}

	// rustls 无 GREASE 能力。
	if p.EnableGREASE {
		t.Error("rustls does not implement GREASE; EnableGREASE must be false")
	}

	// 签名算法不得含 sha1(0x0201)，且 ECDSA_P384 必须排在 P256 之前（SUPPORTED_SIG_ALGS 顺序）。
	for _, alg := range p.SignatureAlgorithms {
		if alg == 0x0201 {
			t.Error("rsa_pkcs1_sha1 must not be advertised (rustls omits it)")
		}
	}
	if p.SignatureAlgorithms[0] != 0x0503 {
		t.Errorf("first signature scheme must be ecdsa_secp384r1_sha384, got %#x", p.SignatureAlgorithms[0])
	}
}

// TestCodexRustlsProfileExtensionsExcludeECH 校验扩展表与 rustls 一致：
// 无 encrypted_client_hello(65037)，renegotiation_info 由 SCSV 套件而非 65281 扩展表达。
func TestCodexRustlsProfileExtensionsExcludeECH(t *testing.T) {
	p := CodexRustlsProfile()

	for _, id := range p.Extensions {
		if id == 65037 {
			t.Error("ECH(65037) must not appear: rustls only sends it when ECH is explicitly configured")
		}
		if id == 65281 {
			t.Error("renegotiation_info extension must not appear: rustls expresses it via the SCSV suite")
		}
	}

	// 基线取 rustls 一手串 (43-11-10-13-23-5-0-18-51-45-35)，再插入 16(alpn)：纯 rustls 不设
	// ALPN，但本仓必须声明 http/1.1（见 CodexRustlsProfile 注释）。其余逐位对齐。
	want := []uint16{43, 11, 10, 13, 23, 5, 0, 18, 51, 45, 16, 35}
	if len(p.Extensions) != len(want) {
		t.Fatalf("extension count: got %d, want %d", len(p.Extensions), len(want))
	}
	for id := range want {
		if p.Extensions[id] != want[id] {
			t.Errorf("extension[%d]: got %d, want %d", id, p.Extensions[id], want[id])
		}
	}

	// ALPN 必须在扩展表内，否则 Profile.ALPNProtocols 会被 dialer 静默丢弃（dialer 只按
	// 扩展表逐项构造）。
	if !hasExtensionID(p.Extensions, 16) {
		t.Error("extensions must include 16(alpn), otherwise ALPNProtocols has no effect")
	}
}

// TestCodexRustlsProfileKeepsHTTP11ALPN 钉死 ALPN 只能是 http/1.1。
//
// 真 Codex 会协商 h2，但本仓 TLS 指纹 Transport 用 DialTLSContext 返回 *utls.UConn，
// net/http 无法断言为 *tls.Conn，因此永不启用 HTTP/2。声明 h2 会让服务端切到 h2、
// 客户端仍按 HTTP/1.1 写报文，直接产生 "malformed HTTP response"。
func TestCodexRustlsProfileKeepsHTTP11ALPN(t *testing.T) {
	p := CodexRustlsProfile()
	if len(p.ALPNProtocols) != 1 || p.ALPNProtocols[0] != "http/1.1" {
		t.Fatalf("ALPN must stay [http/1.1] to match the transport's actual HTTP version, got %v", p.ALPNProtocols)
	}
}

// TestCodexRustlsProfileSpecRoundTrip 校验 profile 被 dialer 完整消费：
// 每个扩展 ID 都必须映射到具名扩展，不能退化成空数据 GenericExtension。
func TestCodexRustlsProfileSpecRoundTrip(t *testing.T) {
	p := CodexRustlsProfile()
	spec := buildClientHelloSpecFromProfile(p)

	if len(spec.CipherSuites) != len(p.CipherSuites) {
		t.Fatalf("spec cipher count: got %d, want %d", len(spec.CipherSuites), len(p.CipherSuites))
	}
	if spec.CipherSuites[0] != 0x1302 {
		t.Errorf("spec must preserve profile cipher order, first=%#x", spec.CipherSuites[0])
	}
	if len(spec.Extensions) != len(p.Extensions) {
		t.Fatalf("spec extension count: got %d, want %d", len(spec.Extensions), len(p.Extensions))
	}
	for i, ext := range spec.Extensions {
		if generic, ok := ext.(*utls.GenericExtension); ok {
			t.Errorf("extension[%d] id=%d fell back to GenericExtension (empty payload); dialer needs a named case",
				i, generic.Id)
		}
	}

	// 关键参数必须真的落到扩展里，而不是被默认值覆盖。
	var alpnSeen, keyShareSeen, groupsSeen bool
	for _, ext := range spec.Extensions {
		switch e := ext.(type) {
		case *utls.ALPNExtension:
			alpnSeen = true
			if len(e.AlpnProtocols) != 1 || e.AlpnProtocols[0] != "http/1.1" {
				t.Errorf("spec ALPN: got %v", e.AlpnProtocols)
			}
		case *utls.KeyShareExtension:
			keyShareSeen = true
			if len(e.KeyShares) != 1 || uint16(e.KeyShares[0].Group) != 0x001d {
				t.Errorf("spec key_share: got %v", e.KeyShares)
			}
		case *utls.SupportedCurvesExtension:
			groupsSeen = true
			if len(e.Curves) != 3 || uint16(e.Curves[0]) != 0x001d {
				t.Errorf("spec supported_groups: got %v", e.Curves)
			}
		}
	}
	for name, seen := range map[string]bool{"ALPN": alpnSeen, "KeyShare": keyShareSeen, "SupportedCurves": groupsSeen} {
		if !seen {
			t.Errorf("%s extension missing from spec", name)
		}
	}
}

// TestNodeDefaultProfileKeepsLegacyBehaviour 保证 Claude 侧零行为变更：
// 除 Name 外字段全空，由 dialer 回落到包级 Node.js 24.x 常量。
func TestNodeDefaultProfileKeepsLegacyBehaviour(t *testing.T) {
	p := NodeDefaultProfile()

	if p.Name != ProfileNameNodeDefault {
		t.Fatalf("unexpected name %q", p.Name)
	}
	if len(p.CipherSuites) != 0 || len(p.Curves) != 0 || len(p.PointFormats) != 0 ||
		len(p.SignatureAlgorithms) != 0 || len(p.ALPNProtocols) != 0 || len(p.SupportedVersions) != 0 ||
		len(p.KeyShareGroups) != 0 || len(p.PSKModes) != 0 || len(p.Extensions) != 0 {
		t.Error("Node default profile must leave all fields empty so dialer uses the built-in Node.js 24.x defaults")
	}
	if p.EnableGREASE {
		t.Error("Node default profile must keep GREASE off (OpenSSL has none), same as before the change")
	}

	// 与 nil profile 产出的 spec 必须字节级一致 —— 这是「Anthropic 账号不受本次改动影响」的硬证据。
	fromProfile := buildClientHelloSpecFromProfile(p)
	fromNil := buildClientHelloSpecFromProfile(nil)
	if len(fromProfile.CipherSuites) != len(fromNil.CipherSuites) {
		t.Fatalf("cipher count differs from nil profile: %d vs %d",
			len(fromProfile.CipherSuites), len(fromNil.CipherSuites))
	}
	for i := range fromNil.CipherSuites {
		if fromProfile.CipherSuites[i] != fromNil.CipherSuites[i] {
			t.Errorf("cipher[%d] differs from nil profile", i)
		}
	}
	if len(fromProfile.Extensions) != len(fromNil.Extensions) {
		t.Fatalf("extension count differs from nil profile: %d vs %d",
			len(fromProfile.Extensions), len(fromNil.Extensions))
	}
}

// TestCodexRustlsProfileDiffersFromNodeDefault 防止两套内置指纹被误改成同源。
func TestCodexRustlsProfileDiffersFromNodeDefault(t *testing.T) {
	codex := buildClientHelloSpecFromProfile(CodexRustlsProfile())
	node := buildClientHelloSpecFromProfile(NodeDefaultProfile())

	if codex.CipherSuites[0] == node.CipherSuites[0] {
		t.Error("codex and node profiles must not share the same first cipher suite")
	}
	if len(codex.CipherSuites) == len(node.CipherSuites) {
		t.Error("codex (10 suites) and node (17 suites) must differ in suite count")
	}
}
