package service

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Trae 是固定协议平台：上游为字节 Trae IDE 自研 Cloud-IDE-JWT 协议，聊天端点固定为
// /api/agent/v3/llm_utils_chat（SSE 自定义事件流，无 OpenAI 兼容端点、无 /v1/responses、
// 无 Anthropic /v1/messages）。账号类型统一按 API Key 建档（凭据为 IDE 会话令牌），
// 分 CN（trae-api-cn.mchost.guru）与国际（a0ai-api-sg.byteintlapi.com）双域，由
// credentials.realm 选择。
//
// 积分与签到不走聊天域：CN 账号另有 UG 域 api.trae.cn（/trae/api/v2/ug/checkin_credits/*、
// /trae/api/v2/pay/ide_user_ent_usage），且必须使用与聊天链路**不同**的客户端指纹
// （UG 链路带 IDE 插件 UA、不发 x-uid），混用会被上游风控判为异常。OAuth 换票域
// （api.trae.com.cn / api-sg-central.trae.ai）又是第三个 host。三者分别由
// GetTraeBaseURL / GetTraeBillingBaseURL / GetTraeOAuthBaseURL 解析。

const (
	// 聊天域路径
	traeChatPath      = "/api/agent/v3/llm_utils_chat"
	traeChatFallback  = "/api/ide/v1/chat"
	traeGetUserInfo   = "/cloudide/api/v3/trae/GetUserInfo"
	traeExchangeToken = "/cloudide/api/v3/trae/oauth/ExchangeToken"

	// UG 域路径（积分 / 签到 / 权益包用量）
	traeCheckinStatusPath = "/trae/api/v2/ug/checkin_credits/status"
	traeCheckinClaimPath  = "/trae/api/v2/ug/checkin_credits/claim"
	traeEntUsagePath      = "/trae/api/v2/pay/ide_user_ent_usage"
)

// DefaultTraeModelIDs 是 Trae CN 官方目录的默认模型 ID，供 /v1/models 在尚未同步
// 上游列表时回退，以及账号白名单预填。上游模型表由 x-ide-version-code 决定（见
// credentials.ide_version_code），此处取当前 CN 目录的常用子集。
func DefaultTraeModelIDs() []string {
	return []string{
		"glm-5.3",
		"glm-5.2",
		"glm-5.1",
		"glm-5",
		"glm-5-turbo",
		"glm-5v-turbo",
		"glm-4.7",
		"glm-4.6",
		"qwen-3.7-plus",
		"qwen-3.6-plus",
		"qwen-3.5",
		"qwen3-coder",
		"kimi-k2.7-code",
		"kimi-k2.6",
		"kimi-k2.5",
		"kimi-k2",
		"DeepSeek-V4-Pro",
		"DeepSeek-V4-Flash",
		"deepseek-V3.1",
		"minimax-m3",
		"minimax-m2.7",
		"Doubao-Seed-2.1-Pro",
		"Doubao-Seed-2.0-Code",
		"seed-code-pro-0430",
		"auto",
	}
}

// IsTrae 报告账号是否为 Trae 平台（receiver 形式，风格对齐 IsWorkbuddy / IsQoderPlatform）。
func (a *Account) IsTrae() bool {
	return a != nil && a.Platform == PlatformTrae
}

// GetTraeRealm 返回 trae 账号的地域："cn" / "global"。
// credentials.realm 显式值优先；缺省时按 base_url / billing_base_url 主机名推断
// （国际域名为 mchost.guru 之外的 byteintlapi.com / trae.ai），仍无法判定按 "cn"。
func (a *Account) GetTraeRealm() string {
	if a == nil || !a.IsTrae() {
		return ""
	}
	return traeRealmFromValues(a.GetCredential("realm"), a.GetCredential("base_url"), a.GetCredential("billing_base_url"))
}

// traeRealmFromValues 按 realm/base_url 原始值判定地域（账号方法与凭据写入侧归一化
// 共用同一口径，防两处漂移）：显式 "cn"/"global" 优先（大小写/空白宽容）；缺省时
// 按主机名兜底推断；否则 "cn"。
func traeRealmFromValues(realmRaw, baseURLRaw, billingURLRaw string) string {
	switch realm := strings.ToLower(strings.TrimSpace(realmRaw)); realm {
	case "global":
		return "global"
	case "cn":
		return "cn"
	}
	for _, raw := range []string{baseURLRaw, billingURLRaw} {
		host := traeURILowerHost(raw)
		if host == "" {
			continue
		}
		// CN 侧主机：mchost.guru（聊天）与 *.trae.cn / *.trae.com.cn（UG/OAuth）。
		if strings.Contains(host, "mchost.guru") || strings.HasSuffix(host, "trae.cn") || strings.HasSuffix(host, "trae.com.cn") {
			return "cn"
		}
		if strings.Contains(host, "byteintlapi.com") || strings.HasSuffix(host, "trae.ai") {
			return "global"
		}
	}
	return "cn"
}

// GetTraeBaseURL 返回 trae 账号的聊天域 base_url：优先 credentials.base_url 覆盖
// （自定义中转），否则按 realm 返回 CN / 国际官方默认值。
func (a *Account) GetTraeBaseURL() string {
	if a == nil || !a.IsTrae() {
		return ""
	}
	if baseURL := strings.TrimRight(strings.TrimSpace(a.GetCredential("base_url")), "/"); baseURL != "" {
		return baseURL
	}
	if a.GetTraeRealm() == "global" {
		return DefaultTraeGlobalBaseURL
	}
	return DefaultTraeBaseURL
}

// GetTraeBillingBaseURL 返回 trae 账号的 UG 域 base_url（积分查询 / 每日签到）：
// 优先 credentials.billing_base_url 覆盖；否则按 realm 返回默认值。
//
// 与 GetTraeBaseURL（聊天域）刻意分离：两者是不同主机、不同客户端指纹，
// credentials.base_url（聊天自定义中转）不得作用于 UG 域。
func (a *Account) GetTraeBillingBaseURL() string {
	if a == nil || !a.IsTrae() {
		return ""
	}
	if baseURL := strings.TrimRight(strings.TrimSpace(a.GetCredential("billing_base_url")), "/"); baseURL != "" {
		return baseURL
	}
	if a.GetTraeRealm() == "global" {
		return DefaultTraeGlobalBillingBaseURL
	}
	return DefaultTraeBillingBaseURL
}

// GetTraeOAuthBaseURL 返回 trae 账号的 OAuth 换票域 base_url（ExchangeToken /
// GetUserInfo）：优先 credentials.oauth_base_url 覆盖，否则按 realm 返回默认值。
func (a *Account) GetTraeOAuthBaseURL() string {
	if a == nil || !a.IsTrae() {
		return ""
	}
	if baseURL := strings.TrimRight(strings.TrimSpace(a.GetCredential("oauth_base_url")), "/"); baseURL != "" {
		return baseURL
	}
	if a.GetTraeRealm() == "global" {
		return DefaultTraeGlobalOAuthBaseURL
	}
	return DefaultTraeOAuthBaseURL
}

// TraeCredentials 是 trae 账号凭据的归一化视图。
type TraeCredentials struct {
	AccessToken  string
	RefreshToken string
	UID          string
	Nickname     string
	Realm        string
	BaseURL      string
	// DeviceID / MachineID 上游风控指纹：UG 域（签到/积分）缺 device_id 直接返回
	// 业务码 9004（参数错误）。两者缺省时由账号 ID 稳定派生（见 traeStableDeviceID）。
	DeviceID  string
	MachineID string
	// IDEVersion / IDEVersionCode 决定上游可用模型表（version-code 不匹配会拿到
	// 列表里没有的模型并在流内报 4001）。缺省用包内常量，可在凭据里覆盖以跟随版本。
	IDEVersion      string
	IDEVersionCode  string
	DeviceBrand     string
	DeviceType      string
	OSVersion       string
	ExpiresAt       int64
	RefreshExpireAt int64
	BillingBaseURL  string
	OAuthBaseURL    string
	// ClientID OAuth 换票的客户端标识：CN IDE 与 SOLO 是两个不同值，缺省按 realm 选择。
	ClientID string
	// ReqSource UG 域请求来源（1=Trae CN IDE，2=SOLO/企业版）；同一账号积分互通，
	// 缺省 1。可配是因为部分 SOLO 账号只在 req_source=2 下返回完整用量。
	ReqSource int
}

// GetTraeCredentials 从 credentials 读取 trae 凭据，同时兼容 snake_case（access_token
// 等）与多个 camelCase 别名。
//
// 为什么要多个别名：用户最自然的粘贴源是 Trae 本地 storage.json，而它的字段是
// **expiredAt / refreshExpiredAt**（过去式），不是 expiresAt；traework2api 的 auth
// 文件则用 accessToken / refreshToken。两类形态都必须收到，否则用户粘进来的到期
// 时间会被当成「未知」，直接错过 refreshToken 过期告警（过期后无法自动续期）。
func (a *Account) GetTraeCredentials() TraeCredentials {
	if a == nil || !a.IsTrae() {
		return TraeCredentials{}
	}
	read := func(keys ...string) string {
		for _, key := range keys {
			if v := strings.TrimSpace(a.GetCredential(key)); v != "" {
				return v
			}
		}
		return ""
	}
	creds := TraeCredentials{
		AccessToken:    read("access_token", "accessToken"),
		RefreshToken:   read("refresh_token", "refreshToken"),
		UID:            read("uid", "userId"),
		Nickname:       read("nickname"),
		Realm:          read("realm"),
		BaseURL:        read("base_url"),
		DeviceID:       read("device_id", "deviceId"),
		MachineID:      read("machine_id", "machineId"),
		IDEVersion:     read("ide_version", "ideVersion"),
		IDEVersionCode: read("ide_version_code", "ideVersionCode"),
		DeviceBrand:    read("device_brand", "deviceBrand"),
		DeviceType:     read("device_type", "deviceType"),
		OSVersion:      read("os_version", "osVersion"),
		BillingBaseURL: read("billing_base_url", "billingBaseUrl"),
		OAuthBaseURL:   read("oauth_base_url", "oauthBaseUrl"),
		ClientID:       read("client_id", "clientId"),
	}
	if raw := read("expires_at", "expiresAt", "expiredAt"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
			creds.ExpiresAt = traeNormalizeEpochSeconds(v)
		} else if ts := traeParseISOTimeOrZero(raw); ts > 0 {
			creds.ExpiresAt = ts
		}
	}
	if raw := read("refresh_expires_at", "refreshExpiresAt", "refreshExpiredAt"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
			creds.RefreshExpireAt = traeNormalizeEpochSeconds(v)
		} else if ts := traeParseISOTimeOrZero(raw); ts > 0 {
			creds.RefreshExpireAt = ts
		}
	}
	if raw := read("req_source", "reqSource"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			creds.ReqSource = v
		}
	}
	return creds
}

// traeNormalizeEpochSeconds 把上游/客户端可能给出的毫秒时间戳归一为秒：
// ExchangeToken 的 TokenExpireAt 是 epoch 毫秒，而手动粘贴的 expiredAt 常为秒。
func traeNormalizeEpochSeconds(v int64) int64 {
	if v > 1_000_000_000_000 {
		return v / 1000
	}
	return v
}

// traeCredentialValueString 把凭据值转为字符串（兼容 JSON 反序列化的多种数字类型）。
func traeCredentialValueString(v any) string {
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

// hasTraeCredentialKeys 报告凭据中是否携带任意 trae 专属键（snake 或 camel）。
func hasTraeCredentialKeys(credentials map[string]any) bool {
	for _, key := range []string{
		"access_token", "accessToken",
		"refresh_token", "refreshToken",
		"expires_at", "expiresAt", "expiredAt",
		"refresh_expires_at", "refreshExpiresAt", "refreshExpiredAt",
		"device_id", "deviceId",
		"machine_id", "machineId",
		"ide_version", "ideVersion",
		"ide_version_code", "ideVersionCode",
		"device_brand", "deviceBrand",
		"device_type", "deviceType",
		"os_version", "osVersion",
		"billing_base_url", "billingBaseUrl",
		"oauth_base_url", "oauthBaseUrl",
		"client_id", "clientId",
		"req_source", "reqSource",
		"realm", "uid", "userId", "nickname",
		// 浏览器 OAuth 登录产物（trae_login_service）：设备密钥对与登录域回显。
		"device_public_key", "devicePublicKey",
		"device_private_key", "devicePrivateKey",
		"login_host", "loginHost", "login_region", "loginRegion", "auth_source", "authSource",
	} {
		if v, ok := credentials[key]; ok && v != nil {
			return true
		}
	}
	return false
}

// NormalizeTraeCredentials 校验并原地规范化 trae 账号的 credentials：
//   - 把 traework2api auth 文件的 camelCase 键（accessToken/refreshToken/expiresAt/
//     refreshExpiredAt/deviceId/machineId/...）归一为 snake_case 存回；
//   - realm 仅允许空 / "cn" / "global"（归一为小写；空值按 base_url 推断后回填）；
//   - 要求 access_token / refresh_token 至少一个非空（只有 refreshToken 时，
//     首次出站前由 ExchangeToken 换取 access_token）；
//   - expires_at / refresh_expires_at 统一归一为 epoch 秒（上游毫秒时间戳兼容）；
//   - req_source 归一为 1 或 2（其余取值回落到 1）；
//   - 未识别的键（api_key 等）原样保留、不报错；
//   - 凭据中不含任何 trae 专属键时为 no-op，使批量更新等混合平台路径可安全调用。
func NormalizeTraeCredentials(credentials map[string]any) error {
	if credentials == nil {
		return nil
	}
	if !hasTraeCredentialKeys(credentials) {
		return nil
	}
	for _, pair := range [][2]string{
		{"accessToken", "access_token"},
		{"refreshToken", "refresh_token"},
		{"expiresAt", "expires_at"},
		// Trae 本地 storage.json 的字段是过去式 expiredAt / refreshExpiredAt，而部分
		// 实现用现在式 expiresAt：两者都必须归一为 snake 入库，否则同一语义在库里
		// 存成两个名字，下游（列表告警 / 提前换票）只认一个就会静默失效。
		{"expiredAt", "expires_at"},
		{"refreshExpiresAt", "refresh_expires_at"},
		{"refreshExpiredAt", "refresh_expires_at"},
		{"deviceId", "device_id"},
		{"machineId", "machine_id"},
		{"ideVersion", "ide_version"},
		{"ideVersionCode", "ide_version_code"},
		{"deviceBrand", "device_brand"},
		{"deviceType", "device_type"},
		{"osVersion", "os_version"},
		{"billingBaseUrl", "billing_base_url"},
		{"oauthBaseUrl", "oauth_base_url"},
		{"clientId", "client_id"},
		{"reqSource", "req_source"},
		{"userId", "uid"},
		{"devicePublicKey", "device_public_key"},
		{"devicePrivateKey", "device_private_key"},
		{"loginHost", "login_host"},
		{"loginRegion", "login_region"},
		{"authSource", "auth_source"},
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
	realm := traeRealmFromValues(
		traeCredentialValueString(credentials["realm"]),
		traeCredentialValueString(credentials["base_url"]),
		traeCredentialValueString(credentials["billing_base_url"]),
	)
	switch raw := traeCredentialValueString(credentials["realm"]); strings.ToLower(raw) {
	case "":
		credentials["realm"] = realm
	case "cn", "global":
		credentials["realm"] = strings.ToLower(raw)
	default:
		return infraerrors.BadRequest("INVALID_TRAE_CREDENTIALS", `realm must be empty, "cn", or "global"`)
	}
	// 时间戳归一为 epoch 秒（手动粘贴 storage.json 的 expiredAt 常是毫秒或 ISO 串）。
	for _, key := range []string{"expires_at", "refresh_expires_at"} {
		raw, ok := credentials[key]
		if !ok || raw == nil {
			continue
		}
		value := traeCredentialValueString(raw)
		if value == "" {
			delete(credentials, key)
			continue
		}
		if v, err := strconv.ParseInt(value, 10, 64); err == nil {
			if normalized := traeNormalizeEpochSeconds(v); normalized != v {
				credentials[key] = normalized
			}
			continue
		}
		if ts := traeParseISOTimeOrZero(value); ts > 0 {
			credentials[key] = ts
		} else {
			delete(credentials, key)
		}
	}
	// req_source 归一：仅 1/2 合法，其余（含非数字）回落 1。
	if raw, ok := credentials["req_source"]; ok && raw != nil {
		source := 1
		if v, err := strconv.Atoi(traeCredentialValueString(raw)); err == nil && (v == 1 || v == 2) {
			source = v
		}
		credentials["req_source"] = source
	}
	accessToken := traeCredentialValueString(credentials["access_token"])
	refreshToken := traeCredentialValueString(credentials["refresh_token"])
	if accessToken == "" && refreshToken == "" {
		return infraerrors.BadRequest("INVALID_TRAE_CREDENTIALS", "access_token or refresh_token is required")
	}
	return nil
}

// traeURILowerHost 取 URL 的小写主机名（解析失败/无主机返回空串）。
func traeURILowerHost(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	// 先剔掉 scheme（保留无 scheme 输入的原有行为），再截 authority。
	if scheme := strings.Index(trimmed, "://"); scheme >= 0 {
		trimmed = trimmed[scheme+len("://"):]
	} else {
		trimmed = strings.TrimPrefix(trimmed, "//")
	}
	if slash := strings.IndexAny(trimmed, "/?#"); slash >= 0 {
		trimmed = trimmed[:slash]
	}
	host := trimmed
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	if colon := strings.Index(host, ":"); colon >= 0 {
		host = host[:colon]
	}
	return strings.ToLower(host)
}

// traeParseISOTimeOrZero 解析常见 ISO 时间串为 epoch 秒（解析失败返回 0）。
// Trae storage.json 的 expiredAt 形如 "2026-09-30T12:00:00+08:00"。
func traeParseISOTimeOrZero(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.Unix()
		}
	}
	return 0
}
