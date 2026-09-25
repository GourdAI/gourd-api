package admin

// workbuddy_oauth_handler.go WorkBuddy 设备授权登录的管理端点：
//   - POST /api/v1/admin/workbuddy/oauth/auth-url      → 生成授权 URL（上游签发 state）
//   - POST /api/v1/admin/workbuddy/oauth/exchange-code → 单次轮询（前端反复调用直到 completed）
//
// 结构对齐 GrokOAuthHandler（POST body + response.Success 包装 + response.ErrorFrom 透传
// infraerrors），与 GeminiOAuthHandler 的 /oauth/* 路由命名保持一致。

import (
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// WorkBuddyOAuthHandler 处理 WorkBuddy 设备授权登录的管理端点。
type WorkBuddyOAuthHandler struct {
	workbuddyOAuthService *service.WorkBuddyOAuthService
}

// NewWorkBuddyOAuthHandler 构造 handler；service 由 wire 注入。
func NewWorkBuddyOAuthHandler(workbuddyOAuthService *service.WorkBuddyOAuthService) *WorkBuddyOAuthHandler {
	return &WorkBuddyOAuthHandler{workbuddyOAuthService: workbuddyOAuthService}
}

// WorkBuddyGenerateAuthURLRequest 生成授权 URL 的请求体。
type WorkBuddyGenerateAuthURLRequest struct {
	Realm   string `json:"realm"`
	ProxyID *int64 `json:"proxy_id"`
}

// normalizeWorkbuddyRealmRequest 校验请求携带的 realm。
//
// 与 service 层的归一函数分工不同：service 的归一「空/非法一律回落 cn」是防御性兜底
// （服务可能被其它调用方复用）；这里对非法值显式报 400——非法 realm 是调用方拼写错误，
// 静默回落会让用户以为选了 global 实际登录了 cn，必须显式拒绝而不是猜测意图。
// 空值等价于缺省（cn），与账号侧 GetWorkbuddyRealm 的默认语义一致。
func normalizeWorkbuddyRealmRequest(realm string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(realm)) {
	case "", "cn":
		return "cn", nil
	case "global":
		return "global", nil
	default:
		return "", fmt.Errorf("invalid realm: must be 'cn' or 'global'")
	}
}

// GenerateAuthURL 生成设备授权 URL。
// POST /api/v1/admin/workbuddy/oauth/auth-url
func (h *WorkBuddyOAuthHandler) GenerateAuthURL(c *gin.Context) {
	var req WorkBuddyGenerateAuthURLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	realm, err := normalizeWorkbuddyRealmRequest(req.Realm)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	result, err := h.workbuddyOAuthService.GenerateAuthURL(c.Request.Context(), realm, req.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// WorkBuddyExchangeCodeRequest 轮询请求体（前端持有生成阶段返回的 state 与 realm）。
type WorkBuddyExchangeCodeRequest struct {
	State   string `json:"state"`
	Realm   string `json:"realm"`
	ProxyID *int64 `json:"proxy_id"`
}

// ExchangeCode 单次轮询登录结果；pending 时前端继续轮询，completed 时回填凭据。
// POST /api/v1/admin/workbuddy/oauth/exchange-code
func (h *WorkBuddyOAuthHandler) ExchangeCode(c *gin.Context) {
	var req WorkBuddyExchangeCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.State) == "" {
		response.BadRequest(c, "state is required")
		return
	}
	realm, err := normalizeWorkbuddyRealmRequest(req.Realm)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	result, err := h.workbuddyOAuthService.ExchangeState(c.Request.Context(), req.State, realm, req.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
