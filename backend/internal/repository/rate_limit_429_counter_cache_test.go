package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// newRateLimit429CounterCacheTest 构造走 miniredis 的 429 兜底计数器缓存。
// 与包内其它 miniredis 用例（leader_lock_cache_test.go 等）保持同一初始化方式。
func newRateLimit429CounterCacheTest(t *testing.T) (*rateLimit429CounterCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &rateLimit429CounterCache{rdb: rdb}, mr
}

// rateLimit429CounterTestKey 复刻实现中的 key 规则，用于直接断言 Redis 侧状态。
func rateLimit429CounterTestKey(accountID int64) string {
	return fmt.Sprintf("%s%d", rateLimit429CounterPrefix, accountID)
}

// rateLimit429CounterTestTTL 读取键剩余 TTL（秒）。键不存在返回 -2、无过期返回 -1。
func rateLimit429CounterTestTTL(t *testing.T, c *rateLimit429CounterCache, accountID int64) int64 {
	t.Helper()
	ttl, err := c.rdb.TTL(context.Background(), rateLimit429CounterTestKey(accountID)).Result()
	require.NoError(t, err)
	return int64(ttl.Seconds())
}

// TestRateLimit429CounterCacheIncrements 首次 INCR 返回 1，之后逐次递增。
func TestRateLimit429CounterCacheIncrements(t *testing.T) {
	cache, _ := newRateLimit429CounterCacheTest(t)
	ctx := context.Background()
	const windowSeconds = 1800

	first, err := cache.IncrementRateLimit429Count(ctx, 11, windowSeconds)
	require.NoError(t, err)
	require.Equal(t, int64(1), first, "first increment must start the window at 1")

	for want := int64(2); want <= 5; want++ {
		got, err := cache.IncrementRateLimit429Count(ctx, 11, windowSeconds)
		require.NoError(t, err)
		require.Equal(t, want, got, "increment #%d must return the running count", want)
	}

	// 不同账号的计数相互独立。
	other, err := cache.IncrementRateLimit429Count(ctx, 12, windowSeconds)
	require.NoError(t, err)
	require.Equal(t, int64(1), other, "counters must be per-account")
}

// TestRateLimit429CounterCacheTTLRefreshed 验证滑动窗口语义：每次递增都续期 TTL，
// 而不是只保留首次 INCR 时的剩余时间。
func TestRateLimit429CounterCacheTTLRefreshed(t *testing.T) {
	cache, mr := newRateLimit429CounterCacheTest(t)
	ctx := context.Background()
	const (
		accountID     int64 = 21
		windowSeconds       = 1800
	)

	_, err := cache.IncrementRateLimit429Count(ctx, accountID, windowSeconds)
	require.NoError(t, err)
	require.Equal(t, int64(windowSeconds), rateLimit429CounterTestTTL(t, cache, accountID),
		"first increment must set the full window TTL")

	// 时间前进 1700s：固定窗口实现下剩余 TTL 将只有 100s。
	mr.FastForward(1700 * time.Second)
	require.Equal(t, int64(windowSeconds-1700), rateLimit429CounterTestTTL(t, cache, accountID),
		"TTL must count down between events")

	count, err := cache.IncrementRateLimit429Count(ctx, accountID, windowSeconds)
	require.NoError(t, err)
	require.Equal(t, int64(2), count, "second event must accumulate within the window")

	// 关键断言：TTL 被重新拉满，而不是残留 100s。
	require.Equal(t, int64(windowSeconds), rateLimit429CounterTestTTL(t, cache, accountID),
		"each increment must refresh the TTL (sliding window)")
}

// TestRateLimit429CounterCacheExpiresAfterWindow 窗口内无新事件则计数自然过期，重新从 1 开始。
func TestRateLimit429CounterCacheExpiresAfterWindow(t *testing.T) {
	cache, mr := newRateLimit429CounterCacheTest(t)
	ctx := context.Background()
	const (
		accountID     int64 = 31
		windowSeconds       = 1800
	)

	_, err := cache.IncrementRateLimit429Count(ctx, accountID, windowSeconds)
	require.NoError(t, err)
	_, err = cache.IncrementRateLimit429Count(ctx, accountID, windowSeconds)
	require.NoError(t, err)

	mr.FastForward(1801 * time.Second)

	got, err := cache.IncrementRateLimit429Count(ctx, accountID, windowSeconds)
	require.NoError(t, err)
	require.Equal(t, int64(1), got, "counter must restart at 1 once the window has elapsed")
	require.Equal(t, int64(windowSeconds), rateLimit429CounterTestTTL(t, cache, accountID),
		"restarted counter must carry a fresh full-window TTL")
}

// TestRateLimit429CounterCacheReset 成功后清零：Reset 后的下一次 INCR 回到 1。
func TestRateLimit429CounterCacheReset(t *testing.T) {
	cache, mr := newRateLimit429CounterCacheTest(t)
	ctx := context.Background()
	const (
		accountID     int64 = 41
		windowSeconds       = 1800
	)

	_, err := cache.IncrementRateLimit429Count(ctx, accountID, windowSeconds)
	require.NoError(t, err)
	_, err = cache.IncrementRateLimit429Count(ctx, accountID, windowSeconds)
	require.NoError(t, err)

	require.NoError(t, cache.ResetRateLimit429Count(ctx, accountID))
	require.False(t, mr.Exists(rateLimit429CounterTestKey(accountID)), "reset must delete the counter key")

	got, err := cache.IncrementRateLimit429Count(ctx, accountID, windowSeconds)
	require.NoError(t, err)
	require.Equal(t, int64(1), got, "counter must restart at 1 after reset")
}

// TestRateLimit429CounterCacheMinTTLClamp 短窗口被钳到 60s 下限，避免计数过早过期导致分级冷却失效。
func TestRateLimit429CounterCacheMinTTLClamp(t *testing.T) {
	cache, _ := newRateLimit429CounterCacheTest(t)
	ctx := context.Background()
	const accountID int64 = 51

	_, err := cache.IncrementRateLimit429Count(ctx, accountID, 10)
	require.NoError(t, err)
	require.Equal(t, int64(60), rateLimit429CounterTestTTL(t, cache, accountID),
		"windowSeconds=10 must be clamped to the 60s floor")

	// 恰好等于下限时不钳制。
	_, err = cache.IncrementRateLimit429Count(ctx, accountID, 60)
	require.NoError(t, err)
	require.Equal(t, int64(60), rateLimit429CounterTestTTL(t, cache, accountID))

	// 大于下限时按实际值续期。
	_, err = cache.IncrementRateLimit429Count(ctx, accountID, 120)
	require.NoError(t, err)
	require.Equal(t, int64(120), rateLimit429CounterTestTTL(t, cache, accountID),
		"windows above the floor must be used as-is")
}
