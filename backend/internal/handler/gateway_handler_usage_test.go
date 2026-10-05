package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 订阅重构为「个人额度钱包 + 单一总额池」后，/v1/usage 的 unrestricted 响应不再输出
// 日/周/月三档窗口字段（weekly_window_start 已随三档额度一并退役），改为输出聚合钱包概览。
// 本用例由原 TestUsageUnrestrictedIncludesWeeklyWindowStart 按新语义改写而来：
// 保留「响应必须带订阅侧字段」这一原始意图，但断言对象换成总额池口径。
func TestUsageUnrestrictedReportsSubscriptionWalletOverview(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/usage", nil)

	totalLimit := 20.0
	expiresAt := time.Now().UTC().Add(29 * 24 * time.Hour)
	c.Set(string(middleware.ContextKeySubscription), []*service.UserSubscription{
		{
			ID:            1,
			UserID:        7,
			Status:        service.SubscriptionStatusActive,
			StartsAt:      expiresAt.AddDate(0, 0, -30),
			ExpiresAt:     expiresAt,
			TotalLimitUSD: &totalLimit,
			TotalUsageUSD: 5,
		},
	})

	handler := &GatewayHandler{}
	handler.usageUnrestricted(
		c,
		context.Background(),
		&service.APIKey{Group: &service.Group{
			Name:             "Weekly plan",
			SubscriptionType: service.SubscriptionTypeSubscription,
		}},
		middleware.AuthSubject{},
		nil,
		nil,
		nil,
	)

	require.Equal(t, http.StatusOK, recorder.Code)
	var payload struct {
		Mode      string  `json:"mode"`
		Unit      string  `json:"unit"`
		PlanName  string  `json:"planName"`
		Remaining float64 `json:"remaining"`
		IsValid   bool    `json:"isValid"`
		// 日/周/月窗口字段在新模型下不存在，用 map 捕获以便断言键缺席。
		Subscription map[string]any `json:"subscription"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))

	require.Equal(t, "unrestricted", payload.Mode)
	require.True(t, payload.IsValid)
	require.Equal(t, "USD", payload.Unit)
	require.InDelta(t, 15.0, payload.Remaining, 1e-9)
	// 订阅不绑分组：展示名取钱包（无套餐时回退「个人订阅」），不得再泄露分组名。
	require.Equal(t, "个人订阅", payload.PlanName)
	require.NotContains(t, recorder.Body.String(), "weekly_window_start")

	require.Equal(t, 20.0, payload.Subscription["total_limit_usd"])
	require.Equal(t, 5.0, payload.Subscription["total_usage_usd"])
	require.Equal(t, 15.0, payload.Subscription["remaining_usd"])
	require.Equal(t, float64(1), payload.Subscription["subscription_count"])
	require.Equal(t, false, payload.Subscription["has_unlimited"])
	require.NotZero(t, payload.Subscription["expires_at"])
}
