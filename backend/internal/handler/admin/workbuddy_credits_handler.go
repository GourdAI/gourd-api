package admin

// workbuddy_credits_handler.go WorkBuddy 积分查询/签到与任务三件套的管理端点：
//   - GET  /api/v1/admin/workbuddy/accounts/:id/credits  → 查询剩余积分（落 extra 快照）
//   - POST /api/v1/admin/workbuddy/accounts/:id/checkin  → 手动签到（幂等，已签到返回 already）
//   - POST /api/v1/admin/workbuddy/accounts/:id/activity → 已封死（410，见 RunActivity 注释）
//   - POST /api/v1/admin/workbuddy/accounts/:id/travel   → 手动触发猫猫旅行巡检
//
// 结构对齐 CNProviderHandler（路径参数解析 + response.Success 包装 + response.ErrorFrom
// 透传 infraerrors），与 WorkBuddyOAuthHandler 同组挂在 /admin/workbuddy 下。

import (
	"net/http"
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

// RunActivity 已封死，不再触达上游（2026-11-01）。
// POST /api/v1/admin/workbuddy/accounts/:id/activity → 410 Gone
//
// 为什么要封死这个手动入口，而不是只把定时任务默认关：
//  1. 它绕过 gateway.workbuddy.activity_enabled 门控（service 层 Start() 的门控只管
//     定时 loop），点一次就向 billing 域 POST /v2/report 送 N 条**凭空捏造的
//     chat_request_send 事件**（上游看不到与之对应的真实会话）。参考实现
//     dsh-connect-workbuddy 5.16.0 完全不做这类上报。
//  2. mitmproxy 抓包复盘的封号归因是「通道可识别 + 设备指纹缺失 + 机器行为画像」
//     三者叠加，而捏造活跃记录恰是第三类里最不像人类的那一个；国内站已出现
//     账号级 11140「账号存在异常行为，已被限制使用」。
//  3. 入口对账号状态不设防：RunActivity 只校验账号存在与平台正确，对已被
//     SetError 的受限账号照样发请求，等于主动向上游递新的异常行为证据。
//
// 路由注册保留而不摘除：backend/internal/web/dist 里仍有旧构建产物带着「跑任务」
// 按钮，打到这里要拿到明确理由，而不是裸 404（404 会被前端渲染成通用网络错误，
// 管理员看不出是刻意封禁）。service 层 RunActivity 编排逻辑保留，需要恢复该能力
// 时把本函数改回调用 h.tasksService.RunActivity 即可。
func (h *WorkBuddyCreditsHandler) RunActivity(c *gin.Context) {
	_ = h // 封死后不读取任何 service；保留方法签名以维持路由注册形状。
	response.ErrorWithDetails(
		c,
		http.StatusGone,
		"WorkBuddy activity reporting has been removed: it POSTs fabricated chat_request_send events to /v2/report, which upstream risk control treats as abnormal account behavior (error 11140).",
		"WORKBUDDY_ACTIVITY_DISABLED",
		nil,
	)
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
