//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 窗口起点计算的基准时刻：2026-10-21 是周三。
var (
	windowTestWed = time.Date(2026, 10, 21, 13, 45, 30, 0, time.UTC)
	windowTestMon = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
)

func TestNormalizeSubscriptionResetPeriod(t *testing.T) {
	cases := map[string]string{
		"":        SubscriptionResetPeriodNone,
		"none":    SubscriptionResetPeriodNone,
		"daily":   SubscriptionResetPeriodDaily,
		"weekly":  SubscriptionResetPeriodWeekly,
		"monthly": SubscriptionResetPeriodMonthly,
		// 未知值必须回落 none：宁可当成一次性总额池，也不能猜成某个高频周期
		// （猜错成 daily 会让额度天天重置，等于免费无限用）。
		"dayly": SubscriptionResetPeriodNone,
		"DAILY": SubscriptionResetPeriodNone,
	}
	for in, want := range cases {
		require.Equal(t, want, NormalizeSubscriptionResetPeriod(in), "input=%q", in)
	}
}

func TestSubscriptionWindowStart_Daily(t *testing.T) {
	got := SubscriptionWindowStart(SubscriptionResetPeriodDaily, windowTestWed)
	require.Equal(t, time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC), got)
}

func TestSubscriptionWindowStart_WeeklyStartsMonday(t *testing.T) {
	got := SubscriptionWindowStart(SubscriptionResetPeriodWeekly, windowTestWed)
	require.Equal(t, windowTestMon, got, "周三所在周的起点应是周一 00:00")

	// 周日必须归到**本周**周一（不是下周一）：ISO 8601 与账单周口径一致。
	sunday := time.Date(2026, 10, 25, 23, 59, 0, 0, time.UTC)
	require.Equal(t, windowTestMon, SubscriptionWindowStart(SubscriptionResetPeriodWeekly, sunday))
}

func TestSubscriptionWindowStart_Monthly(t *testing.T) {
	got := SubscriptionWindowStart(SubscriptionResetPeriodMonthly, windowTestWed)
	require.Equal(t, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), got)
}

func TestSubscriptionWindowStart_NoneReturnsZero(t *testing.T) {
	require.True(t, SubscriptionWindowStart(SubscriptionResetPeriodNone, windowTestWed).IsZero())
}

// 非 UTC 输入必须先转到 UTC 再切窗：用户本地 10-21 07:00 (UTC+8) 实际是 UTC 10-20 23:00，
// 属于**前一个** UTC 日。若按本地日切窗，日窗口会比账单日晚 8 小时。
func TestSubscriptionWindowStart_ConvertsToUTC(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	local := time.Date(2026, 10, 21, 7, 0, 0, 0, loc)
	got := SubscriptionWindowStart(SubscriptionResetPeriodDaily, local)
	require.Equal(t, time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), got)
}

func TestNextSubscriptionWindowStart(t *testing.T) {
	require.Equal(t,
		time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC),
		NextSubscriptionWindowStart(SubscriptionResetPeriodDaily, windowTestWed, windowTestWed))
	require.Equal(t,
		time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC),
		NextSubscriptionWindowStart(SubscriptionResetPeriodWeekly, windowTestWed, windowTestWed))
	require.Equal(t,
		time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		NextSubscriptionWindowStart(SubscriptionResetPeriodMonthly, windowTestWed, windowTestWed))
}

// 下一窗口必须严格晚于当前窗口起点，否则调用方按它推进会死循环。
func TestNextSubscriptionWindowStart_StrictlyAfterCurrent(t *testing.T) {
	for _, period := range []string{
		SubscriptionResetPeriodDaily,
		SubscriptionResetPeriodWeekly,
		SubscriptionResetPeriodMonthly,
	} {
		current := SubscriptionWindowStart(period, windowTestWed)
		next := NextSubscriptionWindowStart(period, current, windowTestWed)
		require.True(t, next.After(current), "period=%s", period)
	}
}

func TestSubscriptionWindowExpired(t *testing.T) {
	currentStart := SubscriptionWindowStart(SubscriptionResetPeriodDaily, windowTestWed)

	require.False(t, SubscriptionWindowExpired(SubscriptionResetPeriodDaily, currentStart, windowTestWed),
		"同一窗口不算过期")
	require.True(t, SubscriptionWindowExpired(SubscriptionResetPeriodDaily, currentStart.AddDate(0, 0, -1), windowTestWed),
		"前一天窗口已过期")
	require.True(t, SubscriptionWindowExpired(SubscriptionResetPeriodDaily, time.Time{}, windowTestWed),
		"零值表示尚未建立窗口，需要调用方写入")
	require.False(t, SubscriptionWindowExpired(SubscriptionResetPeriodNone, time.Time{}, windowTestWed),
		"none 周期永不过期（一次性总额池）")
	// 跨多个边界也只看「早于当前起点」：剩余额度本就不累积，无需逐窗口补算。
	require.True(t, SubscriptionWindowExpired(SubscriptionResetPeriodDaily, currentStart.AddDate(0, 0, -30), windowTestWed))
}

// 核心行为：跳窗后用量视为 0，这是「每天 1$」能成立的前提。
func TestEffectiveWindowUsageUSD_ResetsAcrossWindow(t *testing.T) {
	yesterdayStart := SubscriptionWindowStart(SubscriptionResetPeriodDaily, windowTestWed.AddDate(0, 0, -1))
	sub := &UserSubscription{
		ResetPeriod:      SubscriptionResetPeriodDaily,
		TotalLimitUSD:    ptrFloatForWindow(1),
		TotalUsageUSD:    0.99,
		UsageWindowStart: yesterdayStart,
	}
	require.InDelta(t, 0, sub.EffectiveWindowUsageUSD(windowTestWed), 1e-12,
		"昨天的 0.99 已随窗口作废，今天应从 0 开始")

	// 剩余额度必须回到满额 1$，否则用户跳窗后只能拿到 0.01$。
	remaining := sub.RemainingUSDAt(windowTestWed)
	require.NotNil(t, remaining)
	require.InDelta(t, 1, *remaining, 1e-12)
}

func TestEffectiveWindowUsageUSD_SameWindowKeepsUsage(t *testing.T) {
	start := SubscriptionWindowStart(SubscriptionResetPeriodDaily, windowTestWed)
	sub := &UserSubscription{
		ResetPeriod:      SubscriptionResetPeriodDaily,
		TotalLimitUSD:    ptrFloatForWindow(1),
		TotalUsageUSD:    0.4,
		UsageWindowStart: start,
	}
	require.InDelta(t, 0.4, sub.EffectiveWindowUsageUSD(windowTestWed), 1e-12)
	require.InDelta(t, 0.6, *sub.RemainingUSDAt(windowTestWed), 1e-12)
}

// none 周期必须完全不看窗口：一次性总额池跨天不清零。
func TestEffectiveWindowUsageUSD_NoneNeverResets(t *testing.T) {
	sub := &UserSubscription{
		ResetPeriod:      SubscriptionResetPeriodNone,
		TotalLimitUSD:    ptrFloatForWindow(1),
		TotalUsageUSD:    0.9,
		UsageWindowStart: windowTestMon,
	}
	require.InDelta(t, 0.9, sub.EffectiveWindowUsageUSD(windowTestWed), 1e-12)
	require.InDelta(t, 0.1, *sub.RemainingUSDAt(windowTestWed), 1e-12)
}

func TestNormalizeUsageWindow(t *testing.T) {
	t.Run("跨窗口则归零并推进起点", func(t *testing.T) {
		start := SubscriptionWindowStart(SubscriptionResetPeriodDaily, windowTestWed.AddDate(0, 0, -3))
		sub := &UserSubscription{
			ResetPeriod:      SubscriptionResetPeriodDaily,
			TotalUsageUSD:    0.8,
			UsageWindowStart: start,
		}
		require.True(t, sub.NormalizeUsageWindow(windowTestWed))
		require.Zero(t, sub.TotalUsageUSD)
		require.Equal(t, SubscriptionWindowStart(SubscriptionResetPeriodDaily, windowTestWed), sub.UsageWindowStart)
	})

	t.Run("首次使用只建窗口不改用量", func(t *testing.T) {
		sub := &UserSubscription{ResetPeriod: SubscriptionResetPeriodDaily, TotalUsageUSD: 0.3}
		require.True(t, sub.NormalizeUsageWindow(windowTestWed))
		require.InDelta(t, 0.3, sub.TotalUsageUSD, 1e-12, "首次建窗口不得白送也不得没收已用量")
		require.Equal(t, SubscriptionWindowStart(SubscriptionResetPeriodDaily, windowTestWed), sub.UsageWindowStart)
	})

	t.Run("同窗口不动", func(t *testing.T) {
		sub := &UserSubscription{
			ResetPeriod:      SubscriptionResetPeriodDaily,
			TotalUsageUSD:    0.3,
			UsageWindowStart: SubscriptionWindowStart(SubscriptionResetPeriodDaily, windowTestWed),
		}
		require.False(t, sub.NormalizeUsageWindow(windowTestWed))
	})

	t.Run("none 周期不动", func(t *testing.T) {
		sub := &UserSubscription{ResetPeriod: SubscriptionResetPeriodNone, TotalUsageUSD: 5}
		require.False(t, sub.NormalizeUsageWindow(windowTestWed))
		require.InDelta(t, 5, sub.TotalUsageUSD, 1e-12)
	})
}

// 分配必须按窗口用量算余量：跨窗口的订阅应被当成「一分未花」，否则
// 「每天 1$」在第二天只能拆出 0 额度（库里还是昨天的 1.0）。
func TestAllocateSubscriptionUsageAt_ResetsCapacityAcrossWindow(t *testing.T) {
	sub := &UserSubscription{
		ID:               7,
		Status:           SubscriptionStatusActive,
		ExpiresAt:        windowTestWed.Add(48 * time.Hour),
		TotalLimitUSD:    ptrFloatForWindow(1),
		TotalUsageUSD:    1,
		ResetPeriod:      SubscriptionResetPeriodDaily,
		UsageWindowStart: SubscriptionWindowStart(SubscriptionResetPeriodDaily, windowTestWed.AddDate(0, 0, -1)),
	}

	allocs, ok := AllocateSubscriptionUsageAt([]*UserSubscription{sub}, 1, windowTestWed)
	require.True(t, ok, "跳窗后应重新拥有 1$ 额度")
	require.Len(t, allocs, 1)
	require.InDelta(t, 1, allocs[0].Amount, 1e-12)
}

// 额度确实不够时必须报装不下，而不是超额记到钱包上（那是白送）。
func TestSplitSubscriptionUsage_PartialAndRemainder(t *testing.T) {
	start := SubscriptionWindowStart(SubscriptionResetPeriodDaily, windowTestWed)
	sub := &UserSubscription{
		ID:               9,
		Status:           SubscriptionStatusActive,
		ExpiresAt:        windowTestWed.Add(48 * time.Hour),
		TotalLimitUSD:    ptrFloatForWindow(1),
		TotalUsageUSD:    0.6,
		ResetPeriod:      SubscriptionResetPeriodDaily,
		UsageWindowStart: start,
	}

	allocs, remainder := SplitSubscriptionUsageAt([]*UserSubscription{sub}, 1.5, windowTestWed)
	require.Len(t, allocs, 1)
	require.InDelta(t, 0.4, allocs[0].Amount, 1e-12, "钱包只剩 0.4")
	require.InDelta(t, 1.1, remainder, 1e-12, "余下 1.1 必须回落余额")
}

func TestSplitSubscriptionUsage_FullyCovered(t *testing.T) {
	start := SubscriptionWindowStart(SubscriptionResetPeriodDaily, windowTestWed)
	sub := &UserSubscription{
		ID:               10,
		Status:           SubscriptionStatusActive,
		ExpiresAt:        windowTestWed.Add(48 * time.Hour),
		TotalLimitUSD:    ptrFloatForWindow(5),
		TotalUsageUSD:    0,
		ResetPeriod:      SubscriptionResetPeriodDaily,
		UsageWindowStart: start,
	}
	allocs, remainder := SplitSubscriptionUsageAt([]*UserSubscription{sub}, 1.2, windowTestWed)
	require.Len(t, allocs, 1)
	require.InDelta(t, 1.2, allocs[0].Amount, 1e-12)
	require.Zero(t, remainder)
}

func TestSplitSubscriptionUsage_NoWalletFallsBackEntirely(t *testing.T) {
	allocs, remainder := SplitSubscriptionUsageAt(nil, 2, windowTestWed)
	require.Empty(t, allocs)
	require.InDelta(t, 2, remainder, 1e-12)
}

func TestSubscriptionWindowResetsAt(t *testing.T) {
	start := SubscriptionWindowStart(SubscriptionResetPeriodDaily, windowTestWed)
	sub := &UserSubscription{
		Status:           SubscriptionStatusActive,
		ExpiresAt:        windowTestWed.Add(10 * 24 * time.Hour),
		ResetPeriod:      SubscriptionResetPeriodDaily,
		UsageWindowStart: start,
	}
	got := sub.SubscriptionWindowResetsAt(windowTestWed)
	require.NotNil(t, got)
	require.Equal(t, time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC), *got)

	// none 周期没有「下次刷新」。
	sub.ResetPeriod = SubscriptionResetPeriodNone
	require.Nil(t, sub.SubscriptionWindowResetsAt(windowTestWed))
}

// 下次刷新晚于订阅到期时不应展示：到期后就没有额度了。
func TestSubscriptionWindowResetsAt_NilWhenBeyondExpiry(t *testing.T) {
	sub := &UserSubscription{
		Status:           SubscriptionStatusActive,
		ExpiresAt:        windowTestWed.Add(time.Hour), // 当天到期，下一次刷新在明天
		ResetPeriod:      SubscriptionResetPeriodDaily,
		UsageWindowStart: SubscriptionWindowStart(SubscriptionResetPeriodDaily, windowTestWed),
	}
	require.Nil(t, sub.SubscriptionWindowResetsAt(windowTestWed))
}

// 周期快照必须随发放写入订阅行；否则跳窗逻辑永远不生效（等价于一次性总额池）。
func TestCreateSubscription_PersistsResetPeriodSnapshot(t *testing.T) {
	repo := newSubscriptionUserSubRepoStub()
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)

	limit := 1.0
	sub, err := svc.createSubscription(t.Context(), &AssignSubscriptionInput{
		UserID:        42,
		ValidityDays:  30,
		TotalLimitUSD: &limit,
		ResetPeriod:   SubscriptionResetPeriodDaily,
	})
	require.NoError(t, err)
	require.Equal(t, SubscriptionResetPeriodDaily, sub.ResetPeriod)
	require.False(t, sub.UsageWindowStart.IsZero(), "配了周期就必须初始化窗口起点")
}

// 未知周期写入时必须归一为 none，不得把脏值落库（否则每次读取都要重新猜语义）。
func TestCreateSubscription_NormalizesUnknownResetPeriod(t *testing.T) {
	repo := newSubscriptionUserSubRepoStub()
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)

	limit := 1.0
	sub, err := svc.createSubscription(t.Context(), &AssignSubscriptionInput{
		UserID:        43,
		ValidityDays:  30,
		TotalLimitUSD: &limit,
		ResetPeriod:   "dayly",
	})
	require.NoError(t, err)
	require.Equal(t, SubscriptionResetPeriodNone, sub.ResetPeriod)
	require.True(t, sub.UsageWindowStart.IsZero(), "none 周期不应有窗口起点")
}

func ptrFloatForWindow(v float64) *float64 { return &v }
