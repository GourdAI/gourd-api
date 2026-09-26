//go:build unit

package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// trae_headers_test.go 锁住"三套互不兼容指纹头"这一实测纪律（业务纪律 7、8）：
// UG 域必带设备指纹且绝不发 X-Uid、聊天域必带 X-Uid 与三个 token 头、OAuth 域头族极简。

var (
	traeDeviceIDPattern  = regexp.MustCompile(`^\d{15,16}$`)
	traeMachineIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// traeHeaderProbe 假上游：记录每次出站请求的完整头快照。
type traeHeaderProbe struct {
	mu      sync.Mutex
	headers []http.Header
	paths   []string
}

func (p *traeHeaderProbe) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = body
		p.mu.Lock()
		p.headers = append(p.headers, r.Header.Clone())
		p.paths = append(p.paths, r.URL.Path)
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0}`))
	}
}

func (p *traeHeaderProbe) last(t *testing.T) http.Header {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	require.NotEmpty(t, p.headers, "假上游未收到任何请求")
	return p.headers[len(p.headers)-1]
}

// postWith 用真实 HTTP 客户端发送一次带指定头的请求，让头经 wire 往返后被记录。
func postWith(t *testing.T, server *httptest.Server, path string, apply func(*http.Request)) http.Header {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, server.URL+path, nil)
	require.NoError(t, err)
	apply(req)
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)
	return nil
}

func traeHeaderTestCreds(realm string) TraeCredentials {
	return TraeCredentials{
		AccessToken:  "eyJhbGciOiJBTEST.payload.sig",
		RefreshToken: "rt-1",
		UID:          "7001234567890123456",
		Realm:        realm,
	}
}

func TestTraeBillingHeadersCarryDeviceFingerprintAndOmitUID(t *testing.T) {
	t.Parallel()
	probe := &traeHeaderProbe{}
	server := httptest.NewServer(probe.handler())
	defer server.Close()

	account := traeAccount(101, map[string]any{"access_token": "at-101", "uid": "777"})
	creds := account.GetTraeCredentials()
	creds.AccessToken = "Cloud-IDE-JWT eyJhbGciOiJIUzI1NiA.payload.sig"

	postWith(t, server, traeCheckinStatusPath, func(req *http.Request) {
		applyTraeBillingHeaders(req, creds, account)
	})
	header := probe.last(t)

	// 纪律 7：UG 域三件套必须齐全。
	require.NotEmpty(t, header.Get("X-Device-Id"))
	require.NotEmpty(t, header.Get("X-Machine-Id"))
	require.Equal(t, "CN", header.Get("X-User-Region"))
	// 纪律 7：绝不发 X-Uid（真实插件请求无该头，多发即指纹异常）。
	require.Empty(t, header.Get("X-Uid"), "UG 域不得发 X-Uid")
	require.Empty(t, header.Get("X-Ide-Token"))
	require.Empty(t, header.Get("X-Cloudide-Token"))
	require.Empty(t, header.Get("X-App-Id"))
	require.Empty(t, header.Get("X-Ide-Version-Code"))

	// 插件进程身份：UA 是 VSCode/(TRAE SOLO CN)，Accept 为 */*。
	require.Contains(t, header.Get("User-Agent"), "VSCode ")
	require.Contains(t, header.Get("User-Agent"), "TRAE SOLO CN")
	require.Equal(t, "*/*", header.Get("Accept"))
	require.Equal(t, "stable_cn", header.Get("Package-Type"))
	require.Equal(t, "3", header.Get("X-Lgw-Req-Sdk-Type"))
	require.Equal(t, "no-cors", header.Get("Sec-Fetch-Mode"))
	// token 仍是 Cloud-IDE-JWT 前缀 + 裸 JWT（前缀已在 traeTokenValue 里剥掉再拼接）。
	require.Equal(t, "Cloud-IDE-JWT eyJhbGciOiJIUzI1NiA.payload.sig", header.Get("Authorization"))
}

func TestTraeBillingHeadersForGlobalRealm(t *testing.T) {
	t.Parallel()
	probe := &traeHeaderProbe{}
	server := httptest.NewServer(probe.handler())
	defer server.Close()

	global := traeHeaderTestCreds("global")
	postWith(t, server, traeEntUsagePath, func(req *http.Request) {
		applyTraeBillingHeaders(req, global, traeAccount(102, map[string]any{"realm": "global", "access_token": "at"}))
	})
	header := probe.last(t)
	require.Equal(t, "SG", header.Get("X-User-Region"))
	require.Equal(t, "stable", header.Get("Package-Type"))
	require.Contains(t, header.Get("User-Agent"), "(TRAE)")
	require.NotContains(t, header.Get("User-Agent"), "SOLO")
	require.Empty(t, header.Get("X-Uid"))
}

func TestTraeChatHeadersCarryUIDAndTokenTriple(t *testing.T) {
	t.Parallel()
	probe := &traeHeaderProbe{}
	server := httptest.NewServer(probe.handler())
	defer server.Close()

	creds := TraeCredentials{
		AccessToken:    "Bearer eyJchat.token",
		UID:            "700999",
		IDEVersion:     "0.1.70",
		IDEVersionCode: "20261010",
		Realm:          "cn",
	}
	postWith(t, server, traeChatPath, func(req *http.Request) {
		applyTraeChatHeaders(req, creds, traeAccount(103, map[string]any{"uid": "700999"}))
	})
	header := probe.last(t)

	// 纪律 7：聊天域必带 X-Uid 与三个 token 头。
	require.Equal(t, "700999", header.Get("X-Uid"))
	require.Equal(t, "Cloud-IDE-JWT eyJchat.token", header.Get("Authorization"))
	require.Equal(t, "eyJchat.token", header.Get("X-Cloudide-Token"))
	require.Equal(t, "eyJchat.token", header.Get("X-Ide-Token"))
	require.Equal(t, "text/event-stream", header.Get("Accept"))
	require.Equal(t, "Trae/0.1.70", header.Get("User-Agent"))
	require.Equal(t, "20261010", header.Get("X-Ide-Version-Code"))
	require.Equal(t, "20261010", header.Get("X-App-Version-Code"))
	require.Equal(t, traeAppID, header.Get("X-App-Id"))
	// 聊天域同样带设备指纹（IDE 主进程身份）。
	require.Regexp(t, traeDeviceIDPattern, header.Get("X-Device-Id"))
	require.Regexp(t, traeMachineIDPattern, header.Get("X-Machine-Id"))
}

func TestTraeChatHeadersWithoutTokenOmitAuthTriple(t *testing.T) {
	t.Parallel()
	probe := &traeHeaderProbe{}
	server := httptest.NewServer(probe.handler())
	defer server.Close()

	postWith(t, server, traeChatPath, func(req *http.Request) {
		applyTraeChatHeaders(req, TraeCredentials{UID: "u-only"}, traeAccount(104, map[string]any{"uid": "u-only"}))
	})
	header := probe.last(t)
	require.Empty(t, header.Get("Authorization"), "无 access_token 时不得发空 Authorization")
	require.Empty(t, header.Get("X-Cloudide-Token"))
	require.Empty(t, header.Get("X-Ide-Token"))
	require.Equal(t, "u-only", header.Get("X-Uid"))
}

func TestTraeOAuthHeadersAreMinimal(t *testing.T) {
	t.Parallel()
	probe := &traeHeaderProbe{}
	server := httptest.NewServer(probe.handler())
	defer server.Close()

	creds := traeHeaderTestCreds("cn")
	postWith(t, server, traeExchangeToken, func(req *http.Request) {
		applyTraeRefreshHeaders(req, creds, "  Cloud-IDE-JWT exchange.token  ")
	})
	header := probe.last(t)

	require.Equal(t, "application/json", header.Get("Content-Type"))
	require.Equal(t, "application/json", header.Get("Accept"))
	require.Contains(t, header.Get("User-Agent"), "Trae/")
	require.Equal(t, "exchange.token", header.Get("X-Cloudide-Token"))
	// 纪律 7：OAuth 域带任何 IDE 私有头反而可能被拒。
	require.Empty(t, header.Get("X-Uid"))
	require.Empty(t, header.Get("X-Device-Id"))
	require.Empty(t, header.Get("X-Machine-Id"))
	require.Empty(t, header.Get("Authorization"))
	require.Empty(t, header.Get("X-Ide-Token"))
	require.Empty(t, header.Get("X-User-Region"))

	// 空 accessToken 时连 X-Cloudide-Token 都不发。
	postWith(t, server, traeGetUserInfo, func(req *http.Request) {
		applyTraeRefreshHeaders(req, creds, "")
	})
	require.Empty(t, probe.last(t).Get("X-Cloudide-Token"))
}

func TestTraeDeviceIdentityIsStableAndNumeric(t *testing.T) {
	t.Parallel()
	// 纪律 8：设备号必须 15~16 位纯数字（hex/UUID 会被风控拒绝）。
	account := traeAccount(555, map[string]any{"uid": "7001", "access_token": "at-555"})
	deviceID := traeStableDeviceID(account)
	require.Regexp(t, traeDeviceIDPattern, deviceID, "设备号必须为 15~16 位纯数字")
	require.Regexp(t, traeMachineIDPattern, traeStableMachineID(account))

	// 稳定性：同账号（即便取到不同的 Account 指针）必须得到同一组指纹，
	// 否则"每请求换设备号"会被上游判定异常。
	require.Equal(t, deviceID, traeStableDeviceID(traeAccount(555, map[string]any{"uid": "7001", "access_token": "at-555"})))
	require.Equal(t, traeStableMachineID(account), traeStableMachineID(traeAccount(555, map[string]any{"uid": "7001", "access_token": "at-555"})))

	// 不同账号绝不复用（防串话）。
	other := traeStableDeviceID(traeAccount(556, map[string]any{"uid": "7002", "access_token": "at-556"}))
	require.NotEqual(t, deviceID, other)

	// 无 uid/凭据时按账号 ID 派生，同样满足纯数字纪律。
	bare := traeStableDeviceID(&Account{ID: 91, Platform: PlatformTrae})
	require.Regexp(t, traeDeviceIDPattern, bare)
	require.NotEmpty(t, traeStableDeviceID(nil), "nil 账号也必须给出合法指纹")
	require.Regexp(t, traeMachineIDPattern, traeStableMachineID(nil))
}

// 回归（code review P0）：设备指纹不得随 **access_token 轮换** 或 **uid 回填** 而变。
// 旧实现拿 access_token / uid 当派生种子：换票后设备号突变、建档首次回填 uid 时再变
// 一次，而「设备号突变」正是风控最敏感的信号（参考实现明确警告每请求现生成设备号
// 会被盯上）。种子只能是用账号 ID（行生命周期内恒定）。
func TestTraeDeviceIdentitySurvivesTokenRotationAndUIDBackfill(t *testing.T) {
	t.Parallel()
	// 建档时只粘了 refreshToken（无 uid、无 access_token）。
	atCreation := traeAccount(777, map[string]any{"refresh_token": "rt-1"})
	baseline := traeStableDeviceID(atCreation)
	baselineMachine := traeStableMachineID(atCreation)

	// 首次出站换票：access_token 与 uid 同时被回填。
	afterExchange := traeAccount(777, map[string]any{
		"refresh_token": "rt-2", "access_token": "at-1", "uid": "700777",
	})
	require.Equal(t, baseline, traeStableDeviceID(afterExchange), "换票后设备号不得变化")
	require.Equal(t, baselineMachine, traeStableMachineID(afterExchange))

	// 之后每次轮换（refresh_token 一次性、access_token 天天变）。
	for _, token := range []string{"at-2", "at-3", "Cloud-IDE-JWT at-4"} {
		rotated := traeAccount(777, map[string]any{
			"refresh_token": "rt-x", "access_token": token, "uid": "700777",
		})
		require.Equal(t, baseline, traeStableDeviceID(rotated), "token=%s", token)
		require.Equal(t, baselineMachine, traeStableMachineID(rotated))
	}

	// 但账号不同必须不同（防串话）。
	require.NotEqual(t, baseline, traeStableDeviceID(traeAccount(778, map[string]any{})))
}

func TestTraeResolveDeviceIdentityPrefersCredentials(t *testing.T) {
	t.Parallel()
	account := traeAccount(600, map[string]any{"uid": "600"})
	// 凭据显式覆盖优先（用户已抓包拿到的真实设备号）。
	deviceID, machineID := traeResolveDeviceIdentity(TraeCredentials{
		DeviceID: "  1234567890123456  ", MachineID: " AAAA ",
	}, account)
	require.Equal(t, "1234567890123456", deviceID)
	require.Equal(t, "AAAA", machineID)

	// 缺省时稳定派生。
	autoDevice, autoMachine := traeResolveDeviceIdentity(TraeCredentials{}, account)
	require.Equal(t, traeStableDeviceID(account), autoDevice)
	require.Equal(t, traeStableMachineID(account), autoMachine)
	require.Regexp(t, traeDeviceIDPattern, autoDevice)
}

func TestTraeTokenValueStripsSchemePrefixes(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"Cloud-IDE-JWT eyJabc.def.ghi": "eyJabc.def.ghi",
		"cloud-ide-jwt eyJabc.def.ghi": "eyJabc.def.ghi",
		"Bearer eyJabc.def.ghi":        "eyJabc.def.ghi",
		"bearer eyJabc.def.ghi":        "eyJabc.def.ghi",
		"  eyJabc.def.ghi  ":           "eyJabc.def.ghi",
		"eyJabc.def.ghi":               "eyJabc.def.ghi",
		"":                             "",
		"Bearer":                       "Bearer",
		"Bearer ":                      "Bearer",
		"Cloud-IDE-JWT":                "Cloud-IDE-JWT",
		"cloud-ide-jwt-only-prefix":    "cloud-ide-jwt-only-prefix",
	}
	for in, want := range cases {
		require.Equal(t, want, traeTokenValue(in), "input=%q", in)
	}
}

func TestTraeOAuthClientIDDiffersByRealm(t *testing.T) {
	t.Parallel()
	cn := traeOAuthClientID(TraeCredentials{Realm: "cn"})
	global := traeOAuthClientID(TraeCredentials{Realm: "global"})
	require.Equal(t, traeCNClientID, cn)
	require.Equal(t, traeSOLOClientID, global)
	require.NotEqual(t, cn, global, "CN IDE 与 SOLO 是两个不同 client_id")
	// 缺省 realm（未判定）按 CN。
	require.Equal(t, traeCNClientID, traeOAuthClientID(TraeCredentials{}))
	// 凭据显式覆盖优先。
	require.Equal(t, "custom-id", traeOAuthClientID(TraeCredentials{Realm: "global", ClientID: " custom-id "}))
}

func TestTraeUAHelpers(t *testing.T) {
	t.Parallel()
	require.Equal(t, "Trae/"+defaultTraeIDEVersion, traeUAForChat(TraeCredentials{}), "缺省版本可跟随凭据覆盖")
	require.Equal(t, "Trae/9.9.9", traeUAForChat(TraeCredentials{IDEVersion: " 9.9.9 "}))
	require.Equal(t, "VSCode "+defaultTraeVSCodeVersion+" (TRAE SOLO CN)", traeUAForUG(TraeCredentials{}))
	require.Equal(t, "VSCode "+defaultTraeVSCodeVersion+" (TRAE)", traeUAForUG(TraeCredentials{Realm: "global"}))
	require.NotEqual(t, traeUAForChat(TraeCredentials{}), traeUAForUG(TraeCredentials{}),
		"聊天域与 UG 域 UA 必须不同源")
}

// TestTraeHeaderSuitesAreMutuallyExclusive 用同一个账号跑三套头，逐头比对差异，
// 防止后来者"顺手复用"某一族的构造函数（那样会立刻把签到打成 9004/9074）。
func TestTraeHeaderSuitesAreMutuallyExclusive(t *testing.T) {
	t.Parallel()
	account := traeAccount(700, map[string]any{"access_token": "at-700", "uid": "uid-700"})
	creds := account.GetTraeCredentials()

	build := func(apply func(*http.Request)) http.Header {
		req, err := http.NewRequest(http.MethodPost, "https://example.invalid/p", nil)
		require.NoError(t, err)
		apply(req)
		return req.Header
	}
	chat := build(func(req *http.Request) { applyTraeChatHeaders(req, creds, account) })
	billing := build(func(req *http.Request) { applyTraeBillingHeaders(req, creds, account) })
	oauth := build(func(req *http.Request) { applyTraeRefreshHeaders(req, creds, creds.AccessToken) })

	// X-Uid：只有聊天域发。
	require.NotEmpty(t, chat.Get("X-Uid"))
	require.Empty(t, billing.Get("X-Uid"))
	require.Empty(t, oauth.Get("X-Uid"))
	// Accept：三套各不相同。
	require.Equal(t, "text/event-stream", chat.Get("Accept"))
	require.Equal(t, "*/*", billing.Get("Accept"))
	require.Equal(t, "application/json", oauth.Get("Accept"))
	// 设备指纹：OAuth 域不发，聊天与 UG 都发（且必须同值，同一台机器）。
	require.Empty(t, oauth.Get("X-Device-Id"))
	require.Equal(t, chat.Get("X-Device-Id"), billing.Get("X-Device-Id"))
	// OAuth 域不发 Authorization（换票阶段还没有 access token）。
	require.Empty(t, oauth.Get("Authorization"))
	require.NotEmpty(t, chat.Get("Authorization"))
	require.NotEmpty(t, billing.Get("Authorization"))
	// X-Cloudide-Token：聊天域 + OAuth 域发，UG 域不发。
	require.NotEmpty(t, chat.Get("X-Cloudide-Token"))
	require.NotEmpty(t, oauth.Get("X-Cloudide-Token"))
	require.Empty(t, billing.Get("X-Cloudide-Token"))
	// X-User-Region 只在 UG 域存在。
	require.Empty(t, chat.Get("X-User-Region"))
	require.NotEmpty(t, billing.Get("X-User-Region"))
	require.Empty(t, oauth.Get("X-User-Region"))
}
