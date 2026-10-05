package service

// 订阅钱包语义的锁定测试（2026-10-03 产品定案）。
//
// 覆盖三条容易被"顺手改回去"的不变式：
//   1. 额度池是一次性的：到期作废、续费不清零用量；
//   2. 无额度的钱包绝不接管扣费（防「只设有效期忘填额度 = 全平台免费」资损）；
//   3. 跨订阅拆分为「先到期先消耗」，不限额钱包垫底且只动用一份。

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func walletF64(v float64) *float64 { return &v }

func newWallet(id int64, limit *float64, used float64, expiresIn time.Duration) *UserSubscription {
	return &UserSubscription{
		ID:            id,
		UserID:        1,
		TotalLimitUSD: limit,
		TotalUsageUSD: used,
		Status:        SubscriptionStatusActive,
		ExpiresAt:     time.Now().Add(expiresIn),
	}
}

// ── 1. 总额池语义 ────────────────────────────────────────────────

func TestSubscriptionWallet_TotalPoolSemantics(t *testing.T) {
	// 额度为 nil 或 <=0 一律归一为「不限额」。
	for name, limit := range map[string]*float64{
		"nil":      nil,
		"zero":     walletF64(0),
		"negative": walletF64(-3),
	} {
		sub := newWallet(1, limit, 999, time.Hour)
		require.Nil(t, sub.EffectiveTotalLimit(), name+" 应视为不限额")
		require.False(t, sub.HasEffectiveLimit(), name+" 不应有生效额度")
		require.True(t, sub.IsUnlimited(), name)
		require.Nil(t, sub.RemainingUSD(), name+" 不限额没有剩余上限")
		require.True(t, sub.CheckLimit(1e9), name+" 不限额恒放行")
	}

	sub := newWallet(2, walletF64(10), 4, time.Hour)
	require.NotNil(t, sub.EffectiveTotalLimit())
	require.True(t, sub.HasEffectiveLimit())
	require.False(t, sub.IsUnlimited())
	require.InDelta(t, 6.0, *sub.RemainingUSD(), 1e-9)

	// 边界：刚好用完 = 不可再消耗（<= 判定，不含等号放行）。
	require.True(t, sub.CheckLimit(6))
	require.False(t, sub.CheckLimit(6.01))

	// 用量超过额度（并发/差额补扣导致）时剩余钳到 0，不返回负数。
	over := newWallet(3, walletF64(10), 12, time.Hour)
	require.InDelta(t, 0.0, *over.RemainingUSD(), 1e-9)
	require.False(t, over.CheckLimit(0))
}

// 续费只延长有效期，**不得**清零已用额度：总额池是一次性的。
// 这是旧「日/周/月窗口」模型最容易残留的假设，必须锁死。
func TestSubscriptionWallet_RenewalDoesNotResetUsage(t *testing.T) {
	sub := newWallet(4, walletF64(10), 7, -time.Hour)
	sub.Status = SubscriptionStatusExpired

	renewed := *sub
	renewed.ExpiresAt = time.Now().AddDate(0, 0, 30)
	renewed.Status = SubscriptionStatusActive

	require.InDelta(t, 7.0, renewed.TotalUsageUSD, 1e-9, "续费不得把已花掉的钱退回去")
	require.InDelta(t, 3.0, *renewed.RemainingUSD(), 1e-9)
}

// 订阅展示名不再回退分组名（订阅不绑分组）。
func TestSubscriptionWallet_DisplayName(t *testing.T) {
	require.Equal(t, "个人订阅", newWallet(5, walletF64(1), 0, time.Hour).DisplayName())
	require.Equal(t, "包月 Pro", (&UserSubscription{
		Plan: &SubscriptionPlanInfo{ID: 9, Name: "包月 Pro"},
	}).DisplayName())
}

// ── 2. 资损闸门 ──────────────────────────────────────────────────

func TestSubscriptionWalletTakesOver(t *testing.T) {
	limited := newWallet(10, walletF64(5), 0, time.Hour)
	unlimited := newWallet(11, nil, 0, time.Hour)

	require.False(t, SubscriptionWalletTakesOver(nil), "无订阅走余额")
	require.False(t, SubscriptionWalletTakesOver([]*UserSubscription{}), "空切片走余额")
	require.False(t, SubscriptionWalletTakesOver([]*UserSubscription{unlimited}),
		"只有一份「设了有效期但没填额度」的钱包必须退回余额，否则等于全平台免费")
	require.False(t, SubscriptionWalletTakesOver([]*UserSubscription{nil, nil}),
		"切片里的 nil 元素不得 panic")

	require.True(t, SubscriptionWalletTakesOver([]*UserSubscription{limited}))
	require.True(t, SubscriptionWalletTakesOver([]*UserSubscription{unlimited, limited}),
		"混合持有：有一份带额度即接管")
}

// ── 3. 跨订阅拆分：先到期先消耗 ──────────────────────────────────

func TestAllocateSubscriptionUsage(t *testing.T) {
	// A 先到期（1h），B 后到期（30d）。
	a := newWallet(20, walletF64(8), 0, time.Hour)
	b := newWallet(21, walletF64(10), 0, 30*24*time.Hour)

	t.Run("单笔落在单一订阅内优先消耗到期早的", func(t *testing.T) {
		allocs, ok := AllocateSubscriptionUsage([]*UserSubscription{b, a}, 3)
		require.True(t, ok)
		require.Len(t, allocs, 1)
		require.Equal(t, int64(20), allocs[0].SubscriptionID, "入参乱序也必须先消耗 A")
		require.InDelta(t, 3.0, allocs[0].Amount, 1e-9)
	})

	t.Run("额度不足时跨订阅拆分", func(t *testing.T) {
		allocs, ok := AllocateSubscriptionUsage([]*UserSubscription{b, a}, 12)
		require.True(t, ok)
		require.Len(t, allocs, 2)
		require.Equal(t, int64(20), allocs[0].SubscriptionID)
		require.InDelta(t, 8.0, allocs[0].Amount, 1e-9, "A 先被榨干")
		require.Equal(t, int64(21), allocs[1].SubscriptionID)
		require.InDelta(t, 4.0, allocs[1].Amount, 1e-9, "余下 4 落在 B")

		var sum float64
		for _, al := range allocs {
			sum += al.Amount
		}
		require.InDelta(t, 12.0, sum, 1e-9, "拆分总额必须守恒")
	})

	t.Run("跳过已榨干的订阅", func(t *testing.T) {
		done := newWallet(22, walletF64(5), 5, 2*time.Hour)
		allocs, ok := AllocateSubscriptionUsage([]*UserSubscription{done, a}, 2)
		require.True(t, ok)
		require.Len(t, allocs, 1)
		require.Equal(t, int64(20), allocs[0].SubscriptionID)
	})

	t.Run("钱不够明确失败而不是部分扣费", func(t *testing.T) {
		allocs, ok := AllocateSubscriptionUsage([]*UserSubscription{a, b}, 100)
		require.False(t, ok, "装不下必须返回 false，由调用方拒绝或回落")
		require.Nil(t, allocs, "失败时不得返回半截分配（否则调用方可能照扣）")
	})

	t.Run("不限额钱包垫底且只动用一份", func(t *testing.T) {
		free1 := newWallet(30, nil, 0, 10*time.Hour)
		free2 := newWallet(31, nil, 0, 20*time.Hour)
		allocs, ok := AllocateSubscriptionUsage([]*UserSubscription{free2, free1, a}, 10)
		require.True(t, ok)
		require.Len(t, allocs, 2)
		require.Equal(t, int64(20), allocs[0].SubscriptionID, "有限额钱包先被消耗")
		require.InDelta(t, 8.0, allocs[0].Amount, 1e-9)
		require.Equal(t, int64(30), allocs[1].SubscriptionID, "无限额取最早到期的一份")
		require.InDelta(t, 2.0, allocs[1].Amount, 1e-9)
	})

	t.Run("全不限额时全部落到一份", func(t *testing.T) {
		free := newWallet(40, nil, 0, 5*time.Hour)
		other := newWallet(41, nil, 0, 6*time.Hour)
		allocs, ok := AllocateSubscriptionUsage([]*UserSubscription{other, free}, 7)
		require.True(t, ok)
		require.Len(t, allocs, 1, "不得把消耗随机散到多份无限额钱包（用量统计会失真）")
		require.Equal(t, int64(40), allocs[0].SubscriptionID)
	})

	t.Run("过期订阅不参与分配", func(t *testing.T) {
		expired := newWallet(50, walletF64(100), 0, -time.Minute)
		_, ok := AllocateSubscriptionUsage([]*UserSubscription{expired}, 1)
		require.False(t, ok)
	})

	t.Run("零成本与负成本直接放行", func(t *testing.T) {
		allocs, ok := AllocateSubscriptionUsage([]*UserSubscription{a}, 0)
		require.True(t, ok)
		require.Nil(t, allocs)

		allocs, ok = AllocateSubscriptionUsage([]*UserSubscription{a}, -5)
		require.True(t, ok)
		require.Nil(t, allocs, "退款/差额退还走独立路径，不在这里产生分配")
	})

	t.Run("刚好花完不被浮点差误拦", func(t *testing.T) {
		// limit=0.3 used=0.1 cost=0.2：数学上恰好覆盖。浮点下 0.3-0.1 != 0.2，
		// 没有容差就会把这笔合法请求判为「装不下」（表现为额度未用尽却被 429）。
		exact := newWallet(60, walletF64(0.3), 0.1, time.Hour)
		allocs, ok := AllocateSubscriptionUsage([]*UserSubscription{exact}, 0.2)
		require.True(t, ok, "余额刚好等于费用必须放行")
		require.Len(t, allocs, 1)
		require.InDelta(t, 0.2, allocs[0].Amount, 1e-9)

		var sum float64
		for _, al := range allocs {
			sum += al.Amount
		}
		require.InDelta(t, 0.2, sum, 1e-9, "容差不得把多花的钱带进拆分")
	})

	t.Run("多份钱包叠加刚好花完", func(t *testing.T) {
		c1 := newWallet(61, walletF64(0.1), 0, time.Hour)
		c2 := newWallet(62, walletF64(0.2), 0, 2*time.Hour)
		c3 := newWallet(63, walletF64(0.3), 0, 3*time.Hour)
		allocs, ok := AllocateSubscriptionUsage([]*UserSubscription{c1, c2, c3}, 0.1+0.2+0.3)
		require.True(t, ok)
		var sum float64
		for _, al := range allocs {
			sum += al.Amount
		}
		require.InDelta(t, 0.6, sum, 1e-9, "三行拆分合计必须等于费用")
		require.Len(t, allocs, 3, "每份都被榨干时应各记一行")
	})

	t.Run("入参含 nil 元素不 panic", func(t *testing.T) {
		allocs, ok := AllocateSubscriptionUsage([]*UserSubscription{nil, a, nil}, 1)
		require.True(t, ok)
		require.Len(t, allocs, 1)
	})

	t.Run("不修改入参订阅对象", func(t *testing.T) {
		guard := newWallet(60, walletF64(4), 1, time.Hour) // 可花 3
		allocs, ok := AllocateSubscriptionUsage([]*UserSubscription{guard}, 2)
		require.True(t, ok)
		require.Len(t, allocs, 1)
		require.InDelta(t, 2.0, allocs[0].Amount, 1e-9)
		require.InDelta(t, 1.0, guard.TotalUsageUSD, 1e-9, "纯函数：用量由调用方提交")
	})
}

// ── 4. 钱包聚合与拆分判定等价 ────────────────────────────────────

func TestAggregateSubscriptionWallet_EquivalentToAllocation(t *testing.T) {
	a := newWallet(70, walletF64(8), 3, time.Hour)
	b := newWallet(71, walletF64(10), 9, 48*time.Hour)
	subs := []UserSubscription{*a, *b}

	data, ok := aggregateSubscriptionWallet(subs)
	require.True(t, ok)
	require.InDelta(t, 18.0, data.TotalLimit, 1e-9, "有限额钱包额度求和")
	require.InDelta(t, 12.0, data.TotalUsage, 1e-9)
	require.False(t, data.HasUnlimited)
	require.Equal(t, SubscriptionStatusActive, data.Status)
	require.True(t, data.ExpiresAt.After(b.ExpiresAt.Add(-time.Second)), "到期时刻取最晚一份")

	// 关键等价式：聚合判定的「可花总额」必须等于拆分算法给出的总额。
	remainAggregate := data.TotalLimit - data.TotalUsage
	allocs, allocOk := AllocateSubscriptionUsage([]*UserSubscription{a, b}, remainAggregate)
	require.True(t, allocOk, "聚合说还能花 %v，拆分就必须能装下同样的金额", remainAggregate)
	var sum float64
	for _, al := range allocs {
		sum += al.Amount
	}
	require.InDelta(t, remainAggregate, sum, 1e-9)

	// 多一分也不该放行 —— 两侧结论必须一致地拒绝。
	_, overOk := AllocateSubscriptionUsage([]*UserSubscription{a, b}, remainAggregate+0.01)
	require.False(t, overOk)
	require.Less(t, data.TotalLimit-data.TotalUsage, remainAggregate+0.01)
}

func TestAggregateSubscriptionWallet_MixedWithUnlimited(t *testing.T) {
	limited := newWallet(80, walletF64(5), 5, time.Hour) // 已榨干
	free := newWallet(81, nil, 20, 48*time.Hour)

	data, ok := aggregateSubscriptionWallet([]UserSubscription{*limited, *free})
	require.True(t, ok)
	require.True(t, data.HasUnlimited, "持有不限额订阅必须标记出来")
	require.InDelta(t, 5.0, data.TotalLimit, 1e-9, "不限额订阅不进求和")
	require.InDelta(t, 5.0, data.TotalUsage, 1e-9)
	// 聚合侧已「额度耗尽」，但拆分侧仍可把费用落到不限额钱包 —— 因此
	// 资格判定必须先短路 HasUnlimited，不能只看 TotalLimit/TotalUsage。
	_, allocOk := AllocateSubscriptionUsage([]*UserSubscription{limited, free}, 3)
	require.True(t, allocOk)
}

func TestAggregateSubscriptionWallet_NoSubscriptions(t *testing.T) {
	data, ok := aggregateSubscriptionWallet(nil)
	require.False(t, ok)
	require.Nil(t, data)

	data, ok = aggregateSubscriptionWallet([]UserSubscription{})
	require.False(t, ok)
	require.Nil(t, data)
}
