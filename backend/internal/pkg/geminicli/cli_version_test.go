package geminicli

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestCLIUserAgentShape 验证 UA 与官方现行三段式格式一致：
//
//	GeminiCLI/<version>/<model> (<platform>; <arch>; terminal)
func TestCLIUserAgentShape(t *testing.T) {
	ua := CLIUserAgent("gemini-2.5-pro")
	pattern := regexp.MustCompile(`^GeminiCLI/\d+\.\d+\.\d+/[^\s]+ \([a-z0-9]+; [a-z0-9]+; terminal\)$`)
	if !pattern.MatchString(ua) {
		t.Fatalf("UA %q does not match the official three-segment shape", ua)
	}
	if !strings.Contains(ua, "/"+CLIVersion()+"/") {
		t.Fatalf("UA %q must embed the resolved CLI version %q", ua, CLIVersion())
	}
}

// TestCLIUserAgentWithoutModel 验证无 model 时退化为两段式，不出现空段。
func TestCLIUserAgentWithoutModel(t *testing.T) {
	ua := CLIUserAgent("")
	if strings.Contains(ua, "//") {
		t.Fatalf("UA %q must not contain an empty model segment", ua)
	}
	pattern := regexp.MustCompile(`^GeminiCLI/\d+\.\d+\.\d+ \([a-z0-9]+; [a-z0-9]+; terminal\)$`)
	if !pattern.MatchString(ua) {
		t.Fatalf("UA %q does not match the no-model shape", ua)
	}
}

// TestCLIUserAgentWithAuthLibrary 验证 Code Assist UA 以 google-auth-library
// 产品后缀结尾，且仍内嵌解析后的版本号。
func TestCLIUserAgentWithAuthLibrary(t *testing.T) {
	ua := CLIUserAgentWithAuthLibrary("gemini-2.5-pro")
	if !strings.HasSuffix(ua, " "+GoogleAPINodeClientSuffix) {
		t.Fatalf("UA %q must end with the auth-library product suffix %q", ua, GoogleAPINodeClientSuffix)
	}
	if !strings.Contains(ua, "/"+CLIVersion()+"/") {
		t.Fatalf("UA %q must embed the resolved CLI version %q", ua, CLIVersion())
	}
}

// TestCLIVersionFallsBackToBuiltin 验证默认（无覆盖）时使用内置基线。
func TestCLIVersionFallsBackToBuiltin(t *testing.T) {
	if got := CLIVersion(); got != CLICurrentVersion {
		// 仅当测试环境显式设置了覆盖变量时允许不同；此处按默认环境断言。
		if _, ok := lookupOverride(); !ok {
			t.Fatalf("CLIVersion() = %q, want builtin %q", got, CLICurrentVersion)
		}
	}
}

// TestIsSupportedCLIVersion 校验版本合法性的两条判据。
func TestIsSupportedCLIVersion(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{CLICurrentVersion, true},
		{"0.60.1", true},   // 高于基线
		{"1.0.0", true},    // 更高主版本
		{"0.59.0", false},  // 低于基线
		{"0.1.5", false},   // 旧硬编码值，低于基线
		{"", false},        // 空
		{"0.60", false},    // 省略段
		{"v0.60.0", false}, // 带 v 前缀
		{"0.60.0-dev", false},
		{"0.60.0+build.1", false},
		{"0.61.0-dev", false},     // 高于基线的预发布：锁定 Prerelease 分支
		{"0.61.0+build.1", false}, // 高于基线的构建元数据：锁定 Build 分支
		{"abc", false},
	}
	for _, tc := range cases {
		if got := IsSupportedCLIVersion(tc.version); got != tc.want {
			t.Errorf("IsSupportedCLIVersion(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

// TestResolveCLIVersionInvalidOverrideFallsBack 验证非法覆盖回落且不 panic。
func TestResolveCLIVersionInvalidOverrideFallsBack(t *testing.T) {
	if got := resolveCLIVersion("0.1.5"); got != CLICurrentVersion {
		t.Fatalf("resolveCLIVersion(\"0.1.5\") = %q, want fallback %q", got, CLICurrentVersion)
	}
	if got := resolveCLIVersion("  "); got != CLICurrentVersion {
		t.Fatalf("blank override should fall back, got %q", got)
	}
	if got := resolveCLIVersion("9.9.9"); got != "9.9.9" {
		t.Fatalf("valid higher override should be kept, got %q", got)
	}
}

// TestInstallationIDStableAndUUIDShaped 验证设备标识的稳定性与 UUID 形态。
func TestInstallationIDStableAndUUIDShaped(t *testing.T) {
	a := InstallationID("acct-1")
	b := InstallationID("acct-1")
	c := InstallationID("acct-2")

	if a != b {
		t.Fatalf("InstallationID must be stable for the same uid: %q vs %q", a, b)
	}
	if a == c {
		t.Fatalf("InstallationID must differ across uids: %q vs %q", a, c)
	}
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !pattern.MatchString(a) {
		t.Fatalf("InstallationID %q is not UUID v4-shaped", a)
	}
	if got := InstallationID(""); got != "" {
		t.Fatalf("empty uid must yield empty id, got %q", got)
	}
}

// TestInstallationIDForAccount 验证账号 ID 派生与非法输入。
func TestInstallationIDForAccount(t *testing.T) {
	if got := InstallationIDForAccount(0); got != "" {
		t.Fatalf("account 0 must yield empty id, got %q", got)
	}
	if got := InstallationIDForAccount(-1); got != "" {
		t.Fatalf("negative account must yield empty id, got %q", got)
	}
	if got := InstallationIDForAccount(42); got == "" {
		t.Fatalf("valid account must yield non-empty id")
	}
}

// TestFingerprintHeadersIdempotent 验证指纹头幂等（不覆盖已有值）。
func TestFingerprintHeadersIdempotent(t *testing.T) {
	h := map[string]string{}
	set := func(k, v string) { h[k] = v }
	get := func(k string) string { return h[k] }

	ApplyCodeAssistFingerprintHeaders(fingerprintSetter{set: set, get: get})
	if h[CodeAssistAPIClientHeader] != CodeAssistNodeClientValue {
		t.Fatalf("code assist header not applied: %v", h)
	}

	h[CodeAssistAPIClientHeader] = "custom/1.0"
	ApplyCodeAssistFingerprintHeaders(fingerprintSetter{set: set, get: get})
	if h[CodeAssistAPIClientHeader] != "custom/1.0" {
		t.Fatalf("existing header must not be overwritten, got %q", h[CodeAssistAPIClientHeader])
	}

	h2 := map[string]string{}
	set2 := func(k, v string) { h2[k] = v }
	get2 := func(k string) string { return h2[k] }
	ApplyAIStudioFingerprintHeaders(fingerprintSetter{set: set2, get: get2})
	if h2[CodeAssistAPIClientHeader] != AIStudioSDKClientValue {
		t.Fatalf("ai studio header not applied: %v", h2)
	}

	h2[CodeAssistAPIClientHeader] = "custom/2.0"
	ApplyAIStudioFingerprintHeaders(fingerprintSetter{set: set2, get: get2})
	if h2[CodeAssistAPIClientHeader] != "custom/2.0" {
		t.Fatalf("existing header must not be overwritten, got %q", h2[CodeAssistAPIClientHeader])
	}

	// nil 入参不得 panic。
	ApplyCodeAssistFingerprintHeaders(nil)
	ApplyAIStudioFingerprintHeaders(nil)
}

type fingerprintSetter struct {
	set func(string, string)
	get func(string) string
}

func (f fingerprintSetter) Set(k, v string) { f.set(k, v) }
func (f fingerprintSetter) Get(k string) string {
	return f.get(k)
}

// lookupOverride 报告测试环境是否设置了版本覆盖变量。
func lookupOverride() (string, bool) {
	return os.LookupEnv(CLIVersionEnv)
}
