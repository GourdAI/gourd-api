package service

// ─────────────────────────────────────────────────────────────────────────────
// 订阅进度计算（calculateProgress / GetSubscriptionProgress /
// GetUserSubscriptionsWithProgress）的语义锁定测试。
//
// 窗口概念已废除：Daily/Weekly/Monthly 三档滚动额度、UsageWindowProgress 类型、
// *WindowStart / ResetsAt / ResetsInSeconds、以及 progress.GroupName 都不存在了，
// calculateProgress 也不再接收 *Group 参数（旧签名 calculateProgress(sub, group)
// 已删除）。一份订阅 = 一个一次性总额池（total_limit_usd / total_usage_usd）
// 加一个有效期：花完为止、到期作废，额度不随日/周/月滚动重置。
//
// 因此进度只反映「单一总额池」，本文件锁死四条不变式：
//  1. 展示字段（ID / Name / ExpiresAt / ExpiresInDays）全部来自订阅本身，
//     Name 取套餐名，无套餐时回退「个人订阅」（不再有分组名可回退）；
//  2. Percentage = TotalUsageUSD / TotalLimitUSD * 100，钳在 [0, 100]；
//  3. 无分母（不限额钱包：额度 nil / 0 / 负数）时 Percentage 恒为 0、
//     RemainingUSD 恒为 nil —— 没有分母绝不能显示 100%；
//  4. 进度计算零分组依赖、零额外查询，且多份订阅按仓储给出的
//     「先到期先消耗」顺序原样透传。
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"errors"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// --- 验证 calculateProgress 纯函数行为正确（单一总额池口径） ---

func newTestSubscriptionService() *SubscriptionService {
	return &SubscriptionService{}
}

// ptrFloat64 / ptrTime 为包内多个测试文件共用的指针构造器，钱包化后本文件
// 只用到 ptrFloat64（窗口起点字段已随窗口模型一起删除），保留定义以免破坏同包其他测试。
func ptrFloat64(v float64) *float64  { return &v }
func ptrTime(t time.Time) *time.Time { return &t }

// ── 1. 展示字段 ────────────────────────────────────────────────

// 基础字段全部来自订阅本身；无套餐（Plan == nil）时展示名必须是「个人订阅」，
// 不得再回退到任何分组名。
func TestCalculateProgress_BasicFields(t *testing.T) {
	svc := newTestSubscriptionService()
	now := time.Now()

	sub := &UserSubscription{
		ID:        100,
		UserID:    7,
		ExpiresAt: now.Add(30 * 24 * time.Hour),
		Status:    SubscriptionStatusActive,
	}

	progress := svc.calculateProgress(sub)

	require.Equal(t, int64(100), progress.ID)
	require.Equal(t, "个人订阅", progress.Name, "Plan 为空时展示名必须回退「个人订阅」")
	require.Equal(t, sub.ExpiresAt, progress.ExpiresAt)
	require.Equal(t, 30, progress.ExpiresInDays)
}

// 有套餐时展示名取套餐名（Plan.Name）。
func TestCalculateProgress_NameUsesPlanName(t *testing.T) {
	svc := newTestSubscriptionService()

	sub := &UserSubscription{
		ID:        1,
		Plan:      &SubscriptionPlanInfo{ID: 9, Name: "包月 Pro"},
		ExpiresAt: time.Now().Add(10 * 24 * time.Hour),
		Status:    SubscriptionStatusActive,
	}

	progress := svc.calculateProgress(sub)

	require.Equal(t, "包月 Pro", progress.Name)
}

// ── 2. 有限额钱包：Percentage / RemainingUSD ───────────────────

func TestCalculateProgress_LimitedWallet(t *testing.T) {
	svc := newTestSubscriptionService()

	sub := &UserSubscription{
		ID:            1,
		TotalLimitUSD: ptrFloat64(10),
		TotalUsageUSD: 3,
		Status:        SubscriptionStatusActive,
		ExpiresAt:     time.Now().Add(10 * 24 * time.Hour),
	}

	progress := svc.calculateProgress(sub)

	require.NotNil(t, progress.TotalLimitUSD, "有限额钱包的 TotalLimitUSD 不应为 nil")
	require.InDelta(t, 10.0, *progress.TotalLimitUSD, 1e-9)
	require.InDelta(t, 3.0, progress.TotalUsageUSD, 1e-9)
	require.NotNil(t, progress.RemainingUSD)
	require.InDelta(t, 7.0, *progress.RemainingUSD, 1e-9)
	require.InDelta(t, 30.0, progress.Percentage, 1e-9, "3/10 应为 30%")
	require.False(t, progress.Unlimited)
}

// 超额使用：百分比钳到 100，余额钳到 0（不返回负数）。
func TestCalculateProgress_OverLimit_ClampedTo100Percent(t *testing.T) {
	svc := newTestSubscriptionService()

	sub := &UserSubscription{
		ID:            1,
		TotalLimitUSD: ptrFloat64(10),
		TotalUsageUSD: 15, // 已超过总额
		Status:        SubscriptionStatusActive,
		ExpiresAt:     time.Now().Add(10 * 24 * time.Hour),
	}

	progress := svc.calculateProgress(sub)

	require.InDelta(t, 100.0, progress.Percentage, 1e-9, "超额使用应被截断为 100%")
	require.NotNil(t, progress.RemainingUSD)
	require.InDelta(t, 0.0, *progress.RemainingUSD, 1e-9, "超额使用时剩余应为 0")
	require.False(t, progress.Unlimited)
}

// 边界：用量恰好等于额度 → 100% / 剩余 0（既不能被误判成不限额，也不能算成 99.x%）。
func TestCalculateProgress_UsageEqualsLimit(t *testing.T) {
	svc := newTestSubscriptionService()

	sub := &UserSubscription{
		ID:            1,
		TotalLimitUSD: ptrFloat64(10),
		TotalUsageUSD: 10,
		Status:        SubscriptionStatusActive,
		ExpiresAt:     time.Now().Add(10 * 24 * time.Hour),
	}

	progress := svc.calculateProgress(sub)

	require.InDelta(t, 100.0, progress.Percentage, 1e-9)
	require.NotNil(t, progress.RemainingUSD)
	require.InDelta(t, 0.0, *progress.RemainingUSD, 1e-9)
	require.False(t, progress.Unlimited)
}

// 全新钱包（用量 0）：0%、剩余等于全额。
func TestCalculateProgress_FreshWalletZeroUsage(t *testing.T) {
	svc := newTestSubscriptionService()

	sub := &UserSubscription{
		ID:            1,
		TotalLimitUSD: ptrFloat64(50),
		TotalUsageUSD: 0,
		Status:        SubscriptionStatusActive,
		ExpiresAt:     time.Now().Add(30 * 24 * time.Hour),
	}

	progress := svc.calculateProgress(sub)

	require.InDelta(t, 0.0, progress.Percentage, 1e-9)
	require.NotNil(t, progress.RemainingUSD)
	require.InDelta(t, 50.0, *progress.RemainingUSD, 1e-9, "未消耗时剩余应等于总额度")
	require.False(t, progress.Unlimited)
}

// ── 3. 不限额钱包：没有分母不得显示 100% ───────────────────────

// nil / 0 / 负数三种写法都归一为「不限额」：EffectiveTotalLimit 为 nil、
// Unlimited 为 true、RemainingUSD 为 nil，且 Percentage 必须停在 0。
func TestCalculateProgress_UnlimitedWallets(t *testing.T) {
	svc := newTestSubscriptionService()

	cases := []struct {
		name  string
		limit *float64
	}{
		{"额度为 nil", nil},
		{"额度为 0", ptrFloat64(0)},
		{"额度为负数", ptrFloat64(-3)},
	}

	for _, tc := range cases {
		sub := &UserSubscription{
			ID:            1,
			TotalLimitUSD: tc.limit,
			TotalUsageUSD: 3, // 不限额钱包同样可以记录用量，但不参与百分比
			Status:        SubscriptionStatusActive,
			ExpiresAt:     time.Now().Add(10 * 24 * time.Hour),
		}

		require.Nil(t, sub.EffectiveTotalLimit(), tc.name+" 应归一为不限额")
		require.True(t, sub.IsUnlimited(), tc.name)

		progress := svc.calculateProgress(sub)

		require.Nil(t, progress.TotalLimitUSD, tc.name+"：展示口径的 TotalLimitUSD 应为 nil")
		require.True(t, progress.Unlimited, tc.name)
		require.Nil(t, progress.RemainingUSD, tc.name+"：不限额没有「剩余」概念")
		require.InDelta(t, 0.0, progress.Percentage, 1e-9,
			tc.name+"：没有分母时 Percentage 必须为 0，绝不能显示 100%")
		require.InDelta(t, 3.0, progress.TotalUsageUSD, 1e-9, "用量仍应如实透出")
	}
}

// ── 4. 到期与数值安全 ──────────────────────────────────────────

// 过期订阅的剩余天数必须是 0（负数天数会污染前端倒计时）。
func TestCalculateProgress_ExpiredSubscription(t *testing.T) {
	svc := newTestSubscriptionService()

	sub := &UserSubscription{
		ID:        1,
		ExpiresAt: time.Now().Add(-24 * time.Hour), // 已过期
		Status:    SubscriptionStatusExpired,
	}

	progress := svc.calculateProgress(sub)

	require.Equal(t, 0, progress.ExpiresInDays, "过期订阅的剩余天数应为 0")
	require.Equal(t, sub.ExpiresAt, progress.ExpiresAt)
}

// 已用满 + 到期临近的组合，以及不限额钱包：Percentage 不得为负、不得 NaN。
//
// 这里取代旧「ResetsInSeconds 不为负」用例的防御价值：窗口没了，但「数值安全」
// 这条不变式还在，而且更关键 —— 分母为 nil 时绝不允许除零。
func TestCalculateProgress_PercentageNeverNegativeOrNaN(t *testing.T) {
	svc := newTestSubscriptionService()

	soon := time.Now().Add(time.Hour)

	cases := []struct {
		name        string
		limit       *float64
		usage       float64
		wantPercent float64
	}{
		{"用满且即将到期", ptrFloat64(10), 10, 100},
		{"超额且即将到期", ptrFloat64(10), 12.5, 100},
		{"不限额且即将到期", nil, 999, 0},
		{"额度为 0 且即将到期", ptrFloat64(0), 999, 0},
		{"脏数据：负用量", ptrFloat64(10), -1, 0},
	}

	for _, tc := range cases {
		sub := &UserSubscription{
			ID:            1,
			TotalLimitUSD: tc.limit,
			TotalUsageUSD: tc.usage,
			Status:        SubscriptionStatusActive,
			ExpiresAt:     soon,
		}

		progress := svc.calculateProgress(sub)

		require.False(t, math.IsNaN(progress.Percentage), tc.name+"：Percentage 不得为 NaN")
		require.GreaterOrEqual(t, progress.Percentage, 0.0, tc.name+"：Percentage 不得为负")
		require.LessOrEqual(t, progress.Percentage, 100.0, tc.name+"：Percentage 不得超 100")
		require.InDelta(t, tc.wantPercent, progress.Percentage, 1e-9, tc.name)

		if progress.RemainingUSD != nil {
			require.GreaterOrEqual(t, *progress.RemainingUSD, 0.0, tc.name+"：剩余金额不得为负")
		}
	}
}

// ── 5. 仓储读取路径 ────────────────────────────────────────────

// progressCalcUserSubRepoStub 只覆盖进度计算用到的两个读方法，
// 其余方法继承 userSubRepoNoop（一律 panic）—— 因此任何多余的仓储访问
// （旧的「回查分组」那类查询）都会让测试当场失败。
type progressCalcUserSubRepoStub struct {
	userSubRepoNoop

	sub       *UserSubscription
	subs      []UserSubscription
	listErr   error
	getCalls  int
	listCalls int
}

func (r *progressCalcUserSubRepoStub) GetByID(_ context.Context, id int64) (*UserSubscription, error) {
	r.getCalls++
	if r.sub == nil || r.sub.ID != id {
		return nil, ErrSubscriptionNotFound
	}
	cp := *r.sub
	return &cp, nil
}

// ListActiveByUserID 模拟仓储契约：status=active 且未过期，按 expires_at 升序返回
// （即「先到期先消耗」顺序）。这里显式排序，确保顺序断言测的是服务的透传，
// 而不是测试里切片字面量的书写顺序。
func (r *progressCalcUserSubRepoStub) ListActiveByUserID(_ context.Context, _ int64) ([]UserSubscription, error) {
	r.listCalls++
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := append([]UserSubscription(nil), r.subs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ExpiresAt.Before(out[j].ExpiresAt) })
	return out, nil
}

// GetSubscriptionProgress：取行 → 纯内存计算，不查分组、不查套餐边。
func TestGetSubscriptionProgress_CalculatesFromRepoRow(t *testing.T) {
	repo := &progressCalcUserSubRepoStub{
		sub: &UserSubscription{
			ID:            42,
			UserID:        7,
			Plan:          &SubscriptionPlanInfo{ID: 9, Name: "包月 Pro"},
			TotalLimitUSD: ptrFloat64(10),
			TotalUsageUSD: 3,
			Status:        SubscriptionStatusActive,
			ExpiresAt:     time.Now().Add(10 * 24 * time.Hour),
		},
	}
	// groupRepo 故意为 nil：钱包化后进度与分组无关，
	// 任何残留的分组查询都会在这里直接 nil panic 而不是静默通过。
	svc := &SubscriptionService{userSubRepo: repo}

	progress, err := svc.GetSubscriptionProgress(context.Background(), 42)
	require.NoError(t, err)
	require.NotNil(t, progress)
	require.Equal(t, 1, repo.getCalls, "应且仅应按订阅 ID 取一次行")

	require.Equal(t, int64(42), progress.ID)
	require.Equal(t, "包月 Pro", progress.Name)
	require.Equal(t, repo.sub.ExpiresAt, progress.ExpiresAt)
	require.Equal(t, 10, progress.ExpiresInDays)
	require.NotNil(t, progress.TotalLimitUSD)
	require.InDelta(t, 10.0, *progress.TotalLimitUSD, 1e-9)
	require.InDelta(t, 3.0, progress.TotalUsageUSD, 1e-9)
	require.NotNil(t, progress.RemainingUSD)
	require.InDelta(t, 7.0, *progress.RemainingUSD, 1e-9)
	require.InDelta(t, 30.0, progress.Percentage, 1e-9)
	require.False(t, progress.Unlimited)

	// 找不到订阅时口径与仓储一致：ErrSubscriptionNotFound。
	_, err = svc.GetSubscriptionProgress(context.Background(), 404)
	require.ErrorIs(t, err, ErrSubscriptionNotFound)
}

// GetUserSubscriptionsWithProgress：多份订阅各自独立计算，顺序与
// ListActiveByUserID（先到期先消耗）逐一对齐。
func TestGetUserSubscriptionsWithProgress_PreservesConsumptionOrder(t *testing.T) {
	now := time.Now()
	repo := &progressCalcUserSubRepoStub{
		// 书写顺序刻意打乱，由 stub 按 expires_at 升序排回去。
		subs: []UserSubscription{
			{
				ID: 3, UserID: 7,
				Plan:          &SubscriptionPlanInfo{ID: 30, Name: "年付 Max"},
				TotalLimitUSD: ptrFloat64(100),
				TotalUsageUSD: 100, // 用满
				Status:        SubscriptionStatusActive,
				ExpiresAt:     now.Add(3 * 24 * time.Hour),
			},
			{
				ID: 1, UserID: 7,
				Plan:          &SubscriptionPlanInfo{ID: 9, Name: "包月 Pro"},
				TotalLimitUSD: ptrFloat64(10),
				TotalUsageUSD: 3,
				Status:        SubscriptionStatusActive,
				ExpiresAt:     now.Add(1 * 24 * time.Hour),
			},
			{
				ID: 2, UserID: 7,
				TotalLimitUSD: nil, // 手工发放的不限额钱包，无套餐
				TotalUsageUSD: 5,
				Status:        SubscriptionStatusActive,
				ExpiresAt:     now.Add(2 * 24 * time.Hour),
			},
		},
	}
	svc := &SubscriptionService{userSubRepo: repo}

	list, err := svc.GetUserSubscriptionsWithProgress(context.Background(), 7)
	require.NoError(t, err)
	require.Equal(t, 1, repo.listCalls)
	require.Len(t, list, 3)

	// 顺序：先到期先消耗 → 1、2、3
	require.Equal(t, []int64{1, 2, 3}, []int64{list[0].ID, list[1].ID, list[2].ID},
		"进度列表顺序必须与 ListActiveByUserID 的消耗顺序一致")

	// 每份的 Name / TotalLimitUSD / Percentage 独立计算，互不串味。
	require.Equal(t, "包月 Pro", list[0].Name)
	require.Equal(t, 1, list[0].ExpiresInDays)
	require.NotNil(t, list[0].TotalLimitUSD)
	require.InDelta(t, 10.0, *list[0].TotalLimitUSD, 1e-9)
	require.InDelta(t, 30.0, list[0].Percentage, 1e-9)
	require.False(t, list[0].Unlimited)
	require.NotNil(t, list[0].RemainingUSD)
	require.InDelta(t, 7.0, *list[0].RemainingUSD, 1e-9)

	require.Equal(t, "个人订阅", list[1].Name)
	require.Equal(t, 2, list[1].ExpiresInDays)
	require.Nil(t, list[1].TotalLimitUSD)
	require.True(t, list[1].Unlimited)
	require.Nil(t, list[1].RemainingUSD)
	require.InDelta(t, 0.0, list[1].Percentage, 1e-9, "不限额钱包不得显示 100%")
	require.InDelta(t, 5.0, list[1].TotalUsageUSD, 1e-9)

	require.Equal(t, "年付 Max", list[2].Name)
	require.Equal(t, 3, list[2].ExpiresInDays)
	require.NotNil(t, list[2].TotalLimitUSD)
	require.InDelta(t, 100.0, *list[2].TotalLimitUSD, 1e-9)
	require.InDelta(t, 100.0, list[2].Percentage, 1e-9)
	require.False(t, list[2].Unlimited)
	require.NotNil(t, list[2].RemainingUSD)
	require.InDelta(t, 0.0, *list[2].RemainingUSD, 1e-9)
}

// GetUserSubscriptionsWithProgress：仓储错误原样透出（不吞成空列表），
// 无生效订阅时返回空列表而非报错。
func TestGetUserSubscriptionsWithProgress_RepoErrorAndEmptyResult(t *testing.T) {
	dbErr := errors.New("db down")

	failing := &progressCalcUserSubRepoStub{listErr: dbErr}
	svc := &SubscriptionService{userSubRepo: failing}
	list, err := svc.GetUserSubscriptionsWithProgress(context.Background(), 7)
	require.ErrorIs(t, err, dbErr, "仓储错误必须原样返回，不能降级成空列表")
	require.Nil(t, list)

	empty := &progressCalcUserSubRepoStub{}
	svc = &SubscriptionService{userSubRepo: empty}
	list, err = svc.GetUserSubscriptionsWithProgress(context.Background(), 7)
	require.NoError(t, err)
	require.NotNil(t, list, "无订阅应返回空切片而非 nil")
	require.Empty(t, list)
}
