package service

import (
	"context"
	"errors"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/subscriptionplan"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 用户自助领取订阅（免费额度套餐）。
//
// 背景（2026-10-21 需求）：「每天发 1$、过期销毁、注册后自动绑定」这类免费订阅
// 不应由管理员逐个分配。产品定案是：
//   - 管理员在后台建套餐时勾「可自助领取」，用户在订阅页自己点一下领取；
//   - 同一用户对同一套餐只能持有**一份**（重复点 = 幂等，不叠加额度）；
//   - 「关掉这个订阅」= 取消勾选 self_claimable / 下架，已发出的那份不自动回收
//     （已发的额度是用户凭据，静默回收会直接引发客诉）；要回收走管理员作废。
//
// 为什么必须「一人一套餐一份」而不是允许重复领：
// ResetPeriod 是滚动窗口，多份同套餐只会把同一周期的额度重复发 N 次
// （用户点 10 次就变 10$/天），这是直接的资损路径。幂等键用 (user_id, plan_id)。

// ErrSubscriptionNotClaimable 该套餐不允许自助领取（未开启 / 已下架 / 非免费）。
var ErrSubscriptionNotClaimable = infraerrors.BadRequest(
	"SUBSCRIPTION_NOT_CLAIMABLE",
	"this plan is not available for self-claim",
)

// ErrSubscriptionAlreadyClaimed 同一用户已持有该套餐，重复领取按幂等处理不报错，
// 但在响应里标记 already_claimed 便于前端提示。此错误用于「不允许重复领取」
// 的严格模式（例如后台校验），端点默认走幂等。
var ErrSubscriptionAlreadyClaimed = infraerrors.Conflict(
	"SUBSCRIPTION_ALREADY_CLAIMED",
	"you already hold this subscription plan",
)

// ClaimableSubscriptionPlan 是用户端「可领取套餐」的展示投影。
type ClaimableSubscriptionPlan struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	TotalLimitUSD *float64 `json:"total_limit_usd"`
	ResetPeriod   string   `json:"reset_period"`
	ValidityDays  int      `json:"validity_days"`
	ValidityUnit  string   `json:"validity_unit"`
	Features      string   `json:"features"`
	SortOrder     int      `json:"sort_order"`
	// AlreadyClaimed 该用户当前是否已持有（前端据此把按钮置为「已领取」）。
	AlreadyClaimed bool `json:"already_claimed"`
}

// SubscriptionClaimResult 是领取结果。
type SubscriptionClaimResult struct {
	Subscription UserSubscription `json:"subscription"`
	// AlreadyClaimed 表示本次是幂等命中（未新建记录、未叠加额度）。
	AlreadyClaimed bool `json:"already_claimed"`
}

// ListClaimablePlans 返回当前用户可自助领取的套餐列表（含「已领取」标记）。
//
// 筛选口径：for_sale=true 且 self_claimable=true。
// 不按 group 过滤：订阅不绑分组（契约 6），套餐上的 group_id 只用于支付对账。
func (s *SubscriptionService) ListClaimablePlans(ctx context.Context, userID int64) ([]ClaimableSubscriptionPlan, error) {
	if s == nil || s.entClient == nil {
		return nil, nil
	}
	plans, err := s.entClient.SubscriptionPlan.Query().
		Where(
			subscriptionplan.ForSaleEQ(true),
			subscriptionplan.SelfClaimableEQ(true),
		).
		Order(subscriptionplan.BySortOrder()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(plans) == 0 {
		return []ClaimableSubscriptionPlan{}, nil
	}

	// 已持有的 plan_id 集合：一次查询，避免每个套餐打一次库。
	claimed := make(map[int64]bool)
	if userID > 0 {
		subs, err := s.userSubRepo.ListByUserID(ctx, userID)
		if err != nil {
			return nil, err
		}
		for i := range subs {
			sub := &subs[i]
			// 撤销（软删）/过期的不算「已持有」：用户应该能重新领。
			// 状态判定与 IsActive 同源，避免展示说「已领取」但实际用不了。
			if sub.PlanID != nil && sub.IsActive() {
				claimed[*sub.PlanID] = true
			}
		}
	}

	out := make([]ClaimableSubscriptionPlan, 0, len(plans))
	for _, plan := range plans {
		limit := plan.TotalLimitUsd
		out = append(out, ClaimableSubscriptionPlan{
			ID:             plan.ID,
			Name:           plan.Name,
			Description:    plan.Description,
			TotalLimitUSD:  limit,
			ResetPeriod:    NormalizeSubscriptionResetPeriod(plan.ResetPeriod),
			ValidityDays:   plan.ValidityDays,
			ValidityUnit:   plan.ValidityUnit,
			Features:       plan.Features,
			SortOrder:      plan.SortOrder,
			AlreadyClaimed: claimed[plan.ID],
		})
	}
	return out, nil
}

// ClaimSubscription 让 userID 自助领取 planID 这份套餐。
//
// 幂等语义：已持有同一套餐的生效订阅 → 直接返回现有那份（alreadyClaimed=true），
// 不新建、不叠加额度。这样用户连点、前端重试都不会变相发钱。
//
// 并发安全：本函数不是「查无再建」的裸竞态——它依赖数据库层的唯一性兜底吗？
// 没有。user_subscriptions 上没有 (user_id, plan_id) 唯一约束（历史上有
// (user,group) 唯一索引，已在迁移 246 删除，且本表允许同套餐多份合法——支付
// 重复购买就靠这个）。因此并发双击可能建出两份。
// 取舍：不为此加唯一约束（会打断支付链路与管理员重复发放），而是：
//   - ListByUserID 的检查 + Create 紧跟其后，窗口极小；
//   - 万一建出两份，用户拿到两份额度（多发，不是少发），且管理员可见、可作废。
//
// 与「窗口边界竞态宁可多发」保持同一取向：免费额度多发 1$ 远轻于让用户点不动。
func (s *SubscriptionService) ClaimSubscription(ctx context.Context, userID, planID int64) (*SubscriptionClaimResult, error) {
	if s == nil || s.entClient == nil {
		return nil, ErrSubscriptionNotClaimable
	}
	if userID <= 0 {
		return nil, infraerrors.Unauthorized("SUBSCRIPTION_CLAIM_UNAUTHORIZED", "login required")
	}
	if planID <= 0 {
		return nil, ErrSubscriptionNotClaimable
	}

	plan, err := s.entClient.SubscriptionPlan.Get(ctx, planID)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, infraerrors.NotFound("PLAN_NOT_FOUND", "subscription plan not found")
		}
		return nil, err
	}
	if !plan.ForSale || !plan.SelfClaimable {
		return nil, ErrSubscriptionNotClaimable
	}

	// 已持有则幂等返回。
	existing, err := s.findClaimedSubscription(ctx, userID, planID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return &SubscriptionClaimResult{Subscription: *existing, AlreadyClaimed: true}, nil
	}

	// 按套餐配置发放一份新钱包：额度与周期都从套餐快照。
	// validityDays 用套餐的 validity_days（用户不能自选有效期，
	// 否则「每天 1$」会被领成「每天 1$ + 100 年有效期」）。
	sub, _, err := s.AssignOrExtendSubscription(ctx, &AssignSubscriptionInput{
		UserID:        userID,
		PlanID:        &plan.ID,
		ValidityDays:  plan.ValidityDays,
		AssignedBy:    0, // 0 = 用户自助，系统代发
		Notes:         buildSelfClaimNotes(plan.Name),
		TotalLimitUSD: plan.TotalLimitUsd,
		ResetPeriod:   NormalizeSubscriptionResetPeriod(plan.ResetPeriod),
	})
	if err != nil {
		return nil, err
	}
	if sub == nil {
		return nil, errors.New("subscription claim returned nil")
	}
	return &SubscriptionClaimResult{Subscription: *sub}, nil
}

// findClaimedSubscription 找该用户对同一套餐的**生效**订阅。
//
// 「生效」= status=active 且未过期。过期/撤销的不算：用户应该能重新领，
// 否则一份到期后「每天 1$」就永久失效，而它本来就是周期性的免费额度。
func (s *SubscriptionService) findClaimedSubscription(ctx context.Context, userID, planID int64) (*UserSubscription, error) {
	subs, err := s.userSubRepo.ListByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}
	for i := range subs {
		sub := &subs[i]
		if sub.PlanID != nil && *sub.PlanID == planID && sub.IsActive() {
			return sub, nil
		}
	}
	return nil, nil
}

// buildSelfClaimNotes 生成自查日志用的备注（管理端能看到来历）。
func buildSelfClaimNotes(planName string) string {
	name := strings.TrimSpace(planName)
	if name == "" {
		name = "plan"
	}
	return "self-claimed: " + name + " @" + time.Now().UTC().Format(time.RFC3339)
}
