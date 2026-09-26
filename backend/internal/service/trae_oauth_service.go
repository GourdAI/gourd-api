package service

// trae_oauth_service.go Trae 换票服务：用 refresh_token 独立换取 access_token。
//
// 用途是**建档前验证**：粘贴一个 refreshToken，确认能换出发票才允许建成账号，
// 否则建出来的账号只会持续报错（凭据不可用是 Trae 账号最常见的错误来源）。
// 与账号内换票（traeTokenRefresher）的区别：这里没有账号上下文，故不落库、
// 不加账号级锁，只做一次 ExchangeToken + GetUserInfo 并把归一化后的凭据回传前端。
//
// GetUserInfo 用于补 uid/nickname：上游 x-uid 与 token 内的 data.id 必须一致，
// 缺失时前端表单留空让用户误填，就会在签到时拿到 9004。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// traeOAuthTimeout 换票/取用户信息单次调用上限。
const traeOAuthTimeout = 20 * time.Second

// traeOAuthMaxBodyBytes OAuth 响应体读取上限。
const traeOAuthMaxBodyBytes int64 = 1 << 20

// TraeOAuthService 独立换票与用户信息查询。
type TraeOAuthService struct {
	proxyRepo    ProxyRepository
	httpUpstream HTTPUpstream
	cfg          *config.Config
}

// NewTraeOAuthService 构造 Trae 换票服务。
func NewTraeOAuthService(proxyRepo ProxyRepository, httpUpstream HTTPUpstream, cfg *config.Config) *TraeOAuthService {
	return &TraeOAuthService{proxyRepo: proxyRepo, httpUpstream: httpUpstream, cfg: cfg}
}

// TraeExchangeRequest 换票入参。
type TraeExchangeRequest struct {
	Realm        string
	RefreshToken string
	ClientID     string
	ProxyID      *int64
}

// TraeExchangeResult 换票产物（前端据此回填凭据表单）。
type TraeExchangeResult struct {
	Success      bool   `json:"success"`
	Realm        string `json:"realm"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"` // 轮换后的新值，必须回写
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	// RefreshExpiresAt refreshToken 到期时刻（epoch 秒）：过期后无法自动续期，
	// 前端据此展示「凭据还能用多久」告警。
	RefreshExpiresAt int64          `json:"refresh_expires_at,omitempty"`
	UID              string         `json:"uid,omitempty"`
	Nickname         string         `json:"nickname,omitempty"`
	BaseURL          string         `json:"base_url,omitempty"`
	BillingURL       string         `json:"billing_base_url,omitempty"`
	OAuthBaseURL     string         `json:"oauth_base_url,omitempty"`
	Credentials      map[string]any `json:"credentials,omitempty"`
	Error            string         `json:"error,omitempty"`
}

// traeExchangeCredentials 把换票产物组装成可直接入库的 credentials 片段
// （camel/snake 用 snake_case，与 NormalizeTraeCredentials 的规范形态一致）。
func traeExchangeCredentials(realm string, res *TraeExchangeResult) map[string]any {
	creds := map[string]any{"realm": realm}
	if res.AccessToken != "" {
		creds["access_token"] = res.AccessToken
	}
	if res.RefreshToken != "" {
		creds["refresh_token"] = res.RefreshToken
	}
	if res.ExpiresAt > 0 {
		creds["expires_at"] = res.ExpiresAt
	}
	if res.RefreshExpiresAt > 0 {
		creds["refresh_expires_at"] = res.RefreshExpiresAt
	}
	if res.UID != "" {
		creds["uid"] = res.UID
	}
	if res.Nickname != "" {
		creds["nickname"] = res.Nickname
	}
	return creds
}

// ExchangeRefreshToken 用 refresh_token 换取 access_token（可选再取 GetUserInfo 补 uid）。
// 上游/网络失败返回 200 + success=false + error，仅入参非法时用 HTTP 错误码。
func (s *TraeOAuthService) ExchangeRefreshToken(ctx context.Context, req TraeExchangeRequest) (*TraeExchangeResult, error) {
	if s == nil || s.httpUpstream == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "TRAE_OAUTH_NOT_CONFIGURED", "trae oauth service is not configured")
	}
	if strings.TrimSpace(req.RefreshToken) == "" {
		return nil, infraerrors.BadRequest("INVALID_TRAE_CREDENTIALS", "refresh_token is required")
	}
	realm := traeRealmFromValues(req.Realm, "", "")
	result := &TraeExchangeResult{Realm: realm}
	base := &TraeCredentials{Realm: realm, RefreshToken: strings.TrimSpace(req.RefreshToken), ClientID: req.ClientID}
	proxyURL := s.resolveProxyURL(ctx, req.ProxyID)

	exchange, err := s.callExchangeToken(ctx, base, proxyURL)
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}
	result.AccessToken = strings.TrimSpace(exchange.Result.Token)
	if rt := strings.TrimSpace(exchange.Result.RefreshToken); rt != "" {
		result.RefreshToken = rt
	} else {
		result.RefreshToken = base.RefreshToken
	}
	result.ExpiresAt = traeExchangeExpiresAt(*exchange)
	result.RefreshExpiresAt = traeExchangeRefreshExpiresAt(*exchange)
	if result.AccessToken == "" {
		result.Error = "trae exchange returned no token"
		return result, nil
	}
	// uid/nickname 尽力获取：失败不影响换票结论（用户可手填 uid）。
	if uid, nickname, infoErr := s.callGetUserInfo(ctx, base, result.AccessToken, proxyURL); infoErr == nil {
		result.UID = uid
		result.Nickname = nickname
	}
	if result.UID == "" {
		result.UID = traeJWTClaimString(result.AccessToken, "data", "id")
	}
	result.BaseURL = traeRealmChatBaseURL(realm)
	result.BillingURL = traeRealmBillingBaseURL(realm)
	result.OAuthBaseURL = traeRealmOAuthBaseURL(realm)
	result.Success = true
	result.Credentials = traeExchangeCredentials(realm, result)
	return result, nil
}

// callExchangeToken 打一次 ExchangeToken。目标域优先凭据里的 oauth_base_url
// （浏览器登录回显的 loginHost，企业版 console.enterprise.trae.cn 与个人版不同域，
// 写死 realm 默认值会全程 404），缺省回落 realm 默认。
func (s *TraeOAuthService) callExchangeToken(ctx context.Context, creds *TraeCredentials, proxyURL string) (*traeExchangeTokenResponse, error) {
	base := firstTraeNonEmpty(creds.OAuthBaseURL, traeRealmOAuthBaseURL(creds.Realm))
	targetURL := strings.TrimRight(base, "/") + traeExchangeToken
	validatedURL, err := cnValidateProbeURL(s.cfg, targetURL)
	if err != nil {
		return nil, infraerrors.New(http.StatusForbidden, "TRAE_OAUTH_URL_REJECTED", err.Error())
	}
	body, err := json.Marshal(map[string]any{
		"ClientID":     traeOAuthClientID(*creds),
		"RefreshToken": creds.RefreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	})
	if err != nil {
		return nil, fmt.Errorf("marshal trae exchange request: %w", err)
	}
	raw, err := s.post(ctx, validatedURL, func(req *http.Request) {
		applyTraeRefreshHeaders(req, *creds, "")
	}, body, proxyURL)
	if err != nil {
		return nil, err
	}
	var parsed traeExchangeTokenResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse trae exchange response: %w (body: %s)", err, traeTruncateForError(string(raw)))
	}
	if strings.TrimSpace(parsed.Result.Token) == "" {
		return nil, fmt.Errorf("trae exchange failed: no Token in response (body: %s)", traeTruncateForError(string(raw)))
	}
	return &parsed, nil
}

// traeGetUserInfoResponse GetUserInfo 响应。
type traeGetUserInfoResponse struct {
	Result struct {
		UserID       any    `json:"UserID"` // 可能是字符串或数字
		ScreenName   string `json:"ScreenName"`
		EnterpriseID string `json:"EnterpriseID"`
	} `json:"Result"`
}

// callGetUserInfo 取账号 uid/昵称（x-uid 与签到 9004 的直接成因，故建档时就补齐）。
func (s *TraeOAuthService) callGetUserInfo(ctx context.Context, creds *TraeCredentials, accessToken, proxyURL string) (uid, nickname string, err error) {
	target := strings.TrimRight(firstTraeNonEmpty(creds.OAuthBaseURL, traeRealmOAuthBaseURL(creds.Realm)), "/") + traeGetUserInfo
	validatedURL, err := cnValidateProbeURL(s.cfg, target)
	if err != nil {
		return "", "", err
	}
	target = validatedURL
	body, err := json.Marshal(map[string]any{
		"ReqSource":  "IDE",
		"IDEVersion": traeEffective(creds.IDEVersion, defaultTraeIDEVersion),
	})
	if err != nil {
		return "", "", fmt.Errorf("marshal trae user info request: %w", err)
	}
	raw, err := s.post(ctx, target, func(req *http.Request) {
		applyTraeRefreshHeaders(req, *creds, accessToken)
	}, body, proxyURL)
	if err != nil {
		return "", "", err
	}
	var parsed traeGetUserInfoResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", "", fmt.Errorf("parse trae user info response: %w", err)
	}
	return traeCredentialValueString(parsed.Result.UserID), strings.TrimSpace(parsed.Result.ScreenName), nil
}

// post OAuth 域统一出站（带代理与 URL 安全校验）。
func (s *TraeOAuthService) post(
	ctx context.Context,
	url string,
	apply func(*http.Request),
	body []byte,
	proxyURL string,
) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, traeOAuthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build trae oauth request: %w", err)
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	apply(req)
	resp, err := s.httpUpstream.Do(req, proxyURL, 0, 1)
	if err != nil {
		return nil, fmt.Errorf("trae oauth transport error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, traeOAuthMaxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read trae oauth response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("trae oauth returned HTTP %d: %s", resp.StatusCode, traeTruncateForError(string(raw)))
	}
	return raw, nil
}

// resolveProxyURL 按 proxy_id 解析代理地址（无 ID 或查不到返回空串走直连）。
func (s *TraeOAuthService) resolveProxyURL(ctx context.Context, proxyID *int64) string {
	if proxyID == nil || s.proxyRepo == nil {
		return ""
	}
	proxy, err := s.proxyRepo.GetByID(ctx, *proxyID)
	if err != nil || proxy == nil {
		return ""
	}
	return proxy.URL()
}

// traeRealmChatBaseURL / traeRealmBillingBaseURL / traeRealmOAuthBaseURL 按 realm 给
// 出三个域的默认基址（无账号上下文时使用；有账号时一律走 Account 上的方法）。
func traeRealmChatBaseURL(realm string) string {
	if realm == "global" {
		return DefaultTraeGlobalBaseURL
	}
	return DefaultTraeBaseURL
}

func traeRealmBillingBaseURL(realm string) string {
	if realm == "global" {
		return DefaultTraeGlobalBillingBaseURL
	}
	return DefaultTraeBillingBaseURL
}

func traeRealmOAuthBaseURL(realm string) string {
	if realm == "global" {
		return DefaultTraeGlobalOAuthBaseURL
	}
	return DefaultTraeOAuthBaseURL
}
