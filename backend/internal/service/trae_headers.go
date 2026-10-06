package service

// trae_headers.go Trae 出站请求头构造：聊天域、UG 域（积分/签到）、OAuth 域三套
// **互不兼容**的客户端指纹。
//
// 为什么必须三套：Trae 上游按请求头组合识别"是 IDE 主进程、是 IDE 内的插件进程、
// 还是登录换票流程"。实测纪律（参考 traework2api / dsh-router-traework 的断言测试）：
//   - UG 域（签到/积分）走的是 IDE 插件（VSCode 扩展）身份，UA 是 "VSCode x.y.z
//     (TRAE SOLO CN)"，且**不发 x-uid**——"多一个上游没见过的头 = 指纹异常"；
//   - 聊天域走 IDE 主进程身份，UA 是 "Trae/<version>"，必须带 x-uid；
//   - OAuth 域头族极简（只有 content-type/accept/UA），带任何 IDE 私有头反而可能被拒。
//
// 设备指纹（两套纪律**相反**，别互相"顺手复用"）：
//   - **聊天域**（IDE 主进程身份）：x-device-id / x-machine-id 用登录时与上游绑定的
//     那一对真值（凭据优先，缺省由账号 ID 稳定派生），同一账号必须恒定 —— 聊天域
//     每请求换设备号 = 画像漂移，会被风控盯上。
//   - **UG 签到族**（/trae/api/v2/ug/checkin_credits/status|claim）：x-device-id 必须
//     **每轮现抛一个新的 16 位随机数字**。取证依据（mmqz/cpa-multi-plugins@0.12.117
//     plugins/trae/upstream/checkin_device_test.go 包注释，v0.12.65，作者注明"被两次
//     反转才定下"）："the ug check-in x-device-id must be a fresh random 16-digit
//     numeric string — the login hex32 deviceId provably fails claim with 9074,
//     reused/deterministic numeric ids degrade over time, and an empty value yields
//     9004"。⇒ 把聊天域那对持久指纹拿去签到，正是 9074 的成因（固定值会随时间退化）。
//   - **UG 权益族**（/trae/api/v2/pay/ide_user_ent_usage 等非签到端点）：沿用**账号级
//     稳定** device_id，不轮换。同一个参考实现把边界写得很死（client.go ugCheckinRequest
//     v0.12.65 注释原文）："deviceID 非空时覆盖 ugBaseHeaders 的 X-Device-Id ——
//     签到族请求专用，其余 ug 端点（积分查询等）**不受影响**"；其 UgHeaders 对权益族
//     直接发 a.DeviceID（抓包成功基线）。⇒ 没有证据支持在余额探测上轮换设备号，就不
//     越界套用签到族纪律（那会给「余额显示不准」这条排查线新引入一个变量）。
//
// **不发 x-machine-id**：两套 UG 请求都适用（成功抓包的逐头清单里没有该头；参考实现
// ugBaseHeaders 同样不发，X-Machine-Id 只在聊天域 SOLOHeaders 里出现）。
//
// UG 域另外两个每请求新生成的头（同一参考实现 headers.go 的 2026-09-03 成功签到抓包）：
// x-request-id（uuid-v4）与 x-tt-trace-id（"00-<32hex>-<16hex>-01"，16hex 取自本次
// request-id），两者在**同一请求内配对**，换请求必须换 trace。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// Trae IDE 客户端版本画像（可在凭据里覆盖以跟随官方发版）。
	// ide_version_code 决定上游返回的模型表，写死会导致新模型在流内报 4001。
	defaultTraeIDEVersion     = "0.1.61"
	defaultTraeIDEVersionCode = "20260820"
	defaultTraeIDEVersionType = "stable"
	defaultTraeDeviceBrand    = "83DG"
	defaultTraeDeviceType     = "windows"
	defaultTraeOSVersion      = "Windows 11 Pro"
	defaultTraeDeviceCPU      = "Intel"
	defaultTraeRequestTraffic = "prod"
	defaultTraeVSCodeVersion  = "1.107.1"
	traeAppID                 = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	traeAppVersion            = "default"
	traeCNClientID            = "ono9krqynydwx5" // Trae CN IDE
	traeSOLOClientID          = "en1oxy7wnw8j9n" // SOLO / 国际版
	traeJWTAuthScheme         = "Cloud-IDE-JWT"
)

// traeEffective 返回第一个非空字符串（去空白后），用于凭据覆盖常量默认值。
func traeEffective(value, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return fallback
}

// traeUAForChat 聊天域 UA：Trae/<ide_version>。
func traeUAForChat(creds TraeCredentials) string {
	return "Trae/" + traeEffective(creds.IDEVersion, defaultTraeIDEVersion)
}

// traeUAForUG UG 域 UA：VSCode <ver> (TRAE SOLO CN) / (TRAE)。与聊天域不同源，
// 这是插件进程身份，签到接口只认这一套。
func traeUAForUG(creds TraeCredentials) string {
	if creds.Realm == "global" {
		return "VSCode " + defaultTraeVSCodeVersion + " (TRAE)"
	}
	return "VSCode " + defaultTraeVSCodeVersion + " (TRAE SOLO CN)"
}

// traeFreshCheckinDeviceID 现抛一个**新的** 16 位纯数字设备号，供 UG 域（签到/积分）
// 使用。纪律见文件头：签到设备号必须每请求新随机，复用或确定性派生的数字号会随时间
// 退化并被上游以 9074 拒绝（文案是"参与用户太多"，实际语义是"设备号无效"）。
//
// 绝不返回空串：空值上游直接判 9004（参数错误）。crypto/rand 实际不会失败，万一失败
// 也要退化到时刻派生的合法形态，不能因为取随机数失败就发一个空头出去。
func traeFreshCheckinDeviceID() string {
	if id, err := newTraeLoginDeviceID(); err == nil {
		return id
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("trae-fallback-device:%d", time.Now().UnixNano())))
	digits := strconv.FormatUint(uint64(sum[0])<<56|uint64(sum[1])<<48|uint64(sum[2])<<40|
		uint64(sum[3])<<32|uint64(sum[4])<<24|uint64(sum[5])<<16|uint64(sum[6])<<8|uint64(sum[7]), 10)
	for len(digits) < 16 {
		digits = "1" + digits
	}
	if len(digits) > 16 {
		digits = digits[:16]
	}
	if digits[0] == '0' {
		digits = "1" + digits[1:]
	}
	return digits
}

// traeUGStableDeviceID UG **权益族**（ide_user_ent_usage 等非签到端点）的设备号：沿用
// 账号级稳定值（凭据优先，缺省由账号 ID 派生）。取证：参考实现 UgHeaders 直接发
// a.DeviceID，并在新随机制度落地时把它限定为「签到族专用，其余 ug 端点不受影响」。
// 签到族不得用本函数（见 traeFreshCheckinDeviceID）。
func traeUGStableDeviceID(creds TraeCredentials, account *Account) string {
	deviceID, _ := traeResolveDeviceIdentity(creds, account)
	return deviceID
}

// traeNewUGRequestIDs 生成一对**同一请求内配对**的追踪标识（抓包实证：真实客户端每
// 请求现生成，且 trace-id 尾段取自 request-id）。返回 (requestID, traceID)。
func traeNewUGRequestIDs() (string, string) {
	requestID, err := newTraeUUIDv4()
	if err != nil {
		// newTraeUUIDv4 只依赖 crypto/rand，实际不会失败；兼容分支仍必须产出
		// **合法 8-4-4-4-12 且 version/variant 位正确**的形态（缺头或畸形的
		// request-id 同样是一个上游没见过的画像）。
		sum := sha256.Sum256([]byte(fmt.Sprintf("trae-rid:%d", time.Now().UnixNano())))
		h := hex.EncodeToString(sum[:]) // 恰好 32 个十六进制字符
		requestID = h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-8" + h[17:20] + "-" + h[20:32]
	}
	t := make([]byte, 16)
	if _, err := rand.Read(t); err != nil {
		derived := sha256.Sum256([]byte(requestID))
		copy(t, derived[:16])
	}
	traceID := "00-" + hex.EncodeToString(t) + "-" +
		strings.ReplaceAll(requestID, "-", "")[:16] + "-01"
	return requestID, traceID
}

// traeStableDeviceID 由账号 ID 派生 16 位纯数字设备号（**聊天域**兜底用：上游风控
// 画像要求设备号为数字串，hex/UUID 不是设备号）。签到/UG 域不得用它，见
// traeFreshCheckinDeviceID。
func traeStableDeviceID(account *Account) string {
	seed := traeFingerprintSeed(account)
	sum := sha256.Sum256([]byte("trae-device:" + seed))
	// 取前 8 字节十进制展开后裁到 16 位，首位非零（避免被上游当成空值）。
	digits := strconv.FormatUint(uint64(sum[0])<<56|uint64(sum[1])<<48|uint64(sum[2])<<40|
		uint64(sum[3])<<32|uint64(sum[4])<<24|uint64(sum[5])<<16|uint64(sum[6])<<8|uint64(sum[7]), 10)
	for len(digits) < 16 {
		digits = "1" + digits
	}
	if len(digits) > 16 {
		digits = digits[:16]
	}
	if digits[0] == '0' {
		digits = "1" + digits[1:]
	}
	return digits
}

// traeStableMachineID 由账号 ID 派生 32 位 hex 机器码。
func traeStableMachineID(account *Account) string {
	sum := sha256.Sum256([]byte("trae-machine:" + traeFingerprintSeed(account)))
	return hex.EncodeToString(sum[:16])
}

// traeFingerprintSeed 指纹派生种子：**只用账号 ID**（数据库主键，一行记录内永不变化）。
//
// 为什么不能用 uid 或 access_token 参与派生（两处都会漂移，而指纹漂移正是风控画像
// 最敏感的信号——真实客户端对同一账号始终发同一个设备号）：
//   - access_token 会随每次换票轮换（见 trae_token.go），拿它当种子等于「每几天换一次
//     设备号」，正是参考实现点名的反模式；
//   - uid 是建档后惰性回填的（换票时才从 JWT 补齐），种子会在「首次请求」与「回填
//     uid 之后」之间跳变一次，同样构成设备号突变。
//
// 账号 ID 在行的生命周期内恒定，且进程重启、token 轮换、uid 回填都不受影响。
// 需要固定真实设备号时，在凭据里显式提供 device_id / machine_id（凭据优先，见
// traeResolveDeviceIdentity）。
func traeFingerprintSeed(account *Account) string {
	if account == nil {
		return "trae-anonymous"
	}
	return "acct-" + strconv.FormatInt(account.ID, 10)
}

// traeResolveDeviceIdentity 解析账号实际使用的设备指纹（凭据优先，缺省稳定派生）。
func traeResolveDeviceIdentity(creds TraeCredentials, account *Account) (deviceID, machineID string) {
	deviceID = strings.TrimSpace(creds.DeviceID)
	machineID = strings.TrimSpace(creds.MachineID)
	if deviceID == "" {
		deviceID = traeStableDeviceID(account)
	}
	if machineID == "" {
		machineID = traeStableMachineID(account)
	}
	return deviceID, machineID
}

// traeTokenValue 去掉手动粘贴时可能带上的 scheme 前缀，返回裸 JWT。
// 上游两类形态都见过：完整 "Cloud-IDE-JWT eyJ..." 与裸 "eyJ..."。
func traeTokenValue(token string) string {
	token = strings.TrimSpace(token)
	for _, prefix := range []string{traeJWTAuthScheme + " ", "Bearer ", "bearer "} {
		if len(token) > len(prefix) && strings.EqualFold(token[:len(prefix)], prefix) {
			return strings.TrimSpace(token[len(prefix):])
		}
	}
	return token
}

// applyTraeChatHeaders 构造聊天域（llm_utils_chat / ide chat）出站头：IDE 主进程指纹。
func applyTraeChatHeaders(req *http.Request, creds TraeCredentials, account *Account) {
	ideVersion := traeEffective(creds.IDEVersion, defaultTraeIDEVersion)
	ideVersionCode := traeEffective(creds.IDEVersionCode, defaultTraeIDEVersionCode)
	deviceID, machineID := traeResolveDeviceIdentity(creds, account)

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", traeUAForChat(creds))
	if token := traeTokenValue(creds.AccessToken); token != "" {
		req.Header.Set("Authorization", traeJWTAuthScheme+" "+token)
		req.Header.Set("X-Cloudide-Token", token)
		req.Header.Set("X-Ide-Token", token)
	}
	req.Header.Set("X-App-Id", traeAppID)
	req.Header.Set("X-App-Version", traeAppVersion)
	req.Header.Set("X-App-Version-Code", ideVersionCode)
	req.Header.Set("X-Ide-Version", ideVersion)
	req.Header.Set("X-Ide-Version-Code", ideVersionCode)
	req.Header.Set("X-Ide-Version-Type", traeEffective("", defaultTraeIDEVersionType))
	req.Header.Set("X-Device-Type", traeEffective(creds.DeviceType, defaultTraeDeviceType))
	req.Header.Set("X-Device-Brand", traeEffective(creds.DeviceBrand, defaultTraeDeviceBrand))
	req.Header.Set("X-Device-Cpu", defaultTraeDeviceCPU)
	req.Header.Set("X-OS-Version", traeEffective(creds.OSVersion, defaultTraeOSVersion))
	req.Header.Set("X-Machine-Id", machineID)
	req.Header.Set("X-Device-Id", deviceID)
	req.Header.Set("Request-Traffic-Type", defaultTraeRequestTraffic)
	if uid := strings.TrimSpace(creds.UID); uid != "" {
		req.Header.Set("X-Uid", uid)
	}
}

// applyTraeBillingHeaders 构造 UG 域（权益用量查询 / 每日签到）出站头：IDE 插件进程指纹。
//
// 不接 *Account：机器码属于聊天域，UG 域一律不发；设备号由**调用方**按端点族算好后
// 传入（签到族走 traeFreshCheckinDeviceID，权益族走 traeUGStableDeviceID）。把账号
// 对象递进来只会诱使后来者对两族统一"顺手"取一个值 —— 那正是本纪律被两次反转的起点。
//
// 与聊天域的四处关键差异（均为实测/一手抓包换来的纪律）：
//  1. UA 是 VSCode/(TRAE SOLO CN) 而不是 Trae/<ver>；
//  2. **不发 X-Uid**（真实插件签到请求里没有该头，多发即指纹异常）；
//  3. **不发 X-Machine-Id**（抓包成功的签到请求逐头清单里没有它；多一个上游
//     没见过的头 = 指纹异常，本仓头文件自己就写着这条纪律）；
//  4. 设备号按族分流，并配一对每请求新生成的 X-Request-Id / X-TT-Trace-Id。
//
// deviceID 语义：
//   - 签到编排（status→claim→回查）必须显式传入**当轮新随机值**且全程同值（上游把它们
//     当配对校验参数，一轮内突变反而会被拒）；
//   - 权益用量查询传入 traeUGStableDeviceID 的账号稳定值（无证据支持在此轮换）；
//   - 传空串仅属兼容兜底：现抛一个新随机值，不静默发空头（空 = 9004）。
func applyTraeBillingHeaders(req *http.Request, creds TraeCredentials, deviceID string) {
	ideVersion := traeEffective(creds.IDEVersion, defaultTraeIDEVersion)
	if strings.TrimSpace(deviceID) == "" {
		deviceID = traeFreshCheckinDeviceID()
	}
	requestID, traceID := traeNewUGRequestIDs()

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("User-Agent", traeUAForUG(creds))
	if token := traeTokenValue(creds.AccessToken); token != "" {
		req.Header.Set("Authorization", traeJWTAuthScheme+" "+token)
	}
	req.Header.Set("X-Device-Id", deviceID)
	req.Header.Set("X-User-Region", traeUserRegion(creds))
	req.Header.Set("Package-Type", traePackageType(creds))
	req.Header.Set("App-Version", ideVersion)
	req.Header.Set("X-Market-Client-Id", "VSCode "+defaultTraeVSCodeVersion)
	req.Header.Set("X-Device-Brand", traeEffective(creds.DeviceBrand, defaultTraeDeviceBrand))
	req.Header.Set("X-Device-Type", traeEffective(creds.DeviceType, defaultTraeDeviceType))
	req.Header.Set("X-OS-Version", traeEffective(creds.OSVersion, defaultTraeOSVersion))
	req.Header.Set("X-Lgw-Req-Sdk-Type", "3")
	req.Header.Set("X-Request-Id", requestID)
	req.Header.Set("X-TT-Trace-Id", traceID)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "no-cors")
	req.Header.Set("Sec-Fetch-Site", "none")
}

// applyTraeRefreshHeaders 构造 OAuth 域（ExchangeToken / GetUserInfo）出站头：
// 登录换票流程的头族刻意极简，带上 IDE 私有头反而可能被拒。
func applyTraeRefreshHeaders(req *http.Request, creds TraeCredentials, accessToken string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", traeUAForChat(creds))
	if token := traeTokenValue(accessToken); token != "" {
		req.Header.Set("X-Cloudide-Token", token)
	}
}

// traeUserRegion UG 域地区标识：CN 为 CN，国际为 SG（缺省按 realm）。
func traeUserRegion(creds TraeCredentials) string {
	if creds.Realm == "global" {
		return "SG"
	}
	return "CN"
}

// traePackageType UG 域包类型标识。
func traePackageType(creds TraeCredentials) string {
	if creds.Realm == "global" {
		return "stable"
	}
	return "stable_cn"
}

// traeOAuthClientID 换票 client_id：凭据显式覆盖优先，否则 SOLO/国际用 SOLO 值、
// CN IDE 用 IDE 值。
func traeOAuthClientID(creds TraeCredentials) string {
	if clientID := strings.TrimSpace(creds.ClientID); clientID != "" {
		return clientID
	}
	if creds.Realm == "global" {
		return traeSOLOClientID
	}
	return traeCNClientID
}
