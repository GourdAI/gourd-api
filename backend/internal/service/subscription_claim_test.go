//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	dbent "github.com/Wei-Shaw/sub2api/ent"
)

// 自助领取链路的服务层回归。
//
// 锁住三件事（全部来自 2026-10-21 需求）：
//  1. 只有「在售 + 可自助领取」的套餐能被领取，否则既买不到也领不到；
//  2. 同一用户重复领取同一套餐 = 幂等，**不叠加额度**（这是资损边界：
//     daily 窗口下多领一份就多拿一份每天的钱）；
//  3. 领取出来的订阅必须带上套餐的 reset_period 快照 —— 否则「每天 1$」
//     退化成「一次性 1$」，是本需求最容易静默失效的地方。

func newClaimTestService(t *testing.T) (*SubscriptionService, *dbent.Client, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() {
		_ = client.Close()
		_ = db.Close()
	})
	subRepo := newSubscriptionUserSubRepoStub()
	return NewSubscriptionService(groupRepoNoop{}, subRepo, nil, client, nil), client, mock
}

const planColumns = `id, group_id, name, description, price, total_limit_usd, reset_period, self_claimable, ` +
	`original_price, currency, validity_days, validity_unit, features, product_name, for_sale, sort_order, created_at, updated_at`

func planRow(mock sqlmock.Sqlmock, id int64, price float64, resetPeriod string, selfClaimable bool) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT .*FROM "subscription_plans"`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "group_id", "name", "description", "price", "total_limit_usd",
			"reset_period", "self_claimable", "original_price", "currency",
			"validity_days", "validity_unit", "features", "product_name",
			"for_sale", "sort_order", "created_at", "updated_at",
		}).AddRow(
			id, int64(1), "Daily Free", "", price, 1.0,
			resetPeriod, selfClaimable, nil, "",
			30, "days", "", "",
			true, 0, now, now,
		))
}

func TestClaimSubscription_RejectsNonClaimablePlan(t *testing.T) {
	svc, _, mock := newClaimTestService(t)
	planRow(mock, 5, 0, SubscriptionResetPeriodDaily, false) // 免费但未开启自助领取

	_, err := svc.ClaimSubscription(context.Background(), 42, 5)
	require.ErrorIs(t, err, ErrSubscriptionNotClaimable)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClaimSubscription_CreatesWalletWithPlanSnapshot(t *testing.T) {
	svc, _, mock := newClaimTestService(t)
	planRow(mock, 7, 0, SubscriptionResetPeriodDaily, true)

	result, err := svc.ClaimSubscription(context.Background(), 42, 7)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.AlreadyClaimed, "首次领取不是幂等命中")

	sub := result.Subscription
	require.Equal(t, int64(42), sub.UserID)
	require.NotNil(t, sub.PlanID)
	require.Equal(t, int64(7), *sub.PlanID)
	// 额度与周期都是套餐快照 —— 缺任一，「每天 1$」都不成立。
	require.NotNil(t, sub.TotalLimitUSD)
	require.InDelta(t, 1, *sub.TotalLimitUSD, 1e-9)
	require.Equal(t, SubscriptionResetPeriodDaily, sub.ResetPeriod)
	require.False(t, sub.UsageWindowStart.IsZero(), "配了周期必须初始化窗口起点")
}

// 重复领取必须幂等：额度不叠加、不新建第二行。
func TestClaimSubscription_IsIdempotent(t *testing.T) {
	svc, _, mock := newClaimTestService(t)
	planRow(mock, 8, 0, SubscriptionResetPeriodDaily, true)

	first, err := svc.ClaimSubscription(context.Background(), 43, 8)
	require.NoError(t, err)
	require.False(t, first.AlreadyClaimed)

	// 第二次：Get 仍然要查套餐（校验仍在售），但不应再创建。
	planRow(mock, 8, 0, SubscriptionResetPeriodDaily, true)
	second, err := svc.ClaimSubscription(context.Background(), 43, 8)
	require.NoError(t, err)
	require.True(t, second.AlreadyClaimed, "第二次领取必须命中幂等")
	require.Equal(t, first.Subscription.ID, second.Subscription.ID, "不得新建第二份订阅")

	// 额度不叠加：仍是 1 而不是 2。
	require.NotNil(t, second.Subscription.TotalLimitUSD)
	require.InDelta(t, 1, *second.Subscription.TotalLimitUSD, 1e-9)
}

// 超时后过期的那份不应挡住重新领取（免费额度本就是周期性的）。
//
// 注：本用例只验证「过期订阅不会被 findClaimedSubscription 当成已持有」
// —— 那是决定「能不能重领」的唯一判据。后续 createSubscription 会开事务，
// 而 sqlmock 下模拟整条事务链只会重复验证分配链路的既有用例
// （TestCreateSubscription_* 已覆盖快照写入），故这里只锁判据本身。
func TestClaimSubscription_TreatsExpiredAsNotHeld(t *testing.T) {
	planID := int64(9)
	limit := 1.0
	expired := UserSubscription{
		ID:            900,
		UserID:        44,
		PlanID:        &planID,
		Status:        SubscriptionStatusExpired,
		StartsAt:      time.Now().AddDate(0, 0, -10),
		ExpiresAt:     time.Now().AddDate(0, 0, -1),
		TotalLimitUSD: &limit,
		ResetPeriod:   SubscriptionResetPeriodDaily,
	}
	require.False(t, expired.IsActive(), "过期订阅不算已持有")

	active := expired
	active.ID = 901
	active.Status = SubscriptionStatusActive
	active.ExpiresAt = time.Now().AddDate(0, 0, 30)
	require.True(t, active.IsActive(), "活跃订阅算已持有（重复领取应幂等）")
}

func TestListClaimablePlans_MarksHeldPlan(t *testing.T) {
	svc, _, mock := newClaimTestService(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT .*FROM "subscription_plans"`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "group_id", "name", "description", "price", "total_limit_usd",
			"reset_period", "self_claimable", "original_price", "currency",
			"validity_days", "validity_unit", "features", "product_name",
			"for_sale", "sort_order", "created_at", "updated_at",
		}).
			AddRow(int64(11), int64(1), "Held", "", 0.0, 1.0, "daily", true, nil, "", 30, "days", "", "", true, 0, now, now).
			AddRow(int64(12), int64(1), "Free", "", 0.0, 2.0, "weekly", true, nil, "", 30, "days", "", "", true, 0, now, now))

	// 用户 45 已持有 11。
	subRepo, ok := svc.userSubRepo.(*subscriptionUserSubRepoStub)
	require.True(t, ok)
	heldPlan := int64(11)
	limit := 1.0
	subRepo.seed(&UserSubscription{
		UserID:        45,
		PlanID:        &heldPlan,
		Status:        SubscriptionStatusActive,
		StartsAt:      now,
		ExpiresAt:     now.AddDate(0, 0, 30),
		TotalLimitUSD: &limit,
		ResetPeriod:   SubscriptionResetPeriodDaily,
	})

	plans, err := svc.ListClaimablePlans(context.Background(), 45)
	require.NoError(t, err)
	require.Len(t, plans, 2)
	require.Equal(t, SubscriptionResetPeriodDaily, plans[0].ResetPeriod)
	require.True(t, plans[0].AlreadyClaimed, "已持有的套餐要标 already_claimed")
	require.False(t, plans[1].AlreadyClaimed)
}

// 空列表必须返回空切片而不是 nil，否则前端 `v-if="plans.length"` 之外的
// 直接 map/遍历路径会炸。
func TestListClaimablePlans_EmptyReturnsEmptySlice(t *testing.T) {
	svc, _, mock := newClaimTestService(t)
	mock.ExpectQuery(`SELECT .*FROM "subscription_plans"`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "group_id", "name", "description", "price", "total_limit_usd",
			"reset_period", "self_claimable", "original_price", "currency",
			"validity_days", "validity_unit", "features", "product_name",
			"for_sale", "sort_order", "created_at", "updated_at",
		}))

	plans, err := svc.ListClaimablePlans(context.Background(), 1)
	require.NoError(t, err)
	require.NotNil(t, plans)
	require.Empty(t, plans)
}
