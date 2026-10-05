package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// SubscriptionSummaryItem represents a subscription item in summary
//
// 一份订阅 = 一个个人额度钱包：只有一份总额池，不再按日/周/月分档，
// 也不携带任何分组信息（订阅不授予分组准入）。
type SubscriptionSummaryItem struct {
	ID        int64   `json:"id"`
	PlanID    *int64  `json:"plan_id,omitempty"`
	Name      string  `json:"name"`
	Status    string  `json:"status"`
	ExpiresAt *string `json:"expires_at,omitempty"`

	// TotalLimitUSD / RemainingUSD 为 nil 表示不限额（无上限）。
	TotalLimitUSD *float64 `json:"total_limit_usd,omitempty"`
	TotalUsageUSD float64  `json:"total_usage_usd,omitempty"`
	RemainingUSD  *float64 `json:"remaining_usd,omitempty"`
	Unlimited     bool     `json:"unlimited"`
}

// SubscriptionProgressInfo represents subscription with progress info
type SubscriptionProgressInfo struct {
	Subscription *dto.UserSubscription         `json:"subscription"`
	Progress     *service.SubscriptionProgress `json:"progress"`
}

// SubscriptionHandler handles user subscription operations
type SubscriptionHandler struct {
	subscriptionService *service.SubscriptionService
}

// NewSubscriptionHandler creates a new user subscription handler
func NewSubscriptionHandler(subscriptionService *service.SubscriptionService) *SubscriptionHandler {
	return &SubscriptionHandler{
		subscriptionService: subscriptionService,
	}
}

// List handles listing current user's subscriptions
// GET /api/v1/subscriptions
func (h *SubscriptionHandler) List(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not found in context")
		return
	}

	subscriptions, err := h.subscriptionService.ListUserSubscriptions(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	out := make([]dto.UserSubscription, 0, len(subscriptions))
	for i := range subscriptions {
		out = append(out, *dto.UserSubscriptionFromService(&subscriptions[i]))
	}
	response.Success(c, out)
}

// GetActive handles getting current user's active subscriptions
// GET /api/v1/subscriptions/active
func (h *SubscriptionHandler) GetActive(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not found in context")
		return
	}

	subscriptions, err := h.subscriptionService.ListActiveUserSubscriptions(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	out := make([]dto.UserSubscription, 0, len(subscriptions))
	for i := range subscriptions {
		out = append(out, *dto.UserSubscriptionFromService(&subscriptions[i]))
	}
	response.Success(c, out)
}

// GetProgress handles getting subscription progress for current user
// GET /api/v1/subscriptions/progress
func (h *SubscriptionHandler) GetProgress(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not found in context")
		return
	}

	// Get all active subscriptions with progress
	subscriptions, err := h.subscriptionService.ListActiveUserSubscriptions(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	result := make([]SubscriptionProgressInfo, 0, len(subscriptions))
	for i := range subscriptions {
		sub := &subscriptions[i]
		progress, err := h.subscriptionService.GetSubscriptionProgress(c.Request.Context(), sub.ID)
		if err != nil {
			// Skip subscriptions with errors
			continue
		}
		result = append(result, SubscriptionProgressInfo{
			Subscription: dto.UserSubscriptionFromService(sub),
			Progress:     progress,
		})
	}

	response.Success(c, result)
}

// GetSummary handles getting a summary of current user's subscription status
// GET /api/v1/subscriptions/summary
func (h *SubscriptionHandler) GetSummary(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not found in context")
		return
	}

	// Get all active subscriptions
	subscriptions, err := h.subscriptionService.ListActiveUserSubscriptions(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	var totalUsed float64
	items := make([]SubscriptionSummaryItem, 0, len(subscriptions))

	for _, sub := range subscriptions {
		item := SubscriptionSummaryItem{
			ID:            sub.ID,
			PlanID:        sub.PlanID,
			Name:          sub.DisplayName(),
			Status:        sub.Status,
			TotalLimitUSD: sub.EffectiveTotalLimit(),
			TotalUsageUSD: sub.TotalUsageUSD,
			RemainingUSD:  sub.RemainingUSD(),
			Unlimited:     sub.IsUnlimited(),
		}

		// Format expiration time
		if !sub.ExpiresAt.IsZero() {
			formatted := sub.ExpiresAt.Format("2006-01-02T15:04:05Z07:00")
			item.ExpiresAt = &formatted
		}

		// 总额池累计：已消耗金额不随有效期滚动重置。
		totalUsed += sub.TotalUsageUSD

		items = append(items, item)
	}

	summary := struct {
		ActiveCount   int                       `json:"active_count"`
		TotalUsedUSD  float64                   `json:"total_used_usd"`
		TotalLimitUSD *float64                  `json:"total_limit_usd"`
		Unlimited     bool                      `json:"unlimited"`
		Subscriptions []SubscriptionSummaryItem `json:"subscriptions"`
	}{
		ActiveCount:   len(subscriptions),
		TotalUsedUSD:  totalUsed,
		Subscriptions: items,
	}

	// 聚合钱包额度：任一份不限额则整体视为不限额（nil 上限）。
	// 口径与 service.SubscriptionWalletTakesOver / AllocateSubscriptionUsage 一致：
	// Σ可花 = Σtotal_limit − Σtotal_usage。
	var aggregateLimit float64
	hasLimitedWallet := false
	for _, sub := range subscriptions {
		if limit := sub.EffectiveTotalLimit(); limit != nil {
			aggregateLimit += *limit
			hasLimitedWallet = true
		} else {
			summary.Unlimited = true
		}
	}
	if hasLimitedWallet && !summary.Unlimited {
		summary.TotalLimitUSD = &aggregateLimit
	}

	response.Success(c, summary)
}
