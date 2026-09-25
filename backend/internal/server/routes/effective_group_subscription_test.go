//go:build unit

package routes

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// effective_group_subscription_test.go 覆盖 P1③：生效分组为订阅型且与 auth 阶段按主分组
// 加载的订阅不同组时，必须按生效分组重新加载订阅并覆写 ctx。
//
// 背景：鉴权中间件按**主分组**加载订阅（api_key_auth.go），而生效分组由请求期决议产生。
// 写入期已禁止「标准↔订阅」混合，但「订阅A ↔ 订阅B」仍可能：若不重载，
// usage_logs.subscription_id 会记成主分组的订阅（账目归属错）。
//
// 安全边界：重载**失败时不得覆写**——保留主分组订阅，由计费层按生效分组查出无订阅而拒绝；
// 绝不能沿用主分组订阅放行，也不能降级为余额模式。

func newSubscriptionTestContext(existing *service.UserSubscription) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil).WithContext(context.Background())
	if existing != nil {
		c.Set(string(middleware.ContextKeySubscription), existing)
	}
	return c, w
}

func subscriptionGroup(id int64) *service.Group {
	return &service.Group{
		ID:               id,
		Name:             "sub-group",
		Platform:         service.PlatformOpenAI,
		Status:           service.StatusActive,
		SubscriptionType: service.SubscriptionTypeSubscription,
		Hydrated:         true,
	}
}

// 生效分组与已加载订阅不同组 → 必须重载并覆写为生效分组的订阅。
func TestReloadSubscriptionForEffectiveGroup_SwitchesToEffectiveGroup(t *testing.T) {
	primarySub := &service.UserSubscription{ID: 11, UserID: 42, GroupID: 1}
	c, _ := newSubscriptionTestContext(primarySub)

	effective := subscriptionGroup(2)
	targetSub := &service.UserSubscription{ID: 22, UserID: 42, GroupID: 2}

	var gotUser, gotGroup int64
	loader := func(_ context.Context, userID, groupID int64) (*service.UserSubscription, error) {
		gotUser, gotGroup = userID, groupID
		return targetSub, nil
	}

	reloadSubscriptionForEffectiveGroup(c, effective, 42, loader)

	require.Equal(t, int64(42), gotUser, "必须按当前用户重载")
	require.Equal(t, int64(2), gotGroup, "必须按生效分组 ID 重载，而不是主分组")

	current, ok := middleware.GetSubscriptionFromContext(c)
	require.True(t, ok)
	require.NotNil(t, current)
	require.Equal(t, int64(22), current.ID, "ctx 中的订阅必须被替换为生效分组的订阅（账目归属正确）")
	require.Equal(t, int64(2), current.GroupID)
}

// 已加载订阅本就属于生效分组 → 不重载（热路径，避免多余 DB 往返）。
func TestReloadSubscriptionForEffectiveGroup_SameGroupSkipsReload(t *testing.T) {
	existing := &service.UserSubscription{ID: 22, UserID: 42, GroupID: 2}
	c, _ := newSubscriptionTestContext(existing)

	called := false
	loader := func(_ context.Context, _, _ int64) (*service.UserSubscription, error) {
		called = true
		return nil, nil
	}

	reloadSubscriptionForEffectiveGroup(c, subscriptionGroup(2), 42, loader)

	require.False(t, called, "ctx 中已是生效分组的订阅，不得重复加载")
	current, ok := middleware.GetSubscriptionFromContext(c)
	require.True(t, ok)
	require.Equal(t, int64(22), current.ID)
}

// 重载失败（生效分组无有效订阅）→ 绝不覆写，保留主分组订阅交由计费层拒绝。
func TestReloadSubscriptionForEffectiveGroup_LoadFailureKeepsPrimary(t *testing.T) {
	primarySub := &service.UserSubscription{ID: 11, UserID: 42, GroupID: 1}
	c, _ := newSubscriptionTestContext(primarySub)

	loader := func(_ context.Context, _, _ int64) (*service.UserSubscription, error) {
		return nil, errors.New("subscription not found")
	}

	reloadSubscriptionForEffectiveGroup(c, subscriptionGroup(2), 42, loader)

	current, ok := middleware.GetSubscriptionFromContext(c)
	require.True(t, ok)
	require.Equal(t, int64(11), current.ID, "重载失败必须保留主分组订阅，不得清空（否则可能降级为余额模式放行）")
	require.Equal(t, int64(1), current.GroupID)
}

// 重载返回 nil 且无错误 → 同样不覆写。
func TestReloadSubscriptionForEffectiveGroup_NilSubscriptionKeepsPrimary(t *testing.T) {
	primarySub := &service.UserSubscription{ID: 11, UserID: 42, GroupID: 1}
	c, _ := newSubscriptionTestContext(primarySub)

	loader := func(_ context.Context, _, _ int64) (*service.UserSubscription, error) { return nil, nil }

	reloadSubscriptionForEffectiveGroup(c, subscriptionGroup(2), 42, loader)

	current, ok := middleware.GetSubscriptionFromContext(c)
	require.True(t, ok)
	require.Equal(t, int64(11), current.ID)
}

// 生效分组是标准型 → 不触发重载（订阅语义不适用）。
func TestReloadSubscriptionForEffectiveGroup_StandardGroupNoOp(t *testing.T) {
	primarySub := &service.UserSubscription{ID: 11, UserID: 42, GroupID: 1}
	c, _ := newSubscriptionTestContext(primarySub)

	standard := &service.Group{ID: 2, Status: service.StatusActive, Hydrated: true}

	called := false
	loader := func(_ context.Context, _, _ int64) (*service.UserSubscription, error) {
		called = true
		return nil, nil
	}

	reloadSubscriptionForEffectiveGroup(c, standard, 42, loader)

	require.False(t, called, "标准型分组不涉及订阅，不得触发重载")
	current, ok := middleware.GetSubscriptionFromContext(c)
	require.True(t, ok)
	require.Equal(t, int64(11), current.ID)
}

// loader 为 nil（订阅服务未注入，如 SimpleMode/测试桩）→ 安全 no-op，不得 panic。
func TestReloadSubscriptionForEffectiveGroup_NilLoaderIsNoOp(t *testing.T) {
	primarySub := &service.UserSubscription{ID: 11, UserID: 42, GroupID: 1}
	c, _ := newSubscriptionTestContext(primarySub)

	require.NotPanics(t, func() {
		reloadSubscriptionForEffectiveGroup(c, subscriptionGroup(2), 42, nil)
	})

	current, ok := middleware.GetSubscriptionFromContext(c)
	require.True(t, ok)
	require.Equal(t, int64(11), current.ID)
}
