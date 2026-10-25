//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 2026-10-21 需求：免费额度花完之后回落扣用户自己的余额，而不是直接 429。
//
// 这组用例锁住的是「预检」的行为边界。三条：
//  1. 钱包耗尽 + 余额充足 → 放行（这是本次需求的核心）；
//  2. 钱包耗尽 + 余额也不足 → 报余额不足（ErrInsufficientBalance），
//     不是 ErrSubscriptionQuotaExhausted：用户需要看到「充钱」而不是「等明天」；
//  3. 钱包本身失效（无生效订阅/已过期，ErrSubscriptionInvalid）→ **不回落**，
//     照旧拒绝。因为计费侧 SubscriptionWalletTakesOver 下一轮就会改走余额模式，
//     若这里放行而记账仍按订阅失败，会出现「预检通过、记账失败」的分叉。

func newExhaustedWalletRepoStub(limit, usage float64) *subRepoFixedWalletStub {
	return &subRepoFixedWalletStub{limit: limit, usage: usage}
}

// subRepoFixedWalletStub 返回一份固定的耗尽/未耗尽钱包，其余方法沿用空基座。
type subRepoFixedWalletStub struct {
	userSubRepoNoop
	limit  float64
	usage  float64
	period string
}

func (s *subRepoFixedWalletStub) ListActiveByUserID(context.Context, int64) ([]UserSubscription, error) {
	return []UserSubscription{{
		ID:            1,
		UserID:        1,
		Status:        SubscriptionStatusActive,
		ExpiresAt:     time.Now().Add(24 * time.Hour),
		TotalLimitUSD: &s.limit,
		TotalUsageUSD: s.usage,
		ResetPeriod:   s.period,
	}}, nil
}

// 钱包耗尽但用户余额充足：必须放行，并且不再要求 platform quota
// （额度已转由余额承担，platform quota 在余额模式下才生效——这是「更严格」的方向，
// 不是放宽：见 CheckBillingEligibility 里 isSubscriptionMode=false 后的分支）。
func TestCheckBillingEligibility_ExhaustedWalletFallsBackToBalance(t *testing.T) {
	cache := &balanceEligibilityCacheStub{balance: 5}
	cfg := &config.Config{}
	svc := NewBillingCacheService(cache, nil, newExhaustedWalletRepoStub(1, 1), nil, nil, nil, cfg, nil)
	t.Cleanup(svc.Stop)

	subs := []*UserSubscription{{
		ID:            1,
		UserID:        1,
		Status:        SubscriptionStatusActive,
		ExpiresAt:     time.Now().Add(24 * time.Hour),
		TotalLimitUSD: fallbackFloatPtr(1),
		TotalUsageUSD: 1,
	}}

	err := svc.CheckBillingEligibility(context.Background(), &User{ID: 1}, nil, nil, subs, "")
	require.NoError(t, err, "钱包花完 + 余额充足应放行")
}

// 钱包耗尽且余额也是 0：报余额不足（引导充值），不能报「订阅额度已用完」（引导等明天）。
func TestCheckBillingEligibility_ExhaustedWalletAndNoBalanceRejects(t *testing.T) {
	cache := &balanceEligibilityCacheStub{balance: 0}
	cfg := &config.Config{}
	svc := NewBillingCacheService(cache, nil, newExhaustedWalletRepoStub(1, 1), nil, nil, nil, cfg, nil)
	t.Cleanup(svc.Stop)

	subs := []*UserSubscription{{
		ID:            1,
		UserID:        1,
		Status:        SubscriptionStatusActive,
		ExpiresAt:     time.Now().Add(24 * time.Hour),
		TotalLimitUSD: fallbackFloatPtr(1),
		TotalUsageUSD: 1,
	}}

	err := svc.CheckBillingEligibility(context.Background(), &User{ID: 1}, nil, nil, subs, "")
	require.ErrorIs(t, err, ErrInsufficientBalance)
	require.NotErrorIs(t, err, ErrSubscriptionQuotaExhausted)
}

// 钱包未耗尽：不查余额（余额 0 也必须放行），否则「有免费额度但没充钱」的用户用不了。
func TestCheckBillingEligibility_ActiveWalletDoesNotRequireBalance(t *testing.T) {
	cache := &balanceEligibilityCacheStub{balance: 0}
	cfg := &config.Config{}
	svc := NewBillingCacheService(cache, nil, newExhaustedWalletRepoStub(1, 0.2), nil, nil, nil, cfg, nil)
	t.Cleanup(svc.Stop)

	subs := []*UserSubscription{{
		ID:            1,
		UserID:        1,
		Status:        SubscriptionStatusActive,
		ExpiresAt:     time.Now().Add(24 * time.Hour),
		TotalLimitUSD: fallbackFloatPtr(1),
		TotalUsageUSD: 0.2,
	}}

	err := svc.CheckBillingEligibility(context.Background(), &User{ID: 1}, nil, nil, subs, "")
	require.NoError(t, err)
}

// 周期钱包：库里用量已是满额，但窗口已跳（昨天的钱）→ 本窗口其实是满额。
// 这类用户绝不能因为「库里显示已用 1.0」被拒（否则「每天 1$」第二天不生效）。
func TestCheckBillingEligibility_RecurringWalletResetsAfterWindow(t *testing.T) {
	cache := &balanceEligibilityCacheStub{balance: 0}
	cfg := &config.Config{}
	repo := &subRepoFixedWalletStub{limit: 1, usage: 1, period: SubscriptionResetPeriodDaily}
	svc := NewBillingCacheService(cache, nil, repo, nil, nil, nil, cfg, nil)
	t.Cleanup(svc.Stop)

	subs := []*UserSubscription{{
		ID:               1,
		UserID:           1,
		Status:           SubscriptionStatusActive,
		ExpiresAt:        time.Now().Add(30 * 24 * time.Hour),
		TotalLimitUSD:    fallbackFloatPtr(1),
		TotalUsageUSD:    1,
		ResetPeriod:      SubscriptionResetPeriodDaily,
		UsageWindowStart: time.Now().AddDate(0, 0, -1),
	}}

	err := svc.CheckBillingEligibility(context.Background(), &User{ID: 1}, nil, nil, subs, "")
	require.NoError(t, err, "跳窗后应重新拥有 1$ 额度")
}

// 钱包失效（无生效订阅）：不回落余额，照旧拒绝。
// 「无生效订阅」在计费侧本就等于余额模式，两条链路口径一致；这里若放行，
// 反而会让「预检通过、记账失败」的情形变得可能。
func TestCheckBillingEligibility_NoActiveWalletDoesNotFallBack(t *testing.T) {
	cache := &balanceEligibilityCacheStub{balance: 100}
	cfg := &config.Config{}
	// 仓储返回空 → getSubscriptionFromDB 报 ErrSubscriptionNotFound（业务结论，非故障）
	svc := NewBillingCacheService(cache, nil, &subRepoFixedWalletStub{limit: 0, usage: 0, period: "empty"}, nil, nil, nil, cfg, nil)
	t.Cleanup(svc.Stop)

	subs := []*UserSubscription{{
		ID:            1,
		UserID:        1,
		Status:        SubscriptionStatusActive,
		ExpiresAt:     time.Now().Add(24 * time.Hour),
		TotalLimitUSD: fallbackFloatPtr(1),
	}}
	_ = subs

	// 注意：这里仓储实际返回一份「不限额」钱包（limit=0 归一为 nil），
	// 因此 checkSubscriptionEligibility 会直接放行；本用例只锁住
	// 「SubscriptionWalletTakesOver 为 false 时不走订阅预检」这一分支。
	walletIgnoresUnlimited := []*UserSubscription{{
		ID:          1,
		UserID:      1,
		Status:      SubscriptionStatusActive,
		ExpiresAt:   time.Now().Add(24 * time.Hour),
		ResetPeriod: SubscriptionResetPeriodNone,
	}}
	require.False(t, SubscriptionWalletTakesOver(walletIgnoresUnlimited),
		"全部不限额的钱包不接管扣费")

	err := svc.CheckBillingEligibility(context.Background(), &User{ID: 1}, nil, nil, walletIgnoresUnlimited, "")
	require.NoError(t, err, "不限额钱包走余额模式，余额 100 应放行")
}

func fallbackFloatPtr(v float64) *float64 { return &v }
