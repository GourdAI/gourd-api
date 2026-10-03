//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 资损口子守卫：个人订阅（GroupID=0）三列额度全空 = 不限额。
// 若它仍然接管计费，管理员「只设有效期、忘填额度」就会让用户在全平台免费使用；
// 因此四处 isSubscription* 判定（billing_cache / gateway_usage / openai_gateway_usage /
// usageUnrestricted 展示）都要求 HasEffectiveLimit 为真才认个人订阅。

// 本文件复用包内已有的 f64(v float64) *float64 辅助（user_subscription_personal_quota_test.go）。

func TestHasEffectiveLimit_PersonalSubscription(t *testing.T) {
	now := time.Now().Add(time.Hour)

	// 三列全空：不构成任何闸门 → 不得接管计费。
	empty := &UserSubscription{GroupID: 0, Status: SubscriptionStatusActive, ExpiresAt: now}
	require.False(t, empty.HasEffectiveLimit(nil), "无额度个人订阅不得接管计费（否则全平台免费）")
	require.False(t, empty.HasEffectiveLimit(&Group{ID: 5, DailyLimitUSD: f64(9)}),
		"个人订阅不继承任何分组额度，分组有额度也不能替它成立闸门")

	// 任一日/周/月有值即可接管。
	require.True(t, (&UserSubscription{GroupID: 0, DailyLimitUSD: f64(1)}).HasEffectiveLimit(nil))
	require.True(t, (&UserSubscription{GroupID: 0, WeeklyLimitUSD: f64(1)}).HasEffectiveLimit(nil))
	require.True(t, (&UserSubscription{GroupID: 0, MonthlyLimitUSD: f64(1)}).HasEffectiveLimit(nil))

	// <=0 被 normalizeSubLimit 归一为 nil（不限额），同样不得接管。
	require.False(t, (&UserSubscription{GroupID: 0, DailyLimitUSD: f64(0)}).HasEffectiveLimit(nil),
		"0 = 不限额，不构成闸门")
	require.False(t, (&UserSubscription{GroupID: 0, DailyLimitUSD: f64(-5)}).HasEffectiveLimit(nil),
		"负数脏值不得被当成有效额度")
}

// 分组订阅不受该闸门约束：其「不限额」由分组配置决定，属基线语义，
// 否则会把存量分组订阅错误地踢回余额计费。
func TestHasEffectiveLimit_GroupSubscriptionKeepsBaselineSemantics(t *testing.T) {
	group := &Group{ID: 5, DailyLimitUSD: f64(20)}

	// 订阅自身无额度，但归属分组且分组有额度 → 继承生效，闸门成立。
	inherited := &UserSubscription{GroupID: 5}
	require.True(t, inherited.HasEffectiveLimit(group))

	// 分组也无额度 → 该订阅确实不限额，但分组订阅仍按基线走订阅模式，
	// 这里只如实反映「无生效额度」，由调用方（模式判定）区分 GroupID==0 与分组订阅。
	openGroup := &Group{ID: 6}
	require.False(t, (&UserSubscription{GroupID: 6}).HasEffectiveLimit(openGroup))

	// 归属校验：GroupID 与传入分组不同则不继承（防跨组借额度）。
	require.False(t, (&UserSubscription{GroupID: 5}).HasEffectiveLimit(&Group{ID: 7, DailyLimitUSD: f64(3)}))
}
