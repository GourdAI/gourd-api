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
// 设备指纹：x-device-id 必须是 15~16 位纯数字（发 hex/UUID 在风控眼里不是设备号，
// 签到直接 9074），x-machine-id 为 32 位 hex。凭据缺省时由账号 ID 稳定派生：
// 同一账号每次请求、重启后都得到同一组指纹，避免"每请求换设备号"被判定为异常。

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
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

// traeStableDeviceID 由账号 ID 派生 16 位纯数字设备号（上游风控画像要求设备号为
// 数字串；hex/UUID 会被判为非法设备号 → 签到 9074）。
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

// applyTraeBillingHeaders 构造 UG 域（积分查询 / 每日签到）出站头：IDE 插件进程指纹。
//
// 与聊天域的三处关键差异（均为实测换来的纪律）：
//  1. UA 是 VSCode/(TRAE SOLO CN) 而不是 Trae/<ver>；
//  2. **不发 X-Uid**（真实插件签到请求里没有该头，多发即指纹异常）；
//  3. Accept 为 */*、Sec-Fetch-* 三件套齐全（插件走 fetch no-cors 形态）。
func applyTraeBillingHeaders(req *http.Request, creds TraeCredentials, account *Account) {
	ideVersion := traeEffective(creds.IDEVersion, defaultTraeIDEVersion)
	deviceID, machineID := traeResolveDeviceIdentity(creds, account)

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("User-Agent", traeUAForUG(creds))
	if token := traeTokenValue(creds.AccessToken); token != "" {
		req.Header.Set("Authorization", traeJWTAuthScheme+" "+token)
	}
	req.Header.Set("X-Device-Id", deviceID)
	req.Header.Set("X-Machine-Id", machineID)
	req.Header.Set("X-User-Region", traeUserRegion(creds))
	req.Header.Set("Package-Type", traePackageType(creds))
	req.Header.Set("App-Version", ideVersion)
	req.Header.Set("X-Market-Client-Id", "VSCode "+defaultTraeVSCodeVersion)
	req.Header.Set("X-Device-Brand", traeEffective(creds.DeviceBrand, defaultTraeDeviceBrand))
	req.Header.Set("X-Device-Type", traeEffective(creds.DeviceType, defaultTraeDeviceType))
	req.Header.Set("X-OS-Version", traeEffective(creds.OSVersion, defaultTraeOSVersion))
	req.Header.Set("X-Lgw-Req-Sdk-Type", "3")
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
