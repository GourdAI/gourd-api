package geminicli

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
)

// CLIVersionEnv 是 CLICurrentVersion 的可选运维覆盖。
//
// 存在的理由：Google 会对新模型/新端点设客户端版本下限，命中时上游直接返回
// 版本不支持的报错（例如 400 "Client version X.Y.Z is no longer supported"）。
// 在没有本开关之前，这类变更必须等 sub2api 发一个新版本才能适配，
// 而改动本身只是一个常量。claude 包的 SUB2API_CLAUDE_CLI_VERSION 与
// xai 包的 XAI_GROK_CLI_VERSION 都是同样的做法。
//
// 必须通过进程级环境变量注入（配置文件/后台设置不生效），因为版本号在包初始化时解析一次。
const CLIVersionEnv = "SUB2API_GEMINI_CLI_VERSION"

// resolvedCLIVersion 在包初始化时解析一次。
//
// ⚠️ 故意不做成"每次调用读一次环境变量"：伪装身份必须在一个进程的生命周期内保持恒定。
// User-Agent 由多个代码路径写入（Code Assist 客户端、messages/chat 兼容层、账号测试、
// OAuth 辅助请求），若两次读到不同的值（例如进程运行中有人改了环境变量），
// 同一个账号的请求就会呈现不一致的客户端版本，被上游判为非正版客户端。
var resolvedCLIVersion = resolveCLIVersion(os.Getenv(CLIVersionEnv))

// CLIVersion 返回对外伪装的 Gemini CLI 版本号（三段 semver）。
//
// 所有需要该版本号的位置都必须走本函数，不要直接引用 CLICurrentVersion——
// 后者只是"没有覆盖时的内置基线"。
func CLIVersion() string {
	return resolvedCLIVersion
}

// IsSupportedCLIVersion 判断运维给的覆盖值是否可用。
//
// 判据有两条，缺一不可：
//  1. 严格三段纯数字（"0.60.0"）。带 -local / -dev / +build 等后缀的版本号会让
//     上游把请求当作非官方客户端；一旦漏进去，此后所有上游请求都声称这个版本。
//  2. 不低于内置基线 CLICurrentVersion。向下覆盖没有任何使用场景——
//     旧版本反而更容易触发上游的版本下限拦截。
func IsSupportedCLIVersion(version string) bool {
	version = strings.TrimSpace(version)
	if version == "" {
		return false
	}
	// semver 允许 "v1.2" 与预发布/构建元数据，这里都不接受：
	// Canonical 相等可排除省略段，再显式排除预发布与构建元数据。
	canonical := "v" + version
	if !semver.IsValid(canonical) || semver.Canonical(canonical) != canonical {
		return false
	}
	if semver.Prerelease(canonical) != "" || semver.Build(canonical) != "" {
		return false
	}
	return semver.Compare(canonical, "v"+CLICurrentVersion) >= 0
}

// resolveCLIVersion 把环境变量的原始值解析成可用的版本号。
// 空值静默回落（未配置是正常状态）；非空但不合法则回落并告警——
// 静默忽略一个显式配置会让运维以为已经生效。
func resolveCLIVersion(raw string) string {
	version := strings.TrimSpace(raw)
	if version == "" {
		return CLICurrentVersion
	}
	if !IsSupportedCLIVersion(version) {
		slog.Warn("ignoring invalid Gemini CLI version override; falling back to the built-in pin",
			"env", CLIVersionEnv,
			"value", version,
			"builtin", CLICurrentVersion,
			"requirement", "strict three-part semver, not older than the built-in pin")
		return CLICurrentVersion
	}
	return version
}

// CLIUserAgent 构造官方现行形态的 User-Agent：
//
//	GeminiCLI/<version>/<model> (<platform>; <arch>; terminal)
//
// 官方客户端（google-gemini/gemini-cli contentGenerator.ts）按此三段式拼装；
// 旧形态 "GeminiCLI/<version> (<Platform>; <ARCH>)" 已与官方现行流量不一致。
// model 为空时退化为无 model 段的两段式，避免出现空段。
//
// Code Assist 路径应使用 CLIUserAgentWithAuthLibrary（官方经 google-auth-library
// 会追加产品后缀）；AI Studio 路径使用 CLIUserAgent。
func CLIUserAgent(model string) string {
	model = strings.TrimSpace(model)
	version := CLIVersion()
	platform, arch := platformTokens()
	if model == "" {
		return "GeminiCLI/" + version + " (" + platform + "; " + arch + "; terminal)"
	}
	return "GeminiCLI/" + version + "/" + model + " (" + platform + "; " + arch + "; terminal)"
}

// GoogleAPINodeClientSuffix 是 google-auth-library 拦截器追加到 UA 末尾的产品标识。
// 官方 Code Assist 出站请求的完整 UA 形如：
//
//	GeminiCLI/<version>/<model> (<platform>; <arch>; terminal) google-api-nodejs-client/10.9.0
const GoogleAPINodeClientSuffix = "google-api-nodejs-client/10.9.0"

// CLIUserAgentWithAuthLibrary 返回带 google-auth-library 产品后缀的 Code Assist UA。
func CLIUserAgentWithAuthLibrary(model string) string {
	return CLIUserAgent(model) + " " + GoogleAPINodeClientSuffix
}

// CodeAssistAPIRequestHeader 是 google-auth-library 对 Code Assist 路径
// 自动附加的指纹头（AuthClient.DEFAULT_REQUEST_INTERCEPTOR）：
//
//	x-goog-api-client: gl-node/<nodeVersion>
//
// 官方 Gemini CLI 锁定 google-auth-library 10.x，该头在所有 Code Assist
// 出站请求上恒定出现；缺失会让请求看起来不像官方 Node 客户端。
const CodeAssistAPIClientHeader = "x-goog-api-client"

// CodeAssistNodeClientValue 是 gl-node/<nodeVersion> 的取值。
// 与 antigravity 包中既有先例保持一致（client.go 的 gl-node/22.21.1）。
const CodeAssistNodeClientValue = "gl-node/22.21.1"

// AIStudioSDKClientValue 是 AI Studio（generativelanguage）路径上
// @google/genai SDK 自动设置的 x-goog-api-client 取值。
//
// 官方实际值为 `google-genai-sdk/1.30.0 gl-node/v22.21.1`（两 token）；
// 本网关有意采用单 token 简化形式，避免与 CodeAssist 路径的 gl-node 值耦合。
//
// 官方 Gemini CLI 0.60.0 锁定 @google/genai 1.30.0。
const AIStudioSDKClientValue = "google-genai-sdk/1.30.0"

// ApplyCodeAssistFingerprintHeaders 为 Code Assist 系出站请求补齐官方指纹头。
// 幂等：已存在同名头时不覆盖（保留调用方的显式配置）。
func ApplyCodeAssistFingerprintHeaders(h interface {
	Set(string, string)
	Get(string) string
}) {
	if h == nil {
		return
	}
	if strings.TrimSpace(h.Get(CodeAssistAPIClientHeader)) == "" {
		h.Set(CodeAssistAPIClientHeader, CodeAssistNodeClientValue)
	}
}

// ApplyAIStudioFingerprintHeaders 为 AI Studio 系出站请求补齐官方指纹头。
// 幂等：已存在同名头时不覆盖。
func ApplyAIStudioFingerprintHeaders(h interface {
	Set(string, string)
	Get(string) string
}) {
	if h == nil {
		return
	}
	if strings.TrimSpace(h.Get(CodeAssistAPIClientHeader)) == "" {
		h.Set(CodeAssistAPIClientHeader, AIStudioSDKClientValue)
	}
}

// platformTokens 返回官方 UA 使用的 platform/arch 段。
//
// 官方取值来自 process.platform / process.arch（小写）：win32 / darwin / linux、
// x64 / arm64。网关侧把 Go 的运行时标识映射到同一命名空间；
// 仅 windows / amd64 / 386 与官方命名不同，需要显式改写，
// darwin / linux / arm64 等未列出的取值本来就在同一命名空间内，统一走 default 小写化。
func platformTokens() (string, string) {
	platform := runtime.GOOS
	switch runtime.GOOS {
	case "windows":
		platform = "win32"
	default:
		platform = strings.ToLower(platform)
	}

	arch := runtime.GOARCH
	switch runtime.GOARCH {
	case "amd64":
		arch = "x64"
	case "386":
		arch = "ia32"
	default:
		arch = strings.ToLower(arch)
	}
	return platform, arch
}

// InstallationID 按账号稳定派生一个 UUID 形态的 installation id。
//
// 对齐官方语义（utils/installationManager.ts）：官方每个安装持有一个随机 UUID，
// 用于 API-key 路径的 x-gemini-api-privileged-user-id 头与遥测；
// 网关侧照 workbuddy_headers.go 的盐式派生，为每个账号派生一个
// 「固定盐、账号间互异、跨重启稳定」的等价标识。
// 派生结果按 RFC 4122 置位 version 4 / variant 位，与官方 randomUUID() 的形态一致。
func InstallationID(uid string) string {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("gcli:installation:" + uid))
	// sum 是数组，先拷贝到切片再改写 version/variant 位。
	b := make([]byte, 16)
	copy(b, sum[:16])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	h := hex.EncodeToString(b)  // 32 hex chars
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// InstallationIDForAccount 按账号 ID 派生稳定的 installation id。
// 账号 ID 缺失（<=0）时返回空串，调用方应跳过该头。
func InstallationIDForAccount(accountID int64) string {
	if accountID <= 0 {
		return ""
	}
	return InstallationID("acct-" + strconv.FormatInt(accountID, 10))
}
