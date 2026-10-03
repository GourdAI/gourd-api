package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func f64(v float64) *float64 { return &v }

// 个人订阅（GroupID=0）：不继承任何分组额度，只用订阅自有额度。
func TestUserSubscriptionPersonalLimit_IgnoresGroupLimits(t *testing.T) {
	sub := &UserSubscription{
		ID:              1,
		GroupID:         0,
		DailyLimitUSD:   f64(5),
		WeeklyLimitUSD:  f64(20),
		MonthlyLimitUSD: f64(50),
	}
	group := &Group{ID: 7, Name: "标准组", DailyLimitUSD: f64(100)}

	require.True(t, sub.IsPersonal())
	assert.Equal(t, 5.0, *sub.EffectiveDailyLimit(group))
	assert.Equal(t, 20.0, *sub.EffectiveWeeklyLimit(group))
	assert.Equal(t, 50.0, *sub.EffectiveMonthlyLimit(group))

	// 分组额度绝不能泄漏到个人订阅上
	ownOnly := &UserSubscription{ID: 2, GroupID: 0}
	assert.Nil(t, ownOnly.EffectiveDailyLimit(group), "个人订阅无自有额度时不受分组额度约束")
	assert.True(t, ownOnly.CheckDailyLimit(group, 1e9), "不限额的个人订阅不应被分组额度拦住")
}

// 分组订阅：自有额度优先于分组额度。
func TestUserSubscriptionGroupLimit_OwnOverridesGroup(t *testing.T) {
	group := &Group{ID: 7, Name: "Pro", DailyLimitUSD: f64(100)}

	own := &UserSubscription{ID: 3, GroupID: 7, DailyLimitUSD: f64(5)}
	assert.Equal(t, 5.0, *own.EffectiveDailyLimit(group))

	inherited := &UserSubscription{ID: 4, GroupID: 7}
	assert.Equal(t, 100.0, *inherited.EffectiveDailyLimit(group))

	// 自有额度为 0 表示不限额（与分组 HasDailyLimit 的 >0 语义一致）
	zero := &UserSubscription{ID: 5, GroupID: 7, DailyLimitUSD: f64(0)}
	assert.Nil(t, zero.EffectiveDailyLimit(group))
}

// 订阅归属 A 组时，不应继承 B 组的额度（防止展示/校验口径分叉）。
func TestUserSubscriptionLimit_ForeignGroupNotInherited(t *testing.T) {
	sub := &UserSubscription{ID: 6, GroupID: 1}
	other := &Group{ID: 2, Name: "other", DailyLimitUSD: f64(9)}
	assert.Nil(t, sub.EffectiveDailyLimit(other))
}

// Check*Limit 以 ActualCost 为阈值：达到额度即拒，未达放行。
func TestUserSubscriptionCheckLimit_Boundary(t *testing.T) {
	sub := &UserSubscription{
		ID:            7,
		GroupID:       0,
		DailyLimitUSD: f64(10),
		DailyUsageUSD: 9,
	}
	assert.True(t, sub.CheckDailyLimit(nil, 1), "刚好用满仍放行")
	assert.False(t, sub.CheckDailyLimit(nil, 1.01), "超出额度应拒绝")
	assert.True(t, sub.CheckWeeklyLimit(nil, 1e9), "未配置周额度=不限")
}

// DisplayName 在 Group 边缺失（个人订阅）时回退，避免调用方 nil 解引用。
func TestUserSubscriptionDisplayName(t *testing.T) {
	assert.Equal(t, "个人订阅", (&UserSubscription{GroupID: 0}).DisplayName())
	assert.Equal(t, "Pro", (&UserSubscription{GroupID: 7, Group: &Group{Name: "Pro"}}).DisplayName())
	// 分组订阅但边未加载：不假装是个人订阅
	assert.Equal(t, "", (&UserSubscription{GroupID: 7}).DisplayName())
}

// calculateProgress 必须容忍 nil group（个人订阅），并按自有额度出进度。
func TestCalculateProgress_PersonalSubscriptionWithNilGroup(t *testing.T) {
	svc := newTestSubscriptionService()
	now := time.Now()

	sub := &UserSubscription{
		ID:               1,
		GroupID:          0,
		ExpiresAt:        now.Add(10 * 24 * time.Hour),
		DailyLimitUSD:    f64(10),
		DailyUsageUSD:    3,
		DailyWindowStart: ptrTime(now.Add(-12 * time.Hour)),
	}

	progress := svc.calculateProgress(sub, nil)

	require.NotNil(t, progress.Daily)
	assert.Equal(t, "个人订阅", progress.GroupName)
	assert.Equal(t, 10.0, progress.Daily.LimitUSD)
	assert.Equal(t, 7.0, progress.Daily.RemainingUSD)
	assert.Equal(t, 30.0, progress.Daily.Percentage)
	assert.Nil(t, progress.Weekly, "未配置周额度则不产出周进度")
	assert.Nil(t, progress.Monthly)
}
