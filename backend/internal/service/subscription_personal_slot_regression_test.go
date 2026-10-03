//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// personalSlotRepoStub 复刻真实仓储的「分组订阅优先，缺失回退个人订阅」语义，
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
	// 与仓储一致：分组槽位无专属订阅时回退个人订阅（group_id=0）。
	if groupID != 0 {
		if sub := r.match(userID, 0); sub != nil {
			cp := *sub
			return &cp, nil
		}
	}
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

// P0-2：分组槽位 miss 时不得直读个人订阅槽位跳过 DB。
// 否则用户新分配的分组专属订阅会被个人订阅遮蔽 —— 扣错订阅、额度与归属全错。
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

	// 分配分组专属订阅并失效分组槽位（模拟 AssignSubscription 的失效路径）。
	repo.subs = append(repo.subs, activeSub(2, 10, 20))
	svc.InvalidateSubCache(10, 20)

	sub, err := svc.GetActiveSubscription(context.Background(), 10, 20)
	require.NoError(t, err)
	require.Equal(t, int64(20), sub.GroupID, "必须命中分组专属订阅，不能被个人订阅遮蔽")
	require.Equal(t, 2, repo.activeCalls, "分组槽位 miss 必须回源 DB")
}

// P0-2 补充：回退命中个人订阅后，分组槽位打负哨兵；后续同分组请求走负哨兵 → 个人槽位，
// 既不多打 DB，也不会把「无专属订阅」误当成「有专属订阅」。
func TestGetActiveSubscription_FallbackToPersonalIsCachedAsNegativeSlot(t *testing.T) {
	repo := &personalSlotRepoStub{subs: []UserSubscription{activeSub(1, 10, 0)}}
	svc := newPersonalSlotSvc(t, repo)

	first, err := svc.GetActiveSubscription(context.Background(), 10, 20)
	require.NoError(t, err)
	require.Equal(t, int64(0), first.GroupID, "回退返回的必须是个人订阅本体（保留个人额度口径）")
	svc.subCacheL1.Wait()
	require.Equal(t, 1, repo.activeCalls)

	second, err := svc.GetActiveSubscription(context.Background(), 10, 20)
	require.NoError(t, err)
	require.Equal(t, int64(0), second.GroupID)
	require.Equal(t, 1, repo.activeCalls, "分组槽位负哨兵 + 个人槽位应命中缓存，不再回源")
}

// 无任何订阅时：分组槽位回源后按未命中处理，且不做分组槽位负缓存
// （避免新建个人订阅后最长 30s 才生效）。
func TestGetActiveSubscription_NoSubscriptionAtAll(t *testing.T) {
	repo := &personalSlotRepoStub{}
	svc := newPersonalSlotSvc(t, repo)

	_, err := svc.GetActiveSubscription(context.Background(), 10, 20)
	require.ErrorIs(t, err, ErrSubscriptionNotFound)
	svc.subCacheL1.Wait()
	_, ok := svc.subCacheL1.Get(subCacheKey(10, 20))
	require.False(t, ok, "分组槽位不得被负缓存")
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

// 分组槽位负哨兵仍在 TTL 内存活、但个人槽位刚被失效时（改额度/新建个人订阅），
// 必须回源而不是直接报 NotFound；否则新个人订阅最长 30s 不生效。
func TestGetActiveSubscription_GroupNegativeSlotWithEvictedPersonalSlotRefetches(t *testing.T) {
	repo := &personalSlotRepoStub{subs: []UserSubscription{activeSub(1, 10, 0)}}
	svc := newPersonalSlotSvc(t, repo)

	// 第一次：分组无专属订阅 → 回退个人订阅，分组槽位落负哨兵。
	first, err := svc.GetActiveSubscription(context.Background(), 10, 20)
	require.NoError(t, err)
	require.Equal(t, int64(0), first.GroupID)
	svc.subCacheL1.Wait()
	require.Equal(t, 1, repo.activeCalls)

	// 失效个人槽位（模拟 applyAssignedLimits / 新建个人订阅），分组负哨兵不动。
	svc.InvalidateSubCacheSync(10, 0)

	second, err := svc.GetActiveSubscription(context.Background(), 10, 20)
	require.NoError(t, err, "个人槽位已失效时必须回源，不得因分组负哨兵直接报无订阅")
	require.Equal(t, int64(0), second.GroupID)
	require.Equal(t, 2, repo.activeCalls, "应回源一次拿到最新个人订阅")
}
