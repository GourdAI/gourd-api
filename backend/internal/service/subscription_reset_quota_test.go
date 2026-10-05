//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// resetQuotaUserSubRepoStub 支持 GetByID、ResetUsage，
// 其余方法继承 userSubRepoNoop（panic）。
//
// 钱包化后「重置额度」只有一个动作：整包清零 total_usage_usd。
// 旧 stub 的 ResetUsageWindows / ResetDailyUsage / ResetWeeklyUsage /
// ResetMonthlyUsage 四个窗口方法及其 dailyStart / periodicStart 窗口锚点字段，
// 都随「日/周/月滚动窗口」概念一并删除（总额池不滚动、无窗口可重置）。
type resetQuotaUserSubRepoStub struct {
	userSubRepoNoop

	sub *UserSubscription

	resetCalled bool
	resetErr    error
	resetID     int64
}

func (r *resetQuotaUserSubRepoStub) GetByID(_ context.Context, id int64) (*UserSubscription, error) {
	if r.sub == nil || r.sub.ID != id {
		return nil, ErrSubscriptionNotFound
	}
	cp := *r.sub
	return &cp, nil
}

// ResetUsage 模拟仓储层「绝对写 0」：只归零已用额度，不触碰额度列。
func (r *resetQuotaUserSubRepoStub) ResetUsage(_ context.Context, id int64) error {
	r.resetCalled = true
	r.resetID = id
	if r.resetErr != nil {
		return r.resetErr
	}
	if r.sub == nil {
		return nil
	}
	r.sub.TotalUsageUSD = 0
	return nil
}

func newResetQuotaSvc(stub *resetQuotaUserSubRepoStub) *SubscriptionService {
	return NewSubscriptionService(groupRepoNoop{}, stub, nil, nil, nil)
}

func TestAdminResetQuota_ResetsUsage(t *testing.T) {
	stub := &resetQuotaUserSubRepoStub{
		sub: &UserSubscription{ID: 1, UserID: 10, TotalLimitUSD: walletF64(20), TotalUsageUSD: 99.9},
	}
	svc := newResetQuotaSvc(stub)

	result, err := svc.AdminResetQuota(context.Background(), 1)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, stub.resetCalled, "应调用 ResetUsage 整包清零")
	require.Equal(t, int64(1), stub.resetID, "重置应落到查到的那份订阅上")
}

// TestAdminResetQuota_KeepsLimitClearsOnlyUsage 锁住「重置只免钱、不加钱」：
// 清零的是已用额度，总额度快照必须原样保留（改额度只有 Create / UpdateAssignedLimit 两个入口）。
func TestAdminResetQuota_KeepsLimitClearsOnlyUsage(t *testing.T) {
	stub := &resetQuotaUserSubRepoStub{
		sub: &UserSubscription{ID: 2, UserID: 10, TotalLimitUSD: walletF64(20), TotalUsageUSD: 8},
	}
	svc := newResetQuotaSvc(stub)

	result, err := svc.AdminResetQuota(context.Background(), 2)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, stub.resetCalled)
	require.Zero(t, result.TotalUsageUSD)
	require.NotNil(t, result.TotalLimitUSD)
	require.Equal(t, float64(20), *result.TotalLimitUSD)
	require.Equal(t, float64(20), *stub.sub.TotalLimitUSD)
}

func TestAdminResetQuota_SubscriptionNotFound(t *testing.T) {
	stub := &resetQuotaUserSubRepoStub{sub: nil}
	svc := newResetQuotaSvc(stub)

	_, err := svc.AdminResetQuota(context.Background(), 999)

	require.ErrorIs(t, err, ErrSubscriptionNotFound)
	require.False(t, stub.resetCalled, "订阅不存在时不得清零用量")
}

func TestAdminResetQuota_ResetUsageError(t *testing.T) {
	dbErr := errors.New("db error")
	stub := &resetQuotaUserSubRepoStub{
		sub:      &UserSubscription{ID: 4, UserID: 10, TotalUsageUSD: 5},
		resetErr: dbErr,
	}
	svc := newResetQuotaSvc(stub)

	_, err := svc.AdminResetQuota(context.Background(), 4)

	require.ErrorIs(t, err, dbErr)
	require.True(t, stub.resetCalled)
	// 清零失败必须原样上抛，且不得把未刷新的订阅当成重置结果返回。
	require.NotZero(t, stub.sub.TotalUsageUSD, "清零失败时用量应保持原值")
}

func TestAdminResetQuota_ReturnsRefreshedSub(t *testing.T) {
	stub := &resetQuotaUserSubRepoStub{
		sub: &UserSubscription{
			ID:            6,
			UserID:        10,
			TotalLimitUSD: walletF64(20),
			TotalUsageUSD: 99.9,
		},
	}

	svc := newResetQuotaSvc(stub)
	result, err := svc.AdminResetQuota(context.Background(), 6)

	require.NoError(t, err)
	// ResetUsage stub 会将 sub.TotalUsageUSD 归零，
	// 服务应返回第二次 GetByID 的刷新值而非初始的 99.9
	require.Equal(t, float64(0), result.TotalUsageUSD, "返回的订阅应反映已归零的用量")
	require.True(t, stub.resetCalled)
}
