package admin

// preview_sync_credentials.go 「同步上游模型」preview 入口（未落库凭据）的凭据白名单。
//
// 背景：SyncUpstreamModelsPreview 原先只接收 platform / type / base_url / api_key /
// model_mapping，其中 api_key 还是 binding:"required"。而 WorkBuddy / Trae / Qoder
// 三个固定协议平台的目录拉取（service/workbuddy_models.go、trae_models.go、
// qoder_models.go，由 upstream_models_native.go 分派）读取的是
// credentials.access_token / personal_token / realm / uid / device_token 等字段，
// 拿 api_key 一条都喂不进去 —— 结果是新建账号页对这三个平台无法在落库前拉取上游
// 目录，管理员只能「先建号 → 再进编辑页点同步」，白做一遍。
//
// 本文件把 preview 的入参扩展到这三平台所需的凭据族，同时用**显式白名单**收口：
// 请求体里的 credentials 只允许下列键进入临时账号对象，未知键直接丢弃。这不是
// 走形式 —— 该 handler 允许管理员提交任意凭据内容，若照单全收，就能通过伪造
// account_mode / api_protocol / header_overrides 一类控制键去试探下游行为分支；
// 白名单保证 preview 能触及的只有「身份票据 + 地域/端点 + 设备画像」这些
// 目录拉取确实需要的字段。
//
// 键名一律归一到 snake_case：service 层三个 GetXxxCredentials() 同时兼容 snake 与
// camel（见 trae.go / qoder.go / workbuddy.go 里的 read(...) 调用），但落一份规范
// 形态便于排查，也避免同一含义出现两个键时取到意外的那个。
//
// 临时账号 ID 恒为 0：目录拉取链路里的 token 刷新会调 persistAccountCredentials，
// 该函数已对 ID<=0 早退（只更新内存、不写库），因此 preview 不会污染 accounts 表。

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// previewSyncCredentialKeys 是 preview 允许透传的凭据键：原始键 → 归一后的规范键。
var previewSyncCredentialKeys = map[string]string{
	// 共享：身份票据与地域/端点
	"access_token":  "access_token",
	"accessToken":   "access_token",
	"refresh_token": "refresh_token",
	"refreshToken":  "refresh_token",
	"realm":         "realm",
	"uid":           "uid",
	"userId":        "uid",
	"user_id":       "uid",
	"nickname":      "nickname",
	"domain":        "domain",
	"base_url":      "base_url",
	"baseUrl":       "base_url",

	// WorkBuddy / Qoder：设备令牌与企业标识
	"device_token":  "device_token",
	"deviceToken":   "device_token",
	"enterprise_id": "enterprise_id",
	"enterpriseId":  "enterprise_id",

	// Qoder：PAT（pt- 开头，目录拉取的换票种子）
	"personal_token": "personal_token",
	"personalToken":  "personal_token",

	// 过期时刻（决定同步前要不要先换票）
	"expires_at":         "expires_at",
	"expiresAt":          "expires_at",
	"refresh_expires_at": "refresh_expires_at",
	"refreshExpiresAt":   "refresh_expires_at",

	// Trae：设备画像与版本指纹。
	// 必须透传 device_id / machine_id 的理由：临时账号 ID=0 使 traeFingerprintSeed 恒为
	// "acct-0"，派生出的 X-Device-Id / X-Machine-Id 永远同一个值（traeStableDeviceID）；
	// 而 traeResolveDeviceIdentity 是「凭据优先、缺天才派生」，把表单真值递进来就能
	// 避开这个同一指纹问题。
	// 关于 ide_version / ide_version_code：**不要把本段当成「抬高版本就能多拉模型」的
	// 入口** —— 上游按 function 分片下发可调表（见 traeCatalogFunction），版本码新旧不决定
	// 表内容。2026-09-29 实测：把版本头换成更新的 IDE 真值 1.107.1/20260212，拉到的表反而
	// 少了 glm-5.3-flash（详见 trae_sse.go 里 traeModelUnavailableHint 的纠正记录）。
	// 本处只作为「表单已有值的透传位」保留，缺省仍走 0.1.61/20260820。
	"device_id":        "device_id",
	"deviceId":         "device_id",
	"machine_id":       "machine_id",
	"machineId":        "machine_id",
	"ide_version":      "ide_version",
	"ideVersion":       "ide_version",
	"ide_version_code": "ide_version_code",
	"ideVersionCode":   "ide_version_code",
	"device_brand":     "device_brand",
	"deviceBrand":      "device_brand",
	"device_type":      "device_type",
	"deviceType":       "device_type",
	"os_version":       "os_version",
	"osVersion":        "os_version",
	"oauth_base_url":   "oauth_base_url",
	"oauthBaseUrl":     "oauth_base_url",
	"billing_base_url": "billing_base_url",
	"billingBaseUrl":   "billing_base_url",
	"client_id":        "client_id",
	"req_source":       "req_source",
	"api_key":          "api_key",
	"apiKey":           "api_key",
}

// model_mapping 不在表内：它由顶格字段 req.ModelMapping 单独处理（需要保留对象
// 形态，而本表只做标量转字符串），放在这里只会诱使后来者以为能从 credentials 透传。

// 故意**不**放进白名单的控制类键（不在表内即被丢弃，这里只是把理由写清楚）：
//   - header_overrides / system_credentials：可以直接改写出站头族，preview 不该有这权限；
//   - account_mode / api_protocol：改变转发协议分支，目录拉取用不到它们；
//   - proxy_id：临时账号不会加载 Proxy 关联（upstreamModelsProxyURL 要求 Proxy 非空），
//     传了也无效果，而「拿任意代理去打上游」正是该接口最该避免的形象。

// previewSyncCredentialString 把 JSON 值转为字符串（兼容 json.Number / float64 等
// 反序列化形态：expires_at 之类的时间戳经 encoding/json 会变成 float64，直接
// FormatFloat 会给出 1.78e+09 这种带指数的形态，上游不认；整数优先走 FormatInt。
// 风格对齐 service 层的 xxxCredentialValueString）。
func previewSyncCredentialString(value any) string {
	switch val := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(val)
	case json.Number:
		return strings.TrimSpace(val.String())
	case float64:
		if val == math.Trunc(val) && math.Abs(val) < 1e15 {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case bool:
		if val {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}
