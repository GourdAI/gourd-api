package admin

// workbuddy_credits_handler.go WorkBuddy 积分查询/签到与任务三件套的管理端点：
//   - GET  /api/v1/admin/workbuddy/accounts/:id/credits  → 查询剩余积分（落 extra 快照）
//   - POST /api/v1/admin/workbuddy/accounts/:id/checkin  → 手动签到（幂等，已签到返回 already）
//   - POST /api/v1/admin/workbuddy/accounts/:id/activity → 手动触发活跃上报 + 连登奖励链
//   - POST /api/v1/admin/workbuddy/accounts/:id/travel   → 手动触发猫猫旅行巡检
//
// 结构对齐 CNProviderHandler（路径参数解析 + response.Success 包装 + response.ErrorFrom
// 透传 infraerrors），与 WorkBuddyOAuthHandler 同组挂在 /admin/workbuddy 下。

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// WorkBuddyCreditsHandler 处理 WorkBuddy 积分、签到与任务三件套的管理端点。
type WorkBuddyCreditsHandler struct {
	creditsService *service.WorkBuddyCreditsService
	tasksService   *service.WorkBuddyTasksService
}

// NewWorkBuddyCreditsHandler 构造 handler；两个 service 均由 wire 注入。
func NewWorkBuddyCreditsHandler(
	creditsService *service.WorkBuddyCreditsService,
	tasksService *service.WorkBuddyTasksService,
) *WorkBuddyCreditsHandler {
	return &WorkBuddyCreditsHandler{creditsService: creditsService, tasksService: tasksService}
}

// QueryCredits 查询账号剩余积分。
// GET /api/v1/admin/workbuddy/accounts/:id/credits
func (h *WorkBuddyCreditsHandler) QueryCredits(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.creditsService == nil {
		response.BadRequest(c, "workbuddy credits service is not enabled")
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
// POST /api/v1/admin/workbuddy/accounts/:id/checkin
func (h *WorkBuddyCreditsHandler) Checkin(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.creditsService == nil {
		response.BadRequest(c, "workbuddy credits service is not enabled")
		return
	}
	result, err := h.creditsService.Checkin(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// RunActivity 手动触发账号的对话活跃上报（含连登奖励链）。
// POST /api/v1/admin/workbuddy/accounts/:id/activity
func (h *WorkBuddyCreditsHandler) RunActivity(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.tasksService == nil {
		response.BadRequest(c, "workbuddy tasks service is not enabled")
		return
	}
	result, err := h.tasksService.RunActivity(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// RunTravel 手动触发账号的猫猫旅行巡检。
// POST /api/v1/admin/workbuddy/accounts/:id/travel
func (h *WorkBuddyCreditsHandler) RunTravel(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.tasksService == nil {
		response.BadRequest(c, "workbuddy tasks service is not enabled")
		return
	}
	result, err := h.tasksService.RunTravel(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
