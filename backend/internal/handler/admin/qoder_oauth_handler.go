package admin

// qoder_oauth_handler.go Qoder 设备授权登录的管理端点：
//   - POST /api/v1/admin/qoder/oauth/auth-url      → 生成登录页 URL（本地保管 PKCE verifier）
//   - POST /api/v1/admin/qoder/oauth/exchange-code → 单次轮询（前端反复调用直到 completed）
//
// 结构对齐 WorkBuddyOAuthHandler（POST body + response.Success 包装 + response.ErrorFrom
// 透传 infraerrors）。与 workbuddy 的差异：
//   1. realm 缺省为 global（Qoder 官方客户端默认国际域，workbuddy 缺省 cn）；
//   2. 轮询入参键为 login_id（前端 qoder.ts 契约），对应 service.WaitLogin 的 loginID；
//   3. 响应 JSON 键与前端 src/api/admin/qoder.ts 对齐：auth-url 返回
//      {login_url, login_id}，exchange-code 返回 {status, credentials?, account_name?}。

import (
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// QoderOAuthHandler 处理 Qoder 设备授权登录的管理端点。
type QoderOAuthHandler struct {
	qoderOAuthService *service.QoderOAuthService
}

// NewQoderOAuthHandler 构造 handler；service 由 wire 注入。
func NewQoderOAuthHandler(qoderOAuthService *service.QoderOAuthService) *QoderOAuthHandler {
	return &QoderOAuthHandler{qoderOAuthService: qoderOAuthService}
}

// QoderGenerateAuthURLRequest 生成授权 URL 的请求体。
type QoderGenerateAuthURLRequest struct {
	Realm   string `json:"realm"`
	ProxyID *int64 `json:"proxy_id"`
}

// normalizeQoderRealmRequest 校验请求携带的 realm。
//
// 与 WorkBuddy handler 同一分工原则：service 层的归一「空/非法一律回落 global」是
// 防御性兜底；这里对非法值显式报 400，空值等价于缺省（global），与账号侧
// GetQoderRealm 的默认语义一致。
func normalizeQoderRealmRequest(realm string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(realm)) {
	case "", "global":
		return "global", nil
	case "cn":
		return "cn", nil
	default:
		return "", fmt.Errorf("invalid realm: must be 'cn' or 'global'")
	}
}

// QoderExchangeCodeRequest 轮询请求体（前端持有生成阶段返回的 login_id 与 realm）。
type QoderExchangeCodeRequest struct {
	LoginID string `json:"login_id"`
	Realm   string `json:"realm"`
	ProxyID *int64 `json:"proxy_id"`
}

// GenerateAuthURL 生成设备授权 URL。
// POST /api/v1/admin/qoder/oauth/auth-url
func (h *QoderOAuthHandler) GenerateAuthURL(c *gin.Context) {
	var req QoderGenerateAuthURLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	realm, err := normalizeQoderRealmRequest(req.Realm)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	result, err := h.qoderOAuthService.StartLogin(realm, req.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, buildQoderAuthURLResponse(result))
}

// ExchangeCode 单次轮询登录结果；pending 时前端继续轮询，completed 时回填凭据。
// POST /api/v1/admin/qoder/oauth/exchange-code
func (h *QoderOAuthHandler) ExchangeCode(c *gin.Context) {
	var req QoderExchangeCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.LoginID) == "" {
		response.BadRequest(c, "login_id is required")
		return
	}
	// WaitLogin 不接收 realm（登录会话在 StartLogin 时已绑定域）；此处仅校验
	// 非法值显式 400，防止前端拼写错误被静默回落 global。
	if _, err := normalizeQoderRealmRequest(req.Realm); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	result, err := h.qoderOAuthService.WaitLogin(c.Request.Context(), req.LoginID, req.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, buildQoderExchangeResponse(result))
}

// qoderAuthURLResponse / qoderExchangeResponse 是与前端 src/api/admin/qoder.ts
// 契约对齐的响应形态（auth_url 等服务层原始键不直接透出）。
type qoderAuthURLResponse struct {
	LoginURL string `json:"login_url"`
	LoginID  string `json:"login_id"`
}

type qoderExchangeResponse struct {
	Status      string            `json:"status"`
	Credentials map[string]string `json:"credentials,omitempty"`
	AccountName string            `json:"account_name,omitempty"`
}

// buildQoderAuthURLResponse 把服务层 StartLogin 结果映射为前端契约。
func buildQoderAuthURLResponse(result *service.QoderAuthURLResult) *qoderAuthURLResponse {
	if result == nil {
		return nil
	}
	return &qoderAuthURLResponse{
		LoginURL: result.AuthURL,
		LoginID:  result.LoginID,
	}
}

// buildQoderExchangeResponse 把服务层 WaitLogin 结果映射为前端契约：
// pending → {status}; completed → {status, credentials, account_name}。
func buildQoderExchangeResponse(result *service.QoderOAuthExchangeResult) *qoderExchangeResponse {
	if result == nil {
		return nil
	}
	resp := &qoderExchangeResponse{Status: result.Status}
	if result.Token != nil {
		resp.Credentials = qoderOAuthTokenCredentials(result.Token)
		resp.AccountName = qoderOAuthAccountName(result.Token)
	}
	return resp
}

// qoderOAuthTokenCredentials 把设备流兑换出的凭据折算为账号 credentials 键值对
// （snake_case，可直接落入账号凭据 JSONB）。
func qoderOAuthTokenCredentials(token *service.QoderOAuthTokenInfo) map[string]string {
	credentials := make(map[string]string, 9)
	if token.AccessToken != "" {
		credentials["access_token"] = token.AccessToken
	}
	if token.RefreshToken != "" {
		credentials["refresh_token"] = token.RefreshToken
	}
	if token.DeviceToken != "" {
		credentials["device_token"] = token.DeviceToken
	}
	if token.UID != "" {
		credentials["uid"] = token.UID
	}
	if token.Realm != "" {
		credentials["realm"] = token.Realm
	}
	if token.Nickname != "" {
		credentials["nickname"] = token.Nickname
	}
	if token.UserType != "" {
		credentials["user_type"] = token.UserType
	}
	if token.OrganizationID != "" {
		credentials["organization_id"] = token.OrganizationID
	}
	if token.OrganizationName != "" {
		credentials["organization_name"] = token.OrganizationName
	}
	return credentials
}

// qoderOAuthAccountName 返回账号展示名：nickname 优先，回落 organization name。
func qoderOAuthAccountName(token *service.QoderOAuthTokenInfo) string {
	if token.Nickname != "" {
		return token.Nickname
	}
	return token.OrganizationName
}
