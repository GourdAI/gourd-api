package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// lockingRenewalRepo 锁定「续期必须走行锁读」这条不变式。
//
// 钱包化后分配寻址改由 findAssignmentTarget → ListByUserID 完成（旧的
// GetByUserIDAndGroupID / ExistsByUserIDAndGroupID 槽位读已随分组绑定一起删除）。
// 这次寻址读是不加锁的普通读，可能拿到旧快照，所以本 stub 把 stale 与 current
// 故意分成两份数据：只有断言结果取自 GetByIDForUpdate（current）的字段，才能证明
// 服务没有拿不加锁的寻址读去推算新的到期时刻 / 写回用量。
type lockingRenewalRepo struct {
	userSubRepoNoop
	mu        sync.Mutex
	stale     UserSubscription
	current   UserSubscription
	lockReads int
}

// ListByUserID 模拟不加锁的寻址读：始终返回 stale 快照。
func (r *lockingRenewalRepo) ListByUserID(_ context.Context, _ int64) ([]UserSubscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return []UserSubscription{r.stale}, nil
}

func (r *lockingRenewalRepo) GetByID(_ context.Context, _ int64) (*UserSubscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy := r.current
	return &copy, nil
}

func (r *lockingRenewalRepo) GetByIDForUpdate(_ context.Context, _ int64) (*UserSubscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lockReads++
	copy := r.current
	return &copy, nil
}

func (r *lockingRenewalRepo) ExtendExpiry(_ context.Context, _ int64, expiresAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current.ExpiresAt = expiresAt
	return nil
}

func (r *lockingRenewalRepo) UpdateStatus(_ context.Context, _ int64, status string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current.Status = status
	return nil
}

func (r *lockingRenewalRepo) UpdateNotes(_ context.Context, _ int64, notes string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current.Notes = notes
	return nil
}

func (r *lockingRenewalRepo) Update(_ context.Context, sub *UserSubscription) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = *sub
	return nil
}

func TestAssignOrExtendSubscriptionUsesLockedCurrentRow(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	lockedExpiry := now.AddDate(0, 0, 20)
	planID := int64(13)
	repo := &lockingRenewalRepo{
		stale: UserSubscription{ID: 7, UserID: 11, PlanID: &planID, ExpiresAt: now.Add(-time.Hour), Status: SubscriptionStatusExpired, Notes: "stale"},
		current: UserSubscription{
			ID: 7, UserID: 11, PlanID: &planID, StartsAt: now.AddDate(0, 0, -10), ExpiresAt: lockedExpiry,
			Status: SubscriptionStatusSuspended, Notes: "current", TotalLimitUSD: walletF64(20), TotalUsageUSD: 4,
		},
	}
	// groupRepoNoop 会在任何分组访问上 panic：订阅不绑定分组、也不授予分组准入，
	// 续期链路必须一次分组都不碰（契约 2 / 契约 6）。
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
	svc.now = func() time.Time { return now }

	sub, extended, err := svc.AssignOrExtendSubscription(context.Background(), &AssignSubscriptionInput{
		UserID: 11, PlanID: &planID, ValidityDays: 5, Notes: "renewed",
	})

	require.NoError(t, err)
	require.True(t, extended)
	require.Equal(t, 1, repo.lockReads)
	require.Equal(t, lockedExpiry.AddDate(0, 0, 5), sub.ExpiresAt)
	require.Equal(t, SubscriptionStatusActive, sub.Status)
	require.Equal(t, "current\nrenewed", sub.Notes)
	// 旧断言 *sub.DailyWindowStart == windowStart 已随「日/周/月滚动窗口」概念一并删除：
	// 唯一事实源 UserSubscription 不再有任何 WindowStart 字段，总额池不滚动、无窗口可锚定。
	// 原「续期不动已花掉的钱」这一价值由下面两条替代并加强：
	// 续费只延长有效期，用量原样保持、额度不被整行写回吞掉（产品定案 4）。
	require.Equal(t, float64(4), sub.TotalUsageUSD)
	require.NotNil(t, sub.TotalLimitUSD)
	require.Equal(t, float64(20), *sub.TotalLimitUSD)
}

// TestAssignOrExtendSubscriptionReviveKeepsTotalUsage 锁住产品定案 4 的复活分支：
// 过期钱包被复活时走整行 Update，写回的 total_usage_usd 必须取自 GetByIDForUpdate
// 的行锁快照（6.5），既不能清零（清零 = 凭空再发一笔钱），也不能退回不加锁寻址读
// 里的旧快照（1.0 = 吞掉并发 IncrementUsage，用量虚低 = 变相提权）。
func TestAssignOrExtendSubscriptionReviveKeepsTotalUsage(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	expiredExpiry := now.Add(-2 * time.Hour)
	planID := int64(53)
	repo := &lockingRenewalRepo{
		stale: UserSubscription{ID: 57, UserID: 61, PlanID: &planID, ExpiresAt: expiredExpiry, Status: SubscriptionStatusExpired, Notes: "old", TotalUsageUSD: 1},
		current: UserSubscription{
			ID: 57, UserID: 61, PlanID: &planID, StartsAt: now.AddDate(0, 0, -30), ExpiresAt: expiredExpiry,
			Status: SubscriptionStatusExpired, Notes: "old", TotalLimitUSD: walletF64(20), TotalUsageUSD: 6.5,
		},
	}
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
	svc.now = func() time.Time { return now }

	sub, extended, err := svc.AssignOrExtendSubscription(context.Background(), &AssignSubscriptionInput{
		UserID: 61, PlanID: &planID, ValidityDays: 5, Notes: "renewed",
	})

	require.NoError(t, err)
	require.True(t, extended)
	require.Equal(t, 1, repo.lockReads)
	require.Equal(t, now, sub.StartsAt, "复活后有效期从当前时刻重新起算")
	require.Equal(t, now.AddDate(0, 0, 5), sub.ExpiresAt)
	require.Equal(t, SubscriptionStatusActive, sub.Status)
	require.Equal(t, "old\nrenewed", sub.Notes)
	require.Equal(t, float64(6.5), sub.TotalUsageUSD, "续费不清零已用额度")
	require.NotNil(t, sub.TotalLimitUSD)
	require.Equal(t, float64(20), *sub.TotalLimitUSD, "复活路径不得改写额度列")
}

func TestAssignOrExtendSubscriptionSerializedRenewalsAccumulateDays(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	initialExpiry := now.AddDate(0, 0, 10)
	planID := int64(23)
	stale := UserSubscription{ID: 17, UserID: 21, PlanID: &planID, StartsAt: now, ExpiresAt: initialExpiry, Status: SubscriptionStatusActive}
	repo := &lockingRenewalRepo{stale: stale, current: stale}
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
	svc.now = func() time.Time { return now }
	input := &AssignSubscriptionInput{UserID: 21, PlanID: &planID, ValidityDays: 7}

	_, _, err := svc.AssignOrExtendSubscription(context.Background(), input)
	require.NoError(t, err)
	second, _, err := svc.AssignOrExtendSubscription(context.Background(), input)
	require.NoError(t, err)

	require.Equal(t, 2, repo.lockReads)
	require.Equal(t, initialExpiry.AddDate(0, 0, 14), second.ExpiresAt)
}

func TestExtendSubscriptionUsesLockedCurrentRow(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	initialExpiry := now.AddDate(0, 0, 10)
	repo := &lockingRenewalRepo{current: UserSubscription{
		ID: 37, UserID: 41, ExpiresAt: initialExpiry, Status: SubscriptionStatusActive,
	}}
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
	svc.now = func() time.Time { return now }

	updated, err := svc.ExtendSubscription(context.Background(), 7, 5)

	require.NoError(t, err)
	require.Equal(t, 1, repo.lockReads)
	require.Equal(t, initialExpiry.AddDate(0, 0, 5), updated.ExpiresAt)
}

func TestAssignSubscriptionDoesNotReactivateRowSuspendedAfterStaleRead(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	planID := int64(33)
	current := UserSubscription{
		ID: 27, UserID: 31, PlanID: &planID, StartsAt: now.AddDate(0, 0, -10), ExpiresAt: now.Add(-time.Hour),
		Status: SubscriptionStatusSuspended, Notes: "suspended", TotalUsageUSD: 4,
	}
	repo := &lockingRenewalRepo{
		stale:   UserSubscription{ID: 27, UserID: 31, PlanID: &planID, ExpiresAt: now.Add(-time.Hour), Status: SubscriptionStatusExpired},
		current: current,
	}
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
	svc.now = func() time.Time { return now }

	sub, reused, err := svc.assignSubscriptionWithReuse(context.Background(), &AssignSubscriptionInput{
		UserID: 31, PlanID: &planID, ValidityDays: 5, Notes: "renewed",
	})

	require.NoError(t, err)
	require.True(t, reused)
	require.Equal(t, 1, repo.lockReads)
	require.Equal(t, current, repo.current)
	require.Equal(t, SubscriptionStatusSuspended, sub.Status)
	require.Equal(t, current.ExpiresAt, sub.ExpiresAt)
	require.Equal(t, current.Notes, sub.Notes)
}
