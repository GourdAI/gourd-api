package service

// qoder.go Qoder 平台契约：平台常量、上游端点解析、凭据归一化视图与校验、
// 默认模型目录。
//
// Qoder 是固定协议平台：上游为自研 COSY 签名协议（base64 变体编码 + RSA/AES
// 会话封装 + COSY Bearer 签名），账号凭据为设备令牌（dt-）/PAT 交换出的
// securityOauthToken / drt- 刷新令牌，分 cn / global 双域，由 credentials.realm
// 选择（缺省 global，与 workbuddy 的缺省 cn 相反——Qoder 官方客户端默认国际域）。
//
// 本文件只承载平台契约层；签名/会话见 qoder_cosy.go，请求体见 qoder_payload.go，
// 上游发送见 qoder_client.go。平台常量 PlatformQoder 与 IsQoder/IsQoderPlatform
// 定义在 domain_constants.go（镜像 domain.PlatformQoder）。

import (
	"encoding/json"
	"strconv"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Qoder 上游固定路径（cn / global 双域共用路径，仅域名不同）。
const (
	// qoderChatPath chat SSE 端点（流式 agent 对话）。
	qoderChatPath = "/algo/api/v2/service/pro/sse/agent_chat_generation"
	// qoderChatQuery chat 端点固定查询串（FetchKeys/AgentId/Encode）。
	qoderChatQuery = "?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	// qoderModelListPath 模型列表端点。
	qoderModelListPath = "/algo/api/v2/model/list?Encode=1"
	// qoderJobTokenPath PAT → securityOauthToken 交换端点。
	qoderJobTokenPath = "/algo/api/v3/user/jobToken?Encode=1"
	// qoderUserinfoPath 设备令牌身份信息端点（openapi 域）。
	qoderUserinfoPath = "/api/v1/userinfo"
	// qoderPlanPath 套餐信息端点（openapi 域）。
	qoderPlanPath = "/api/v2/user/plan"
	// qoderQuotaPath 用量端点（openapi 域）。
	qoderQuotaPath = "/api/v2/quota/usage"
	// qoderCampaignsPath 活动列表/可领取状态端点（openapi 域 /sash 前缀）。
	// 上游按 cosy-clienttype 定向下发：只有桌面端（10）才会返回每日活动。
	qoderCampaignsPath = "/sash/api/v1/me/campaigns"
)

// Qoder 双域主机（global / cn）。algo 系列与 openapi 系列分属不同子域。
const (
	qoderAlgoHostGlobal    = "api1.qoder.sh"
	qoderAlgo2HostGlobal   = "api2.qoder.sh"
	qoderCenterHostGlobal  = "center.qoder.sh"
	qoderOpenAPIHostGlobal = "openapi.qoder.sh"

	qoderAlgoHostCN    = "gateway.qoder.com.cn"
	qoderAlgo2HostCN   = "gateway.qoder.com.cn"
	qoderCenterHostCN  = "gateway.qoder.com.cn"
	qoderOpenAPIHostCN = "openapi.qoder.com.cn"

	// qoderDeviceLoginBaseGlobal / qoderDeviceLoginBaseCN OAuth 设备授权登录页基础域。
	qoderDeviceLoginBaseGlobal = "https://qoder.com/device/selectAccounts"
	qoderDeviceLoginBaseCN     = "https://qoder.com.cn/device/selectAccounts"

	// qoderPollEndpointGlobal / qoderPollEndpointCN OAuth 设备令牌轮询端点。
	qoderPollEndpointGlobal = "https://openapi.qoder.sh/api/v1/deviceToken/poll"
	qoderPollEndpointCN     = "https://openapi.qoder.com.cn/api/v1/deviceToken/poll"
)

// resolveQoderEndpoints 按 realm 与可选 baseURL 覆盖解析 Qoder 各端点。
// baseURL（credentials.base_url）非空时作为自定义中转：chat/modelList/jobToken
// 全部指向该域（官方路径拼接在域名之后，不做路径改写）；userinfo/plan/quota 与
// OAuth 端点仍走官方 openapi 域（自定义中转通常只代理 algo 系列签名接口）。
func resolveQoderEndpoints(realm, baseURL string) qoderEndpoints {
	isCN := realm == "cn"
	endpoints := qoderEndpoints{}
	if isCN {
		endpoints.AlgoBase = "https://" + qoderAlgoHostCN
		endpoints.Algo2Base = "https://" + qoderAlgo2HostCN
		endpoints.CenterBase = "https://" + qoderCenterHostCN
		endpoints.OpenAPIBase = "https://" + qoderOpenAPIHostCN
		endpoints.DeviceLoginBase = qoderDeviceLoginBaseCN
		endpoints.PollEndpoint = qoderPollEndpointCN
	} else {
		endpoints.AlgoBase = "https://" + qoderAlgoHostGlobal
		endpoints.Algo2Base = "https://" + qoderAlgo2HostGlobal
		endpoints.CenterBase = "https://" + qoderCenterHostGlobal
		endpoints.OpenAPIBase = "https://" + qoderOpenAPIHostGlobal
		endpoints.DeviceLoginBase = qoderDeviceLoginBaseGlobal
		endpoints.PollEndpoint = qoderPollEndpointGlobal
	}
	if base := strings.TrimSpace(baseURL); base != "" {
		base = strings.TrimRight(base, "/")
		if !strings.Contains(base, "://") {
			base = "https://" + base
		}
		endpoints.AlgoBase = base
		endpoints.Algo2Base = base
		endpoints.CenterBase = base
	}
	endpoints.ChatURL = endpoints.AlgoBase + qoderChatPath + qoderChatQuery
	endpoints.ModelListURL = endpoints.Algo2Base + qoderModelListPath
	endpoints.JobTokenURL = endpoints.CenterBase + qoderJobTokenPath
	endpoints.UserinfoURL = endpoints.OpenAPIBase + qoderUserinfoPath
	endpoints.PlanURL = endpoints.OpenAPIBase + qoderPlanPath
	endpoints.QuotaURL = endpoints.OpenAPIBase + qoderQuotaPath
	// 活动端点：CampaignsURL 是状态查询；CampaignClaimBase 用于拼 /{campaignId}/claim。
	endpoints.CampaignsURL = endpoints.OpenAPIBase + qoderCampaignsPath
	endpoints.CampaignClaimBase = endpoints.OpenAPIBase + qoderCampaignsPath
	return endpoints
}

// qoderEndpoints 是一组解析完成的 Qoder 上游端点。
type qoderEndpoints struct {
	ChatURL      string
	ModelListURL string
	JobTokenURL  string
	UserinfoURL  string
	PlanURL      string
	QuotaURL     string
	// CampaignsURL 活动列表端点；CampaignClaimBase 是其前缀，领取时拼
	// /{campaignId}/claim（path 参数是 UUID 形态的 campaignId，不是 campaignKey）。
	CampaignsURL      string
	CampaignClaimBase string
	DeviceLoginBase   string
	PollEndpoint      string
	// AlgoBase / Algo2Base / CenterBase / OpenAPIBase 是四个上游域根（自定义
	// 中转覆盖前三者；OpenAPIBase 恒官方，承载无签名直连接口与 OAuth 轮询）。
	AlgoBase    string
	Algo2Base   string
	CenterBase  string
	OpenAPIBase string
}

// chatURLFor 返回 chat SSE 完整 URL（供网关拼接调用，保持与字段解耦）。
func (e qoderEndpoints) chatURLFor() string { return e.ChatURL }

// DefaultQoderModelIDs 是 Qoder 官方目录的默认模型 ID（静态表，供 /v1/models
// 在尚未同步上游列表时回退，以及账号白名单预填；后续接线批次使用）。
//
// 来源：Qoder CN 客户端 model classes 目录（app 场景 14 项，2026-09-22 实测对齐
// 上游 /api/v2/model/list）。上游共 14 个 key：少一个都取不到对应模型。
func DefaultQoderModelIDs() []string {
	return []string{
		"auto",
		"qmodel_38max",
		"qfmodel",
		"qmodel_latest",
		"qmodel",
		"q37fmodel",
		"dmodel",
		"dfmodel",
		"gmodel",
		"gfmodel",
		"gm51model",
		"kmodel_latest",
		"kmodel",
		"mmodel",
	}
}

// qoderModelCatalog 是模型 key → 展示名的静态目录（payload 侧按 key 映射
// model_config 展示字段；来源同上：Qoder CN 客户端目录真值）。
var qoderModelCatalog = map[string]string{
	"auto":          "Auto",
	"qmodel_38max":  "Qwen3.8-Max",
	"qfmodel":       "Qwen3.8-Flash",
	"qmodel_latest": "Qwen3.7-Max",
	"qmodel":        "Qwen3.7-Plus",
	"q37fmodel":     "Qwen3.7-Flash",
	"dmodel":        "DeepSeek-V4-Pro",
	"dfmodel":       "DeepSeek-Flash",
	"gmodel":        "GLM-5.3",
	"gfmodel":       "GLM-5.3-Flash",
	"gm51model":     "GLM-5.2",
	"kmodel_latest": "Kimi-K3",
	"kmodel":        "Kimi-K2.8-Preview",
	"mmodel":        "MiniMax-M2.7",
}

// qoderModelDisplayName 返回模型 key 的展示名（未知 key 原样返回）。
func qoderModelDisplayName(key string) string {
	if name, ok := qoderModelCatalog[key]; ok {
		return name
	}
	return key
}

// qoderModelAliasCatalog 是展示名（及其他常见写法）→ 官方模型 key 的别名目录。
// 归一化只有单向下行（展示名 → key）是安全的：Qoder 上游只认 14 个封闭 key，
// 任何非 key 的模型名发给上游都会被静默回落（实测 qwen3.8-flash 请求 credits=0、
// 且响应的 model 字段恒为 auto，客户端会看到「模型不一致」）。
//
// 键统一按小写比较（调用方先 ToLower）；值必须是 qoderModelCatalog 中存在的 key。
var qoderModelAliasCatalog = map[string]string{
	// Qwen
	"qwen3.8-max":   "qmodel_38max",
	"qwen3.8-flash": "qfmodel",
	"qwen3.7-max":   "qmodel_latest",
	"qwen3.7-plus":  "qmodel",
	"qwen3.7-flash": "q37fmodel",
	// DeepSeek
	"deepseek-v4-pro":   "dmodel",
	"deepseek-flash":    "dfmodel",
	"deepseek-v4-flash": "dfmodel",
	// GLM
	"glm-5.3":       "gmodel",
	"glm-5.3-flash": "gfmodel",
	"glm-5.2":       "gm51model",
	// Kimi
	"kimi-k3":           "kmodel_latest",
	"kimi-k2.8-preview": "kmodel",
	"kimi-k2.8":         "kmodel",
	// MiniMax
	"minimax-m2.7": "mmodel",
	// 自身 key 的直通（幂等，避免调用方分支）
	"auto":          "auto",
	"qmodel_38max":  "qmodel_38max",
	"qfmodel":       "qfmodel",
	"qmodel_latest": "qmodel_latest",
	"qmodel":        "qmodel",
	"q37fmodel":     "q37fmodel",
	"dmodel":        "dmodel",
	"dfmodel":       "dfmodel",
	"gmodel":        "gmodel",
	"gfmodel":       "gfmodel",
	"gm51model":     "gm51model",
	"kmodel_latest": "kmodel_latest",
	"kmodel":        "kmodel",
	"mmodel":        "mmodel",
}

// normalizeQoderModelKey 把任意模型写法归一为 Qoder 官方 key：
// 官方 key 原样返回；展示名/别名（qwen3.8-flash 等）映射为对应 key；
// 未知值原样返回（保持宽容，交由上游判定）。
// 输入大小写不敏感（大小写不同的官方 key 也归一为规范小写形态）。
func normalizeQoderModelKey(model string) string {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return ""
	}
	lower := strings.ToLower(trimmed)
	if mapped, ok := qoderModelAliasCatalog[lower]; ok {
		return mapped
	}
	return trimmed
}

// isQoderModelCode 报告 model 是否为 Qoder 平台模型代号（14 项封闭集合，
// 输入需已转小写）。供计费 fallback 精确匹配使用：代号无品牌前缀，
// 必须用精确集合判定，不得用子串规则。
func isQoderModelCode(model string) bool {
	_, ok := qoderModelCatalog[model]
	return ok
}

// isQoderAutoResponseModel 报告上游响应 model 声明是否为 Qoder 的智能路由占位
// 值 "auto"。Qoder 上游 chat 端点对**任何** key 的响应都与请求无关地恒报
// model:"auto"（实测发 qfmodel 也回 auto），因此 sent==官方key 且响应==auto 时
// 不是真的「模型不一致」，属于协议特性，审计口径应豁免。
func isQoderAutoResponseModel(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), "auto")
}

// GetQoderRealm 返回 qoder 账号的地域：credentials.realm（"cn"/"global"）优先；
// 缺省时兼容 domain 字段含 qoder.com.cn 判为 cn；两者都无法判定时按 "global"
// （Qoder 官方客户端默认国际域，与 workbuddy 缺省 cn 相反）。
func (a *Account) GetQoderRealm() string {
	if a == nil || !a.IsQoderPlatform() {
		return ""
	}
	switch realm := strings.ToLower(strings.TrimSpace(a.GetCredential("realm"))); realm {
	case "global":
		return "global"
	case "cn":
		return "cn"
	}
	if strings.Contains(strings.ToLower(a.GetCredential("domain")), "qoder.com.cn") {
		return "cn"
	}
	return "global"
}

// GetQoderBaseURL 返回 qoder 账号的上游 base_url：优先 credentials.base_url
// 覆盖（自定义中转），否则按 realm 返回官方默认 algo 域。
func (a *Account) GetQoderBaseURL() string {
	if a == nil || !a.IsQoderPlatform() {
		return ""
	}
	if baseURL := strings.TrimSpace(a.GetCredential("base_url")); baseURL != "" {
		return strings.TrimRight(baseURL, "/")
	}
	if a.GetQoderRealm() == "cn" {
		return DefaultQoderCNBaseURL
	}
	return DefaultQoderGlobalBaseURL
}

// QoderCredentials 是 qoder 账号凭据的归一化视图。
//
// AccessToken 语义（二选一必填）：
//   - dt-xxx：OAuth 设备令牌（Authorization Bearer 直连 openapi/网关）；
//   - securityOauthToken：PAT（pt-）经 jobToken 交换得到的会话令牌。
//
// RefreshToken 可选（drt- 刷新令牌，PAT 交换场景使用）；
// 用户直接粘贴 drt-/pt- 令牌时分别落入 RefreshToken/PersonalToken。
type QoderCredentials struct {
	AccessToken   string
	RefreshToken  string
	DeviceToken   string
	PersonalToken string // pt- PAT（可选：客户端凭据，交换 securityOauthToken 用）
	UID           string
	Realm         string
	Domain        string
	BaseURL       string
	Nickname      string
	ExpiresAt     int64
}

// GetQoderCredentials 从 credentials 读取 qoder 凭据（snake_case 为主，
// 兼容驼峰键以宽容直接粘贴的第三方导出格式）。
func (a *Account) GetQoderCredentials() QoderCredentials {
	if a == nil || !a.IsQoderPlatform() {
		return QoderCredentials{}
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
	creds := QoderCredentials{
		AccessToken:   read("access_token", "accessToken"),
		RefreshToken:  read("refresh_token", "refreshToken"),
		DeviceToken:   read("device_token", "deviceToken"),
		PersonalToken: read("personal_token", "personalToken"),
		UID:           read("uid", ""),
		Realm:         read("realm", ""),
		Domain:        read("domain", ""),
		BaseURL:       read("base_url", ""),
		Nickname:      read("nickname", ""),
	}
	if raw := read("expires_at", "expiresAt"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
			creds.ExpiresAt = v
		}
	}
	return creds
}

// qoderCredentialValueString 把凭据值转为字符串（兼容 JSON 反序列化的多种数字类型，
// 风格对齐 workbuddyCredentialValueString）。
func qoderCredentialValueString(v any) string {
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

// hasQoderCredentialKeys 报告凭据中是否携带任意 qoder 专属键。
func hasQoderCredentialKeys(credentials map[string]any) bool {
	for _, key := range []string{
		"access_token", "accessToken",
		"refresh_token", "refreshToken",
		"expires_at", "expiresAt",
		"device_token", "deviceToken",
		"personal_token", "personalToken",
		"realm", "domain", "uid", "nickname",
	} {
		if v, ok := credentials[key]; ok && v != nil {
			return true
		}
	}
	return false
}

// NormalizeQoderCredentials 校验并原地规范化 qoder 账号的 credentials：
//   - 兼容 JSON 整串粘贴：access_token / device_token / refresh_token 的值若是
//     以 "{" 开头的合法 JSON 对象字符串，递归取出其中的凭据键并归一为顶层；
//   - 把驼峰键归一为 snake_case 存回；
//   - realm 仅允许空 / "cn" / "global"（归一为小写）；
//   - 要求 access_token / device_token / personal_token / refresh_token 至少一个
//     非空（dt- 设备令牌可直用；drt-/pt- 由客户端在发送时交换）；
//   - 未识别的键原样保留、不报错；
//   - 凭据中不含任何 qoder 专属键时为 no-op，使批量更新等混合平台路径可安全调用。
func NormalizeQoderCredentials(credentials map[string]any) error {
	if credentials == nil {
		return nil
	}
	if !hasQoderCredentialKeys(credentials) {
		return nil
	}
	// JSON 整串粘贴兼容：值形如 {"access_token":...} 的对象字符串展开进顶层。
	for _, key := range []string{"access_token", "device_token", "refresh_token"} {
		raw, ok := credentials[key].(string)
		if !ok || !strings.HasPrefix(strings.TrimSpace(raw), "{") {
			continue
		}
		var nested map[string]any
		if err := json.Unmarshal([]byte(raw), &nested); err != nil || nested == nil {
			continue // 不是合法 JSON 对象：按原值保留（可能是伪装成对象形态的 token）
		}
		for _, nk := range []string{
			"access_token", "refresh_token", "device_token", "personal_token",
			"expires_at", "uid", "realm", "domain", "nickname",
		} {
			v, ok := nested[nk]
			if !ok || v == nil || qoderCredentialValueString(v) == "" {
				continue
			}
			if existing := qoderCredentialValueString(credentials[nk]); existing == "" {
				credentials[nk] = v
			}
		}
		delete(credentials, key)
	}
	// JSON 粘贴常见形态：嵌套只带 device_token（dt-）时提升为 access_token
	//（与凭据模型「device_token 为 access_token 的冗余」对齐，保证账号可用）。
	if qoderCredentialValueString(credentials["access_token"]) == "" {
		if dt := qoderCredentialValueString(credentials["device_token"]); dt != "" {
			credentials["access_token"] = dt
		}
	}
	// 驼峰 → snake 归一。
	for _, pair := range [][2]string{
		{"accessToken", "access_token"},
		{"refreshToken", "refresh_token"},
		{"expiresAt", "expires_at"},
		{"deviceToken", "device_token"},
		{"personalToken", "personal_token"},
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
		realm := strings.ToLower(qoderCredentialValueString(raw))
		switch realm {
		case "", "cn", "global":
			credentials["realm"] = realm
		default:
			return infraerrors.BadRequest("INVALID_QODER_CREDENTIALS", `realm must be empty, "cn", or "global"`)
		}
	}
	// token 必填校验：dt- 设备令牌 / securityOauthToken / drt- 刷新令牌 / pt- PAT
	// 任一存在即可（后两者在发送路径按需交换）。
	hasToken := false
	for _, key := range []string{"access_token", "device_token", "personal_token", "refresh_token"} {
		if qoderCredentialValueString(credentials[key]) != "" {
			hasToken = true
			break
		}
	}
	if !hasToken {
		return infraerrors.BadRequest("INVALID_QODER_CREDENTIALS",
			"access_token (dt-/securityOauthToken), device_token, personal_token (pt-) or refresh_token (drt-) is required")
	}
	return nil
}
