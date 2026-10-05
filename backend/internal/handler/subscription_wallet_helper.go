package handler

// 订阅计费判定辅助。
//
// CheckBillingEligibility 需要「该用户全部生效订阅钱包」做聚合额度判定
// （先到期先消耗、允许单笔跨订阅拆分），而历史 handler 只持有
// GetSubscriptionFromContext 返回的「最先到期的一份」。这里提供统一入口，
// 避免在每个 handler 里重复声明切片变量。

import (
	"github.com/gin-gonic/gin"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// subscriptionsForEligibility 返回上下文中的全部生效订阅钱包。
// 中间件未加载订阅或用户无生效钱包时返回 nil（计费侧据此回落余额扣费）。
func subscriptionsForEligibility(c *gin.Context) []*service.UserSubscription {
	if c == nil {
		return nil
	}
	subs, _ := middleware2.GetSubscriptionsFromContext(c)
	return subs
}
