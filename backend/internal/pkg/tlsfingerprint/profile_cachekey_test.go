package tlsfingerprint

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// nodeShape 是一份与包内默认值"看起来一样但显式写出"的形状，
// 用于验证 CacheKey 不把"留空"与"显式等于默认"归一。
func nodeShape() *Profile {
	return &Profile{
		Name:         "A",
		EnableGREASE: true,
		CipherSuites: []uint16{0x1301, 0x1302, 0x1303},
		Curves:       []uint16{0x001d, 0x0017},
		Extensions:   []uint16{0, 23, 65281},
		ALPNProtocols: []string{
			"h2", "http/1.1",
		},
	}
}

// TestProfileCacheKeyNilIsEmpty 固定 nil 的语义：空串。
// repository 侧的缓存键拼接直接依赖这个约定。
func TestProfileCacheKeyNilIsEmpty(t *testing.T) {
	var p *Profile
	require.Equal(t, "", p.CacheKey(), "nil profile must produce an empty key segment")
}

// TestProfileCacheKeyIsOrderSensitive 顺序是 JA3/JA4 的构成部分，
// 因此套件/扩展/ALPN 的排列变化必须产生不同的键。
func TestProfileCacheKeyIsOrderSensitive(t *testing.T) {
	base := nodeShape()
	reordered := nodeShape()
	reordered.CipherSuites = []uint16{0x1302, 0x1301, 0x1303}
	require.NotEqual(t, base.CacheKey(), reordered.CacheKey(),
		"cipher suite order must change the cache key")

	alpnReordered := nodeShape()
	alpnReordered.ALPNProtocols = []string{"http/1.1", "h2"}
	require.NotEqual(t, base.CacheKey(), alpnReordered.CacheKey(),
		"ALPN order must change the cache key")

	extReordered := nodeShape()
	extReordered.Extensions = []uint16{23, 0, 65281}
	require.NotEqual(t, base.CacheKey(), extReordered.CacheKey(),
		"extension order must change the cache key")
}

// TestProfileCacheKeyDeterministicAcrossCalls 同一形状必须永远得到同一键，
// 否则缓存键本身变成随机源（正是 C2 那类缺陷的形态）。
func TestProfileCacheKeyDeterministicAcrossCalls(t *testing.T) {
	first := nodeShape().CacheKey()
	for i := 0; i < 64; i++ {
		require.Equal(t, first, nodeShape().CacheKey())
	}
	require.Len(t, first, 16, "key is the short 8-byte digest in hex")
}

// TestProfileCacheKeyDistinguishesEachField 逐字段确认每个 ClientHello 维度都进入键。
// 这条测试是防"以后新增 Profile 字段却忘记纳入摘要"的护栏。
func TestProfileCacheKeyDistinguishesEachField(t *testing.T) {
	base := nodeShape().CacheKey()
	mutations := map[string]func(p *Profile){
		"name":                 func(p *Profile) { p.Name = "B" },
		"grease":               func(p *Profile) { p.EnableGREASE = false },
		"cipher_suites":        func(p *Profile) { p.CipherSuites = append(p.CipherSuites, 0xc02b) },
		"curves":               func(p *Profile) { p.Curves = append(p.Curves, 0x0018) },
		"point_formats":        func(p *Profile) { p.PointFormats = []uint16{1} },
		"signature_algorithms": func(p *Profile) { p.SignatureAlgorithms = []uint16{0x0403} },
		"supported_versions":   func(p *Profile) { p.SupportedVersions = []uint16{0x0303} },
		"key_share_groups":     func(p *Profile) { p.KeyShareGroups = []uint16{0x001d} },
		"psk_modes":            func(p *Profile) { p.PSKModes = []uint16{1} },
		"extensions":           func(p *Profile) { p.Extensions = append(p.Extensions, 10) },
		"alpn_protocols":       func(p *Profile) { p.ALPNProtocols = []string{"http/1.1"} },
		"cipher_suites_length": func(p *Profile) { p.CipherSuites = nil },
		"alpn_empty":           func(p *Profile) { p.ALPNProtocols = nil },
	}
	for field, mutate := range mutations {
		p := nodeShape()
		mutate(p)
		require.NotEqual(t, base, p.CacheKey(), "changing %s must change the cache key", field)
	}
}

// TestProfileCacheKeyBuiltinDefaultsDiffer 两个内置平台默认指纹必须可区分，
// 否则 OpenAI 与 Anthropic 账号会在连接池里共享同一个 TLS 条目。
func TestProfileCacheKeyBuiltinDefaultsDiffer(t *testing.T) {
	require.NotEqual(t, CodexRustlsProfile().CacheKey(), NodeDefaultProfile().CacheKey())
	require.NotEmpty(t, CodexRustlsProfile().CacheKey())
	require.NotEmpty(t, NodeDefaultProfile().CacheKey())
}
