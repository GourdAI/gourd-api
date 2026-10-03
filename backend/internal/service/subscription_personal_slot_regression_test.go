//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// personalSlotRepoStub 复刻真实仓储的「槽位严格归属」语义（分组不回退个人订阅），
// 并记录回源次数，用于验证 L1 槽位是否被错误短路。
type personalSlotRepoStub struct {
	userSubRepoNoop

	subs         []UserSubscription
	activeCalls  int
	limitWrites  []limitWriteCall
	listByUser   []UserSubscription
	existsActive map[int64]bool
}

type limitWriteCall struct {
	id                     int64
	daily, weekly, monthly *float64
}

func (r *personalSlotRepoStub) match(userID, groupID int64) *UserSubscription {
	now := time.Now()
	for i := range r.subs {
		sub := r.subs[i]
		if sub.UserID == userID && sub.GroupID == groupID &&
			sub.Status == SubscriptionStatusActive && sub.ExpiresAt.After(now) {
			return &sub
		}
	}
	return nil
}

func (r *personalSlotRepoStub) GetActiveByUserIDAndGroupID(_ context.Context, userID, groupID int64) (*UserSubscription, error) {
	r.activeCalls++
	if sub := r.match(userID, groupID); sub != nil {
		cp := *sub
		return &cp, nil
	}
	// 与仓储一致：严格归属本槽位，绝不回退到另一个槽位。
	return nil, ErrSubscriptionNotFound
}

func (r *personalSlotRepoStub) UpdateAssignedLimits(_ context.Context, id int64, daily, weekly, monthly *float64) error {
	r.limitWrites = append(r.limitWrites, limitWriteCall{id: id, daily: daily, weekly: weekly, monthly: monthly})
	for i := range r.subs {
		if r.subs[i].ID != id {
			continue
		}
		if daily != nil {
			v := *daily
			r.subs[i].DailyLimitUSD = &v
		}
		if weekly != nil {
			v := *weekly
			r.subs[i].WeeklyLimitUSD = &v
		}
		if monthly != nil {
			v := *monthly
			r.subs[i].MonthlyLimitUSD = &v
		}
	}
	return nil
}

func (r *personalSlotRepoStub) GetByID(_ context.Context, id int64) (*UserSubscription, error) {
	for i := range r.subs {
		if r.subs[i].ID == id {
			cp := r.subs[i]
			return &cp, nil
		}
	}
	return nil, ErrSubscriptionNotFound
}

func (r *personalSlotRepoStub) ListActiveByUserID(context.Context, int64) ([]UserSubscription, error) {
	return r.listByUser, nil
}

func (r *personalSlotRepoStub) ExistsActiveByUserIDAndGroupID(_ context.Context, _, groupID int64) (bool, error) {
	return r.existsActive[groupID], nil
}

// newPersonalSlotSvc 构造带 L1 缓存的订阅服务。
// L1Size 必须给足：ristretto 在极小容量（如 16）下会丢弃全部 Set，
// 那会让「是否命中缓存」的断言退化成恒真（假通过），测不到槽位语义。
func newPersonalSlotSvc(t *testing.T, repo *personalSlotRepoStub) *SubscriptionService {
	t.Helper()
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, &config.Config{
		SubscriptionCache: config.SubscriptionCacheConfig{L1Size: 10000, L1TTLSeconds: 60},
	})
	t.Cleanup(svc.Stop)
	require.NotNil(t, svc.subCacheL1)
	return svc
}

func activeSub(id, userID, groupID int64) UserSubscription {
	return UserSubscription{
		ID:        id,
		UserID:    userID,
		GroupID:   groupID,
		Status:    SubscriptionStatusActive,
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}
}

// 槽位隔离：分组槽位只返回该分组的专属订阅。
// 个人订阅是额度钱包，不得由分组槽位返回（否则等于把钱包当通行证）；
// 它由调用方显式探测 (user,0)，同时刚分配的分组专属订阅不能被个人订阅遮蔽。
func TestGetActiveSubscription_GroupSlotMissDoesNotShadowNewGroupSubscription(t *testing.T) {
	repo := &personalSlotRepoStub{subs: []UserSubscription{
		activeSub(1, 10, 0), // 个人订阅
	}}
	svc := newPersonalSlotSvc(t, repo)

	// 先加载个人订阅，填满 (user,0) 规范槽位。
	personal, err := svc.GetActiveSubscription(context.Background(), 10, 0)
	require.NoError(t, err)
	require.Equal(t, int64(0), personal.GroupID)
	svc.subCacheL1.Wait()
	require.Equal(t, 1, repo.activeCalls)

	// 分组 20 无专属订阅：必须报 NotFound，而不是被个人订阅遮蔽。
	_, err = svc.GetActiveSubscription(context.Background(), 10, 20)
	require.ErrorIs(t, err, ErrSubscriptionNotFound, "分组槽位不得返回个人订阅")
	svc.subCacheL1.Wait()
	require.Equal(t, 2, repo.activeCalls, "分组槽位 miss 必须回源 DB")

	// 分配分组专属订阅并失效分组槽位（模拟 AssignSubscription 的失效路径）。
	repo.subs = append(repo.subs, activeSub(2, 10, 20))
	svc.InvalidateSubCache(10, 20)

	sub, err := svc.GetActiveSubscription(context.Background(), 10, 20)
	require.NoError(t, err)
	require.Equal(t, int64(20), sub.GroupID, "必须命中分组专属订阅")
}

// 两个槽位各自独立缓存：分组无专属订阅的负哨兵不影响个人槽位可取到额度。
func TestGetActiveSubscription_SlotsAreIndependent(t *testing.T) {
	repo := &personalSlotRepoStub{subs: []UserSubscription{activeSub(1, 10, 0)}}
	svc := newPersonalSlotSvc(t, repo)

	// 分组请求：无专属订阅 → NotFound，并落分组槽位负哨兵。
	_, err := svc.GetActiveSubscription(context.Background(), 10, 20)
	require.ErrorIs(t, err, ErrSubscriptionNotFound)
	svc.subCacheL1.Wait()

	// 个人槽位：仍能拿到个人订阅（中间件就是靠这一步给普通分组计费）。
	personal, err := svc.GetActiveSubscription(context.Background(), 10, 0)
	require.NoError(t, err)
	require.Equal(t, int64(0), personal.GroupID)
	// L1 的 Set 是异步的，不等就会多一次回源（不是逻辑错误，但会让下面的「不再回源」断言假失败）。
	svc.subCacheL1.Wait()
	before := repo.activeCalls
	// 两侧二次请求均命中缓存，不再回源。
	_, err = svc.GetActiveSubscription(context.Background(), 10, 20)
	require.ErrorIs(t, err, ErrSubscriptionNotFound)
	_, err = svc.GetActiveSubscription(context.Background(), 10, 0)
	require.NoError(t, err)
	require.Equal(t, before, repo.activeCalls, "两槽位均应命中缓存，不再回源")
}

// 无任何订阅时：两个槽位都回源 NotFound 并各自落负哨兵（避免无订阅用户每请求多一次 DB 往返）。
func TestGetActiveSubscription_NoSubscriptionAtAll(t *testing.T) {
	repo := &personalSlotRepoStub{}
	svc := newPersonalSlotSvc(t, repo)

	_, err := svc.GetActiveSubscription(context.Background(), 10, 20)
	require.ErrorIs(t, err, ErrSubscriptionNotFound)
	svc.subCacheL1.Wait()
	_, ok := svc.subCacheL1.Get(subCacheKey(10, 20))
	require.True(t, ok, "分组槽位应被负缓存（热路径避免 DB 往返）")

	before := repo.activeCalls
	_, err = svc.GetActiveSubscription(context.Background(), 10, 20)
	require.ErrorIs(t, err, ErrSubscriptionNotFound)
	require.Equal(t, before, repo.activeCalls, "负哨兵命中不得再回源")
}

// 新建个人订阅不得被分组槽位的负哨兵影响：中间件探测的是 (user,0) 槽位，
// 分配时按 sub.GroupID=0 失效，因此新额度立即生效（不依赖分组槽位）。
func TestGetActiveSubscription_GroupNegativeSlotDoesNotBlockNewPersonalSubscription(t *testing.T) {
	repo := &personalSlotRepoStub{}
	svc := newPersonalSlotSvc(t, repo)

	// 先确认无订阅 → 分组槽位与个人槽位均落负哨兵。
	_, err := svc.GetActiveSubscription(context.Background(), 10, 20)
	require.ErrorIs(t, err, ErrSubscriptionNotFound)
	_, err = svc.GetActiveSubscription(context.Background(), 10, 0)
	require.ErrorIs(t, err, ErrSubscriptionNotFound)
	svc.subCacheL1.Wait()

	// 管理员新建个人订阅（AssignSubscription 路径：失效 (user, sub.GroupID)=(user,0)）。
	repo.subs = append(repo.subs, activeSub(1, 10, 0))
	svc.InvalidateSubCacheSync(10, 0)

	personal, err := svc.GetActiveSubscription(context.Background(), 10, 0)
	require.NoError(t, err, "新建个人订阅必须立即生效，不被旧负哨兵挡住")
	require.Equal(t, int64(0), personal.GroupID)
}

// P0-5：额度必须按字段增量写入。只填每日额度却把周/月静默清空 = 提权（闸门消失）。
func TestApplyAssignedLimits_WritesOnlyProvidedFields(t *testing.T) {
	repo := &personalSlotRepoStub{subs: []UserSubscription{func() UserSubscription {
		sub := activeSub(1, 10, 0)
		weekly := 50.0
		monthly := 200.0
		sub.WeeklyLimitUSD = &weekly
		sub.MonthlyLimitUSD = &monthly
		return sub
	}()}}
	svc := newPersonalSlotSvc(t, repo)

	daily := 5.0
	require.NoError(t, svc.applyAssignedLimits(context.Background(), &repo.subs[0], &AssignSubscriptionInput{
		DailyLimitUSD: &daily,
	}))

	require.Len(t, repo.limitWrites, 1)
	got := repo.limitWrites[0]
	require.NotNil(t, got.daily)
	require.Nil(t, got.weekly, "未提供的周额度必须传 nil（保持原值），不得整组覆盖")
	require.Nil(t, got.monthly)
	require.NotNil(t, repo.subs[0].WeeklyLimitUSD)
	require.Equal(t, 50.0, *repo.subs[0].WeeklyLimitUSD)
	require.Equal(t, 200.0, *repo.subs[0].MonthlyLimitUSD)
}

// 三者全 nil = 本次不改额度（兑换码/支付续期只续期），不得产生任何写。
func TestApplyAssignedLimits_AllNilIsNoop(t *testing.T) {
	repo := &personalSlotRepoStub{subs: []UserSubscription{activeSub(1, 10, 0)}}
	svc := newPersonalSlotSvc(t, repo)

	require.NoError(t, svc.applyAssignedLimits(context.Background(), &repo.subs[0], &AssignSubscriptionInput{}))
	require.Empty(t, repo.limitWrites)
}

// P0-4：改额度只能走「只写额度列」的通道。
// 若退回整行 Update，会与并发 IncrementUsage 竞争并把用量写回旧值（额度虚低 = 变相提权）。
func TestApplyAssignedLimits_DoesNotTouchUsageColumns(t *testing.T) {
	repo := &personalSlotRepoStub{subs: []UserSubscription{activeSub(1, 10, 0)}}
	svc := newPersonalSlotSvc(t, repo)

	daily := 7.5
	require.NoError(t, svc.applyAssignedLimits(context.Background(), &repo.subs[0], &AssignSubscriptionInput{
		DailyLimitUSD: &daily,
	}))
	require.Equal(t, []limitWriteCall{{id: 1, daily: &daily}}, repo.limitWrites)
}

// P1-3：个人订阅（GroupID=0，无分组边）必须出现在进度列表里，
// 否则用户看不到自己的额度与用量。
func TestGetUserSubscriptionsWithProgress_IncludesPersonalSubscription(t *testing.T) {
	daily := 10.0
	personal := activeSub(1, 10, 0)
	personal.DailyLimitUSD = &daily
	start := time.Now().Add(-time.Hour)
	personal.DailyWindowStart = &start

	orphanGroup := activeSub(2, 10, 99) // 分组订阅但分组边缺失：保守跳过（既有行为）

	repo := &personalSlotRepoStub{listByUser: []UserSubscription{personal, orphanGroup}}
	svc := newPersonalSlotSvc(t, repo)

	progresses, err := svc.GetUserSubscriptionsWithProgress(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, progresses, 1)
	require.Equal(t, int64(1), progresses[0].ID)
	require.NotNil(t, progresses[0].Daily)
	require.Equal(t, 10.0, progresses[0].Daily.LimitUSD)
}

