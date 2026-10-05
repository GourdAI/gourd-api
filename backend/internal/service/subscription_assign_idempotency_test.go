package service

import (
	"context"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/dgraph-io/ristretto"
	"github.com/stretchr/testify/require"
)

func TestWithSubscriptionUpdateTx_ReusesExistingTransaction(t *testing.T) {
	existingTx := &dbent.Tx{}
	ctx := dbent.NewTxContext(context.Background(), existingTx)
	svc := &SubscriptionService{entClient: &dbent.Client{}}

	called := false
	err := svc.withSubscriptionUpdateTx(ctx, func(txCtx context.Context) error {
		called = true
		require.Same(t, existingTx, dbent.TxFromContext(txCtx))
		return nil
	})

	require.NoError(t, err)
	require.True(t, called)
}

func TestMaybeInvalidateAssignmentCaches_DefersForOuterTransactionOwner(t *testing.T) {
	cache, err := ristretto.NewCache(&ristretto.Config{NumCounters: 1_000, MaxCost: 100, BufferItems: 64})
	require.NoError(t, err)
	t.Cleanup(cache.Close)

	svc := &SubscriptionService{subCacheL1: cache}
	// 钱包化后缓存只有一个坐标 user："sub:<uid>"，旧 ":<gid>" 段已随槽位模型删除。
	key := subCacheKey(7)
	require.True(t, cache.Set(key, &UserSubscription{ID: 42}, 1))
	cache.Wait()

	svc.maybeInvalidateAssignmentCaches(7, true)
	_, cachedBeforeCommit := cache.Get(key)
	require.True(t, cachedBeforeCommit, "outer transaction must retain caches until its owner commits")

	svc.maybeInvalidateAssignmentCaches(7, false)
	cache.Wait()
	_, cachedAfterCommit := cache.Get(key)
	require.False(t, cachedAfterCommit, "post-commit invalidation must remove the cached subscription")
}

type groupRepoNoop struct{}

func (groupRepoNoop) Create(context.Context, *Group) error { panic("unexpected Create call") }
func (groupRepoNoop) GetByID(context.Context, int64) (*Group, error) {
	panic("unexpected GetByID call")
}
func (groupRepoNoop) GetByIDLite(context.Context, int64) (*Group, error) {
	panic("unexpected GetByIDLite call")
}
func (groupRepoNoop) Update(context.Context, *Group) error { panic("unexpected Update call") }
func (groupRepoNoop) Delete(context.Context, int64) error  { panic("unexpected Delete call") }
func (groupRepoNoop) DeleteCascade(context.Context, int64) ([]int64, error) {
	panic("unexpected DeleteCascade call")
}
func (groupRepoNoop) List(context.Context, pagination.PaginationParams) ([]Group, *pagination.PaginationResult, error) {
	panic("unexpected List call")
}
func (groupRepoNoop) ListWithFilters(context.Context, pagination.PaginationParams, string, string, string, *bool) ([]Group, *pagination.PaginationResult, error) {
	panic("unexpected ListWithFilters call")
}
func (groupRepoNoop) ListActive(context.Context) ([]Group, error) {
	panic("unexpected ListActive call")
}
func (groupRepoNoop) ListActiveByPlatform(context.Context, string) ([]Group, error) {
	panic("unexpected ListActiveByPlatform call")
}
func (groupRepoNoop) ExistsByName(context.Context, string) (bool, error) {
	panic("unexpected ExistsByName call")
}
func (groupRepoNoop) GetAccountCount(context.Context, int64) (int64, int64, error) {
	panic("unexpected GetAccountCount call")
}
func (groupRepoNoop) DeleteAccountGroupsByGroupID(context.Context, int64) (int64, error) {
	panic("unexpected DeleteAccountGroupsByGroupID call")
}
func (groupRepoNoop) GetAccountIDsByGroupIDs(context.Context, []int64) ([]int64, error) {
	panic("unexpected GetAccountIDsByGroupIDs call")
}
func (groupRepoNoop) BindAccountsToGroup(context.Context, int64, []int64) error {
	panic("unexpected BindAccountsToGroup call")
}
func (groupRepoNoop) UpdateSortOrders(context.Context, []GroupSortOrderUpdate) error {
	panic("unexpected UpdateSortOrders call")
}

type subscriptionGroupRepoStub struct {
	groupRepoNoop
	group *Group
}

func (s *subscriptionGroupRepoStub) GetByID(context.Context, int64) (*Group, error) {
	return s.group, nil
}

// userSubRepoNoop 是 UserSubscriptionRepository 的空实现基座：
// 任何未被具体 stub 覆盖的方法被调用即 panic，用于把「不该发生的仓储访问」变成测试失败。
type userSubRepoNoop struct{}

func (userSubRepoNoop) Create(context.Context, *UserSubscription) error {
	panic("unexpected Create call")
}
func (userSubRepoNoop) GetByID(context.Context, int64) (*UserSubscription, error) {
	panic("unexpected GetByID call")
}
func (userSubRepoNoop) GetByIDForUpdate(context.Context, int64) (*UserSubscription, error) {
	panic("unexpected GetByIDForUpdate call")
}
func (userSubRepoNoop) GetByIDIncludeDeleted(context.Context, int64) (*UserSubscription, error) {
	panic("unexpected GetByIDIncludeDeleted call")
}
func (userSubRepoNoop) Update(context.Context, *UserSubscription) error {
	panic("unexpected Update call")
}
func (userSubRepoNoop) Delete(context.Context, int64) error { panic("unexpected Delete call") }
func (userSubRepoNoop) Restore(context.Context, int64, string) (*UserSubscription, error) {
	panic("unexpected Restore call")
}
func (userSubRepoNoop) ListByUserID(context.Context, int64) ([]UserSubscription, error) {
	panic("unexpected ListByUserID call")
}
func (userSubRepoNoop) ListActiveByUserID(context.Context, int64) ([]UserSubscription, error) {
	panic("unexpected ListActiveByUserID call")
}
func (userSubRepoNoop) List(context.Context, pagination.PaginationParams, *int64, *int64, string, string, string) ([]UserSubscription, *pagination.PaginationResult, error) {
	panic("unexpected List call")
}
func (userSubRepoNoop) ExistsActiveByUserID(context.Context, int64) (bool, error) {
	panic("unexpected ExistsActiveByUserID call")
}
func (userSubRepoNoop) UpdateAssignedLimit(context.Context, int64, *float64) error {
	panic("unexpected UpdateAssignedLimit call")
}
func (userSubRepoNoop) ExtendExpiry(context.Context, int64, time.Time) error {
	panic("unexpected ExtendExpiry call")
}
func (userSubRepoNoop) UpdateStatus(context.Context, int64, string) error {
	panic("unexpected UpdateStatus call")
}
func (userSubRepoNoop) UpdateNotes(context.Context, int64, string) error {
	panic("unexpected UpdateNotes call")
}
func (userSubRepoNoop) ResetUsage(context.Context, int64) error {
	panic("unexpected ResetUsage call")
}
func (userSubRepoNoop) IncrementUsage(context.Context, int64, float64) error {
	panic("unexpected IncrementUsage call")
}
func (userSubRepoNoop) BatchUpdateExpiredStatus(context.Context) (int64, error) {
	panic("unexpected BatchUpdateExpiredStatus call")
}

// subscriptionUserSubRepoStub 是分配链路用的内存钱包仓储。
//
// 索引只有一个坐标 byID：钱包化后寻址靠 ListByUserID + PlanID 过滤
// （旧 byUserGroup 的 (user, group) 槽位键已随 group_id 列一起删除）。
type subscriptionUserSubRepoStub struct {
	userSubRepoNoop

	nextID      int64
	byID        map[int64]*UserSubscription
	createCalls int
}

func newSubscriptionUserSubRepoStub() *subscriptionUserSubRepoStub {
	return &subscriptionUserSubRepoStub{
		nextID: 1,
		byID:   make(map[int64]*UserSubscription),
	}
}

func (s *subscriptionUserSubRepoStub) seed(sub *UserSubscription) {
	if sub == nil {
		return
	}
	cp := *sub
	if cp.ID == 0 {
		cp.ID = s.nextID
		s.nextID++
	}
	s.byID[cp.ID] = &cp
}

func (s *subscriptionUserSubRepoStub) Create(_ context.Context, sub *UserSubscription) error {
	if sub == nil {
		return nil
	}
	s.createCalls++
	cp := *sub
	if cp.ID == 0 {
		cp.ID = s.nextID
		s.nextID++
	}
	sub.ID = cp.ID
	s.byID[cp.ID] = &cp
	return nil
}

func (s *subscriptionUserSubRepoStub) GetByID(_ context.Context, id int64) (*UserSubscription, error) {
	sub := s.byID[id]
	if sub == nil {
		return nil, ErrSubscriptionNotFound
	}
	cp := *sub
	return &cp, nil
}

func (s *subscriptionUserSubRepoStub) GetByIDForUpdate(ctx context.Context, id int64) (*UserSubscription, error) {
	return s.GetByID(ctx, id)
}

func (s *subscriptionUserSubRepoStub) Update(_ context.Context, sub *UserSubscription) error {
	if sub == nil {
		return ErrSubscriptionNilInput
	}
	if s.byID[sub.ID] == nil {
		return ErrSubscriptionNotFound
	}
	cp := *sub
	s.byID[cp.ID] = &cp
	return nil
}

// ListByUserID 返回该用户全部钱包（分配寻址的入口）。
func (s *subscriptionUserSubRepoStub) ListByUserID(_ context.Context, userID int64) ([]UserSubscription, error) {
	out := make([]UserSubscription, 0, len(s.byID))
	for _, sub := range s.byID {
		if sub.UserID == userID {
			out = append(out, *sub)
		}
	}
	return out, nil
}

func (s *subscriptionUserSubRepoStub) ExistsActiveByUserID(_ context.Context, userID int64) (bool, error) {
	for _, sub := range s.byID {
		if sub.UserID == userID && sub.Status == SubscriptionStatusActive && sub.ExpiresAt.After(time.Now()) {
			return true, nil
		}
	}
	return false, nil
}

// UpdateAssignedLimit 只写额度列，绝不触碰用量（与生产仓储口径一致：
// 整行覆盖会吞掉并发 IncrementUsage 写入的用量）。
func (s *subscriptionUserSubRepoStub) UpdateAssignedLimit(_ context.Context, id int64, total *float64) error {
	sub := s.byID[id]
	if sub == nil {
		return ErrSubscriptionNotFound
	}
	sub.TotalLimitUSD = normalizeSubLimit(total)
	return nil
}

func (s *subscriptionUserSubRepoStub) ExtendExpiry(_ context.Context, id int64, expiresAt time.Time) error {
	sub := s.byID[id]
	if sub == nil {
		return ErrSubscriptionNotFound
	}
	sub.ExpiresAt = expiresAt
	return nil
}

func (s *subscriptionUserSubRepoStub) UpdateStatus(_ context.Context, id int64, status string) error {
	sub := s.byID[id]
	if sub == nil {
		return ErrSubscriptionNotFound
	}
	sub.Status = status
	return nil
}

func (s *subscriptionUserSubRepoStub) UpdateNotes(_ context.Context, id int64, notes string) error {
	sub := s.byID[id]
	if sub == nil {
		return ErrSubscriptionNotFound
	}
	sub.Notes = notes
	return nil
}

func (s *subscriptionUserSubRepoStub) ResetUsage(_ context.Context, id int64) error {
	sub := s.byID[id]
	if sub == nil {
		return ErrSubscriptionNotFound
	}
	sub.TotalUsageUSD = 0
	return nil
}

func (s *subscriptionUserSubRepoStub) IncrementUsage(_ context.Context, id int64, costUSD float64) error {
	sub := s.byID[id]
	if sub == nil {
		return ErrSubscriptionNotFound
	}
	sub.TotalUsageUSD += costUSD
	return nil
}

// planIDPtr 构造 *int64（PlanID / 额度坐标）。本文件无构建标签，
// 所以不能复用定义在带 //go:build unit 文件里的 ptrInt64。
func planIDPtr(v int64) *int64 { return &v }

// ── 分配幂等语义（PlanID 为幂等坐标）──────────────────────────────

func TestAssignSubscriptionReuseWhenSemanticsMatch(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	subRepo := newSubscriptionUserSubRepoStub()
	subRepo.seed(&UserSubscription{
		ID:        10,
		UserID:    1001,
		PlanID:    planIDPtr(1),
		StartsAt:  start,
		ExpiresAt: start.AddDate(0, 0, 30),
		Status:    SubscriptionStatusActive,
		Notes:     "init",
	})

	// 订阅不绑定分组：groupRepo 传空实现（任何访问都会 panic），
	// 以此锁定分配链路不再触碰分组仓储（契约 2/6）。
	svc := NewSubscriptionService(groupRepoNoop{}, subRepo, nil, nil, nil)
	sub, err := svc.AssignSubscription(context.Background(), &AssignSubscriptionInput{
		UserID:       1001,
		PlanID:       planIDPtr(1),
		ValidityDays: 30,
		Notes:        "init",
	})
	require.NoError(t, err)
	require.Equal(t, int64(10), sub.ID)
	require.Equal(t, 0, subRepo.createCalls, "reuse should not create new subscription")
	require.Equal(t, start, sub.StartsAt)
	require.Equal(t, start.AddDate(0, 0, 30), sub.ExpiresAt)
}

func TestAssignSubscriptionDoesNotReactivateFutureSuspendedSubscription(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	subRepo := newSubscriptionUserSubRepoStub()
	subRepo.seed(&UserSubscription{
		ID:        13,
		UserID:    1003,
		PlanID:    planIDPtr(1),
		StartsAt:  start,
		ExpiresAt: start.AddDate(0, 0, 30),
		Status:    SubscriptionStatusSuspended,
		Notes:     "assignment",
	})

	svc := NewSubscriptionService(groupRepoNoop{}, subRepo, nil, nil, nil)
	sub, err := svc.AssignSubscription(context.Background(), &AssignSubscriptionInput{
		UserID:       1003,
		PlanID:       planIDPtr(1),
		ValidityDays: 30,
		Notes:        "assignment",
	})

	require.NoError(t, err)
	require.Equal(t, int64(13), sub.ID)
	require.Equal(t, SubscriptionStatusSuspended, sub.Status)
	require.Equal(t, start, sub.StartsAt)
	require.Equal(t, start.AddDate(0, 0, 30), sub.ExpiresAt)
	require.Equal(t, "assignment", sub.Notes)
	require.Equal(t, 0, subRepo.createCalls)
}

func TestAssignSubscriptionDoesNotReactivatePastExpirySuspendedSubscription(t *testing.T) {
	start := time.Now().AddDate(0, 0, -31)
	expiresAt := start.AddDate(0, 0, 30)
	subRepo := newSubscriptionUserSubRepoStub()
	subRepo.seed(&UserSubscription{
		ID:            15,
		UserID:        1005,
		PlanID:        planIDPtr(1),
		StartsAt:      start,
		ExpiresAt:     expiresAt,
		Status:        SubscriptionStatusSuspended,
		TotalLimitUSD: ptrFloat64(100),
		TotalUsageUSD: 6,
		Notes:         "suspended assignment",
	})

	svc := NewSubscriptionService(groupRepoNoop{}, subRepo, nil, nil, nil)
	sub, err := svc.AssignSubscription(context.Background(), &AssignSubscriptionInput{
		UserID:       1005,
		PlanID:       planIDPtr(1),
		ValidityDays: 30,
		Notes:        "suspended assignment",
	})

	require.NoError(t, err)
	require.Equal(t, int64(15), sub.ID)
	require.Equal(t, SubscriptionStatusSuspended, sub.Status, "被人工暂停的钱包不得被一次普通分配唤醒")
	require.Equal(t, start, sub.StartsAt)
	require.Equal(t, expiresAt, sub.ExpiresAt)
	require.Equal(t, "suspended assignment", sub.Notes)
	require.InDelta(t, 100.0, *sub.TotalLimitUSD, 1e-9)
	require.InDelta(t, 6.0, sub.TotalUsageUSD, 1e-9, "分配不得改动总额池")
	require.Equal(t, 0, subRepo.createCalls)
}

func TestAssignSubscriptionRenewsExpiredWalletKeepsUsage(t *testing.T) {
	subRepo := newSubscriptionUserSubRepoStub()
	oldStart := time.Now().Add(-time.Hour)
	subRepo.seed(&UserSubscription{
		ID:            12,
		UserID:        1002,
		PlanID:        planIDPtr(1),
		StartsAt:      oldStart,
		ExpiresAt:     oldStart.AddDate(0, 0, 30),
		Status:        SubscriptionStatusExpired,
		TotalLimitUSD: ptrFloat64(100),
		TotalUsageUSD: 6,
		Notes:         " assignment ",
	})

	svc := NewSubscriptionService(groupRepoNoop{}, subRepo, nil, nil, nil)
	before := time.Now()
	sub, err := svc.AssignSubscription(context.Background(), &AssignSubscriptionInput{
		UserID:       1002,
		PlanID:       planIDPtr(1),
		ValidityDays: 30,
		Notes:        "assignment",
	})
	after := time.Now()

	require.NoError(t, err)
	require.Equal(t, int64(12), sub.ID)
	require.Equal(t, 0, subRepo.createCalls)
	require.Equal(t, SubscriptionStatusActive, sub.Status)
	require.False(t, sub.StartsAt.Before(before))
	require.False(t, sub.StartsAt.After(after))
	require.Equal(t, sub.StartsAt.AddDate(0, 0, 30), sub.ExpiresAt)
	// 总额池是一次性的（产品定案 4）：续费只延长有效期，**不清零已用额度**。
	// 清零等于凭空再发一笔钱，是资损方向，必须锁死。
	require.InDelta(t, 6.0, sub.TotalUsageUSD, 1e-9, "续费不得把已花掉的钱退回去")
	require.InDelta(t, 100.0, *sub.TotalLimitUSD, 1e-9, "未带额度的分配保持原额度不变")
	// 备注在 trim 后相同 → 不重复追加。
	require.Equal(t, " assignment ", sub.Notes)
}

func TestAssignSubscriptionRenewsExpiredAndAppendsDifferentNotes(t *testing.T) {
	subRepo := newSubscriptionUserSubRepoStub()
	oldStart := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	subRepo.seed(&UserSubscription{
		ID:        14,
		UserID:    1004,
		PlanID:    planIDPtr(1),
		StartsAt:  oldStart,
		ExpiresAt: oldStart.AddDate(0, 0, 30),
		Status:    SubscriptionStatusExpired,
		Notes:     "old assignment",
	})

	svc := NewSubscriptionService(groupRepoNoop{}, subRepo, nil, nil, nil)
	sub, err := svc.AssignSubscription(context.Background(), &AssignSubscriptionInput{
		UserID:       1004,
		PlanID:       planIDPtr(1),
		ValidityDays: 30,
		Notes:        "new assignment",
	})

	require.NoError(t, err)
	require.Equal(t, "old assignment\nnew assignment", sub.Notes)
}

func TestAssignSubscriptionConflictWhenSemanticsMismatch(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	subRepo := newSubscriptionUserSubRepoStub()
	subRepo.seed(&UserSubscription{
		ID:        11,
		UserID:    2001,
		PlanID:    planIDPtr(1),
		StartsAt:  start,
		ExpiresAt: start.AddDate(0, 0, 30),
		Status:    SubscriptionStatusActive,
		Notes:     "old-note",
	})

	svc := NewSubscriptionService(groupRepoNoop{}, subRepo, nil, nil, nil)
	_, err := svc.AssignSubscription(context.Background(), &AssignSubscriptionInput{
		UserID:       2001,
		PlanID:       planIDPtr(1),
		ValidityDays: 30,
		Notes:        "new-note",
	})
	require.Error(t, err)
	require.Equal(t, "SUBSCRIPTION_ASSIGN_CONFLICT", infraerrorsReason(err))
	require.Equal(t, 0, subRepo.createCalls, "conflict should not create or mutate existing subscription")
}

func TestBulkAssignSubscriptionCreatedReusedAndConflict(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	subRepo := newSubscriptionUserSubRepoStub()
	// user 1: 语义一致，可 reused
	subRepo.seed(&UserSubscription{
		ID:        21,
		UserID:    1,
		PlanID:    planIDPtr(1),
		StartsAt:  start,
		ExpiresAt: start.AddDate(0, 0, 30),
		Status:    SubscriptionStatusActive,
		Notes:     "same-note",
	})
	// user 3: 语义冲突（有效期不一致），应 failed
	subRepo.seed(&UserSubscription{
		ID:        23,
		UserID:    3,
		PlanID:    planIDPtr(1),
		StartsAt:  start,
		ExpiresAt: start.AddDate(0, 0, 60),
		Status:    SubscriptionStatusActive,
		Notes:     "same-note",
	})

	svc := NewSubscriptionService(groupRepoNoop{}, subRepo, nil, nil, nil)
	result, err := svc.BulkAssignSubscription(context.Background(), &BulkAssignSubscriptionInput{
		UserIDs:      []int64{1, 2, 3},
		PlanID:       planIDPtr(1),
		ValidityDays: 30,
		AssignedBy:   9,
		Notes:        "same-note",
	})
	require.NoError(t, err)
	require.Equal(t, 2, result.SuccessCount)
	require.Equal(t, 1, result.CreatedCount)
	require.Equal(t, 1, result.ReusedCount)
	require.Equal(t, 1, result.FailedCount)
	require.Equal(t, "reused", result.Statuses[1])
	require.Equal(t, "created", result.Statuses[2])
	require.Equal(t, "failed", result.Statuses[3])
	require.Equal(t, 1, subRepo.createCalls)
}

func TestBulkAssignSubscriptionRenewsExpiredWalletKeepsUsage(t *testing.T) {
	subRepo := newSubscriptionUserSubRepoStub()
	oldStart := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	subRepo.seed(&UserSubscription{
		ID:            24,
		UserID:        4,
		PlanID:        planIDPtr(1),
		StartsAt:      oldStart,
		ExpiresAt:     oldStart.AddDate(0, 0, 7),
		Status:        SubscriptionStatusExpired,
		TotalLimitUSD: ptrFloat64(50),
		TotalUsageUSD: 6,
		Notes:         "bulk",
	})

	svc := NewSubscriptionService(groupRepoNoop{}, subRepo, nil, nil, nil)
	before := time.Now()
	result, err := svc.BulkAssignSubscription(context.Background(), &BulkAssignSubscriptionInput{
		UserIDs:      []int64{4},
		PlanID:       planIDPtr(1),
		ValidityDays: 7,
		Notes:        "bulk",
	})
	after := time.Now()

	require.NoError(t, err)
	require.Equal(t, 1, result.SuccessCount)
	require.Equal(t, 0, result.CreatedCount)
	require.Equal(t, 1, result.ReusedCount)
	require.Equal(t, "reused", result.Statuses[4])
	require.Len(t, result.Subscriptions, 1)
	renewed := result.Subscriptions[0]
	require.Equal(t, SubscriptionStatusActive, renewed.Status)
	require.False(t, renewed.StartsAt.Before(before))
	require.False(t, renewed.StartsAt.After(after))
	require.Equal(t, renewed.StartsAt.AddDate(0, 0, 7), renewed.ExpiresAt)
	require.InDelta(t, 6.0, renewed.TotalUsageUSD, 1e-9, "批量续费同样不得清零总额池用量")
	require.Equal(t, "bulk", renewed.Notes)
}

func TestAssignSubscriptionKeepsWorkingWhenIdempotencyStoreUnavailable(t *testing.T) {
	subRepo := newSubscriptionUserSubRepoStub()
	SetDefaultIdempotencyCoordinator(NewIdempotencyCoordinator(failingIdempotencyRepo{}, DefaultIdempotencyConfig()))
	t.Cleanup(func() {
		SetDefaultIdempotencyCoordinator(nil)
	})

	svc := NewSubscriptionService(groupRepoNoop{}, subRepo, nil, nil, nil)
	sub, err := svc.AssignSubscription(context.Background(), &AssignSubscriptionInput{
		UserID:       9001,
		PlanID:       planIDPtr(1),
		ValidityDays: 30,
		Notes:        "new",
	})
	require.NoError(t, err)
	require.NotNil(t, sub)
	require.Equal(t, 1, subRepo.createCalls, "semantic idempotent endpoint should not depend on idempotency store availability")
}

// 无套餐的手工发放（PlanID=nil）按「该用户是否已有生效钱包」寻址，
// 并且必须在用户已有钱包时取到期最晚的一份作为主钱包。
func TestAssignSubscriptionManualGrantTargetsWalletWithoutPlan(t *testing.T) {
	now := time.Now()
	subRepo := newSubscriptionUserSubRepoStub()
	subRepo.seed(&UserSubscription{
		ID:            31,
		UserID:        7001,
		StartsAt:      now.AddDate(0, 0, -10),
		ExpiresAt:     now.AddDate(0, 0, 20),
		Status:        SubscriptionStatusActive,
		TotalUsageUSD: 6,
		Notes:         "manual",
	})

	svc := NewSubscriptionService(groupRepoNoop{}, subRepo, nil, nil, nil)
	limit := 25.0
	sub, err := svc.AssignSubscription(context.Background(), &AssignSubscriptionInput{
		UserID:        7001,
		PlanID:        nil,
		ValidityDays:  30,
		Notes:         "manual",
		TotalLimitUSD: &limit,
	})
	require.NoError(t, err)
	require.Equal(t, int64(31), sub.ID)
	require.Equal(t, 0, subRepo.createCalls, "已有生效钱包时应复用而非新开一份")
	require.Nil(t, sub.PlanID, "手工发放没有套餐")

	// 带额度的请求视为「调额度」：必须走 UpdateAssignedLimit 落库。
	// 注意 AssignSubscription 在这一分支返回的是改额度**前**读到的内存副本
	//（仓储层只写额度列，不回读），所以断言必须看库里的实际写入结果：
	// 额度已更新，且用量没有因为整行覆盖而被吞掉。
	stored, err := subRepo.GetByID(context.Background(), 31)
	require.NoError(t, err)
	require.InDelta(t, 25.0, *stored.TotalLimitUSD, 1e-9, "额度应写入总额池快照")
	require.InDelta(t, 6.0, stored.TotalUsageUSD, 1e-9, "改额度不得覆盖已有用量")
}

func TestNormalizeAssignValidityDays(t *testing.T) {
	require.Equal(t, 30, normalizeAssignValidityDays(0))
	require.Equal(t, 30, normalizeAssignValidityDays(-5))
	require.Equal(t, MaxValidityDays, normalizeAssignValidityDays(MaxValidityDays+100))
	require.Equal(t, 7, normalizeAssignValidityDays(7))
}

func TestDetectAssignSemanticConflictCases(t *testing.T) {
	start := time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC)
	base := &UserSubscription{
		UserID:    1,
		PlanID:    planIDPtr(1),
		StartsAt:  start,
		ExpiresAt: start.AddDate(0, 0, 30),
		Notes:     "same",
	}

	reason, conflict := detectAssignSemanticConflict(base, &AssignSubscriptionInput{
		UserID:       1,
		PlanID:       planIDPtr(1),
		ValidityDays: 30,
		Notes:        "same",
	})
	require.False(t, conflict)
	require.Equal(t, "", reason)

	reason, conflict = detectAssignSemanticConflict(base, &AssignSubscriptionInput{
		UserID:       1,
		PlanID:       planIDPtr(1),
		ValidityDays: 60,
		Notes:        "same",
	})
	require.True(t, conflict)
	require.Equal(t, "validity_days_mismatch", reason)

	reason, conflict = detectAssignSemanticConflict(base, &AssignSubscriptionInput{
		UserID:       1,
		PlanID:       planIDPtr(1),
		ValidityDays: 30,
		Notes:        "other",
	})
	require.True(t, conflict)
	require.Equal(t, "notes_mismatch", reason)
}

// 订阅不再绑定分组：分配链路必须完全不触碰分组仓储（契约 2/6）。
// 用 groupRepoNoop（任何方法都 panic）构造服务并成功分配，即反向锁定
// 「分组存废/类型校验已彻底退出分配路径」。
func TestAssignSubscriptionNeverTouchesGroupRepository(t *testing.T) {
	subRepo := newSubscriptionUserSubRepoStub()
	svc := NewSubscriptionService(groupRepoNoop{}, subRepo, nil, nil, nil)

	sub, err := svc.AssignSubscription(context.Background(), &AssignSubscriptionInput{
		UserID:       8001,
		PlanID:       planIDPtr(99),
		ValidityDays: 30,
	})
	require.NoError(t, err, "分配不得因分组校验被拒绝（分组准入与订阅无关）")
	require.NotNil(t, sub)
	require.Equal(t, 1, subRepo.createCalls)

	// 带额度同样直通，且额度落的是快照而非分组配置。
	subRepo2 := newSubscriptionUserSubRepoStub()
	svc2 := NewSubscriptionService(groupRepoNoop{}, subRepo2, nil, nil, nil)
	limit := 12.5
	sub2, err := svc2.AssignSubscription(context.Background(), &AssignSubscriptionInput{
		UserID:        8002,
		PlanID:        planIDPtr(99),
		ValidityDays:  7,
		TotalLimitUSD: &limit,
	})
	require.NoError(t, err)
	require.InDelta(t, 12.5, *sub2.TotalLimitUSD, 1e-9)
	require.Zero(t, sub2.TotalUsageUSD, "新发钱包用量从 0 开始")
	require.True(t, sub2.HasEffectiveLimit(), "填了额度的钱包应接管扣费")
}

func infraerrorsReason(err error) string {
	return infraerrors.Reason(err)
}
