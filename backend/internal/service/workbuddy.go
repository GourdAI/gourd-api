package service

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// WorkBuddy 是固定协议平台：上游只提供 OpenAI Chat Completions 形态的
// /v2/chat/completions（无 /v1 前缀），账号类型为 API Key，分 CN
// （copilot.tencent.com）与国际（www.workbuddy.ai）双域，由 credentials.realm 选择。
// 存量账号若曾入库裸域 workbuddy.ai，读取侧由 normalizeWorkbuddyStoredBaseURL
// 兜底归一化为 www（裸域会被上游 301 导致 POST 被改写为 GET → 404）。

const (
	workbuddyChatPath    = "/v2/chat/completions"
	workbuddyRefreshPath = "/v2/plugin/auth/token/refresh"

	// billing 域路径（get-user-resource / daily-checkin）。CN 现状固定 /v2 前缀；
	// global 首选无 /v2 前缀（对齐 workbuddy2api client.go 的 R9 结论：国际版 billing
	// 无 /v2），404 时回落 /v2 变体（fallback 逻辑见 workbuddyCreditsRequest）。
	workbuddyBillingMeterPath   = "/billing/meter/get-user-resource"
	workbuddyBillingMeterPathV2 = "/v2/billing/meter/get-user-resource"
	workbuddyCheckinPath        = "/billing/meter/daily-checkin"
	workbuddyCheckinPathV2      = "/v2/billing/meter/daily-checkin"
)

// DefaultWorkbuddyModelIDs 是 WorkBuddy 官方目录的默认模型 ID，
// 供 /v1/models 在尚未同步上游列表时回退，以及账号白名单预填。
func DefaultWorkbuddyModelIDs() []string {
	return []string{
		"glm-5.2",
		"glm-5.1",
		"glm-5.3",
		"glm-5.3-flash",
		"glm-5v-turbo",
		"kimi-k2.7",
		"kimi-k2.6",
		"kimi-k2.5",
		"kimi-k3",
		"kimi-k2.8-preview",
		"minimax-m3",
		"hy3",
		"hy3-preview",
		"hy4-preview",
		"hy4-preview-x",
		"deepseek-v4-pro",
		"deepseek-v4-flash",
		"deepseek-v4.1-flash",
		"gpt-6-astra",
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"gpt-5.6-luna",
		"gpt-5.5",
		"gpt-5.4",
		"gpt-5.3-codex",
		"gemini-3.5-flash",
		"auto",
	}
}

// IsWorkbuddy 报告账号是否为 WorkBuddy 平台。
func (a *Account) IsWorkbuddy() bool {
	return a != nil && a.Platform == PlatformWorkbuddy
}

// GetWorkbuddyRealm 返回 workbuddy 账号的地域：credentials.realm（"cn"/"global"）
// 优先；缺省时兼容 domain 字段含 workbuddy.ai 判为 global；两者都无法判定时按 "cn"。
func (a *Account) GetWorkbuddyRealm() string {
	if a == nil || !a.IsWorkbuddy() {
		return ""
	}
	return workbuddyRealmFromValues(a.GetCredential("realm"), a.GetCredential("domain"))
}

// workbuddyRealmFromValues 按 realm/domain 原始值判定地域（账号方法与凭据写入侧
// 归一化共用同一口径，防两处漂移）：显式 "global"/"cn" 优先（大小写/空白宽容）；
// 缺省时 domain 含 workbuddy.ai 判 global；否则 cn。
func workbuddyRealmFromValues(realmRaw, domainRaw string) string {
	switch realm := strings.ToLower(strings.TrimSpace(realmRaw)); realm {
	case "global":
		return "global"
	case "cn":
		return "cn"
	}
	if strings.Contains(strings.ToLower(domainRaw), "workbuddy.ai") {
		return "global"
	}
	return "cn"
}

// GetWorkbuddyBillingBaseURL 返回 workbuddy 账号的 billing 域 base_url（积分查询 /
// 每日签到）：优先 credentials.billing_base_url 覆盖；否则按 realm 返回官方默认值
// （CN → www.codebuddy.cn，国际 → www.workbuddy.ai）。存量裸域同样读取侧归一化。
//
// 与 GetWorkbuddyBaseURL（chat 域）刻意分离：CN 账号 chat 走 copilot.tencent.com，
// billing 走 codebuddy.cn（对齐 workbuddy2api 的 ChatBaseCN / BillingBaseCN 双域模型），
// credentials.base_url（chat 自定义中转）不得作用于 billing。
func (a *Account) GetWorkbuddyBillingBaseURL() string {
	if a == nil || !a.IsWorkbuddy() {
		return ""
	}
	if baseURL := strings.TrimSpace(a.GetCredential("billing_base_url")); baseURL != "" {
		return normalizeWorkbuddyStoredBaseURL(baseURL, a.GetWorkbuddyRealm())
	}
	if a.GetWorkbuddyRealm() == "global" {
		return DefaultWorkbuddyGlobalBaseURL
	}
	return DefaultWorkbuddyBillingBaseURL
}

// GetWorkbuddyBaseURL 返回 workbuddy 账号的上游 base_url：优先 credentials.base_url
// 覆盖（自定义中转），否则按 realm 返回 CN / 国际官方默认值。存量裸域（workbuddy.ai）
// 读取侧归一化为 www（见 normalizeWorkbuddyStoredBaseURL）。
func (a *Account) GetWorkbuddyBaseURL() string {
	if a == nil || !a.IsWorkbuddy() {
		return ""
	}
	if baseURL := strings.TrimSpace(a.GetCredential("base_url")); baseURL != "" {
		return normalizeWorkbuddyStoredBaseURL(baseURL, a.GetWorkbuddyRealm())
	}
	if a.GetWorkbuddyRealm() == "global" {
		return DefaultWorkbuddyGlobalBaseURL
	}
	return DefaultWorkbuddyBaseURL
}

// normalizeWorkbuddyStoredBaseURL 归一化存量 workbuddy base_url：realm 为 global 且
// host 精确等于裸域 workbuddy.ai（缺 www）时改写为 www.workbuddy.ai；其余原样返回
// （仅去首尾空白与末尾斜杠）。
//
// 背景（2026-09 国际版 404 事件）：裸域在 APISIX 边缘对 POST 无条件 301（Location
// 指向 www），Go http.Client 默认跟随重定向时会把 POST 改写为 GET 并丢弃请求体，
// 上游只注册了 POST 路由 → 返回 "404 page not found"。前端默认端点已修为带 www；
// 本函数兜底修复前已入库的存量账号。精确 host 匹配保证绝不误伤自定义中转与子域。
func normalizeWorkbuddyStoredBaseURL(raw string, realm string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if realm != "global" {
		return trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return trimmed
	}
	if !strings.EqualFold(parsed.Hostname(), "workbuddy.ai") {
		return trimmed
	}
	host := "www." + strings.ToLower(parsed.Hostname())
	if port := parsed.Port(); port != "" {
		host += ":" + port
	}
	parsed.Host = host
	return strings.TrimRight(parsed.String(), "/")
}

// WorkbuddyCredentials 是 workbuddy 账号凭据的归一化视图。
type WorkbuddyCredentials struct {
	AccessToken  string
	RefreshToken string
	DeviceToken  string
	UID          string
	EnterpriseID string
	Realm        string
	Domain       string
	BaseURL      string
	Nickname     string
	ExpiresAt    int64
	// BillingBaseURL billing 域（积分/签到）自定义覆盖；空 = 按 realm 官方默认值。
	BillingBaseURL string
}

// GetWorkbuddyCredentials 从 credentials 读取 workbuddy 凭据，同时兼容 snake_case
// （access_token 等）与 camelCase（accessToken 等，用于直接粘贴 workbuddy2api 的 auth 文件字段）。
func (a *Account) GetWorkbuddyCredentials() WorkbuddyCredentials {
	if a == nil || !a.IsWorkbuddy() {
		return WorkbuddyCredentials{}
	}
	read := func(snakeKey, camelKey string) string {
		if v := strings.TrimSpace(a.GetCredential(snakeKey)); v != "" {
			return v
		}
		if camelKey == "" {
			return ""
		}
		return strings.TrimSpace(a.GetCredential(camelKey))
	}
	creds := WorkbuddyCredentials{
		AccessToken:  read("access_token", "accessToken"),
		RefreshToken: read("refresh_token", "refreshToken"),
		DeviceToken:  read("device_token", "deviceToken"),
		UID:          read("uid", ""),
		EnterpriseID: read("enterprise_id", "enterpriseId"),
		Realm:        read("realm", ""),
		Domain:       read("domain", ""),
		BaseURL:      read("base_url", ""),
		Nickname:     read("nickname", ""),

		BillingBaseURL: read("billing_base_url", "billingBaseUrl"),
	}
	if raw := read("expires_at", "expiresAt"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
			creds.ExpiresAt = v
		}
	}
	return creds
}

// workbuddyCredentialValueString 把凭据值转为字符串（兼容 JSON 反序列化的多种数字类型）。
func workbuddyCredentialValueString(v any) string {
	switch val := v.(type) {
	case string:
		return strings.TrimSpace(val)
	case json.Number:
		return val.String()
	case float64:
		return strconv.FormatInt(int64(val), 10)
	case int64:
		return strconv.FormatInt(val, 10)
	case int:
		return strconv.Itoa(val)
	default:
		return ""
	}
}

// hasWorkbuddyCredentialKeys 报告凭据中是否携带任意 workbuddy 专属键（snake 或 camel）。
func hasWorkbuddyCredentialKeys(credentials map[string]any) bool {
	for _, key := range []string{
		"access_token", "accessToken",
		"refresh_token", "refreshToken",
		"expires_at", "expiresAt",
		"device_token", "deviceToken",
		"enterprise_id", "enterpriseId",
		"realm", "domain", "uid", "nickname",
		"billing_base_url", "billingBaseUrl",
	} {
		if v, ok := credentials[key]; ok && v != nil {
			return true
		}
	}
	return false
}

// NormalizeWorkbuddyCredentials 校验并原地规范化 workbuddy 账号的 credentials：
//   - 把 workbuddy2api auth 文件的 camelCase 键（accessToken/refreshToken/expiresAt/
//     deviceToken/enterpriseId）归一为 snake_case 存回；
//   - realm 仅允许空 / "cn" / "global"（归一为小写）；
//   - 要求 access_token / refresh_token 至少一个非空；
//   - 写入侧底部归一化：realm 为 global 且 base_url / billing_base_url 主机为裸域
//     workbuddy.ai（缺 www）时改写为 www.workbuddy.ai（裸域会被上游 301 导致
//     POST 被改写为 GET → 404 page not found，2026-09 国际版 404 事件）；
//   - 未识别的键（api_key 等）原样保留、不报错；
//   - 凭据中不含任何 workbuddy 专属键时为 no-op，使批量更新等混合平台路径可安全调用。
func NormalizeWorkbuddyCredentials(credentials map[string]any) error {
	if credentials == nil {
		return nil
	}
	if !hasWorkbuddyCredentialKeys(credentials) {
		return nil
	}
	for _, pair := range [][2]string{
		{"accessToken", "access_token"},
		{"refreshToken", "refresh_token"},
		{"expiresAt", "expires_at"},
		{"deviceToken", "device_token"},
		{"enterpriseId", "enterprise_id"},
		{"billingBaseUrl", "billing_base_url"},
	} {
		raw, ok := credentials[pair[0]]
		if !ok {
			continue
		}
		delete(credentials, pair[0])
		if raw != nil {
			credentials[pair[1]] = raw
		}
	}
	if raw, ok := credentials["realm"]; ok && raw != nil {
		realm := strings.ToLower(workbuddyCredentialValueString(raw))
		switch realm {
		case "", "cn", "global":
			credentials["realm"] = realm
		default:
			return infraerrors.BadRequest("INVALID_WORKBUDDY_CREDENTIALS", `realm must be empty, "cn", or "global"`)
		}
	}
	// 写入侧归一化：realm 判定与账号侧 GetWorkbuddyRealm 同口径（realm 值优先，
	// 缺省时兼容 domain 含 workbuddy.ai）。精确 host 匹配裸域，不伤自定义中转/子域。
	realm := workbuddyRealmFromValues(
		workbuddyCredentialValueString(credentials["realm"]),
		workbuddyCredentialValueString(credentials["domain"]),
	)
	for _, key := range []string{"base_url", "billing_base_url"} {
		raw, ok := credentials[key]
		if !ok || raw == nil {
			continue
		}
		value := workbuddyCredentialValueString(raw)
		if value == "" {
			continue
		}
		if normalized := normalizeWorkbuddyStoredBaseURL(value, realm); normalized != value {
			credentials[key] = normalized
		}
	}
	accessToken := workbuddyCredentialValueString(credentials["access_token"])
	refreshToken := workbuddyCredentialValueString(credentials["refresh_token"])
	if accessToken == "" && refreshToken == "" {
		return infraerrors.BadRequest("INVALID_WORKBUDDY_CREDENTIALS", "access_token or refresh_token is required")
	}
	return nil
}
