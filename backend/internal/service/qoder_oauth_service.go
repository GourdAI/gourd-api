package service

// qoder_oauth_service.go Qoder OAuth 设备授权登录服务（PKCE 设备流：生成登录页
// URL → 用户浏览器完成登录 → 前端轮询 deviceToken/poll 拿令牌 → userinfo 补身份）。
//
// 上游设备流语义（逆向官方客户端登录实现）：
//  1. StartLogin：本地生成 PKCE（verifier=base64url(32 随机字节)、
//     challenge=base64url(sha256(verifier))）与 nonce（16 随机字节 hex），
//     loginURL = DeviceLoginBase + "?nonce=..&challenge=..&challenge_method=S256
//     &client_id=e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"；pending 状态本地保存
//     （与 workbuddy 的「上游保管 state」不同，Qoder 的 verifier 必须留在本地）；
//  2. PollToken：GET PollEndpoint + "?nonce=..&verifier=..&challenge_method=S256"，
//     404 = 用户尚未完成登录（继续轮询）；200 返回 JSON{token, refresh_token}
//     （dt- 设备令牌）；
//  3. 成功后用 device token GET userinfo（Authorization: Bearer dt-xxx）拿
//     name/userId/organization_id/organization_name/userType（best-effort）。
//
// 出站使用本服务独立的 http.Client（可按 proxyID 解析的代理配置），与
// WorkBuddyOAuthService 的「独立 client + 代理」模式一致。
//
// pending 状态在内存 map（loginID → pending），带 10 分钟 deadline；
// 进程重启丢失 pending 仅影响进行中的登录（用户重新发起即可）。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
)

const (
	// qoderOAuthClientID 官方设备流 client_id。
	qoderOAuthClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"

	// qoderOAuthTimeout 单次设备流上游调用的总时长上限。
	qoderOAuthTimeout = 30 * time.Second

	// qoderOAuthPendingTTL pending 登录会话的有效期（10 分钟）。
	qoderOAuthPendingTTL = 10 * time.Minute

	// QoderOAuthStatusPending / QoderOAuthStatusCompleted 轮询结果状态
	// （语义与 WorkBuddyOAuthStatus* 对齐）。
	QoderOAuthStatusPending   = "pending"
	QoderOAuthStatusCompleted = "completed"
)

// QoderOAuthService 负责 Qoder 设备授权登录的服务端编排。
type QoderOAuthService struct {
	proxyRepo ProxyRepository

	mu      sync.Mutex
	pending map[string]*qoderOAuthPending // key: loginID
	nextID  int64

	// baseURL / pollURL 仅供单测注入（httptest 假上游）；生产恒为空。
	baseURL string
	pollURL string
}

// qoderOAuthPending 是一次进行中的登录会话。
type qoderOAuthPending struct {
	Nonce    string
	Verifier string
	Realm    string
	Deadline time.Time
}

// NewQoderOAuthService 构造设备授权服务；proxyRepo 可为 nil（表示不支持代理选择）。
func NewQoderOAuthService(proxyRepo ProxyRepository) *QoderOAuthService {
	return &QoderOAuthService{proxyRepo: proxyRepo, pending: map[string]*qoderOAuthPending{}}
}

// NewQoderOAuthServiceWithEndpoints 构造指向自定义上游端点的设备授权服务
// （单测注入 httptest 假上游；生产恒用 NewQoderOAuthService）。
func NewQoderOAuthServiceWithEndpoints(proxyRepo ProxyRepository, loginBase, pollURL string) *QoderOAuthService {
	return &QoderOAuthService{
		proxyRepo: proxyRepo,
		pending:   map[string]*qoderOAuthPending{},
		baseURL:   strings.TrimSpace(loginBase),
		pollURL:   strings.TrimSpace(pollURL),
	}
}

// QoderAuthURLResult 是授权 URL 生成结果（回给后台前端展示/跳转）。
type QoderAuthURLResult struct {
	LoginID string `json:"login_id"`
	AuthURL string `json:"auth_url"`
	Nonce   string `json:"nonce"`
	Realm   string `json:"realm"`
	Expires int64  `json:"expires_at"`
}

// QoderOAuthTokenInfo 是设备流兑换出的完整凭据与账号展示信息。
type QoderOAuthTokenInfo struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	DeviceToken      string `json:"device_token"`
	Name             string `json:"name"`
	UID              string `json:"uid"`
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
	UserType         string `json:"user_type"`
	Realm            string `json:"realm"`
	Nickname         string `json:"nickname"`
}

// QoderOAuthExchangeResult 是单次轮询结果：pending 时 Token 为 nil。
type QoderOAuthExchangeResult struct {
	Status string               `json:"status"`
	Token  *QoderOAuthTokenInfo `json:"token,omitempty"`
}

// qoderOAuthEndpointsFor 返回本次登录使用的登录页基础 URL 与 poll 端点
// （测试注入优先，生产按 realm 取官方双域）。
func (s *QoderOAuthService) qoderOAuthEndpointsFor(realm string) (loginBase, pollEndpoint string) {
	eps := resolveQoderEndpoints(realm, "")
	if s != nil && s.baseURL != "" {
		return s.baseURL, firstNonEmptyQoder(s.pollURL, eps.PollEndpoint)
	}
	if s != nil && s.pollURL != "" {
		return eps.DeviceLoginBase, s.pollURL
	}
	return eps.DeviceLoginBase, eps.PollEndpoint
}

// firstNonEmptyQoder 返回第一个非空参数。
func firstNonEmptyQoder(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// qoderOAuthProxyURL 解析 proxyID 对应的代理 URL（模式对齐 workbuddyOAuthProxyURL）。
func (s *QoderOAuthService) qoderOAuthProxyURL(ctx context.Context, proxyID *int64) (string, error) {
	if proxyID == nil {
		return "", nil
	}
	if s == nil || s.proxyRepo == nil {
		return "", infraerrors.New(http.StatusBadRequest, "QODER_OAUTH_PROXY_NOT_AVAILABLE", "proxy repository is not available")
	}
	proxy, err := s.proxyRepo.GetByID(ctx, *proxyID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return "", infraerrors.New(http.StatusBadRequest, "QODER_OAUTH_PROXY_NOT_FOUND", "configured proxy was not found")
		}
		return "", infraerrors.New(http.StatusServiceUnavailable, "QODER_OAUTH_PROXY_LOOKUP_FAILED", "proxy lookup is temporarily unavailable")
	}
	if proxy == nil {
		return "", infraerrors.New(http.StatusBadRequest, "QODER_OAUTH_PROXY_NOT_FOUND", "configured proxy was not found")
	}
	return proxy.URL(), nil
}

// qoderOAuthHTTPClient 构造设备流专用 http.Client（低频操作，无需连接池复用）。
func qoderOAuthHTTPClient(proxyURL string) (*http.Client, error) {
	transport := &http.Transport{}
	if strings.TrimSpace(proxyURL) != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, infraerrors.Newf(http.StatusBadRequest, "QODER_OAUTH_INVALID_PROXY", "invalid proxy url: %v", err)
		}
		if err := proxyutil.ConfigureTransportProxy(transport, parsed); err != nil {
			return nil, infraerrors.Newf(http.StatusBadRequest, "QODER_OAUTH_UNSUPPORTED_PROXY", "unsupported proxy: %v", err)
		}
	}
	return &http.Client{Timeout: qoderOAuthTimeout, Transport: transport}, nil
}

// StartLogin 生成一次设备授权登录：本地产生 PKCE 与 nonce，保存 pending，
// 返回登录页 URL。region 归一为 cn/global（缺省 global）。
func (s *QoderOAuthService) StartLogin(region string, proxyID *int64) (*QoderAuthURLResult, error) {
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "QODER_OAUTH_SERVICE_UNAVAILABLE", "qoder oauth service is not configured")
	}
	realm := normalizeQoderRealm(region)
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "QODER_OAUTH_RANDOM_FAILED", "generate pkce verifier failed: %v", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	challengeSum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeSum[:])
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "QODER_OAUTH_RANDOM_FAILED", "generate nonce failed: %v", err)
	}
	nonce := hex.EncodeToString(nonceBytes)

	loginBase, _ := s.qoderOAuthEndpointsFor(realm)
	loginURL := fmt.Sprintf("%s?nonce=%s&challenge=%s&challenge_method=S256&client_id=%s",
		loginBase, url.QueryEscape(nonce), url.QueryEscape(challenge), qoderOAuthClientID)

	loginID := fmt.Sprintf("qoder-login-%d", time.Now().UnixNano())
	deadline := time.Now().Add(qoderOAuthPendingTTL)
	s.mu.Lock()
	// 顺手清理过期 pending，防长期运行膨胀。
	for id, p := range s.pending {
		if time.Now().After(p.Deadline) {
			delete(s.pending, id)
		}
	}
	s.pending[loginID] = &qoderOAuthPending{Nonce: nonce, Verifier: verifier, Realm: realm, Deadline: deadline}
	s.mu.Unlock()

	return &QoderAuthURLResult{
		LoginID: loginID,
		AuthURL: loginURL,
		Nonce:   nonce,
		Realm:   realm,
		Expires: deadline.Unix(),
	}, nil
}

// qoderPollResponse 是 poll 端点的响应形态。
type qoderPollResponse struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
}

// qoderUserinfoResponse 是 userinfo 端点的响应形态。
//
// 键名为 2026-09-22 实测真值：用户 ID 键是 id（初版误写 userId 导致 uid 恒空，
// 进而 COSY 身份回落伪 uid 被上游判 105 Login expired）；name 为登录名、
// username 为展示名（昵称取 username 优先、回落 name）。
type qoderUserinfoResponse struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Username         string `json:"username"`
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
	UserType         string `json:"userType"`
}

// WaitLogin 轮询一次登录结果（前端反复调用直到 completed）：
//   - pending 不存在/已过期 → error（前端应重新发起登录）；
//   - poll 404 → pending（用户尚未完成登录）；
//   - 200 且 token 非空 → completed（best-effort 拉 userinfo 补身份）；
//   - 其他状态/解析失败 → error。
func (s *QoderOAuthService) WaitLogin(ctx context.Context, loginID string, proxyID *int64) (*QoderOAuthExchangeResult, error) {
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "QODER_OAUTH_SERVICE_UNAVAILABLE", "qoder oauth service is not configured")
	}
	loginID = strings.TrimSpace(loginID)
	if loginID == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "QODER_OAUTH_LOGIN_ID_REQUIRED", "login_id is required")
	}
	s.mu.Lock()
	p, ok := s.pending[loginID]
	s.mu.Unlock()
	if !ok {
		return nil, infraerrors.New(http.StatusNotFound, "QODER_OAUTH_LOGIN_NOT_FOUND", "login session not found or already consumed")
	}
	if time.Now().After(p.Deadline) {
		s.CancelLogin(loginID)
		return nil, infraerrors.New(http.StatusGone, "QODER_OAUTH_LOGIN_EXPIRED", "login session expired, restart login")
	}
	proxyURL, err := s.qoderOAuthProxyURL(ctx, proxyID)
	if err != nil {
		return nil, err
	}
	client, err := qoderOAuthHTTPClient(proxyURL)
	if err != nil {
		return nil, err
	}
	_, pollEndpoint := s.qoderOAuthEndpointsFor(p.Realm)
	pollURL := fmt.Sprintf("%s?nonce=%s&verifier=%s&challenge_method=S256",
		pollEndpoint, url.QueryEscape(p.Nonce), url.QueryEscape(p.Verifier))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "QODER_OAUTH_POLL_REQUEST_FAILED", "build qoder poll request failed: %v", err)
	}
	req.Header.Set("accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "QODER_OAUTH_UPSTREAM_UNREACHABLE", "qoder oauth upstream request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, workbuddyUpstreamErrorBodyLimit))
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "QODER_OAUTH_UPSTREAM_READ_FAILED", "read qoder poll body failed: %v", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return &QoderOAuthExchangeResult{Status: QoderOAuthStatusPending}, nil
	}
	if resp.StatusCode >= 400 {
		return nil, infraerrors.Newf(http.StatusBadGateway, "QODER_OAUTH_POLL_HTTP_ERROR",
			"qoder poll failed: HTTP %d: %s", resp.StatusCode, workbuddyTruncateForError(string(raw)))
	}
	var pollResp qoderPollResponse
	if err := json.Unmarshal(raw, &pollResp); err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "QODER_OAUTH_POLL_PARSE_FAILED", "parse qoder poll response failed: %v", err)
	}
	deviceToken := strings.TrimSpace(pollResp.Token)
	if deviceToken == "" {
		// 200 但无 token：视作未完成（上游边界态，继续轮询）。
		return &QoderOAuthExchangeResult{Status: QoderOAuthStatusPending}, nil
	}
	// 成功：消费 pending（一次性），拉 userinfo 补身份。
	s.mu.Lock()
	delete(s.pending, loginID)
	s.mu.Unlock()
	info := &QoderOAuthTokenInfo{
		AccessToken:  deviceToken,
		RefreshToken: strings.TrimSpace(pollResp.RefreshToken),
		DeviceToken:  deviceToken,
		Realm:        p.Realm,
	}
	s.fillQoderUserinfo(ctx, client, deviceToken, info)
	return &QoderOAuthExchangeResult{Status: QoderOAuthStatusCompleted, Token: info}, nil
}

// fillQoderUserinfo 用设备令牌拉取账号身份信息（best-effort：任何失败都留空）。
func (s *QoderOAuthService) fillQoderUserinfo(ctx context.Context, client *http.Client, deviceToken string, info *QoderOAuthTokenInfo) {
	eps := resolveQoderEndpoints(info.Realm, "")
	ui, err := qoderFetchUserinfoWithClient(ctx, client, deviceToken, eps.UserinfoURL)
	if err != nil || ui == nil {
		return
	}
	info.UID = strings.TrimSpace(ui.ID)
	info.Name = strings.TrimSpace(ui.Name)
	info.Nickname = firstNonEmptyQoder(strings.TrimSpace(ui.Username), info.Name)
	info.OrganizationID = strings.TrimSpace(ui.OrganizationID)
	info.OrganizationName = strings.TrimSpace(ui.OrganizationName)
	info.UserType = strings.TrimSpace(ui.UserType)
}

// qoderFetchUserinfo 用默认 client 拉取 userinfo（出站自愈路径使用）。
func qoderFetchUserinfo(ctx context.Context, token, userinfoURL string) (*qoderUserinfoResponse, error) {
	return qoderFetchUserinfoWithClient(ctx, &http.Client{Timeout: qoderOAuthTimeout}, token, userinfoURL)
}

// qoderFetchUserinfoWithClient 以 Bearer 令牌请求 userinfo 端点取身份真值。
// 任何失败返回 error（调用方 best-effort 处理，不阻断主流程）。
func qoderFetchUserinfoWithClient(ctx context.Context, client *http.Client, token, userinfoURL string) (*qoderUserinfoResponse, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("qoder userinfo: empty bearer token")
	}
	if strings.TrimSpace(userinfoURL) == "" {
		return nil, fmt.Errorf("qoder userinfo: empty endpoint")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userinfoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("qoder userinfo: build request: %w", err)
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qoder userinfo: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, workbuddyUpstreamErrorBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("qoder userinfo: read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("qoder userinfo: HTTP %d: %s", resp.StatusCode, workbuddyTruncateForError(string(raw)))
	}
	var ui qoderUserinfoResponse
	if err := json.Unmarshal(raw, &ui); err != nil {
		return nil, fmt.Errorf("qoder userinfo: parse: %w", err)
	}
	return &ui, nil
}

// CancelLogin 取消进行中的登录会话（幂等：不存在也是成功）。
func (s *QoderOAuthService) CancelLogin(loginID string) error {
	loginID = strings.TrimSpace(loginID)
	if loginID == "" {
		return infraerrors.New(http.StatusBadRequest, "QODER_OAUTH_LOGIN_ID_REQUIRED", "login_id is required")
	}
	s.mu.Lock()
	delete(s.pending, loginID)
	s.mu.Unlock()
	return nil
}

// normalizeQoderRealm 归一化 realm 入参：仅接受 "cn"/"global"（大小写不敏感、
// 容忍空白），其余（含空）回落 "global"——对齐账号侧 GetQoderRealm 的缺省语义。
func normalizeQoderRealm(realm string) string {
	if strings.EqualFold(strings.TrimSpace(realm), "cn") {
		return "cn"
	}
	return "global"
}
