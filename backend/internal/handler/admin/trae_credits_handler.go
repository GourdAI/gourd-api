package admin

// trae_credits_handler.go Trae 积分查询/签到与换票探测的管理端点：
//   - GET  /api/v1/admin/trae/accounts/:id/credits   → 查询剩余积分（落 extra 快照）
//   - POST /api/v1/admin/trae/accounts/:id/checkin   → 手动签到（幂等，已签到返回 already）
//   - POST /api/v1/admin/trae/oauth/exchange-token   → 用 refresh_token 试换 access_token
//     （建档前验证 refreshToken 是否可用，避免建出一个只能报错的账号）
//
// 结构对齐 WorkBuddyCreditsHandler（路径参数解析 + response.Success 包装 +
// response.ErrorFrom 透传 infraerrors）。上游探测/网络失败一律 200 + success=false，
// 只有"账号不存在/平台不符/服务未启用"才用 HTTP 错误码。

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"strconv"
)

// TraeCreditsHandler 处理 Trae 积分、签到与换票探测的管理端点。
type TraeCreditsHandler struct {
	creditsService *service.TraeCreditsService
	oauthService   *service.TraeOAuthService
}

// NewTraeCreditsHandler 构造 handler；两个 service 均由 wire 注入（oauthService 可为 nil，
// 表示换票探测端点不可用）。
func NewTraeCreditsHandler(
	creditsService *service.TraeCreditsService,
	oauthService *service.TraeOAuthService,
) *TraeCreditsHandler {
	return &TraeCreditsHandler{creditsService: creditsService, oauthService: oauthService}
}

// QueryCredits 查询账号剩余积分。
// GET /api/v1/admin/trae/accounts/:id/credits
func (h *TraeCreditsHandler) QueryCredits(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.creditsService == nil {
		response.BadRequest(c, "trae credits service is not enabled")
		return
	}
	result, err := h.creditsService.QueryCredits(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// Checkin 手动触发账号签到（幂等：今天已签到返回 status=already，仍是成功语义）。
// POST /api/v1/admin/trae/accounts/:id/checkin
func (h *TraeCreditsHandler) Checkin(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.creditsService == nil {
		response.BadRequest(c, "trae credits service is not enabled")
		return
	}
	result, err := h.creditsService.Checkin(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// traeExchangeTokenRequest 换票探测请求体。
type traeExchangeTokenRequest struct {
	Realm        string `json:"realm"`
	RefreshToken string `json:"refresh_token"`
	ClientID     string `json:"client_id"`
	ProxyID      int64  `json:"proxy_id"`
}

// ExchangeToken 用 refresh_token 试换 access_token（建档前验证凭据可用性）。
// POST /api/v1/admin/trae/oauth/exchange-token
func (h *TraeCreditsHandler) ExchangeToken(c *gin.Context) {
	var req traeExchangeTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request body")
		return
	}
	if strings.TrimSpace(req.RefreshToken) == "" {
		response.BadRequest(c, "refresh_token is required")
		return
	}
	if h == nil || h.oauthService == nil {
		response.BadRequest(c, "trae oauth service is not enabled")
		return
	}
	var proxyID *int64
	if req.ProxyID > 0 {
		proxyID = &req.ProxyID
	}
	result, err := h.oauthService.ExchangeRefreshToken(c.Request.Context(), service.TraeExchangeRequest{
		Realm:        req.Realm,
		RefreshToken: strings.TrimSpace(req.RefreshToken),
		ClientID:     strings.TrimSpace(req.ClientID),
		ProxyID:      proxyID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
