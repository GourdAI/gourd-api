package service

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
)

// 订阅额度窗口（周期重置）——纯函数层，不碰数据库，便于单测与复用。
//
// 语义：ResetPeriod 决定「额度是每周期发一次还是一次性」。
//
//	none    一次性总额池（花完为止，到期作废，不滚动）
//	daily   自然日窗口，UTC 00:00 切窗
//	weekly  自然周窗口，周一 00:00(UTC) 切窗
//	monthly 自然月窗口，每月 1 日 00:00(UTC) 切窗
//
// 核心不变量（所有实现必须守住）：
//  1. 同一时刻只存在一个窗口，窗口边界由 now 唯一决定，与调用顺序无关；
//  2. 窗口切换时 total_usage_usd 视为 0——上窗口剩余额度**不累积**，
//     因为「每天 1$、过期销毁」的产品语义就是「不结转」；
//  3. 窗口函数必须是幂等的：同一 now 反复调用结果相同，才能安全地在
//     读路径做懒归一（读失败不能改变语义）。
const (
	subscriptionWindowDailyDuration   = 24 * time.Hour
	subscriptionWindowWeeklyStartWeek = time.Monday
)

// NormalizeSubscriptionResetPeriod 把外部输入归一为合法周期值。
// 空串、未知值一律回落 none（一次性总额池），保证老数据/老前端不会因为
// 一个拼错的字段把计费变成「每分钟清零」这种灾难行为。
func NormalizeSubscriptionResetPeriod(raw string) string {
	switch raw {
	case domain.SubscriptionResetPeriodDaily,
		domain.SubscriptionResetPeriodWeekly,
		domain.SubscriptionResetPeriodMonthly:
		return raw
	default:
		return domain.SubscriptionResetPeriodNone
	}
}

// IsRecurringSubscriptionResetPeriod 报告该周期是否需要按窗口重置额度。
func IsRecurringSubscriptionResetPeriod(period string) bool {
	return NormalizeSubscriptionResetPeriod(period) != domain.SubscriptionResetPeriodNone
}

// SubscriptionWindowStart 返回 now 所在窗口的起点（UTC）。
//
// 一周从周一算起（ISO 8601）；一月按自然月 1 日 00:00 UTC。
// 返回零值 time.Time{} 表示该周期不做窗口（none）。
func SubscriptionWindowStart(period string, now time.Time) time.Time {
	period = NormalizeSubscriptionResetPeriod(period)
	if period == domain.SubscriptionResetPeriodNone {
		return time.Time{}
	}
	utc := now.UTC()
	y, m, d := utc.Date()
	dayStart := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)

	switch period {
	case domain.SubscriptionResetPeriodDaily:
		return dayStart
	case domain.SubscriptionResetPeriodWeekly:
		// Weekday(): Sunday=0 ... Saturday=6。周一起算要先把周日挪到 6。
		offset := (int(utc.Weekday()) + 6) % 7
		return dayStart.AddDate(0, 0, -offset)
	case domain.SubscriptionResetPeriodMonthly:
		return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Time{}
	}
}

// NextSubscriptionWindowStart 返回 current 所在窗口的下一窗口起点。
// 用于展示「下次刷新时间」与后台批处理的边界推进（必须严格晚于 current，
// 否则调用方会死循环）。current 为零值时按 now 起算。
func NextSubscriptionWindowStart(period string, current time.Time, now time.Time) time.Time {
	period = NormalizeSubscriptionResetPeriod(period)
	if period == domain.SubscriptionResetPeriodNone {
		return time.Time{}
	}
	base := current
	if base.IsZero() {
		base = SubscriptionWindowStart(period, now)
	}
	windowStart := SubscriptionWindowStart(period, base)
	switch period {
	case domain.SubscriptionResetPeriodDaily:
		return windowStart.AddDate(0, 0, 1)
	case domain.SubscriptionResetPeriodWeekly:
		return windowStart.AddDate(0, 0, 7)
	case domain.SubscriptionResetPeriodMonthly:
		return windowStart.AddDate(0, 1, 0)
	default:
		return time.Time{}
	}
}

// SubscriptionWindowExpired 报告 usageWindowStart 是否已跨过 now 所在窗口的起点。
//
// usageWindowStart 为零值（列 NULL）时视为「尚未建立窗口」，返回 true，
// 让调用方负责写入第一个窗口起点。
//
// 比较用 Before 而不是 Equal：只要记录起点早于当前窗口起点，就说明至少跨了一个
// 边界（可能跨了多个——用户很久没用过），此时用量一律归零即可，无需逐窗口补算，
// 因为剩余额度本就不累积。
func SubscriptionWindowExpired(period string, usageWindowStart time.Time, now time.Time) bool {
	period = NormalizeSubscriptionResetPeriod(period)
	if period == domain.SubscriptionResetPeriodNone {
		return false
	}
	if usageWindowStart.IsZero() {
		return true
	}
	return usageWindowStart.Before(SubscriptionWindowStart(period, now))
}

// subscriptionWindowEffectiveUsage 返回在 now 这个窗口内「应当被计入」的用量。
//
// 窗口已切换则该行用量视为 0（上窗口剩余不结转，是本特性的核心语义）。
func subscriptionWindowEffectiveUsage(period string, usageWindowStart time.Time, usage float64, now time.Time) float64 {
	if SubscriptionWindowExpired(period, usageWindowStart, now) {
		return 0
	}
	return usage
}

// SubscriptionWindowResetsAt 返回该订阅下一次额度刷新时刻；nil 表示不刷新
// （none 周期或已过期）。用于用户端「下次刷新」展示。
func (s *UserSubscription) SubscriptionWindowResetsAt(now time.Time) *time.Time {
	if s == nil {
		return nil
	}
	if !IsRecurringSubscriptionResetPeriod(s.ResetPeriod) {
		return nil
	}
	if !s.ExpiresAt.After(now) {
		return nil
	}
	current := s.UsageWindowStart
	if SubscriptionWindowExpired(s.ResetPeriod, current, now) {
		current = SubscriptionWindowStart(s.ResetPeriod, now)
	}
	next := NextSubscriptionWindowStart(s.ResetPeriod, current, now)
	if next.IsZero() || next.After(s.ExpiresAt) {
		// 下次刷新会晚于订阅到期：到期后就没有额度了，不再展示刷新。
		return nil
	}
	return &next
}

// NormalizeUsageWindow 把订阅行的窗口状态按 now 归一：
//   - 窗口已跨边界 → TotalUsageUSD 归零、UsageWindowStart 推到当前窗口起点；
//   - 尚未建立窗口 → 写入当前窗口起点，用量保持不变（首次使用，不白送也不没收）。
//
// 返回 needPersist 表示是否需要把归一结果写回数据库。
//
// 注意：本方法只改内存对象，绝不隐式写库——读路径（列表/聚合/预检）拿到的
// 可能是缓存里的共享切片，在此处落库会带来「读接口触发写」与并发写放大。
// 落库由调用方决定：计费事务用条件 UPDATE、后台批处理用批量 UPDATE。
func (s *UserSubscription) NormalizeUsageWindow(now time.Time) (needPersist bool) {
	if s == nil {
		return false
	}
	if !IsRecurringSubscriptionResetPeriod(s.ResetPeriod) {
		return false
	}
	evaluatedAt := now.UTC()
	if s.UsageWindowStart.IsZero() {
		s.UsageWindowStart = SubscriptionWindowStart(s.ResetPeriod, evaluatedAt)
		return true
	}
	if !SubscriptionWindowExpired(s.ResetPeriod, s.UsageWindowStart, evaluatedAt) {
		return false
	}
	s.TotalUsageUSD = 0
	s.UsageWindowStart = SubscriptionWindowStart(s.ResetPeriod, evaluatedAt)
	return true
}

// EffectiveWindowUsageUSD 返回当前窗口内的有效用量（窗口过期视为 0，不改对象）。
// 与 NormalizeUsageWindow 同一口径，供纯计算场景使用。
func (s *UserSubscription) EffectiveWindowUsageUSD(now time.Time) float64 {
	if s == nil {
		return 0
	}
	return subscriptionWindowEffectiveUsage(s.ResetPeriod, s.UsageWindowStart, s.TotalUsageUSD, now.UTC())
}
