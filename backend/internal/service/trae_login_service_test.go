//go:build unit

package service

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// traeLoginUpstream 假上游：把出站请求交给注入的 handler（httptest 形态），
// 并记录 path/body 序列供断言。HTTPUpstream 契约，不出真实网络。
type traeLoginUpstream struct {
	handler func(*http.Request) (*http.Response, error)

	mu     sync.Mutex
	paths  []string
	bodies []string
}

func (u *traeLoginUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	body := ""
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		body = string(raw)
		req.Body = io.NopCloser(bytes.NewReader(raw))
	}
	u.mu.Lock()
	u.paths = append(u.paths, req.URL.Path)
	u.bodies = append(u.bodies, body)
	u.mu.Unlock()
	return u.handler(req)
}

func (u *traeLoginUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func (u *traeLoginUpstream) pathLog() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...)
}

func newTraeLoginTestService(handler func(*http.Request) (*http.Response, error)) (*TraeLoginService, *traeLoginUpstream) {
	upstream := &traeLoginUpstream{handler: handler}
	cfg := &config.Config{Security: config.SecurityConfig{
		// 测试用 httptest（http://127.0.0.1）：关闭白名单并允许 http 出站。
		URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
	}}
	oauth := &TraeOAuthService{httpUpstream: upstream, cfg: cfg}
	return NewTraeLoginService(oauth), upstream
}

func writeTraeLoginJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

// ---------------------------------------------------------------------------
// 授权 URL 拼装
// ---------------------------------------------------------------------------

func TestTraeBuildVerificationURI(t *testing.T) {
	uri := traeBuildVerificationURI("www.trae.cn", traeVerificationParams{
		AuthFrom: "trae", PluginVersion: "1.0.0", ClientID: "ono9krqynydwx5",
		LoginTraceID: "trace-1", CallbackURL: "http://127.0.0.1:49682/authorize",
		MachineID: "b5f0e0f2-9944-42ca-857e-608eeb98c8e8", DeviceID: "4175290190306059",
		CodeChallenge: "abc-_12",
	})
	require.True(t, strings.HasPrefix(uri, "https://www.trae.cn/authorization?"), uri)
	// cockpit 纪律：auth_callback_url 与 client_id 不做 URL 编码（原样拼接）。
	require.Contains(t, uri, "&auth_callback_url=http://127.0.0.1:49682/authorize&")
	require.Contains(t, uri, "&client_id=ono9krqynydwx5&")
	// 必须通过授权页的回调格式校验（/^http:\/\/127\.0\.0\.1:(\d+)\/authorize$/）。
	require.Regexp(t, regexp.MustCompile(`auth_callback_url=http://127\.0\.0\.1:\d+/authorize&`), uri)
	require.Contains(t, uri, "&code_challenge=abc-_12&code_challenge_method=S256")
	require.Contains(t, uri, "?login_version=1&auth_from=trae&login_channel=native_ide&", "参数顺序照抄 cockpit，不得重排")

	parsed, err := url.Parse(uri)
	require.NoError(t, err)
	q := parsed.Query()
	// device_id 与 x_device_id 同值、machine_id 与 x_machine_id 同值（上游双发）。
	require.Equal(t, q.Get("device_id"), q.Get("x_device_id"))
	require.Equal(t, q.Get("machine_id"), q.Get("x_machine_id"))
	require.Equal(t, "trace-1", q.Get("login_trace_id"))
}

// ---------------------------------------------------------------------------
// 回调 URL 解析（真实回跳形态：isRedirect + authCodeInfo JSON + userInfo JSON）
// ---------------------------------------------------------------------------

func traeRealCallbackURL(t *testing.T, baseURL, traceID string, expireAtMilli int64) string {
	t.Helper()
	authCodeInfo, err := json.Marshal(map[string]any{
		"AuthCode": "CODE-777", "ExpireAt": expireAtMilli, "ExpireDuration": 600000,
	})
	require.NoError(t, err)
	userInfo, err := json.Marshal(map[string]any{
		"AIRegion": "CN", "Region": "CN", "UserID": "1237380756941412", "ScreenName": "FrankZ",
	})
	require.NoError(t, err)
	q := url.Values{}
	q.Set("isRedirect", "true")
	q.Set("scope", "trae")
	q.Set("authCodeInfo", string(authCodeInfo))
	q.Set("loginTraceID", traceID)
	q.Set("host", "https://api.trae.com.cn")
	q.Set("userRegion", "cn")
	q.Set("userInfo", string(userInfo))
	return baseURL + "/authorize?" + q.Encode()
}

func TestTraeParseCallbackURL_RealRedirectShape(t *testing.T) {
	p, err := traeParseCallbackURL(traeRealCallbackURL(t, "http://127.0.0.1:41961", "trace-9", time.Now().Add(9*time.Minute).UnixMilli()))
	require.NoError(t, err)
	require.Equal(t, "CODE-777", p.authCode)
	require.Equal(t, "https://api.trae.com.cn", p.loginHost)
	require.Equal(t, "trace-9", p.traceID)
	require.Equal(t, "cn", p.loginRegion)
	require.Equal(t, "1237380756941412", p.cbUID)
	require.Equal(t, "FrankZ", p.cbNickname)
}

func TestTraeParseCallbackURL_Errors(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		_, err := traeParseCallbackURL("   ")
		require.Error(t, err)
	})
	t.Run("no query", func(t *testing.T) {
		_, err := traeParseCallbackURL("http://127.0.0.1:1/authorize")
		require.ErrorContains(t, err, "no query parameters")
	})
	t.Run("error param travels via errMsg not error", func(t *testing.T) {
		p, err := traeParseCallbackURL("http://127.0.0.1:1/authorize?error=access_denied")
		require.NoError(t, err)
		require.Contains(t, p.errMsg, "access_denied")
	})
	t.Run("expired authCodeInfo is fatal", func(t *testing.T) {
		raw := traeRealCallbackURL(t, "http://127.0.0.1:1", "trace-1", time.Now().Add(-time.Minute).UnixMilli())
		_, err := traeParseCallbackURL(raw)
		require.ErrorContains(t, err, "expired")
	})
	t.Run("bare code param tolerated", func(t *testing.T) {
		p, err := traeParseCallbackURL("http://127.0.0.1:1/authorize?code=PLAIN-CODE")
		require.NoError(t, err)
		require.Equal(t, "PLAIN-CODE", p.authCode)
	})
}

// ---------------------------------------------------------------------------
// StartLogin：guidance → 授权 URL → pending 会话
// ---------------------------------------------------------------------------

func withTraeLoginEndpoints(t *testing.T, guidanceURL, exchangeOrigin string) {
	t.Helper()
	oldCN, oldGlobal := traeCNLoginGuidanceURLs, traeGlobalLoginGuidanceURLs
	oldCNOrigins, oldGlobalOrigins := traeCNAccountAPIOrigins, traeGlobalAccountAPIOrigins
	traeCNLoginGuidanceURLs = []string{guidanceURL}
	traeGlobalLoginGuidanceURLs = []string{guidanceURL}
	traeCNAccountAPIOrigins = []string{exchangeOrigin}
	traeGlobalAccountAPIOrigins = []string{exchangeOrigin}
	t.Cleanup(func() {
		traeCNLoginGuidanceURLs, traeGlobalLoginGuidanceURLs = oldCN, oldGlobal
		traeCNAccountAPIOrigins, traeGlobalAccountAPIOrigins = oldCNOrigins, oldGlobalOrigins
	})
}

func passthroughHandler(r *http.Request) (*http.Response, error) { return http.DefaultClient.Do(r) }

func TestTraeStartLogin_GuidanceAndPendingSession(t *testing.T) {
	guidance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		require.NotEmpty(t, body["loginTraceID"], "cockpit 纪律：loginTraceID 双键名都发")
		require.Equal(t, body["loginTraceID"], body["login_trace_id"])
		writeTraeLoginJSON(w, http.StatusOK, `{"ResponseMetadata":{},"Result":{"LoginHost":"www.trae.cn"}}`)
	}))
	defer guidance.Close()
	withTraeLoginEndpoints(t, guidance.URL, guidance.URL)

	svc, _ := newTraeLoginTestService(passthroughHandler)
	result, err := svc.StartLogin(context.Background(), "cn", "", nil)
	require.NoError(t, err)
	require.Contains(t, result.LoginURL, "https://www.trae.cn/authorization?")
	require.Contains(t, result.LoginURL, "code_challenge_method=S256")
	require.Regexp(t, regexp.MustCompile(`^http://127\.0\.0\.1:\d+/authorize\?$`), result.Callback)
	require.True(t, result.ExpiresAt > time.Now().Unix())

	svc.mu.Lock()
	session, ok := svc.pending[result.LoginID]
	svc.mu.Unlock()
	require.True(t, ok, "pending 会话必须按 login_id 存留")
	require.True(t, traeIsNumericID(session.DeviceID), "device_id 必须 8~24 位纯数字: %s", session.DeviceID)
	require.NotEmpty(t, session.CodeVerifier)
}

func TestTraeStartLogin_CNGuidanceFailureDegradesGlobalHardFails(t *testing.T) {
	guidance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTraeLoginJSON(w, http.StatusInternalServerError, `{"ResponseMetadata":{"Error":{"Code":"500"}}}`)
	}))
	defer guidance.Close()
	withTraeLoginEndpoints(t, guidance.URL, guidance.URL)

	svc, _ := newTraeLoginTestService(passthroughHandler)
	result, err := svc.StartLogin(context.Background(), "cn", "", nil)
	require.NoError(t, err, "CN 全失败降级默认域而非报错（cockpit 同策略）")
	require.Contains(t, result.LoginURL, traeCNDefaultLoginHost)

	_, globalErr := svc.StartLogin(context.Background(), "global", "", nil)
	require.Error(t, globalErr, "国际域无默认兜底：必须硬报错（错误数据中心比失败更贵）")
}

// ---------------------------------------------------------------------------
// SubmitCallback：authCode 端到端换票
// ---------------------------------------------------------------------------

func seedTraeSession(svc *TraeLoginService, loginID, realm, traceID, loginHost string) {
	svc.mu.Lock()
	svc.pending[loginID] = &traeLoginPending{
		Realm: realm, ClientID: "ono9krqynydwx5", LoginTraceID: traceID,
		CodeVerifier: "verifier-abc", DeviceID: "4175290190306059",
		MachineID: "b5f0e0f2-9944-42ca-857e-608eeb98c8e8",
		LoginHost: loginHost, Deadline: time.Now().Add(traeLoginPendingTTL),
	}
	svc.mu.Unlock()
}

func TestTraeSubmitCallback_AuthCodeEndToEnd(t *testing.T) {
	var exchange *httptest.Server
	exchange = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case traeAuthCodeExchangePath:
			raw, _ := io.ReadAll(r.Body)
			body := map[string]any{}
			_ = json.Unmarshal(raw, &body)
			require.Equal(t, "ono9krqynydwx5", body["ClientID"])
			require.Equal(t, "CODE-777", body["AuthCode"])
			require.Equal(t, "verifier-abc", body["CodeVerifier"])
			device, ok := body["DeviceInfo"].(map[string]any)
			require.True(t, ok, "换票必须携带 DeviceInfo")
			// 空公钥会被上游拒 401/20405 —— 必须是新生成的 P-256 SPKI PEM。
			require.NotEmpty(t, device["DevicePublicKey"])
			require.Equal(t, "IDE_PC", device["PlatformCode"])
			writeTraeLoginJSON(w, http.StatusOK, `{"Result":{"AccessToken":"AT-1","RefreshToken":"RT-1","TokenExpireAt":1799999999000,"RefreshExpireAt":1899999999000,"BoundDeviceID":"9988776655443322"}}`)
		case traeGetUserInfo:
			writeTraeLoginJSON(w, http.StatusOK, `{"Result":{"UserID":"7000000000000000001","ScreenName":"FrankZ"}}`)
		default:
			writeTraeLoginJSON(w, http.StatusNotFound, "404 page not found")
		}
	}))
	defer exchange.Close()
	withTraeLoginEndpoints(t, exchange.URL+"/guidance", exchange.URL)

	svc, upstream := newTraeLoginTestService(passthroughHandler)
	seedTraeSession(svc, "trae-login-trace-1", "cn", "trace-1", exchange.URL)

	result, err := svc.SubmitCallback(context.Background(), "trae-login-trace-1",
		traeRealCallbackURL(t, "http://127.0.0.1:41961", "trace-1", time.Now().Add(9*time.Minute).UnixMilli()))
	require.NoError(t, err)
	require.Equal(t, "completed", result.Status)

	creds := result.Credentials
	require.Equal(t, "AT-1", creds.AccessToken)
	require.Equal(t, "RT-1", creds.RefreshToken)
	require.Equal(t, int64(1799999999), creds.ExpiresAt, "毫秒必须归一为秒")
	require.Equal(t, int64(1899999999), creds.RefreshExpireAt)
	// GetUserInfo 优先于回调回显；device_id 以服务端 BoundDeviceID 为准。
	require.Equal(t, "7000000000000000001", creds.UID)
	require.Equal(t, "FrankZ", creds.Nickname)
	require.Equal(t, "9988776655443322", creds.DeviceID)
	require.Equal(t, "b5f0e0f2994442ca857e608eeb98c8e8", creds.MachineID, "machine_id 落 32hex（UG 域画像）")
	require.Contains(t, creds.DevicePublicKey, "BEGIN PUBLIC KEY")
	require.Contains(t, creds.DevicePrivateKey, "BEGIN PRIVATE KEY")
	require.Equal(t, "FrankZ", result.AccountName())

	svc.mu.Lock()
	_, still := svc.pending["trae-login-trace-1"]
	svc.mu.Unlock()
	require.False(t, still, "会话必须一次性消费")

	// 换票打到 /trae/api/v3/oauth/ExchangeToken（无 /cloudide 前缀），随后同域 GetUserInfo。
	require.Equal(t, []string{traeAuthCodeExchangePath, traeGetUserInfo}, upstream.pathLog())
}

func TestTraeSubmitCallback_RefreshTokenOnlyPathCarriesLoginHost(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case traeExchangeToken:
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			require.Equal(t, "RT-CB", body["RefreshToken"])
			writeTraeLoginJSON(w, http.StatusOK, `{"Result":{"Token":"AT-2","RefreshToken":"RT-2","TokenExpireAt":1899999999000}}`)
		case traeGetUserInfo:
			writeTraeLoginJSON(w, http.StatusOK, `{"Result":{"UserID":"555","ScreenName":"EntUser"}}`)
		default:
			writeTraeLoginJSON(w, http.StatusNotFound, "404 page not found")
		}
	}))
	defer api.Close()

	svc, _ := newTraeLoginTestService(passthroughHandler)
	seedTraeSession(svc, "trae-login-ent", "cn", "t-ent", "https://api.trae.com.cn")

	q := url.Values{}
	q.Set("refreshToken", "RT-CB")
	q.Set("loginTraceID", "t-ent")
	q.Set("host", api.URL) // 企业版回调回显自有域
	result, err := svc.SubmitCallback(context.Background(), "trae-login-ent", "http://127.0.0.1:9/authorize?"+q.Encode())
	require.NoError(t, err)
	require.Equal(t, "AT-2", result.Credentials.AccessToken)
	require.Equal(t, api.URL, result.Credentials.OAuthBaseURL, "登录域必须覆盖 realm 默认（否则账号续票/签到全程 404）")
	require.Equal(t, api.URL, result.Credentials.LoginHost)
}

func TestTraeSubmitCallback_SessionSemantics(t *testing.T) {
	svc, _ := newTraeLoginTestService(func(r *http.Request) (*http.Response, error) {
		require.Fail(t, "不应发生出站")
		return nil, io.EOF
	})

	t.Run("unknown login id is 404", func(t *testing.T) {
		_, err := svc.SubmitCallback(context.Background(), "nope", "http://127.0.0.1:1/authorize?code=x")
		require.Equal(t, http.StatusNotFound, infraerrors.Code(err), "不存在≠过期：服务重启时前端文案不同")
	})
	t.Run("expired session is gone (410)", func(t *testing.T) {
		svc.mu.Lock()
		svc.pending["trae-login-dead"] = &traeLoginPending{Deadline: time.Now().Add(-time.Second), LoginTraceID: "d"}
		svc.mu.Unlock()
		_, err := svc.SubmitCallback(context.Background(), "trae-login-dead", "http://127.0.0.1:1/authorize?code=x")
		require.Equal(t, http.StatusGone, infraerrors.Code(err))
		svc.mu.Lock()
		_, still := svc.pending["trae-login-dead"]
		svc.mu.Unlock()
		require.False(t, still, "过期会话必须被清除")
	})
	t.Run("trace mismatch rejects without consuming session", func(t *testing.T) {
		seedTraeSession(svc, "trae-login-mismatch", "cn", "real-trace", "https://api.trae.com.cn")
		q := url.Values{}
		q.Set("code", "C")
		q.Set("loginTraceID", "other-trace")
		_, err := svc.SubmitCallback(context.Background(), "trae-login-mismatch", "http://127.0.0.1:1/authorize?"+q.Encode())
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
		svc.mu.Lock()
		_, alive := svc.pending["trae-login-mismatch"]
		svc.mu.Unlock()
		require.True(t, alive, "贴错链接不消耗会话（允许立即重贴）")
	})
	t.Run("no code rejects without consuming session", func(t *testing.T) {
		seedTraeSession(svc, "trae-login-nocode", "cn", "nc", "https://api.trae.com.cn")
		_, err := svc.SubmitCallback(context.Background(), "trae-login-nocode", "http://127.0.0.1:1/authorize?foo=bar")
		require.ErrorContains(t, err, "authCode")
		svc.mu.Lock()
		_, alive := svc.pending["trae-login-nocode"]
		svc.mu.Unlock()
		require.True(t, alive)
	})
	t.Run("cancel is idempotent", func(t *testing.T) {
		seedTraeSession(svc, "trae-login-cancel", "cn", "c", "https://api.trae.com.cn")
		require.NoError(t, svc.CancelLogin("trae-login-cancel"))
		require.NoError(t, svc.CancelLogin("trae-login-cancel"))
	})
}

func TestTraeSubmitCallback_ExchangeCandidatesAllFail(t *testing.T) {
	guidance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTraeLoginJSON(w, http.StatusNotFound, "404 page not found")
	}))
	defer guidance.Close()
	withTraeLoginEndpoints(t, guidance.URL, guidance.URL)

	svc, _ := newTraeLoginTestService(passthroughHandler)
	seedTraeSession(svc, "trae-login-fail", "cn", "f", "https://api.trae.com.cn")
	_, err := svc.SubmitCallback(context.Background(), "trae-login-fail",
		traeRealCallbackURL(t, "http://127.0.0.1:1", "f", time.Now().Add(time.Minute).UnixMilli()))
	require.Equal(t, http.StatusBadGateway, infraerrors.Code(err))
	require.ErrorContains(t, err, "auth-code exchange failed")
}

// ---------------------------------------------------------------------------
// 纯函数补充
// ---------------------------------------------------------------------------

func TestTraeExtractLoginHost_MultiPath(t *testing.T) {
	require.Equal(t, "www.trae.cn", traeExtractLoginHost([]byte(`{"Result":{"LoginHost":"www.trae.cn"}}`)))
	require.Equal(t, "https://x", traeExtractLoginHost([]byte(`{"result":{"loginUrl":"https://x"}}`)))
	require.Equal(t, "a.b", traeExtractLoginHost([]byte(`{"data":{"Result":{"LoginHost":"a.b"}}}`)))
	require.Equal(t, "top.example", traeExtractLoginHost([]byte(`{"LoginHost":"top.example"}`)))
	require.Empty(t, traeExtractLoginHost([]byte(`{"ResponseMetadata":{}}`)))
	require.Empty(t, traeExtractLoginHost([]byte(`not json`)))
}

func TestTraeParseAuthCodeExchange_FieldVariants(t *testing.T) {
	access, refresh, expires, refreshExpire, bound := traeParseAuthCodeExchange(
		[]byte(`{"Result":{"Token":"TOK","RefreshToken":"RT","TokenExpireDuration":3600,"RefreshExpireAt":1800000000000,"BoundDeviceID":"1234567890"}}`))
	require.Equal(t, "TOK", access)
	require.Equal(t, "RT", refresh)
	require.InDelta(t, time.Now().Unix()+3600, expires, 5, "duration 秒形态换算 now+d")
	require.Equal(t, int64(1800000000), refreshExpire)
	require.Equal(t, "1234567890", bound)

	// 非数字 BoundDeviceID 必须丢弃（不能污染设备号画像）。
	_, _, _, _, bad := traeParseAuthCodeExchange([]byte(`{"Result":{"Token":"T","BoundDeviceID":"not-numeric"}}`))
	require.Empty(t, bad)

	// 无 Result 信封不 panic。
	a2, _, _, _, _ := traeParseAuthCodeExchange([]byte(`{"error":"boom"}`))
	require.Empty(t, a2)
}

func TestTraeDeviceKeyPairIsFreshP256(t *testing.T) {
	pub, priv, err := generateTraeDeviceKeyPair()
	require.NoError(t, err)
	block, _ := pem.Decode([]byte(pub))
	require.NotNil(t, block)
	require.Equal(t, "PUBLIC KEY", block.Type)
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	require.NoError(t, err)
	ecKey, ok := key.(*ecdsa.PublicKey)
	require.True(t, ok)
	require.Equal(t, elliptic.P256(), ecKey.Curve)

	block2, _ := pem.Decode([]byte(priv))
	require.NotNil(t, block2, "私钥必须是可解析 PEM（后续 DeviceProof 与迁移依赖）")
	_, err = x509.ParsePKCS8PrivateKey(block2.Bytes)
	require.NoError(t, err)

	// 每次登录新生成（重复登录不得复用设备密钥）。
	pub2, _, err := generateTraeDeviceKeyPair()
	require.NoError(t, err)
	require.NotEqual(t, pub, pub2)
}

func TestTraeLoginDeviceIDAndPortShape(t *testing.T) {
	id, err := newTraeLoginDeviceID()
	require.NoError(t, err)
	require.Len(t, id, 16)
	require.True(t, traeIsNumericID(id))
	require.NotEqual(t, byte('0'), id[0], "设备号首位不得为零")

	for i := 0; i < 50; i++ {
		port, perr := newTraeCallbackPort()
		require.NoError(t, perr)
		require.GreaterOrEqual(t, port, 20000)
		require.LessOrEqual(t, port, 65534)
	}
}

func TestTraeNormalizeCredentialsLoginKeys(t *testing.T) {
	// 登录产物 camelCase（traework2api 文件形态）必须归一 snake 入库。
	creds := map[string]any{
		"accessToken":     "AT",
		"devicePublicKey": "PUB",
		"loginHost":       "https://api.trae.com.cn",
		"loginRegion":     "cn",
	}
	require.NoError(t, NormalizeTraeCredentials(creds))
	require.Equal(t, "AT", creds["access_token"])
	require.Equal(t, "PUB", creds["device_public_key"])
	require.Equal(t, "https://api.trae.com.cn", creds["login_host"])
	require.Equal(t, "cn", creds["login_region"])
	require.NotContains(t, creds, "accessToken")
	require.NotContains(t, creds, "devicePublicKey")

	// 全新形态键（snake device_private_key）同样必须命中敏感清单（camel 别名同理），
	// 否则设备私钥会明文回显管理端。
	require.True(t, IsSensitiveCredentialKey("device_private_key"))
	require.True(t, IsSensitiveCredentialKey("devicePrivateKey"), "camelCase 别名必须同样命中")
	require.Equal(t, "device_private_key", CanonicalCredentialKey("devicePrivateKey"))

	// 不含任何 trae 键时保持 no-op（批量更新混合平台路径依赖此语义）。
	plain := map[string]any{"prompt": "x"}
	require.NoError(t, NormalizeTraeCredentials(plain))
	require.NotContains(t, plain, "realm", "no-op 路径不得回填 realm")
}

func TestTraeLoginCredentialExpiryNormalization(t *testing.T) {
	// handler 落库的 expires_at 是字符串数字：归一化保留可解析形态（读取侧
	// GetTraeCredentials 同时支持字符串秒与 ISO 串），不得被删键。
	creds := map[string]any{"access_token": "AT", "expires_at": "1799999999"}
	require.NoError(t, NormalizeTraeCredentials(creds))
	require.Contains(t, creds, "expires_at")
	seconds, err := strconv.ParseInt(traeCredentialValueString(creds["expires_at"]), 10, 64)
	require.NoError(t, err)
	require.EqualValues(t, 1799999999, seconds)

	// authCode 回包的毫秒值（json.Number）写入也要归一为秒。
	creds2 := map[string]any{"access_token": "AT", "expires_at": json.Number("1799999999000")}
	require.NoError(t, NormalizeTraeCredentials(creds2))
	v, err := strconv.ParseInt(traeCredentialValueString(creds2["expires_at"]), 10, 64)
	require.NoError(t, err)
	require.EqualValues(t, 1799999999, v)
}
