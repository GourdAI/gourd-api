//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeRateLimit429Counter 是 RateLimit429CounterCache 的内存实现。
type fakeRateLimit429Counter struct {
	mu      sync.Mutex
	counts  map[int64]int64
	incErr  error
	resetCh []int64
}

func newFakeRateLimit429Counter() *fakeRateLimit429Counter {
	return &fakeRateLimit429Counter{counts: make(map[int64]int64)}
}

func (f *fakeRateLimit429Counter) IncrementRateLimit429Count(_ context.Context, accountID int64, _ int) (int64, error) {
	if f.incErr != nil {
		return 0, f.incErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[accountID]++
	return f.counts[accountID], nil
}

func (f *fakeRateLimit429Counter) ResetRateLimit429Count(_ context.Context, accountID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.counts, accountID)
	f.resetCh = append(f.resetCh, accountID)
	return nil
}

// rateLimit429RepoStub 复用 gemini mock 的接口面，仅覆写 SetRateLimited。
type rateLimit429RepoStub struct {
	mockAccountRepoForGemini
	lastUntil *time.Time
	calls     int
}

func (r *rateLimit429RepoStub) SetRateLimited(_ context.Context, _ int64, until time.Time) error {
	r.calls++
	r.lastUntil = &until
	return nil
}

// TestRateLimit429EscalationCooldownTiers 验证分级冷却的阶梯值。
func TestRateLimit429EscalationCooldownTiers(t *testing.T) {
	cases := []struct {
		count int64
		want  time.Duration
	}{
		{0, 0},
		{1, 0}, // 第 1 次不升级
		{2, 60 * time.Second},
		{3, 5 * time.Minute},
		{4, 30 * time.Minute},
		{5, 2 * time.Hour},
		{9, 2 * time.Hour}, // 封顶
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, rateLimit429EscalationCooldown(tc.count), "count=%d", tc.count)
	}
}

// TestEscalate429FallbackCooldown_NoCache 无计数器时返回原值。
func TestEscalate429FallbackCooldown_NoCache(t *testing.T) {
	svc := &RateLimitService{}
	base := 5 * time.Second
	got := svc.escalate429FallbackCooldown(context.Background(), &Account{ID: 1}, base)
	require.Equal(t, base, got)
}

// TestEscalate429FallbackCooldown_FirstEventKeepsBase 第 1 次保持默认。
func TestEscalate429FallbackCooldown_FirstEventKeepsBase(t *testing.T) {
	svc := &RateLimitService{rateLimit429CounterCache: newFakeRateLimit429Counter()}
	base := 5 * time.Second
	got := svc.escalate429FallbackCooldown(context.Background(), &Account{ID: 1}, base)
	require.Equal(t, base, got)
}

// TestEscalate429FallbackCooldown_Escalates 连续事件按阶梯递增。
func TestEscalate429FallbackCooldown_Escalates(t *testing.T) {
	svc := &RateLimitService{rateLimit429CounterCache: newFakeRateLimit429Counter()}
	base := 5 * time.Second
	account := &Account{ID: 1}

	want := []time.Duration{
		base,             // 1st
		60 * time.Second, // 2nd
		5 * time.Minute,  // 3rd
		30 * time.Minute, // 4th
		2 * time.Hour,    // 5th
		2 * time.Hour,    // 6th (capped)
	}
	for i, w := range want {
		got := svc.escalate429FallbackCooldown(context.Background(), account, base)
		require.Equal(t, w, got, "event #%d", i+1)
	}
}

// TestEscalate429FallbackCooldown_NoAccount 无账号时返回原值。
func TestEscalate429FallbackCooldown_NoAccount(t *testing.T) {
	svc := &RateLimitService{rateLimit429CounterCache: newFakeRateLimit429Counter()}
	base := 5 * time.Second
	require.Equal(t, base, svc.escalate429FallbackCooldown(context.Background(), nil, base))
	require.Equal(t, base, svc.escalate429FallbackCooldown(context.Background(), &Account{ID: 0}, base))
}

// TestEscalate429FallbackCooldown_IncrementErrorFallsBack 计数失败时降级为原值。
func TestEscalate429FallbackCooldown_IncrementErrorFallsBack(t *testing.T) {
	cache := newFakeRateLimit429Counter()
	cache.incErr = errors.New("redis down")
	svc := &RateLimitService{rateLimit429CounterCache: cache}
	base := 5 * time.Second
	got := svc.escalate429FallbackCooldown(context.Background(), &Account{ID: 1}, base)
	require.Equal(t, base, got)
}

// TestResetRateLimit429Counter 验证成功路径清零。
func TestResetRateLimit429Counter(t *testing.T) {
	cache := newFakeRateLimit429Counter()
	svc := &RateLimitService{rateLimit429CounterCache: cache}
	ctx := context.Background()

	_, err := cache.IncrementRateLimit429Count(ctx, 7, 1800)
	require.NoError(t, err)
	require.Equal(t, int64(1), cache.counts[7])

	svc.ResetRateLimit429Counter(ctx, 7)
	require.Empty(t, cache.counts)

	// 无缓存/无账号时静默。
	(&RateLimitService{}).ResetRateLimit429Counter(ctx, 7)
	svc.ResetRateLimit429Counter(ctx, 0)
}

// TestApply429FallbackRateLimit_UsesEscalation 端到端：apply429FallbackRateLimit 采用升级值。
func TestApply429FallbackRateLimit_UsesEscalation(t *testing.T) {
	repo := &rateLimit429RepoStub{}
	cache := newFakeRateLimit429Counter()
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	svc.SetRateLimit429CounterCache(cache)

	account := &Account{ID: 3, Platform: PlatformAnthropic}
	ctx := context.Background()

	// 1st：配置默认值（无 settingService 时 = 5s）
	svc.apply429FallbackRateLimit(ctx, account, "no_reset_time")
	require.Equal(t, 1, repo.calls)
	first := repo.lastUntil
	require.NotNil(t, first)

	// 2nd：升级到 60s
	svc.apply429FallbackRateLimit(ctx, account, "no_reset_time")
	second := repo.lastUntil
	require.NotNil(t, second)
	require.True(t, second.After(first.Add(30*time.Second)),
		"second cooldown should be escalated well beyond the base value: first=%v second=%v", first, second)

	// 3rd：升级到 5min
	svc.apply429FallbackRateLimit(ctx, account, "no_reset_time")
	third := repo.lastUntil
	require.True(t, third.After(time.Now().Add(4*time.Minute)),
		"third cooldown should be ~5min, got %v", time.Until(*third))
}
