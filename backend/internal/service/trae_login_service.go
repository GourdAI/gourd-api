package service

// trae_login_service.go Trae 真 OAuth 登录（浏览器授权 + 回调链接粘贴完成）。
//
// 协议链路（cockpit-tools trae_oauth.rs / cpa-multi-plugins plugins/trae 双源
// 交叉印证，2026-09-26 取证）：
//  1. POST {guidance}/cloudide/api/v3/trae/GetLoginGuidance body
//     {loginTraceID, login_trace_id} → Result.LoginHost（裸域名，无 scheme）；
//  2. 浏览器打开 {LoginHost}/authorization?login_version=1&auth_from=...&...
//     &code_challenge=...&code_challenge_method=S256（参数顺序与编码规则照抄
//     build_verification_uri，auth_callback_url 与 client_id 不做 URL 编码）；
//  3. 授权页**强制校验**回调地址形如 http://127.0.0.1:<port>/authorize
//     （/^http:\/\/127\.0\.0\.1:(\d+)\/authorize$/，其余 host/path 直接渲染
//     "登录失败/网络错误"）。服务端永远收不到该回调——浏览器在**用户机器**上
//     重定向到本机端口。因此完成路径是「粘贴」：用户登录后地址栏会停在
//     http://127.0.0.1:<port>/authorize?...authCodeInfo=...，把整串复制回来；
//  4. authCode → POST {origin}/trae/api/v3/oauth/ExchangeToken（注意**没有**
//     /cloudide 前缀，与 refreshToken 续期是两个不同端点）body
//     {ClientID, AuthCode, CodeVerifier, DeviceInfo, IDEVersion}；DeviceInfo
//     必须携带本次登录新生成的 EC P-256 SPKI 公钥（DevicePublicKey）——空值
//     会被上游拒为 HTTP 401 / code 20405（设备绑定拒绝）；
//  5. 回调也可能直接带 refreshToken（部分登录形态），此时跳过 authCode 换票，
//     走既有 ExchangeRefreshToken 续期链路；
//  6. 换票成功后 GetUserInfo 补 uid/nickname（x-uid 缺失会在签到时拿 9004）。
//
// 与 qoder（本地 PKCE + 上游轮询端点）的差异：Trae **没有**服务端轮询端点，
// 登录结果只能从浏览器回调 URL 中提取，故本服务用「auth-url → 粘贴 submit」
// 两步代替「auth-url → 轮询」。pending 会话按 login_id 存内存（15 分钟 TTL、
// 惰性清理、一次性消费），与 qoder_oauth_service 同构。

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	// traeLoginPendingTTL 一次登录会话的有效期（对齐 cockpit OAUTH_TIMEOUT 的
	// 放大值：用户切去浏览器输密码/扫码，10 分钟偏紧）。
	traeLoginPendingTTL = 15 * time.Minute

	// traeGuidanceProbeTimeout 单个 GetLoginGuidance 端点探测上限。三段兜底最坏
	// 15s；auth-url 是浏览器点击后的同步请求，再长用户就会以为按钮没生效。
	traeGuidanceProbeTimeout = 5 * time.Second

	// traeAuthCodeExchangePath 授权码换票端点（**不带** /cloudide 前缀）。
	traeAuthCodeExchangePath = "/trae/api/v3/oauth/ExchangeToken"
	// traeLoginGuidancePath 登录引导端点。
	traeLoginGuidancePath = "/cloudide/api/v3/trae/GetLoginGuidance"
)

// Trae GetLoginGuidance 候选端点（cockpit TRAE_CN/INTL_LOGIN_GUIDANCE_URLS 原值）。
var (
	traeCNLoginGuidanceURLs = []string{
		"https://api.trae.cn/cloudide/api/v3/trae/GetLoginGuidance",
		"https://api.trae.com.cn/cloudide/api/v3/trae/GetLoginGuidance",
		"https://www.trae.cn/cloudide/api/v3/trae/GetLoginGuidance",
	}
	traeGlobalLoginGuidanceURLs = []string{
		"https://api.marscode.com/cloudide/api/v3/trae/GetLoginGuidance",
		"https://api.trae.ai/cloudide/api/v3/trae/GetLoginGuidance",
		"https://www.trae.ai/cloudide/api/v3/trae/GetLoginGuidance",
	}
)

// 授权码换票的账号 API origin 候选（cockpit default_account_api_config +
// cpa live-verified 2026-09-03：国际站 api.* 主机在 TLB 边缘 404 本路径，
// 必须优先 grow* 系列）。
var (
	traeCNAccountAPIOrigins     = []string{"https://api.trae.cn", "https://api.trae.com.cn"}
	traeGlobalAccountAPIOrigins = []string{"https://grow-normal.trae.ai", "https://growsg-normal.trae.ai", "https://grow-normal.traeapi.us", "https://api.marscode.com", "https://api.trae.ai"}
	traeCNDefaultLoginHost      = "https://www.trae.cn"
	traeGlobalDefaultLoginHost  = "https://www.trae.ai"
)

// TraeLoginService Trae OAuth 登录服务（pending 会话 + 授权 URL + 回调解析 + 换票）。
type TraeLoginService struct {
	oauth *TraeOAuthService

	mu      sync.Mutex
	pending map[string]*traeLoginPending
}

// traeLoginPending 一次进行中的登录会话。
type traeLoginPending struct {
	Realm        string
	ClientID     string
	LoginTraceID string
	CodeVerifier string
	DeviceID     string
	MachineID    string // UUID v4 形态（授权 URL / DeviceInfo 用）
	LoginHost    string // GetLoginGuidance 结果（带 scheme）
	ProxyID      *int64
	Deadline     time.Time
}

// NewTraeLoginService 构造登录服务；复用 TraeOAuthService 的出站传输与代理解析。
func NewTraeLoginService(oauth *TraeOAuthService) *TraeLoginService {
	return &TraeLoginService{oauth: oauth, pending: map[string]*traeLoginPending{}}
}

// TraeLoginStartResult 授权 URL 生成结果。
type TraeLoginStartResult struct {
	LoginID   string `json:"login_id"`
	LoginURL  string `json:"login_url"`
	Callback  string `json:"callback_url_prefix"`
	ExpiresAt int64  `json:"expires_at"`
}

// TraeLoginCredentials 登录完成后的凭据集（snake_case，可直接并入账号 credentials）。
type TraeLoginCredentials struct {
	Realm            string `json:"realm"`
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresAt        int64  `json:"expires_at"`
	RefreshExpireAt  int64  `json:"refresh_expires_at"`
	UID              string `json:"uid"`
	Nickname         string `json:"nickname"`
	DeviceID         string `json:"device_id"`
	MachineID        string `json:"machine_id"`
	DevicePublicKey  string `json:"device_public_key"`
	DevicePrivateKey string `json:"device_private_key"`
	LoginHost        string `json:"login_host"`
	LoginRegion      string `json:"login_region"`
	OAuthBaseURL     string `json:"oauth_base_url"`
	IDEVersion       string `json:"ide_version"`
}

// TraeLoginResult submit 的完成态载荷。
type TraeLoginResult struct {
	Status      string                `json:"status"` // pending | completed
	Credentials *TraeLoginCredentials `json:"credentials,omitempty"`
	Error       string                `json:"error,omitempty"`
}

// AccountName 登录账号展示名（nickname 优先，回落 uid）。
func (r *TraeLoginResult) AccountName() string {
	if r == nil || r.Credentials == nil {
		return ""
	}
	if name := strings.TrimSpace(r.Credentials.Nickname); name != "" {
		return name
	}
	return strings.TrimSpace(r.Credentials.UID)
}

// StartLogin 生成一次浏览器授权登录：本地产生 PKCE / 设备标识 → GetLoginGuidance
// 拿登录域 → 拼授权 URL。realm 归一为 cn/global。
func (s *TraeLoginService) StartLogin(ctx context.Context, realm, clientID string, proxyID *int64) (*TraeLoginStartResult, error) {
	if s == nil || s.oauth == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "TRAE_LOGIN_NOT_CONFIGURED", "trae login service is not configured")
	}
	realm = traeRealmFromValues(realm, "", "")
	traceID, err := newTraeUUIDv4()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "TRAE_LOGIN_RANDOM_FAILED", "generate login trace id failed: %v", err)
	}
	verifierBytes := make([]byte, 48)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "TRAE_LOGIN_RANDOM_FAILED", "generate pkce verifier failed: %v", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	challengeSum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeSum[:])
	// device_id 必须是 8~24 位纯数字（上游设备号画像；hex/UUID 会被授权页判非法，
	// 表现为用户侧"网络错误，请刷新页面重试"）。machine_id 用 UUID v4 形态。
	deviceID, err := newTraeLoginDeviceID()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "TRAE_LOGIN_RANDOM_FAILED", "generate device id failed: %v", err)
	}
	machineID, err := newTraeUUIDv4()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "TRAE_LOGIN_RANDOM_FAILED", "generate machine id failed: %v", err)
	}
	port, err := newTraeCallbackPort()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "TRAE_LOGIN_RANDOM_FAILED", "allocate callback port failed: %v", err)
	}
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d/authorize", port)

	creds := TraeCredentials{Realm: realm, ClientID: strings.TrimSpace(clientID)}
	proxyURL := s.oauth.resolveProxyURL(ctx, proxyID)
	loginHost, guidanceErr := s.requestLoginGuidance(ctx, creds, traceID, proxyURL)
	if guidanceErr != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "TRAE_LOGIN_GUIDANCE_FAILED", "trae GetLoginGuidance failed: %v", guidanceErr)
	}

	verificationURI := traeBuildVerificationURI(loginHost, traeVerificationParams{
		AuthFrom:      "trae",
		PluginVersion: defaultTraeOAuthPluginVersion,
		ClientID:      traeOAuthClientID(creds),
		LoginTraceID:  traceID,
		CallbackURL:   callbackURL,
		MachineID:     machineID,
		DeviceID:      deviceID,
		CodeChallenge: challenge,
	})

	loginID := "trae-login-" + traceID
	deadline := time.Now().Add(traeLoginPendingTTL)
	s.mu.Lock()
	for id, p := range s.pending {
		if time.Now().After(p.Deadline) {
			delete(s.pending, id)
		}
	}
	s.pending[loginID] = &traeLoginPending{
		Realm: realm, ClientID: traeOAuthClientID(creds), LoginTraceID: traceID,
		CodeVerifier: verifier, DeviceID: deviceID, MachineID: machineID,
		LoginHost: loginHost, ProxyID: proxyID, Deadline: deadline,
	}
	s.mu.Unlock()

	return &TraeLoginStartResult{
		LoginID:   loginID,
		LoginURL:  verificationURI,
		Callback:  fmt.Sprintf("http://127.0.0.1:%d/authorize?", port),
		ExpiresAt: deadline.Unix(),
	}, nil
}

// SubmitCallback 处理用户粘贴回来的回调 URL：解析授权码 → 换票 → 补身份信息。
// 会话一次性消费（成功即删除）。URL 里没有授权码时不消耗会话（允许重贴）。
func (s *TraeLoginService) SubmitCallback(ctx context.Context, loginID, pastedURL string) (*TraeLoginResult, error) {
	if s == nil || s.oauth == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "TRAE_LOGIN_NOT_CONFIGURED", "trae login service is not configured")
	}
	loginID = strings.TrimSpace(loginID)
	if loginID == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "TRAE_LOGIN_ID_REQUIRED", "login_id is required")
	}
	s.mu.Lock()
	session, found := s.pending[loginID]
	expired := found && time.Now().After(session.Deadline)
	if expired {
		delete(s.pending, loginID)
	}
	s.mu.Unlock()
	// 区分两种失败（前端文案不同）：过期可以确定地告诉用户「重新发起」；
	// 不存在更可能是服务重启（内存会话丢失）或 login_id 错带。
	if expired {
		return nil, infraerrors.New(http.StatusGone, "TRAE_LOGIN_EXPIRED", "login session expired, restart login")
	}
	if !found {
		return nil, infraerrors.New(http.StatusNotFound, "TRAE_LOGIN_NOT_FOUND", "login session not found (service restarted or already completed), restart login")
	}
	params, err := traeParseCallbackURL(pastedURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadRequest, "TRAE_LOGIN_CALLBACK_INVALID", "%v", err)
	}
	if params.errMsg != "" {
		return nil, infraerrors.Newf(http.StatusBadRequest, "TRAE_LOGIN_CALLBACK_ERROR", "authorization failed: %s", params.errMsg)
	}
	// loginTraceID 校验：上游原样回显发起值；带了但不匹配 = 贴了别的登录的链接。
	// 允许缺省（部分形态不回显）。
	if params.traceID != "" && params.traceID != session.LoginTraceID {
		return nil, infraerrors.New(http.StatusBadRequest, "TRAE_LOGIN_TRACE_MISMATCH", "callback URL belongs to another login, paste the URL from THIS login attempt")
	}
	if params.authCode == "" && params.refreshToken == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "TRAE_LOGIN_NO_CODE", "no authCode/refreshToken in pasted URL — copy the FULL address-bar URL (everything after ?)")
	}
	proxyURL := s.oauth.resolveProxyURL(ctx, session.ProxyID)

	result, err := s.completeLogin(ctx, session, params, proxyURL)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	delete(s.pending, loginID)
	s.mu.Unlock()
	return result, nil
}

// CancelLogin 主动结束一次登录会话（幂等）。
func (s *TraeLoginService) CancelLogin(loginID string) error {
	if s == nil {
		return infraerrors.New(http.StatusInternalServerError, "TRAE_LOGIN_NOT_CONFIGURED", "trae login service is not configured")
	}
	loginID = strings.TrimSpace(loginID)
	if loginID == "" {
		return infraerrors.New(http.StatusBadRequest, "TRAE_LOGIN_ID_REQUIRED", "login_id is required")
	}
	s.mu.Lock()
	delete(s.pending, loginID)
	s.mu.Unlock()
	return nil
}

// completeLogin 依据回调参数换票并组装凭据。
func (s *TraeLoginService) completeLogin(
	ctx context.Context,
	session *traeLoginPending,
	params traeCallbackParams,
	proxyURL string,
) (*TraeLoginResult, error) {
	loginHost := traeEnsureHTTPSScheme(firstTraeNonEmpty(params.loginHost, session.LoginHost))
	oauthBase := loginHost

	var (
		accessToken, refreshToken string
		expiresAt, refreshExpire  int64
		devicePub, devicePriv     string
		boundDeviceID             string
	)

	switch {
	case params.refreshToken != "" && params.authCode == "":
		// 登录形态直接下发 refreshToken：走既有续期端点换 access_token。
		exchange, err := s.oauth.callExchangeToken(ctx, &TraeCredentials{
			Realm: session.Realm, RefreshToken: params.refreshToken, ClientID: session.ClientID,
			OAuthBaseURL: oauthBase,
		}, proxyURL)
		if err != nil {
			return nil, infraerrors.Newf(http.StatusBadGateway, "TRAE_LOGIN_EXCHANGE_FAILED", "trae refresh exchange failed: %v", err)
		}
		accessToken = strings.TrimSpace(exchange.Result.Token)
		refreshToken = firstTraeNonEmpty(strings.TrimSpace(exchange.Result.RefreshToken), params.refreshToken)
		expiresAt = traeExchangeExpiresAt(*exchange)
		refreshExpire = traeExchangeRefreshExpiresAt(*exchange)
	default:
		// authCode 换票（必须携带新生成的设备公钥，空值 → 401/20405）。
		pubPEM, privPEM, err := generateTraeDeviceKeyPair()
		if err != nil {
			return nil, infraerrors.Newf(http.StatusInternalServerError, "TRAE_LOGIN_DEVICE_KEY_FAILED", "generate device key pair failed: %v", err)
		}
		body, err := json.Marshal(map[string]any{
			"ClientID":     session.ClientID,
			"AuthCode":     params.authCode,
			"CodeVerifier": session.CodeVerifier,
			"IDEVersion":   defaultTraeIDEVersion,
			"DeviceInfo": traeBuildDeviceInfo(
				session.DeviceID, session.MachineID, pubPEM,
			),
		})
		if err != nil {
			return nil, infraerrors.Newf(http.StatusInternalServerError, "TRAE_LOGIN_MARSHAL_FAILED", "marshal exchange request failed: %v", err)
		}
		raw, usedURL, err := s.exchangeWithCandidates(ctx, session.Realm, oauthBase, params.loginHost, body, proxyURL)
		if err != nil {
			return nil, err
		}
		devicePub, devicePriv = pubPEM, privPEM
		oauthBase = firstTraeNonEmpty(originOfTraeURL(usedURL), oauthBase)
		accessToken, refreshToken, expiresAt, refreshExpire, boundDeviceID = traeParseAuthCodeExchange(raw)
		if accessToken == "" && refreshToken != "" {
			accessToken = refreshToken // 少数回包只给 refreshToken；直接当 access 用（cpa 同语义）
		}
		if accessToken == "" {
			return nil, infraerrors.New(http.StatusBadGateway, "TRAE_LOGIN_EXCHANGE_EMPTY", "trae exchange returned no access token")
		}
	}

	// uid/nickname 身份链（cockpit resolveLoginUID 同口径）：GetUserInfo 优先，
	// 回调 userInfo 回显兵底（重复登录同账号时昵称立即可用），都拿不到那么前端手填。
	uid, nickname := "", ""
	if apiUID, apiNick, infoErr := s.callLoginUserInfo(ctx, session.Realm, oauthBase, accessToken, proxyURL); infoErr == nil {
		uid = firstTraeNonEmpty(apiUID, params.cbUID)
		nickname = firstTraeNonEmpty(apiNick, params.cbNickname)
	} else {
		uid, nickname = params.cbUID, params.cbNickname
	}
	if uid == "" {
		uid = traeJWTClaimString(accessToken, "data", "id")
	}

	// 设备指纹落库：**上游绑定的就是本次登录生成的 device_id/machine_id**
	//（DeviceInfo 随换票上传，服务端 BoundDeviceID 回显为准）。此后聊天/UG 域
	// 全部复用真值（凭据覆盖优先级已在 traeResolveDeviceIdentity 就位），
	// 不再走账号 ID 派生的兜底。
	deviceID := firstTraeNonEmpty(boundDeviceID, session.DeviceID)
	machineID := strings.ReplaceAll(session.MachineID, "-", "") // 32hex，满足 UG 域画像

	// refreshToken 域兜底：authCode 端点与续期端点不同 host 族，优先信回包/回调
	// 的 loginHost；缺省回 realm 默认。与默认一致时不落键，保持列表整洁。
	var oauthOverride string
	if oauthBase != "" && !strings.EqualFold(strings.TrimRight(oauthBase, "/"), traeRealmOAuthBaseURL(session.Realm)) {
		oauthOverride = strings.TrimRight(oauthBase, "/")
	}

	out := &TraeLoginCredentials{
		Realm: session.Realm, AccessToken: accessToken, RefreshToken: refreshToken,
		ExpiresAt: expiresAt, RefreshExpireAt: refreshExpire,
		UID: uid, Nickname: nickname,
		DeviceID: deviceID, MachineID: machineID,
		DevicePublicKey: devicePub, DevicePrivateKey: devicePriv,
		LoginHost: traeEnsureHTTPSScheme(params.loginHost), LoginRegion: params.loginRegion,
		OAuthBaseURL: oauthOverride,
		IDEVersion:   defaultTraeIDEVersion,
	}
	return &TraeLoginResult{Status: "completed", Credentials: out}, nil
}

// requestLoginGuidance 依次探测候选 GetLoginGuidance 端点，返回登录域（带
// scheme）。CN 全失败降级官方默认域（cockpit 同策略，避免引导接口抖动卡死登录）；
// 国际全失败硬报错（默认域可能把 CN 账号引到错误数据中心）。
func (s *TraeLoginService) requestLoginGuidance(
	ctx context.Context,
	creds TraeCredentials,
	traceID, proxyURL string,
) (string, error) {
	endpoints := traeGlobalLoginGuidanceURLs
	if creds.Realm != "global" {
		endpoints = traeCNLoginGuidanceURLs
	}
	body, err := json.Marshal(map[string]any{"loginTraceID": traceID, "login_trace_id": traceID})
	if err != nil {
		return "", err
	}
	var lastErr error
	for _, endpoint := range endpoints {
		probeCtx, cancel := context.WithTimeout(ctx, traeGuidanceProbeTimeout)
		raw, err := s.oauth.post(probeCtx, endpoint, func(req *http.Request) {
			applyTraeRefreshHeaders(req, creds, "")
		}, body, proxyURL)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if host := traeExtractLoginHost(raw); host != "" {
			return traeEnsureHTTPSScheme(host), nil
		}
		lastErr = fmt.Errorf("response missing LoginHost")
	}
	if creds.Realm != "global" {
		return traeCNDefaultLoginHost, nil
	}
	return "", fmt.Errorf("all guidance endpoints failed (last: %v)", lastErr)
}

// exchangeWithCandidates 按候选 URL 依次尝试授权码换票，返回首个带回 token 的响应。
// 候选顺序（cockpit candidate_account_api_origins）：官方账号 API origin →
// 回调 loginHost 派生（www. → api.）→ loginHost 原样。
func (s *TraeLoginService) exchangeWithCandidates(
	ctx context.Context,
	realm, oauthBase, callbackHost string,
	body []byte,
	proxyURL string,
) (raw []byte, usedURL string, err error) {
	creds := TraeCredentials{Realm: realm}
	var candidates []string
	if realm == "global" {
		candidates = append(candidates, traeGlobalAccountAPIOrigins...)
	} else {
		candidates = append(candidates, traeCNAccountAPIOrigins...)
	}
	for _, derived := range []string{callbackHost, oauthBase} {
		if derived == "" {
			continue
		}
		candidates = append(candidates, derived)
		if host := traeURILowerHost(derived); strings.HasPrefix(host, "www.") {
			scheme := "https"
			if i := strings.Index(derived, "://"); i > 0 {
				scheme = derived[:i]
			}
			candidates = append(candidates, scheme+"://api."+strings.TrimPrefix(host, "www."))
		}
	}
	seen := map[string]struct{}{}
	var errs []string
	for _, origin := range candidates {
		origin = strings.TrimRight(traeEnsureHTTPSScheme(origin), "/")
		if origin == "" {
			continue
		}
		target := origin + traeAuthCodeExchangePath
		if _, dup := seen[target]; dup {
			continue
		}
		seen[target] = struct{}{}
		validated, verr := cnValidateProbeURL(s.oauth.cfg, target)
		if verr != nil {
			errs = append(errs, fmt.Sprintf("%s => rejected by URL policy: %v", target, verr))
			continue
		}
		rawBytes, perr := s.oauth.post(ctx, validated, func(req *http.Request) {
			applyTraeRefreshHeaders(req, creds, "")
		}, body, proxyURL)
		if perr != nil {
			errs = append(errs, fmt.Sprintf("%s => %v", validated, perr))
			continue
		}
		if traeExchangeHasToken(rawBytes) {
			return rawBytes, validated, nil
		}
		errs = append(errs, fmt.Sprintf("%s => no token in body (%s)", validated, traeTruncateForError(string(rawBytes))))
	}
	return nil, "", infraerrors.Newf(http.StatusBadGateway, "TRAE_LOGIN_EXCHANGE_FAILED",
		"trae auth-code exchange failed: %s", strings.Join(errs, " | "))
}

// callLoginUserInfo 登录后补 uid/nickname；base 用换票成功域（企业版与个人版
// 不同 host，写死 realm 默认会 404）。
func (s *TraeLoginService) callLoginUserInfo(
	ctx context.Context,
	realm, base, accessToken, proxyURL string,
) (uid, nickname string, err error) {
	target := strings.TrimRight(firstTraeNonEmpty(base, traeRealmOAuthBaseURL(realm)), "/") + traeGetUserInfo
	validated, err := cnValidateProbeURL(s.oauth.cfg, target)
	if err != nil {
		return "", "", err
	}
	body, err := json.Marshal(map[string]any{"ReqSource": "IDE", "IDEVersion": defaultTraeIDEVersion})
	if err != nil {
		return "", "", err
	}
	raw, err := s.oauth.post(ctx, validated, func(req *http.Request) {
		applyTraeRefreshHeaders(req, TraeCredentials{Realm: realm}, accessToken)
	}, body, proxyURL)
	if err != nil {
		return "", "", err
	}
	var parsed traeGetUserInfoResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", "", fmt.Errorf("parse trae user info: %w", err)
	}
	return traeCredentialValueString(parsed.Result.UserID), strings.TrimSpace(parsed.Result.ScreenName), nil
}

// ---------------------------------------------------------------------------
// 授权 URL 拼装（参数顺序与编码规则逐字照抄 cockpit build_verification_uri：
// 顺序可能被上游页面校验，auth_callback_url / client_id 不做 URL 编码）
// ---------------------------------------------------------------------------

const defaultTraeOAuthPluginVersion = "1.0.0"

type traeVerificationParams struct {
	AuthFrom      string
	PluginVersion string
	ClientID      string
	LoginTraceID  string
	CallbackURL   string
	MachineID     string
	DeviceID      string
	CodeChallenge string
}

func traeBuildVerificationURI(loginHost string, p traeVerificationParams) string {
	pairs := []struct {
		key    string
		value  string
		encode bool
	}{
		{"login_version", "1", false},
		{"auth_from", p.AuthFrom, false},
		{"login_channel", "native_ide", false},
		{"plugin_version", p.PluginVersion, true},
		{"auth_type", "local", false},
		{"client_id", p.ClientID, false},
		{"redirect", "0", false},
		{"login_trace_id", p.LoginTraceID, true},
		{"auth_callback_url", p.CallbackURL, false},
		{"machine_id", p.MachineID, true},
		{"device_id", p.DeviceID, true},
		{"x_device_id", p.DeviceID, true},
		{"x_machine_id", p.MachineID, true},
		{"x_device_brand", defaultTraeDeviceBrand, true},
		{"x_device_type", defaultTraeDeviceType, true},
		{"x_os_version", defaultTraeOSVersion, true},
		{"x_env", defaultTraeRequestTraffic, true},
		{"x_app_version", defaultTraeIDEVersion, true},
		{"x_app_type", "trae", true},
		{"code_challenge", p.CodeChallenge, true},
		{"code_challenge_method", "S256", false},
	}
	parts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		value := pair.value
		if pair.encode {
			value = url.QueryEscape(value)
		}
		parts = append(parts, pair.key+"="+value)
	}
	return strings.TrimRight(traeEnsureHTTPSScheme(loginHost), "/") + "/authorization?" + strings.Join(parts, "&")
}

// ---------------------------------------------------------------------------
// 回调 URL 解析（真实回跳形态：isRedirect + authCodeInfo(JSON) + loginTraceID +
// host + userRegion + userInfo(JSON)；参数别名表照抄 cockpit pick_query_value）
// ---------------------------------------------------------------------------

type traeCallbackParams struct {
	errMsg       string
	authCode     string
	refreshToken string
	loginHost    string
	loginRegion  string
	traceID      string
	cbUID        string
	cbNickname   string
}

func traeParseCallbackURL(raw string) (traeCallbackParams, error) {
	var p traeCallbackParams
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return p, fmt.Errorf("callback URL is empty")
	}
	candidate := trimmed
	if !strings.Contains(candidate, "://") {
		candidate = "http://" + strings.TrimLeft(candidate, "/")
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return p, fmt.Errorf("callback URL not parseable")
	}
	values := parsed.Query()
	// 部分浏览器/表单会把参数放在 fragment（#?a=b）；cockpit 同规则：fragment
	// 只在 query 缺该键时兜底。
	if len(values) == 0 && parsed.Fragment != "" {
		frag := strings.TrimPrefix(parsed.Fragment, "?")
		if parsed, err = url.Parse("http://x/?" + frag); err == nil {
			values = parsed.Query()
		}
	}
	if len(values) == 0 {
		return p, fmt.Errorf("pasted URL carries no query parameters — copy the full address-bar URL after login")
	}
	for _, key := range []string{"error", "error_code", "err", "errorCode"} {
		if v := strings.TrimSpace(values.Get(key)); v != "" {
			desc := strings.TrimSpace(values.Get("error_description"))
			p.errMsg = strings.TrimSpace(v + " " + desc)
			return p, nil
		}
	}
	if ir := strings.ToLower(strings.TrimSpace(values.Get("isRedirect"))); ir == "false" || ir == "0" || ir == "no" {
		p.errMsg = "isRedirect=false (authorization was cancelled or rejected upstream)"
		return p, nil
	}
	for _, key := range []string{"loginHost", "login_host", "LoginHost", "host", "consoleHost"} {
		if v := strings.TrimSpace(values.Get(key)); v != "" {
			p.loginHost = v
			break
		}
	}
	for _, key := range []string{"loginRegion", "login_region", "region", "Region", "userRegion", "user_region"} {
		if v := strings.TrimSpace(values.Get(key)); v != "" {
			p.loginRegion = strings.ToLower(v)
			break
		}
	}
	for _, key := range []string{"loginTraceID", "loginTraceId", "login_trace_id", "trace_id"} {
		if v := strings.TrimSpace(values.Get(key)); v != "" {
			p.traceID = v
			break
		}
	}
	for _, key := range []string{"refreshToken", "refresh_token", "RefreshToken", "refresh-token"} {
		if v := strings.TrimSpace(values.Get(key)); v != "" {
			p.refreshToken = v
			break
		}
	}
	for _, key := range []string{"authCode", "auth_code", "AuthCode", "authorization_code", "code"} {
		if v := strings.TrimSpace(values.Get(key)); v != "" {
			p.authCode = v
			break
		}
	}
	if p.authCode == "" {
		for _, key := range []string{"authCodeInfo", "auth_code_info", "AuthCodeInfo"} {
			if v := strings.TrimSpace(values.Get(key)); v != "" {
				code, expireErr := traeAuthCodeFromInfo(v)
				if expireErr != "" {
					return traeCallbackParams{}, fmt.Errorf("%s", expireErr)
				}
				if code != "" {
					p.authCode = code
					break
				}
			}
		}
	}
	// userInfo 回显（JSON）：GetUserInfo 失败时的身份兜底。
	for _, key := range []string{"userInfo", "user_info", "UserInfo", "userinfo"} {
		if v := strings.TrimSpace(values.Get(key)); v != "" {
			var info map[string]any
			if json.Unmarshal([]byte(v), &info) == nil {
				p.cbUID = traeFirstStringKey(info, "UserID", "userId", "uid", "user_id")
				p.cbNickname = traeFirstStringKey(info, "ScreenName", "screenName", "Nickname", "nickname", "Name", "name")
				if p.cbUID != "" || p.cbNickname != "" {
					break
				}
			}
		}
	}
	return p, nil
}

// traeAuthCodeFromInfo 解析 authCodeInfo（JSON 字符串 {AuthCode, ExpireAt(ms)}）。
// ExpireAt 过期直接拒（旧链接重放必然换票失败，给出明确指引比 502 友好）。
func traeAuthCodeFromInfo(raw string) (code, fatalErr string) {
	var info map[string]any
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return "", ""
	}
	code = traeFirstStringKey(info, "AuthCode", "authCode", "auth_code", "code")
	for _, key := range []string{"ExpireAt", "expireAt", "expire_at", "expiresAt"} {
		if n, ok := traeToInt64(info[key]); ok && n > 0 {
			if traeNormalizeEpochSeconds(n)*1000 < time.Now().UnixMilli() {
				return "", "authCode in the pasted URL has expired — restart the login and paste immediately"
			}
			break
		}
	}
	return code, ""
}

// traeExtractLoginHost 从 GetLoginGuidance 响应提取 LoginHost（多路径容错，
// cockpit extract_login_guidance_host 同规则；响应是火山引擎信封 Result.LoginHost）。
func traeExtractLoginHost(raw []byte) string {
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return ""
	}
	keys := []string{"LoginHost", "loginHost", "LoginURL", "loginUrl", "login_url"}
	for _, sub := range []map[string]any{top, traeMapKey(top, "Result"), traeMapKey(top, "result"), traeMapKey(top, "data")} {
		if sub == nil {
			continue
		}
		if v := traeFirstStringKey(sub, keys...); v != "" {
			return v
		}
		if inner := traeMapKey(sub, "Result"); inner != nil {
			if v := traeFirstStringKey(inner, keys...); v != "" {
				return v
			}
		}
	}
	return ""
}

// traeParseAuthCodeExchange 解析授权码换票回包：Result.{AccessToken|Token}、
// RefreshToken、TokenExpireAt（毫秒）/TokenExpireDuration（秒）、
// RefreshExpireAt、BoundDeviceID（服务端实际绑定的设备号，后续指纹以它为准）。
func traeParseAuthCodeExchange(raw []byte) (access, refresh string, expiresAt, refreshExpire int64, boundDeviceID string) {
	var env struct {
		Result map[string]any `json:"Result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Result == nil {
		return "", "", 0, 0, ""
	}
	r := env.Result
	access = traeFirstStringKey(r, "AccessToken", "accessToken", "Token", "token")
	refresh = traeFirstStringKey(r, "RefreshToken", "refreshToken")
	if n, ok := traeToInt64(r["TokenExpireAt"]); ok && n > 0 {
		expiresAt = traeNormalizeEpochSeconds(n)
	}
	if expiresAt == 0 {
		if d, ok := traeToInt64(r["TokenExpireDuration"]); ok && d > 0 {
			expiresAt = time.Now().Unix() + d
		}
	}
	if n, ok := traeToInt64(r["RefreshExpireAt"]); ok && n > 0 {
		refreshExpire = traeNormalizeEpochSeconds(n)
	}
	boundDeviceID = traeFirstStringKey(r, "BoundDeviceID", "boundDeviceId")
	if !traeIsNumericID(boundDeviceID) {
		boundDeviceID = ""
	}
	return access, refresh, expiresAt, refreshExpire, boundDeviceID
}

func traeExchangeHasToken(raw []byte) bool {
	access, _, _, _, _ := traeParseAuthCodeExchange(raw)
	return access != ""
}

// ---------------------------------------------------------------------------
// DeviceInfo 与设备密钥（换票必须携带；公钥上传后服务端绑定）
// ---------------------------------------------------------------------------

type traeExchangeDeviceInfo struct {
	DeviceID        string `json:"DeviceID"`
	MachineID       string `json:"MachineID"`
	PlatformCode    string `json:"PlatformCode"`
	DeviceType      string `json:"DeviceType"`
	DeviceName      string `json:"DeviceName"`
	DeviceModel     string `json:"DeviceModel"`
	ClientVersion   string `json:"ClientVersion"`
	DevicePublicKey string `json:"DevicePublicKey"`
	DeviceBrand     string `json:"DeviceBrand"`
	DeviceCPU       string `json:"DeviceCPU"`
	OSInfo          string `json:"OSInfo"`
	OSVersion       string `json:"OSVersion"`
}

func traeBuildDeviceInfo(deviceID, machineID, publicKeyPEM string) traeExchangeDeviceInfo {
	return traeExchangeDeviceInfo{
		DeviceID:        deviceID,
		MachineID:       machineID,
		PlatformCode:    "IDE_PC",
		DeviceType:      "PC",
		DeviceName:      "DESKTOP-GOURDAPI",
		DeviceModel:     defaultTraeDeviceBrand,
		ClientVersion:   defaultTraeIDEVersion,
		DevicePublicKey: publicKeyPEM,
		DeviceBrand:     "Microsoft", // deviceBrandForContext("windows")
		DeviceCPU:       "",
		OSInfo:          defaultTraeDeviceType,
		OSVersion:       defaultTraeOSVersion,
	}
}

// generateTraeDeviceKeyPair 新生成一对 EC P-256 密钥（PEM），公钥随换票上传、
// 私钥随凭据落库（官方客户端"每登录一套设备密钥"行为；私钥留待刷新
// DeviceProof 与账号迁移自证使用）。
func generateTraeDeviceKeyPair() (publicPEM, privatePEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate P-256 key: %w", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", fmt.Errorf("marshal pkcs8: %w", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", fmt.Errorf("marshal spki: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})), nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func newTraeUUIDv4() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// newTraeLoginDeviceID 生成 16 位纯数字设备号（上游画像 8~24 位；含字母直接
// 被授权页判非法）。首位非零。
func newTraeLoginDeviceID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	digits := make([]byte, 16)
	for i, v := range b {
		digits[i] = '0' + v%10
	}
	if digits[0] == '0' {
		digits[0] = '1' + b[0]%9
	}
	return string(digits), nil
}

// newTraeCallbackPort 伪随机回调端口（20000~65534）；只用于通过授权页的
// 127.0.0.1 格式校验，无人监听，用户从地址栏复制。
func newTraeCallbackPort() (int, error) {
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		return 0, err
	}
	return 20000 + int(uint16(b[0])<<8|uint16(b[1]))%45535, nil
}

func traeEnsureHTTPSScheme(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		return trimmed
	}
	return "https://" + strings.TrimLeft(trimmed, "/")
}

func originOfTraeURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

func firstTraeNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func traeMapKey(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if sub, ok := m[key].(map[string]any); ok {
		return sub
	}
	return nil
}

func traeFirstStringKey(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			if s := strings.TrimSpace(traeCredentialValueString(v)); s != "" {
				return s
			}
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func traeToInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
			return i, true
		}
	}
	return 0, false
}

// traeIsNumericID 8~24 位纯数字（cockpit is_numeric_id 同口径）。
func traeIsNumericID(v string) bool {
	if len(v) < 8 || len(v) > 24 {
		return false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
