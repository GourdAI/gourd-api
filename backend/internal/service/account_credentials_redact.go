package service

import "strings"

// SensitiveCredentialKeys 列出 Account.Credentials JSON map 中绝不允许返回到前端的子键。
// dto 层做响应脱敏、service 层做更新合并都引用此清单——新增凭证类型时务必同步。
var SensitiveCredentialKeys = []string{
	// OAuth
	"access_token", "refresh_token", "id_token", "agent_private_key",
	// Trae 登录时上传公钥对应的设备私钥（刷新 DeviceProof 用，绝不可回显）。
	"device_private_key",
	// API Key 类
	"api_key", "session_key", "cookie",
	// Grok Web SSO / password (must never persist or echo after Build OAuth)
	"password", "sso_token", "sso", "sso-rw", "clearTextPassword",
	// 云服务凭据
	"aws_secret_access_key", "aws_session_token",
	"service_account_json", "service_account", "private_key",
}

var sensitiveCredentialKeySet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(SensitiveCredentialKeys))
	for _, k := range SensitiveCredentialKeys {
		m[normalizeCredentialKey(k)] = struct{}{}
	}
	return m
}()

// normalizeCredentialKey 把凭据子键归一为「大小写与分隔符无关」的形态。
//
// 必要性：清单用 snake_case 书写，但多个平台的凭据**合法接受** camelCase 别名
// （Trae 直接兼容 traework2api 的 auth 文件字段 accessToken / refreshToken /
// idToken，见 trae.go GetTraeCredentials）。若判定做精确匹配，这些别名会整体绕过
// 响应脱敏并在管理端明文回显。归一后 accessToken / access_token / ACCESS-TOKEN
// 收敛为同一键，两表不再漂移。
//
// 归一只做「转小写 + 丢弃分隔符」，因此命中关系是**单向收紧**：原清单里每个键的
// 精确形态归一后仍在清单内，既有行为不变。
func normalizeCredentialKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range strings.ToLower(strings.TrimSpace(key)) {
		switch r {
		case '_', '-', '.', ' ':
			continue
		default:
			_, _ = b.WriteRune(r)
		}
	}
	return b.String()
}

// IsSensitiveCredentialKey 判断指定键是否为敏感凭证子键（大小写/分隔符无关，
// 故 camelCase 别名同样命中）。
func IsSensitiveCredentialKey(key string) bool {
	_, ok := sensitiveCredentialKeySet[normalizeCredentialKey(key)]
	return ok
}

// sensitiveKeyByNormalized 归一键 → 规范名（清单里的书写形态）索引，供别名也能得到
// 与前端约定一致的 `has_<规范名>` 状态位。
var sensitiveKeyByNormalized = func() map[string]string {
	m := make(map[string]string, len(SensitiveCredentialKeys))
	for _, k := range SensitiveCredentialKeys {
		m[normalizeCredentialKey(k)] = k
	}
	return m
}()

// CanonicalCredentialKey 返回敏感子键的规范名（清单里的书写形态）；非敏感键原样返回。
//
// 用途：脱敏后的 `has_<key>` 状态位必须用规范名，否则用户以别名（accessToken）建档时
// 会产出 `has_accessToken`，前端只认 `has_access_token`，面板会误显示为未配置凭据。
func CanonicalCredentialKey(key string) string {
	if canonical, ok := sensitiveKeyByNormalized[normalizeCredentialKey(key)]; ok {
		return canonical
	}
	return key
}

// MergePreservingSensitiveCreds 把 incoming 写入 existing 之上，但敏感子键采用"incoming 没提供就保留 existing"
// 的语义。返回新的 map，不修改入参。
//
// 用途：前端编辑账号通常采用"全对象 PUT"模式；脱敏后前端 spread 旧 credentials 时不会带上敏感键，
// 直接覆盖会清空已有 token。此函数保证：
//   - 非敏感键：完全由 incoming 决定（用户可以编辑、删除非敏感字段）。
//   - 敏感键：incoming 显式提供则覆盖（用户主动旋转 token），否则保留 existing。
func MergePreservingSensitiveCreds(existing, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(incoming)+len(SensitiveCredentialKeys))
	for k, v := range incoming {
		out[k] = v
	}
	// incoming 的归一键索引：用户用别名（accessToken）提交同一凭据时，也算「显式提供」，
	// 不得再回填 existing 的 snake 形态，否则一条凭据两份副本、且旧副本永不轮换。
	incomingNormalized := make(map[string]struct{}, len(incoming))
	for k := range incoming {
		incomingNormalized[normalizeCredentialKey(k)] = struct{}{}
	}
	for _, key := range SensitiveCredentialKeys {
		if _, hasIncoming := incoming[key]; hasIncoming {
			continue
		}
		if _, hasAlias := incomingNormalized[normalizeCredentialKey(key)]; hasAlias {
			continue
		}
		if existingVal, ok := existing[key]; ok {
			out[key] = existingVal
		}
	}
	return out
}
