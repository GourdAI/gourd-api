package admin

// qoder_credits_handler.go Qoder 积分查询与每日活动 Credits 领取的管理端点：
//   - GET  /api/v1/admin/qoder/accounts/:id/credits → 查询积分余额与活动状态（落 extra 快照）
//   - POST /api/v1/admin/qoder/accounts/:id/checkin → 手动领取本轮活动 Credits（幂等）
//
// 结构对齐 WorkBuddyCreditsHandler（路径参数解析 + response.Success 包装 +
// response.ErrorFrom 透传 infraerrors），与 QoderOAuthHandler 同组挂在 /admin/qoder 下。
// 上游探测/网络失败仍返回 200 + success=false（观测失败不等于请求失败），
// 仅账号不存在/平台不符/服务未启用才走 HTTP 错误码。

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// QoderCreditsHandler 处理 Qoder 积分查询与活动 Credits 领取的管理端点。
type QoderCreditsHandler struct {
	creditsService *service.QoderCreditsService
}

// NewQoderCreditsHandler 构造 handler；service 由 wire 注入。
func NewQoderCreditsHandler(creditsService *service.QoderCreditsService) *QoderCreditsHandler {
	return &QoderCreditsHandler{creditsService: creditsService}
}

// QueryCredits 查询账号积分余额与活动可领取状态。
// GET /api/v1/admin/qoder/accounts/:id/credits
func (h *QoderCreditsHandler) QueryCredits(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.creditsService == nil {
		response.BadRequest(c, "qoder credits service is not enabled")
		return
	}
	result, err := h.creditsService.QueryCredits(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// Checkin 手动触发账号领取本轮活动 Credits。
// POST /api/v1/admin/qoder/accounts/:id/checkin
//
// 幂等语义：同一轮重复领取上游返回 replayed=true，本端点回报 status=already
// 且 success=true（不是失败）；账号不具备资格时 status=skipped。
func (h *QoderCreditsHandler) Checkin(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.creditsService == nil {
		response.BadRequest(c, "qoder credits service is not enabled")
		return
	}
	result, err := h.creditsService.Checkin(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
