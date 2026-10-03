package service

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

const subscriptionDayDuration = 24 * time.Hour

type UserSubscription struct {
	ID      int64
	UserID  int64
	GroupID int64

	StartsAt  time.Time
	ExpiresAt time.Time
	Status    string

	DailyWindowStart   *time.Time
	WeeklyWindowStart  *time.Time
	MonthlyWindowStart *time.Time

	DailyUsageUSD   float64
	WeeklyUsageUSD  float64
	MonthlyUsageUSD float64

	// 订阅自有额度：不为 nil 时优先于分组额度。
	// 个人订阅（GroupID==0）仅依赖这三个字段。
	DailyLimitUSD   *float64
	WeeklyLimitUSD  *float64
	MonthlyLimitUSD *float64

	AssignedBy *int64
	AssignedAt time.Time
	Notes      string

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time

	User           *User
	Group          *Group
	AssignedByUser *User
}

func (s *UserSubscription) IsActive() bool {
	return s.Status == SubscriptionStatusActive && time.Now().Before(s.ExpiresAt)
}

func (s *UserSubscription) IsExpired() bool {
	return time.Now().After(s.ExpiresAt)
}

func (s *UserSubscription) DaysRemaining() int {
	return s.daysRemainingAt(time.Now())
}

func (s *UserSubscription) daysRemainingAt(now time.Time) int {
	remaining := s.ExpiresAt.Sub(now)
	if remaining <= 0 {
		return 0
	}

	days := int(remaining / subscriptionDayDuration)
	if remaining%subscriptionDayDuration != 0 {
		days++
	}
	return days
}

func (s *UserSubscription) IsWindowActivated() bool {
	return s.DailyWindowStart != nil || s.WeeklyWindowStart != nil || s.MonthlyWindowStart != nil
}

func (s *UserSubscription) HasOneTimeDailyQuota() bool {
	if s == nil || s.StartsAt.IsZero() || s.ExpiresAt.IsZero() {
		return false
	}
	return !s.ExpiresAt.After(s.StartsAt.AddDate(0, 0, 1))
}

func (s *UserSubscription) NeedsDailyReset() bool {
	return s.NeedsDailyResetAt(time.Now())
}

func (s *UserSubscription) NeedsDailyResetAt(now time.Time) bool {
	_, ok := s.automaticDailyWindowStartAt(now)
	return ok
}

func (s *UserSubscription) NeedsWeeklyReset() bool {
	return s.NeedsWeeklyResetAt(time.Now())
}

func (s *UserSubscription) NeedsWeeklyResetAt(now time.Time) bool {
	if s.WeeklyWindowStart == nil {
		return false
	}
	return !now.Before(s.WeeklyWindowStart.Add(7 * 24 * time.Hour))
}

func (s *UserSubscription) NeedsMonthlyReset() bool {
	return s.NeedsMonthlyResetAt(time.Now())
}

func (s *UserSubscription) NeedsMonthlyResetAt(now time.Time) bool {
	if s.MonthlyWindowStart == nil {
		return false
	}
	return !now.Before(s.MonthlyWindowStart.Add(30 * 24 * time.Hour))
}

func (s *UserSubscription) canAutomaticallyResetDailyAt(now time.Time) bool {
	_, ok := s.automaticDailyWindowStartAt(now)
	return ok
}

// automaticDailyWindowStartAt 计算日窗口按“配置时区日历日”对齐后的当前窗口起点。
// 日额度固定在每天 0 点刷新（与周/月的期限对齐滚动窗口语义不同），因此只要持久化
// 的窗口起点落在更早的日历日，就允许推进到今天 0 点。手动重置、激活等写入的任何
// 非 0 点锚点都会在下一个 0 点被拉回日历日边界，不会永久漂移刷新时刻。
func (s *UserSubscription) automaticDailyWindowStartAt(now time.Time) (time.Time, bool) {
	if s.DailyWindowStart == nil {
		return time.Time{}, false
	}
	if s.HasOneTimeDailyQuota() {
		return time.Time{}, false
	}
	today := timezone.StartOfDay(now)
	if !today.After(timezone.StartOfDay(*s.DailyWindowStart)) {
		return time.Time{}, false
	}
	return today, true
}

func (s *UserSubscription) canAutomaticallyResetWeeklyAt(now time.Time) bool {
	_, ok := s.automaticWindowStartAt(s.WeeklyWindowStart, 7*24*time.Hour, now)
	return ok
}

func (s *UserSubscription) canAutomaticallyResetMonthlyAt(now time.Time) bool {
	_, ok := s.automaticWindowStartAt(s.MonthlyWindowStart, 30*24*time.Hour, now)
	return ok
}

// windowResetAnchor 返回周/月窗口实际推进所依据的锚点。
// 早期订阅把首个窗口初始化在开通日零点；只有这个初始值是无歧义的，之后出现的
// 零点锚点可能来自手动重置，必须保持权威。
// 自动推进（automaticWindowStartAt）与对外展示的重置时间（WeeklyResetTime/
// MonthlyResetTime）必须共用这一修正，否则仪表盘显示的重置时间会早于窗口实际
// 滚动的时间。
// 日窗口按日历日对齐（automaticDailyWindowStartAt），不走这里。
func (s *UserSubscription) windowResetAnchor(previous time.Time) time.Time {
	legacyAnchor := startOfDay(s.StartsAt)
	if legacyAnchor.Before(s.StartsAt) && previous.Equal(legacyAnchor) {
		return s.StartsAt
	}
	return previous
}

// automaticWindowStartAt 计算周/月窗口（期限对齐滚动窗口）的当前窗口起点。
// 窗口从锚点按整数个 period 步进，且不越过订阅到期时间，避免最后一个不完整
// 周期重复发放额度（issue #5051）。日窗口不走此函数，见 automaticDailyWindowStartAt。
func (s *UserSubscription) automaticWindowStartAt(previous *time.Time, period time.Duration, now time.Time) (time.Time, bool) {
	if previous == nil {
		return time.Time{}, false
	}

	anchor := s.windowResetAnchor(*previous)
	next := anchor.Add(period)
	if now.Before(next) || !next.Before(s.ExpiresAt) {
		return time.Time{}, false
	}

	periods := now.Sub(anchor) / period
	lastPeriodBeforeExpiry := (s.ExpiresAt.Sub(anchor) - 1) / period
	if periods > lastPeriodBeforeExpiry {
		periods = lastPeriodBeforeExpiry
	}
	return anchor.Add(periods * period), true
}

func (s *UserSubscription) DailyResetTime() *time.Time {
	if s.DailyWindowStart == nil {
		return nil
	}
	if s.HasOneTimeDailyQuota() {
		t := s.ExpiresAt
		return &t
	}
	// 日窗口按日历日对齐：下次刷新固定在窗口起点所在日的次日 0 点。
	t := timezone.StartOfDay(*s.DailyWindowStart).AddDate(0, 0, 1)
	return &t
}

func (s *UserSubscription) WeeklyResetTime() *time.Time {
	if s.WeeklyWindowStart == nil {
		return nil
	}
	t := s.windowResetAnchor(*s.WeeklyWindowStart).Add(7 * 24 * time.Hour)
	return &t
}

func (s *UserSubscription) MonthlyResetTime() *time.Time {
	if s.MonthlyWindowStart == nil {
		return nil
	}
	t := s.windowResetAnchor(*s.MonthlyWindowStart).Add(30 * 24 * time.Hour)
	return &t
}

// normalizeSubLimit 订阅额度语义：nil / <=0 均视为不限额（与分组 HasDailyLimit 行为一致）。
func normalizeSubLimit(v *float64) *float64 {
	if v == nil || *v <= 0 {
		return nil
	}
	return v
}

// limitFallbackGroup 返回可用于额度回退的分组：仅当订阅归属该分组时，
// 才继承分组额度；个人订阅（GroupID=0）不受任何分组额度约束。
func (s *UserSubscription) limitFallbackGroup(group *Group) *Group {
	if group == nil || s.GroupID != group.ID {
		return nil
	}
	return group
}

// EffectiveDailyLimit 返回生效的日额度：订阅自有额度优先，其次归属分组额度，均未设置返回 nil（不限）。
func (s *UserSubscription) EffectiveDailyLimit(group *Group) *float64 {
	if s.DailyLimitUSD != nil {
		return normalizeSubLimit(s.DailyLimitUSD)
	}
	g := s.limitFallbackGroup(group)
	if g != nil {
		return normalizeSubLimit(g.DailyLimitUSD)
	}
	return nil
}

func (s *UserSubscription) EffectiveWeeklyLimit(group *Group) *float64 {
	if s.WeeklyLimitUSD != nil {
		return normalizeSubLimit(s.WeeklyLimitUSD)
	}
	g := s.limitFallbackGroup(group)
	if g != nil {
		return normalizeSubLimit(g.WeeklyLimitUSD)
	}
	return nil
}

func (s *UserSubscription) EffectiveMonthlyLimit(group *Group) *float64 {
	if s.MonthlyLimitUSD != nil {
		return normalizeSubLimit(s.MonthlyLimitUSD)
	}
	g := s.limitFallbackGroup(group)
	if g != nil {
		return normalizeSubLimit(g.MonthlyLimitUSD)
	}
	return nil
}

// IsPersonal 标记该订阅不绑定分组（全模型通用额度订阅）。
func (s *UserSubscription) IsPersonal() bool {
	return s.GroupID == 0
}

// DisplayName 返回订阅展示名：分组订阅取分组名，
// 个人订阅（Group 边为 nil）回退为「个人订阅」，避免调用方 nil 解引用。
func (s *UserSubscription) DisplayName() string {
	if s == nil {
		return ""
	}
	if s.Group != nil && s.Group.Name != "" {
		return s.Group.Name
	}
	if s.IsPersonal() {
		return "个人订阅"
	}
	return ""
}

func checkLimit(usage float64, limit *float64, additionalCost float64) bool {
	if limit == nil || *limit <= 0 {
		return true
	}
	return usage+additionalCost <= *limit
}

func (s *UserSubscription) CheckDailyLimit(group *Group, additionalCost float64) bool {
	return checkLimit(s.DailyUsageUSD, s.EffectiveDailyLimit(group), additionalCost)
}

func (s *UserSubscription) CheckWeeklyLimit(group *Group, additionalCost float64) bool {
	return checkLimit(s.WeeklyUsageUSD, s.EffectiveWeeklyLimit(group), additionalCost)
}

func (s *UserSubscription) CheckMonthlyLimit(group *Group, additionalCost float64) bool {
	return checkLimit(s.MonthlyUsageUSD, s.EffectiveMonthlyLimit(group), additionalCost)
}

func (s *UserSubscription) CheckAllLimits(group *Group, additionalCost float64) (daily, weekly, monthly bool) {
	daily = s.CheckDailyLimit(group, additionalCost)
	weekly = s.CheckWeeklyLimit(group, additionalCost)
	monthly = s.CheckMonthlyLimit(group, additionalCost)
	return
}
