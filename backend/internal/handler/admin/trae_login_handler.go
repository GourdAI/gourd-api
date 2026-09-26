package admin

// trae_login_handler.go Trae 真 OAuth 登录的管理端点：
//   - POST /api/v1/admin/trae/oauth/auth-url → 生成授权 URL（服务端保管 PKCE verifier）
//   - POST /api/v1/admin/trae/oauth/submit   → 提交粘贴回来的回调 URL，换票并回传凭据
//   - POST /api/v1/admin/trae/oauth/cancel   → 放弃一次登录会话
//
// 为什么没有轮询端点（与 qoder/workbuddy 的关键差异）：Trae 授权页强制校验回调
// 地址必须是 http://127.0.0.1:<port>/authorize，回调只会打到**用户本机**端口，
// 服务端永远收不到，因此登录结果只能由用户把地址栏整串复制回来（submit）。
//
// 契约对齐 qoder_oauth_handler：成功 response.Success 包装，service 的 infraerrors
// HTTP 码原样透传（会话不存在 404 / 已过期 410 / 上游故障 502）。

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// TraeLoginHandler 处理 Trae 浏览器授权登录。
type TraeLoginHandler struct {
	loginService *service.TraeLoginService
}

// NewTraeLoginHandler 构造 handler；service 由 wire 注入。
func NewTraeLoginHandler(loginService *service.TraeLoginService) *TraeLoginHandler {
	return &TraeLoginHandler{loginService: loginService}
}

// TraeAuthURLRequest 生成授权 URL 的请求体。
type TraeAuthURLRequest struct {
	Realm    string `json:"realm"`
	ClientID string `json:"client_id"`
	ProxyID  *int64 `json:"proxy_id"`
}

// TraeLoginSubmitRequest 提交回调 URL 的请求体。
type TraeLoginSubmitRequest struct {
	LoginID     string `json:"login_id"`
	CallbackURL string `json:"callback_url"`
}

type traeAuthURLResponse struct {
	LoginID      string `json:"login_id"`
	LoginURL     string `json:"login_url"`
	CallbackPref string `json:"callback_url_prefix"`
	ExpiresAt    int64  `json:"expires_at"`
	// NeedsPaste 恒为 true，显式告知前端「本平台无轮询，必须粘贴回调链接」，
	// 避免前端照 qoder 范式挂一个永远 pending 的轮询器。
	NeedsPaste bool `json:"needs_paste"`
}

type traeLoginSubmitResponse struct {
	Status      string            `json:"status"`
	Credentials map[string]string `json:"credentials,omitempty"`
	AccountName string            `json:"account_name,omitempty"`
}

// GenerateAuthURL 生成浏览器授权 URL。
// POST /api/v1/admin/trae/oauth/auth-url
func (h *TraeLoginHandler) GenerateAuthURL(c *gin.Context) {
	var req TraeAuthURLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	switch strings.ToLower(strings.TrimSpace(req.Realm)) {
	case "", "cn", "global":
	default:
		response.BadRequest(c, "invalid realm: must be 'cn' or 'global'")
		return
	}
	result, err := h.loginService.StartLogin(c.Request.Context(), req.Realm, req.ClientID, req.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, &traeAuthURLResponse{
		LoginID:      result.LoginID,
		LoginURL:     result.LoginURL,
		CallbackPref: result.Callback,
		ExpiresAt:    result.ExpiresAt,
		NeedsPaste:   true,
	})
}

// Submit 用粘贴的回调 URL 完成登录，回传可直接落库的 credentials。
// POST /api/v1/admin/trae/oauth/submit
func (h *TraeLoginHandler) Submit(c *gin.Context) {
	var req TraeLoginSubmitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	result, err := h.loginService.SubmitCallback(c.Request.Context(), req.LoginID, req.CallbackURL)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	resp := &traeLoginSubmitResponse{Status: result.Status}
	if result.Credentials != nil {
		resp.Credentials = traeLoginCredentials(result.Credentials)
		resp.AccountName = result.AccountName()
	}
	response.Success(c, resp)
}

// Cancel 放弃一次登录会话。POST /api/v1/admin/trae/oauth/cancel
func (h *TraeLoginHandler) Cancel(c *gin.Context) {
	var req TraeLoginSubmitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := h.loginService.CancelLogin(req.LoginID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"status": "cancelled"})
}

// traeLoginCredentials 把登录凭据折算为账号 credentials 键值（snake_case，
// 空值不落键）。device_private_key 属敏感键，由脱敏清单负责不回显。
func traeLoginCredentials(token *service.TraeLoginCredentials) map[string]string {
	credentials := make(map[string]string, 14)
	put := func(key, value string) {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			credentials[key] = trimmed
		}
	}
	put("realm", token.Realm)
	put("access_token", token.AccessToken)
	put("refresh_token", token.RefreshToken)
	put("uid", token.UID)
	put("nickname", token.Nickname)
	put("device_id", token.DeviceID)
	put("machine_id", token.MachineID)
	put("device_public_key", token.DevicePublicKey)
	put("device_private_key", token.DevicePrivateKey)
	put("login_host", token.LoginHost)
	put("login_region", token.LoginRegion)
	put("oauth_base_url", token.OAuthBaseURL)
	put("ide_version", token.IDEVersion)
	if token.ExpiresAt > 0 {
		credentials["expires_at"] = strconv.FormatInt(token.ExpiresAt, 10)
	}
	if token.RefreshExpireAt > 0 {
		credentials["refresh_expires_at"] = strconv.FormatInt(token.RefreshExpireAt, 10)
	}
	return credentials
}
