package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type bulkActionSubscriptionRepo struct {
	UserSubscriptionRepository
	subscriptions map[int64]*UserSubscription
	mutations     []int64
	afterMutation func()
}

func (r *bulkActionSubscriptionRepo) GetByIDIncludeDeleted(_ context.Context, id int64) (*UserSubscription, error) {
	sub := r.subscriptions[id]
	if sub == nil {
		return nil, ErrSubscriptionNotFound
	}
	copy := *sub
	return &copy, nil
}

func (r *bulkActionSubscriptionRepo) GetByID(ctx context.Context, id int64) (*UserSubscription, error) {
	sub, err := r.GetByIDIncludeDeleted(ctx, id)
	if err != nil {
		return nil, err
	}
	if sub.DeletedAt != nil {
		return nil, ErrSubscriptionNotFound
	}
	return sub, nil
}

func (r *bulkActionSubscriptionRepo) GetByIDForUpdate(ctx context.Context, id int64) (*UserSubscription, error) {
	return r.GetByID(ctx, id)
}

func (r *bulkActionSubscriptionRepo) mutated(id int64) {
	r.mutations = append(r.mutations, id)
	if r.afterMutation != nil {
		r.afterMutation()
	}
}

func (r *bulkActionSubscriptionRepo) ExtendExpiry(_ context.Context, id int64, expiresAt time.Time) error {
	r.subscriptions[id].ExpiresAt = expiresAt
	r.mutated(id)
	return nil
}

// ResetUsage 是钱包化后「重置额度」的唯一写入口：整包清零 total_usage_usd，
// 不触碰额度列（旧 ResetUsageWindows 的 daily/weekly/monthly 三档窗口选择已废除）。
func (r *bulkActionSubscriptionRepo) ResetUsage(_ context.Context, id int64) error {
	sub := r.subscriptions[id]
	sub.TotalUsageUSD = 0
	r.mutated(id)
	return nil
}

func (r *bulkActionSubscriptionRepo) Delete(_ context.Context, id int64) error {
	now := time.Now()
	r.subscriptions[id].DeletedAt = &now
	r.mutated(id)
	return nil
}

// ExistsActiveByUserID 是复活闸门的钱包化坐标：只看该用户是否还持有未删除的钱包。
func (r *bulkActionSubscriptionRepo) ExistsActiveByUserID(_ context.Context, userID int64) (bool, error) {
	for _, sub := range r.subscriptions {
		if sub.UserID == userID && sub.DeletedAt == nil {
			return true, nil
		}
	}
	return false, nil
}

func (r *bulkActionSubscriptionRepo) Restore(_ context.Context, id int64, status string) (*UserSubscription, error) {
	sub := r.subscriptions[id]
	sub.DeletedAt, sub.Status = nil, status
	r.mutated(id)
	copy := *sub
	return &copy, nil
}

func TestBulkSubscriptionAction_PartialSuccessAndDeduplication(t *testing.T) {
	for _, action := range []string{"extend", "reset_quota", "revoke", "restore"} {
		t.Run(action, func(t *testing.T) {
			expiresAt := time.Now().AddDate(0, 0, 30)
			repo := &bulkActionSubscriptionRepo{subscriptions: map[int64]*UserSubscription{}}
			for _, id := range []int64{1, 2} {
				sub := &UserSubscription{
					ID: id, UserID: id, Status: SubscriptionStatusActive, ExpiresAt: expiresAt,
					TotalLimitUSD: ptrFloat64(20), TotalUsageUSD: 8,
				}
				if action == "restore" {
					deletedAt := time.Now().Add(-time.Hour)
					sub.DeletedAt = &deletedAt
				}
				repo.subscriptions[id] = sub
			}
			svc := NewSubscriptionService(nil, repo, nil, nil, nil)
			t.Cleanup(svc.Stop)

			result, err := svc.BulkSubscriptionAction(context.Background(), &BulkSubscriptionActionInput{
				SubscriptionIDs: []int64{2, 404, 1, 2, 404}, Action: action, Days: 7,
			})
			require.NoError(t, err)
			require.Equal(t, 2, result.SuccessCount)
			require.Equal(t, 1, result.FailedCount)
			require.Equal(t, []BulkSubscriptionActionItemResult{
				{SubscriptionID: 2, Success: true},
				{SubscriptionID: 404, Error: "subscription not found"},
				{SubscriptionID: 1, Success: true},
			}, result.Results)
			require.Equal(t, []int64{2, 1}, repo.mutations)
			for _, sub := range repo.subscriptions {
				switch action {
				case "extend":
					require.Equal(t, expiresAt.AddDate(0, 0, 7), sub.ExpiresAt)
				case "reset_quota":
					// 原语义「只重置勾选的窗口（Daily/Weekly 归零、Monthly 保持 8）」已随
					// 窗口概念废除：总额池只有一个可重置的口径，勾选无效，一律整体归零；
					// 额度快照不动（重置 = 免掉已花的钱，不是再加一笔钱）。
					require.Zero(t, sub.TotalUsageUSD)
					require.NotNil(t, sub.TotalLimitUSD)
					require.Equal(t, float64(20), *sub.TotalLimitUSD)
				case "revoke":
					require.NotNil(t, sub.DeletedAt)
				case "restore":
					require.Nil(t, sub.DeletedAt)
					require.Equal(t, SubscriptionStatusActive, sub.Status)
				}
			}
		})
	}
}

func TestBulkSubscriptionAction_ValidatesBeforeAnyRepositoryAccess(t *testing.T) {
	tooMany := make([]int64, MaxBulkSubscriptionActions+1)
	for i := range tooMany {
		tooMany[i] = 1
	}
	for name, input := range map[string]*BulkSubscriptionActionInput{
		"nil":              nil,
		"empty IDs":        {Action: "revoke"},
		"too many IDs":     {SubscriptionIDs: tooMany, Action: "revoke"},
		"invalid later ID": {SubscriptionIDs: []int64{1, 0}, Action: "revoke"},
		"negative ID":      {SubscriptionIDs: []int64{1, -1}, Action: "revoke"},
		"unknown action":   {SubscriptionIDs: []int64{1}, Action: "delete"},
		"missing action":   {SubscriptionIDs: []int64{1}},
		"zero adjustment":  {SubscriptionIDs: []int64{1}, Action: "extend"},
		"large adjustment": {SubscriptionIDs: []int64{1}, Action: "extend", Days: MaxValidityDays + 1},
		"small adjustment": {SubscriptionIDs: []int64{1}, Action: "extend", Days: -MaxValidityDays - 1},
	} {
		t.Run(name, func(t *testing.T) {
			// A nil repository would panic if validation allowed any execution.
			svc := &SubscriptionService{}
			result, err := svc.BulkSubscriptionAction(context.Background(), input)
			require.Error(t, err)
			require.Equal(t, 400, infraerrors.Code(err))
			require.Nil(t, result)
		})
	}
	for _, days := range []int{-MaxValidityDays, -1, 1, MaxValidityDays} {
		input := BulkSubscriptionActionInput{SubscriptionIDs: []int64{1}, Action: "extend", Days: days}
		require.NoError(t, input.Validate())
	}
	// 总额池的重置无窗口可勾选：裸 reset_quota 请求现在必须合法
	// （旧「至少勾一个窗口 → 400」校验随三窗口一并废除，ErrInvalidInput 已删除）。
	require.NoError(t, (&BulkSubscriptionActionInput{SubscriptionIDs: []int64{1}, Action: "reset_quota"}).Validate())
}

func TestBulkSubscriptionAction_CancellationPreservesCompletedResults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repo := &bulkActionSubscriptionRepo{
		subscriptions: map[int64]*UserSubscription{1: {ID: 1, UserID: 1}},
		afterMutation: cancel,
	}
	svc := NewSubscriptionService(nil, repo, nil, nil, nil)
	t.Cleanup(svc.Stop)
	result, err := svc.BulkSubscriptionAction(ctx, &BulkSubscriptionActionInput{SubscriptionIDs: []int64{1, 2, 3}, Action: "revoke"})
	require.NoError(t, err)
	require.Equal(t, 1, result.SuccessCount)
	require.Equal(t, 2, result.FailedCount)
	require.Equal(t, []int64{1}, repo.mutations)
	require.True(t, result.Results[0].Success)
	for _, item := range result.Results[1:] {
		require.False(t, item.Success)
		require.Equal(t, context.Canceled.Error(), item.Error)
	}
}

type failingBulkActionSubscriptionRepo struct {
	*bulkActionSubscriptionRepo
	err error
}

func (r failingBulkActionSubscriptionRepo) Delete(context.Context, int64) error {
	return r.err
}

func TestBulkSubscriptionAction_DoesNotExposeInternalErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "internal failure", err: errors.New("postgres: internal connection details"), want: "internal error"},
		{name: "wrapped cancellation", err: fmt.Errorf("postgres: internal connection details: %w", context.Canceled), want: context.Canceled.Error()},
		{name: "wrapped deadline", err: fmt.Errorf("postgres: internal connection details: %w", context.DeadlineExceeded), want: context.DeadlineExceeded.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := failingBulkActionSubscriptionRepo{
				bulkActionSubscriptionRepo: &bulkActionSubscriptionRepo{subscriptions: map[int64]*UserSubscription{1: {ID: 1}}},
				err:                        tc.err,
			}
			svc := NewSubscriptionService(nil, repo, nil, nil, nil)
			t.Cleanup(svc.Stop)
			result, err := svc.BulkSubscriptionAction(context.Background(), &BulkSubscriptionActionInput{SubscriptionIDs: []int64{1}, Action: "revoke"})
			require.NoError(t, err)
			require.Equal(t, 1, result.FailedCount)
			require.Equal(t, tc.want, result.Results[0].Error)
		})
	}
}
