package tlsfingerprint

// 内置平台默认指纹。
//
// WHY：此前 ResolveTLSProfile 在账号启用 TLS 指纹但未绑定模板时，对所有平台一律回落到
// 包内默认的 Node.js 24.x（Claude Code 时代）参数，导致 OpenAI/Codex 账号也交出 Claude
// Code 的 ClientHello。传输层指纹与 HTTP 层声明的客户端身份（originator: codex_cli_rs）
// 互相矛盾，正是 JA4 检测里最典型的「chimera」命中面，比不伪装更糟。
//
// 因此内置一张「平台 → 默认指纹」表：未绑定模板时按 account.Platform 选择与官方客户端
// 同源的形状。显式绑定的模板（tls_fingerprint_profile_id > 0）仍然优先，本表只兜底。
const (
	// ProfileNameNodeDefault 是 Anthropic/Claude 平台的内置默认名（= 包级 Node.js 24.x 参数）。
	// 保持与历史字符串一致：日志与账号测试输出以此名标识指纹，不可随意改名。
	ProfileNameNodeDefault = "Built-in Default (Node.js 24.x)"
	// ProfileNameCodexRustls 是 OpenAI/Codex 平台的内置默认名，对应 rustls 握手形状。
	ProfileNameCodexRustls = "Built-in Default (Codex CLI/rustls)"
)

// CodexRustlsProfile 返回贴近 OpenAI Codex CLI 真实握手特征的 Profile。
//
// 依据（一手来源，非凭记忆）：
//   - Codex CLI 是 Rust 原生二进制，HTTP 传输集中在 codex-http-client（reqwest），
//     TLS 后端显式锁定 rustls（codex-rs/Cargo.toml 的 rustls 0.23 + rustls-tls feature，
//     PR #20676 强制 use_rustls_tls()、PR #27706 切换 aws_lc_rs provider）。
//   - rustls 的 ClientHello 形状取自其源码常量与 ja3_rustls 文档给出的实测串：
//     JA3 = 771,4866-4865-4867-49196-49195-52393-49200-49199-52392-255,
//     43-11-10-13-23-5-0-18-51-45-35,29-23-24,0
//
// 与 Node.js/OpenSSL 默认值的本质差异（每一条都是风控可见信号）：
//  1. TLS1.3 套件顺序是 1302,1301,1303（rustls 以 AES-256 优先），Node 是 1301,1302,1303；
//  2. 完全没有 TLS_RSA_* 非 PFS 套件与 ECDHE-CBC 套件（rustls 只支持 AEAD）；
//  3. 恒追加 TLS_EMPTY_RENEGOTIATION_INFO_SCSV(0x00ff) 到套件尾部——这是 rustls 的标志，
//     而 Node/OpenSSL 用 renegotiation_info 扩展(0xff01) 表达同一语义；
//  4. 扩展表里没有 encrypted_client_hello(65037)：rustls 仅在显式配置 ECH 时才发；
//  5. 签名算法无 sha1(0x0201)，且 ECDSA_P384 优先于 P256（rustls SUPPORTED_SIG_ALGS.mapping
//     的顺序即为对外声明的优先级）；
//  6. rustls 无 GREASE 能力，EnableGREASE 必须为 false。
//
// 两处有意偏离，均为工程约束，不要当作 bug「修正」：
//   - ALPN 固定 ["http/1.1"]：真 Codex（reqwest）会协商 h2，但本仓 TLS 指纹 Transport 通过
//     DialTLSContext 返回 *utls.UConn，net/http 无法将其断言为 *tls.Conn，因此永不启用
//     HTTP/2（见 http_upstream.go 的 ForceAttemptHTTP2: false）。若在此声明 h2，服务端会切到
//     h2 而客户端仍按 HTTP/1.1 写报文，直接产出 "malformed HTTP response"（utls issue #16、
//     golang/go issue #39302 就是这个故障模式）。宁缺不伪：ALPN 必须与实际 HTTP 版本自洽。
//   - key_share 仅 X25519：rustls 开 PQ feature 时会带 X25519MLKEM768，此处保守取必成功的单组，
//     避免组不被支持导致握手失败。
func CodexRustlsProfile() *Profile {
	return &Profile{
		Name: ProfileNameCodexRustls,
		// 1.3 在前且 AES-256 优先，随后是 rustls 支持的 TLS1.2 AEAD 套件，尾部 SCSV(0x00ff)。
		CipherSuites: []uint16{
			0x1302, // TLS_AES_256_GCM_SHA384
			0x1301, // TLS_AES_128_GCM_SHA256
			0x1303, // TLS_CHACHA20_POLY1305_SHA256
			0xc02c, // TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384
			0xc02b, // TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256
			0xcca9, // TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256
			0xc030, // TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384
			0xc02f, // TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
			0xcca8, // TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256
			0x00ff, // TLS_EMPTY_RENEGOTIATION_INFO_SCSV（rustls 恒追加）
		},
		Curves:       []uint16{0x001d, 0x0017, 0x0018}, // X25519, P-256, P-384
		PointFormats: []uint16{0},                      // uncompressed
		SignatureAlgorithms: []uint16{
			0x0503, // ecdsa_secp384r1_sha384
			0x0403, // ecdsa_secp256r1_sha256
			0x0807, // ed25519
			0x0806, // rsa_pss_rsae_sha512
			0x0805, // rsa_pss_rsae_sha384
			0x0804, // rsa_pss_rsae_sha256
			0x0601, // rsa_pkcs1_sha512
			0x0501, // rsa_pkcs1_sha384
			0x0401, // rsa_pkcs1_sha256
		},
		ALPNProtocols:     []string{"http/1.1"}, // 见上方「有意偏离」说明
		SupportedVersions: []uint16{0x0304, 0x0303},
		KeyShareGroups:    []uint16{0x001d},
		PSKModes:          []uint16{1}, // psk_dhe_ke
		// rustls 扩展顺序（非 GREASE）：supported_versions, ec_point_formats, supported_groups,
		// signature_algorithms, extended_master_secret, status_request, server_name,
		// signed_certificate_timestamp, key_share, psk_key_exchange_modes, session_ticket。
		// 无 65037(ECH)、无 65281(renegotiation_info，改由 SCSV 套件表达)。
		//
		// 关于 16(alpn)：上面的一手 rustls 串不含 ALPN，因为那是不设 ALPN 的纯 rustls。真实
		// Codex 经 reqwest 一定会协商 ALPN，而本仓 transport 只能跑 HTTP/1.1，所以必须显式声明
		// http/1.1——dialer 只按扩展表逐项构造（见 dialer.go buildClientHelloSpecFromProfile），
		// 不把 16 列入表内则 Profile.ALPNProtocols 静默不生效。插入位置（psk_key_exchange_modes
		// 之后、session_ticket 之前）无一手抓包依据，属工程选定；如需精确对齐真实 Codex，
		// 应用 tls-fingerprint-web 抓 reqwest 实流后校正。
		Extensions: []uint16{43, 11, 10, 13, 23, 5, 0, 18, 51, 45, 16, 35},
		// rustls 不实现 GREASE；置 true 会造出既非 rustls 亦非浏览器的混合体。
		EnableGREASE: false,
	}
}

// NodeDefaultProfile 返回 Anthropic/Claude 平台使用的内置默认 Profile。
//
// 只设 Name、其余字段留空，由 dialer 回落到包级 Node.js 24.x 常量——与本次改动前的行为
// 字节级一致，确保既有 Claude 账号的指纹不发生任何变化。
func NodeDefaultProfile() *Profile {
	return &Profile{Name: ProfileNameNodeDefault}
}
