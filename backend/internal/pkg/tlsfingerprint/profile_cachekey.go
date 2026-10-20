package tlsfingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// CacheKey 返回该 Profile 影响 ClientHello 形状的短指纹（16 hex）。
//
// WHY：防封加固后账号可绑定/切换 TLS 指纹模板，但出站连接池的缓存键只看
// (isolation, proxy, accountID, protocolMode)，不含模板身份 —— 于是
// 「管理员把账号从模板 A 改绑到模板 B」时，缓存命中的仍是按 A 建好的
// Transport，新模板要等到该条目空闲被淘汰（默认 900s）才生效。表现出来就是
// "改了没生效、也没报错"，且这期间账号交出的 ClientHello 与面板显示不一致，
// 正是本项目 profiles.go 反复警告的 chimera 面。
//
// 因此把决定 ClientHello 的全部字段折成一个短键，并入 TLS 客户端缓存键。
// 只取 sha256 前 8 字节：这是缓存键不是安全摘要，碰撞的代价只是多复用一次
// 旧 Transport，而键太长会让日志与 map 键都难以阅读。
//
// 语义约定：
//   - 返回空串表示"无自定义形状"（nil Profile），调用方应使用与改动前一致的键；
//   - 字段顺序敏感 —— CipherSuites/Extensions 的顺序本身就是 JA3/JA4 的构成，
//     所以 [0x1301,0x1302] 与 [0x1302,0x1301] 必须产生不同的键；
//   - 空切片与非空切片必须可区分 —— dialer 对空切片回落到内置默认值，
//     "留空"与"显式给出与默认相同的值"在形状上等价但配置意图不同，
//     这里按字面区分（两者会得到不同键），不做归一，避免把"留空"这一
//     隐式状态与某个具体模板混为同一个缓存条目。
func (p *Profile) CacheKey() string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("name=")
	b.WriteString(p.Name)
	b.WriteString(";grease=")
	b.WriteString(strconv.FormatBool(p.EnableGREASE))
	writeUint16List(&b, "cs", p.CipherSuites)
	writeUint16List(&b, "cv", p.Curves)
	writeUint16List(&b, "pf", p.PointFormats)
	writeUint16List(&b, "sa", p.SignatureAlgorithms)
	writeUint16List(&b, "sv", p.SupportedVersions)
	writeUint16List(&b, "ks", p.KeyShareGroups)
	writeUint16List(&b, "psk", p.PSKModes)
	writeUint16List(&b, "ext", p.Extensions)
	b.WriteString(";alpn=")
	b.WriteString(strings.Join(p.ALPNProtocols, ","))

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// writeUint16List 以"键=长度:值,值"形式写入，长度入键使 nil 与空切片也可区分。
func writeUint16List(b *strings.Builder, key string, vals []uint16) {
	b.WriteString(";" + key + "=")
	b.WriteString(strconv.Itoa(len(vals)))
	b.WriteString(":")
	for i, v := range vals {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(strconv.FormatUint(uint64(v), 10))
	}
}
