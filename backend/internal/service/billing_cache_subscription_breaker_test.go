package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// subRepoErrStub 只覆盖 ListActiveByUserID，其余方法沿用「被调用即 panic」的空基座。
type subRepoErrStub struct {
	userSubRepoNoop
	err error
}

func (s subRepoErrStub) ListActiveByUserID(context.Context, int64) ([]UserSubscription, error) {
	return nil, s.err
}

func newBreakerSvcForEligibility(t *testing.T, repoErr error) *BillingCacheService {
	t.Helper()
	cfg := &config.Config{}
	cfg.Billing.CircuitBreaker = config.CircuitBreakerConfig{
		Enabled:             true,
		FailureThreshold:    2,
		ResetTimeoutSeconds: 60,
		HalfOpenRequests:    1,
	}
	// cache 传 nil：GetSubscriptionStatus 直读 DB，从而可控地触发错误分支。
	svc := NewBillingCacheService(nil, nil, subRepoErrStub{err: repoErr}, nil, nil, nil, cfg, nil)
	t.Cleanup(svc.Stop)
	return svc
}

// 「查无生效订阅」是业务结论，绝不能计入熔断器失败次数。
// 熔断器是进程级全局单例：一旦打开，所有用户的计费检查都会被拒成 503，
// 而到期/耗尽用户天然会落到这个分支（预检切片可能已陈旧一个 L1 TTL）。
func TestCheckSubscriptionEligibility_NotFoundDoesNotTripBreaker(t *testing.T) {
	svc := newBreakerSvcForEligibility(t, ErrSubscriptionNotFound)

	for i := 0; i < 5; i++ {
		err := svc.checkSubscriptionEligibility(context.Background(), 1)
		require.ErrorIs(t, err, ErrSubscriptionInvalid,
			"无生效订阅应返回业务性的 SUBSCRIPTION_INVALID（第 %d 次）", i)
		require.NotErrorIs(t, err, ErrBillingServiceUnavailable,
			"业务性「查无订阅」不得伪装成计费服务故障")
	}

	require.True(t, svc.circuitBreaker.Allow(),
		"业务性错误反复出现也不得打开熔断器，否则会波及全部用户")
}

// 真正的仓储故障仍必须计入熔断器并返回 503 —— 上面那条豁免不能把这个也放掉。
func TestCheckSubscriptionEligibility_DBErrorStillTripsBreaker(t *testing.T) {
	boom := errors.New("connection reset")
	svc := newBreakerSvcForEligibility(t, boom)

	for i := 0; i < 2; i++ {
		err := svc.checkSubscriptionEligibility(context.Background(), 1)
		require.ErrorIs(t, err, ErrBillingServiceUnavailable, "第 %d 次", i)
		require.ErrorIs(t, err, boom, "原始错误必须保留供排障")
	}

	require.False(t, svc.circuitBreaker.Allow(),
		"连续达到 failure_threshold 的 DB 故障应打开熔断器")
}
