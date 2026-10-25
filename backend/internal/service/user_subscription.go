package service

import (
	"time"
)

const subscriptionDayDuration = 24 * time.Hour

// UserSubscription 是「个人额度钱包」：一份一次性总额度 + 一个有效期。
//
// 它不绑定任何分组，也不授予任何分组准入（2026-10-03 产品定案）。
// Key 归属哪个分组就按那个分组的倍率计费，只是把钱从这个钱包里扣。
//
// 额度模型对齐 new-api：total_limit_usd / total_usage_usd 单一总额池，
// 花完为止，有效期到期后剩余作废，不随日/周/月滚动重置。
type UserSubscription struct {
	ID     int64
	UserID int64
	// PlanID 来源套餐（可空：管理员手工发放的订阅没有套餐）。
	// 仅用于展示与追溯；额度以本行 TotalLimitUSD 快照为准，套餐改额度不回溯。
	PlanID *int64

	StartsAt  time.Time
	ExpiresAt time.Time
	Status    string

	// TotalLimitUSD 总额度（USD）：nil 或 <=0 视为不限额。
	// 配了 ResetPeriod 时，本字段是「**每个窗口**发这么多」，而不是一次性总量。
	TotalLimitUSD *float64
	// TotalUsageUSD 已消耗金额：随请求原子累加，过期/撤销不回滚。
	// 配了 ResetPeriod 时，这里只是「当前窗口内」的用量——跳窗后应视为 0，
	// 读时用 EffectiveWindowUsageUSD / NormalizeUsageWindow 取有效值，
	// 切勿直接拿本字段与 TotalLimitUSD 比较。
	TotalUsageUSD float64

	// ResetPeriod 额度重置周期快照（none/daily/weekly/monthly）。
	// none = 一次性总额池；其余 = 每窗口重发 TotalLimitUSD。
	ResetPeriod string
	// UsageWindowStart 当前额度窗口起点(UTC)；零值表示尚未建立窗口。
	UsageWindowStart time.Time

	AssignedBy *int64
	AssignedAt time.Time
	Notes      string

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time

	User           *User
	Plan           *SubscriptionPlanInfo
	AssignedByUser *User
}

// SubscriptionPlanInfo 是订阅行上需要的套餐最小投影（仅用于展示来源套餐名）。
// 不直接引用 dbent.SubscriptionPlan：避免领域模型反向依赖生成代码。
type SubscriptionPlanInfo struct {
	ID   int64
	Name string
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

// normalizeSubLimit 订阅额度语义：nil / <=0 均视为不限额（与分组 HasDailyLimit 行为一致）。
func normalizeSubLimit(v *float64) *float64 {
	if v == nil || *v <= 0 {
		return nil
	}
	return v
}

// EffectiveTotalLimit 返回生效总额度，nil 表示不限额。
func (s *UserSubscription) EffectiveTotalLimit() *float64 {
	return normalizeSubLimit(s.TotalLimitUSD)
}

// HasEffectiveLimit 报告该订阅是否设有生效额度。
//
// 用于守住一个资损口子：订阅若额度为空，在「订阅模式」下等于全平台不限额免费
// —— 管理员只建了有效期、忘填额度就会发生。因此无额度的订阅不应接管计费
// （退回余额扣费），见各调用方的 isSubscriptionMode 判定。
//
// 产品定案（2026-10-03）：无额度时**静默退回余额计费**，不在分配接口报 400、
// 也不加前端提示。原因：兑换码/支付订单等存量路径会写备注与有效期但不写额度，
// 强校验会直接打断这些入口。请勿将其当作「缺校验」而补上 400。
func (s *UserSubscription) HasEffectiveLimit() bool {
	return s.EffectiveTotalLimit() != nil
}

// IsUnlimited 报告该订阅是否为不限额钱包。
func (s *UserSubscription) IsUnlimited() bool {
	return !s.HasEffectiveLimit()
}

// RemainingUSD 返回剩余额度；nil 表示不限额（无上限）。
//
// 配了 ResetPeriod 的订阅必须用 RemainingUSDAt：本方法没有时间参数，
// 无法判断窗口是否已跳。保留它是为了兼容不关心周期的调用方（等价于 now）。
func (s *UserSubscription) RemainingUSD() *float64 {
	return s.RemainingUSDAt(time.Now())
}

// RemainingUSDAt 返回 now 所在窗口内的剩余额度；nil 表示不限额。
//
// 窗口已跳时按「用量为 0」计算（上窗口剩余不结转），与 EffectiveWindowUsageUSD 同源。
func (s *UserSubscription) RemainingUSDAt(now time.Time) *float64 {
	limit := s.EffectiveTotalLimit()
	if limit == nil {
		return nil
	}
	remaining := *limit - s.EffectiveWindowUsageUSD(now)
	if remaining < 0 {
		remaining = 0
	}
	return &remaining
}

// subscriptionAllocationEpsilon 是额度比较的容差。
//
// 钱包余额是 float64 减法的结果（limit - usage），与同样由累加得出的 cost 比较时
// 会差出 1e-17 量级。不设容差则「刚好花完」这笔合法请求会被判为装不下。
const subscriptionAllocationEpsilon = 1e-9

// CheckLimit 报告再消耗 additionalCost 后是否仍在额度内。不限额恒为 true。
//
// 供单份订阅的直接校验使用；配了重置周期时按当前窗口用量判定。
func (s *UserSubscription) CheckLimit(additionalCost float64) bool {
	return s.CheckLimitAt(additionalCost, time.Now())
}

// CheckLimitAt 是 CheckLimit 的显式时间版本：窗口已跳时按用量 0 计算。
func (s *UserSubscription) CheckLimitAt(additionalCost float64, now time.Time) bool {
	limit := s.EffectiveTotalLimit()
	if limit == nil {
		return true
	}
	return s.EffectiveWindowUsageUSD(now)+additionalCost <= *limit
}

// ExpiryTime 返回用于「先到期先消耗」排序的到期时刻。
func (s *UserSubscription) ExpiryTime() time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.ExpiresAt
}

// DisplayName 返回订阅展示名：有套餐取套餐名，否则回退「个人订阅」。
func (s *UserSubscription) DisplayName() string {
	if s == nil {
		return ""
	}
	if s.Plan != nil && s.Plan.Name != "" {
		return s.Plan.Name
	}
	return "个人订阅"
}

// SubscriptionAllocation 描述一次消耗在某份订阅上落多少金额。
type SubscriptionAllocation struct {
	SubscriptionID int64
	Amount         float64
}

// AllocateSubscriptionUsage 按「先到期先消耗」把一笔费用拆分到多份订阅钱包上。
//
// 规则（2026-10-03 产品定案）：
//   - 有限额的订阅按到期时间升序优先消耗，允许单笔费用跨多份订阅拆分；
//   - 不限额的订阅排在最后，仅当有限额钱包全部耗尽时才动用，且一次只选一份
//     （避免把消耗随机记到多个无限钱包、导致用量统计失真）；
//   - 返回的 ok=false 表示现有钱包装不下这笔费用（调用方应据此拒绝或回落）。
//
// 纯函数：不修改入参订阅对象，便于单测与调用方自行决定提交顺序。
func AllocateSubscriptionUsage(subs []*UserSubscription, cost float64) ([]SubscriptionAllocation, bool) {
	return AllocateSubscriptionUsageAt(subs, cost, time.Now())
}

// AllocateSubscriptionUsageAt 是 AllocateSubscriptionUsage 的显式时间版本。
//
// 配了重置周期的订阅按 now 所在窗口的用量计算可用余量：窗口已跳时该行视为
// 一分未花（上窗口剩余不结转），这是「每天发 1 美元」能否生效的关键——
// 若仍用库里的 total_usage_usd（上一天的累计值），跳窗后的用户会永远只能拿到
// 「1$ 减上一天用量」的残值。
func AllocateSubscriptionUsageAt(subs []*UserSubscription, cost float64, now time.Time) ([]SubscriptionAllocation, bool) {
	if cost <= 0 {
		return nil, true
	}

	limited := make([]*UserSubscription, 0, len(subs))
	unlimited := make([]*UserSubscription, 0, 2)
	for _, sub := range subs {
		if sub == nil || !sub.IsActive() {
			continue
		}
		if sub.HasEffectiveLimit() {
			limited = append(limited, sub)
		} else {
			unlimited = append(unlimited, sub)
		}
	}

	byExpiry := func(list []*UserSubscription) {
		for i := 1; i < len(list); i++ {
			for j := i; j > 0 && list[j-1].ExpiresAt.After(list[j].ExpiresAt); j-- {
				list[j-1], list[j] = list[j], list[j-1]
			}
		}
	}
	byExpiry(limited)
	byExpiry(unlimited)

	allocations := make([]SubscriptionAllocation, 0, len(limited)+1)
	remaining := cost

	for _, sub := range limited {
		if remaining <= subscriptionAllocationEpsilon {
			break
		}
		capacity := sub.EffectiveTotalLimit()
		avail := *capacity - sub.EffectiveWindowUsageUSD(now)
		if avail <= subscriptionAllocationEpsilon {
			continue
		}
		take := remaining
		if avail < remaining {
			take = avail
		}
		allocations = append(allocations, SubscriptionAllocation{SubscriptionID: sub.ID, Amount: take})
		remaining -= take
	}

	if remaining > subscriptionAllocationEpsilon && len(unlimited) > 0 {
		allocations = append(allocations, SubscriptionAllocation{
			SubscriptionID: unlimited[0].ID,
			Amount:         remaining,
		})
		remaining = 0
	}

	if remaining > subscriptionAllocationEpsilon {
		return nil, false
	}
	return allocations, true
}

// FirstBillableSubscription 返回「先到期先消耗」应首选的那份钱包：
// 优先设了额度的生效钱包中到期最早的一份；没有则退到任意生效钱包；
// 连生效的都没有时退到入参中第一个非 nil 行（预检后快照才过期的场景，
// 归到该行而不是静默丢弃这笔已发生的费用）。
//
// 刻意不依赖调用方传入顺序：「先到期先消耗」是产品定案，不能因为某
// 个调用方忘了 ORDER BY 就把钱记到另一份钱包上。生产取数路径
// （ListActiveByUserID / 计费仓储锁定读）本身已按 expires_at 升序，
// 这里只是多一层不依赖顺序的防御。
func FirstBillableSubscription(subs []*UserSubscription) *UserSubscription {
	pick := func(wantLimited bool, wantActive bool) *UserSubscription {
		var best *UserSubscription
		for _, sub := range subs {
			if sub == nil {
				continue
			}
			if wantActive && !sub.IsActive() {
				continue
			}
			if wantLimited && !sub.HasEffectiveLimit() {
				continue
			}
			if best == nil || sub.ExpiresAt.Before(best.ExpiresAt) {
				best = sub
			}
		}
		return best
	}
	if sub := pick(true, true); sub != nil {
		return sub
	}
	if sub := pick(false, true); sub != nil {
		return sub
	}
	return pick(false, false)
}

// SplitSubscriptionUsage 把一笔费用尽可能拆分到多份订阅钱包上，返回「装不下的部分」。
//
// 与 AllocateSubscriptionUsage 的区别只在装不下时的语义：后者返回 ok=false
// 要求调用方自己决定怎么办；本函数直接给出余量金额，供「免费额度用完→回落余额」
// 场景直接把余量交给余额扣减。
//
// 为什么需要它：AllocateSubscriptionRecordings 的旧口径是「装不下就把整笔记到首个
// 钱包上、让用量合法越界」（宁可透支也不白送）。那个口径在「用户只有免费额度、
// 没有余额」时是对的；但用户如果自己有余额，让免费钱包透支而余额一分不动，
// 等于平台白送。产品定案（2026-10-21）：免费额度花完就回落扣自己余额。
//
// 返回值：allocations 为实际能记到钱包的部分；remainder 为需要转余额的金额。
// cost<=0 或没有任何可用钱包时，remainder 分别等于 0 与 cost。
func SplitSubscriptionUsage(subs []*UserSubscription, cost float64) ([]SubscriptionAllocation, float64) {
	return SplitSubscriptionUsageAt(subs, cost, time.Now())
}

// SplitSubscriptionUsageAt 是 SplitSubscriptionUsage 的显式时间版本。
func SplitSubscriptionUsageAt(subs []*UserSubscription, cost float64, now time.Time) ([]SubscriptionAllocation, float64) {
	if cost <= 0 {
		return nil, 0
	}
	allocations, ok := AllocateSubscriptionUsageAt(subs, cost, now)
	if ok {
		return allocations, 0
	}

	// 装不下：自己重算一遍拆到底的分配，算出已装下的部分。
	// 不直接累加 allocations（AllocateSubscriptionUsageAt 在失败时返回 nil）。
	filled := 0.0
	var partial []SubscriptionAllocation
	for _, sub := range sortedLimitedSubscriptions(subs) {
		if filled >= cost-subscriptionAllocationEpsilon {
			break
		}
		capacity := sub.EffectiveTotalLimit()
		if capacity == nil {
			continue
		}
		avail := *capacity - sub.EffectiveWindowUsageUSD(now)
		if avail <= subscriptionAllocationEpsilon {
			continue
		}
		take := cost - filled
		if avail < take {
			take = avail
		}
		partial = append(partial, SubscriptionAllocation{SubscriptionID: sub.ID, Amount: take})
		filled += take
	}
	remainder := cost - filled
	if remainder < subscriptionAllocationEpsilon {
		remainder = 0
	}
	return partial, remainder
}

// sortedLimitedSubscriptions 返回按「先到期先消耗」排序的、生效且有限额的钱包。
// 不限额钱包不参与：它永远装得下，不需要算余量。
func sortedLimitedSubscriptions(subs []*UserSubscription) []*UserSubscription {
	limited := make([]*UserSubscription, 0, len(subs))
	for _, sub := range subs {
		if sub == nil || !sub.IsActive() || !sub.HasEffectiveLimit() {
			continue
		}
		limited = append(limited, sub)
	}
	for i := 1; i < len(limited); i++ {
		for j := i; j > 0 && limited[j-1].ExpiresAt.After(limited[j].ExpiresAt); j-- {
			limited[j-1], limited[j] = limited[j], limited[j-1]
		}
	}
	return limited
}

// AllocateSubscriptionRecordings 用于「请求已发生、钱必须记下来」的记账场景：
// 优先按「先到期先消耗」拆分；现有钱包装不下时，把余量整笔记到首选钱包上
// 让用量合法越界（后续请求会被预检 429），而不是丢弃拆分——丢弃等于白送。
//
// 【已被 SplitSubscriptionUsage 取代（2026-10-21）】保留仅因为仍有调用方
// （网关降级路径）在「无余额可回落」的场景需要旧口径。新代码应优先用
// SplitSubscriptionUsage：用户有余额时，余量应该扣余额而不是让免费钱包透支。
//
// 返回 nil 表示连一行钱包都没有（调用方应自行告警，不要静默当成记账成功）。
//
// 与 AllocateSubscriptionUsage 的区别仅在装不下的处理；两者共用同一份排序与
// 有限额优先口径，因此网关降级路径与原子计费仓储（repo.Apply）行为一致。
// 返回 nil 表示连一行钱包都没有（调用方应自行告警，不要静默当成记账成功）。
func AllocateSubscriptionRecordings(subs []*UserSubscription, cost float64) []SubscriptionAllocation {
	return AllocateSubscriptionRecordingsAt(subs, cost, time.Now())
}

// AllocateSubscriptionRecordingsAt 是 AllocateSubscriptionRecordings 的显式时间版本。
func AllocateSubscriptionRecordingsAt(subs []*UserSubscription, cost float64, now time.Time) []SubscriptionAllocation {
	if cost <= 0 {
		return nil
	}
	if allocations, ok := AllocateSubscriptionUsageAt(subs, cost, now); ok {
		return allocations
	}
	primary := FirstBillableSubscription(subs)
	if primary == nil {
		return nil
	}
	return []SubscriptionAllocation{{SubscriptionID: primary.ID, Amount: cost}}
}
