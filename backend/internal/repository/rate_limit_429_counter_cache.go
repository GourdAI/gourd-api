package repository

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const rateLimit429CounterPrefix = "rate_limit_429_count:account:"

// rateLimit429CounterIncrScript 原子递增 429 兜底计数，并在每次递增时续期 TTL，
// 使计数具备滑动窗口语义：连续事件期间计数保持，最后一次事件后 ttl 秒内未再触发则自然过期。
var rateLimit429CounterIncrScript = redis.NewScript(`
	local key = KEYS[1]
	local ttl = tonumber(ARGV[1])

	local count = redis.call('INCR', key)
	redis.call('EXPIRE', key, ttl)

	return count
`)

type rateLimit429CounterCache struct {
	rdb *redis.Client
}

// NewRateLimit429CounterCache 创建 429 兜底计数器缓存（Redis 实现）。
func NewRateLimit429CounterCache(rdb *redis.Client) service.RateLimit429CounterCache {
	return &rateLimit429CounterCache{rdb: rdb}
}

func (c *rateLimit429CounterCache) IncrementRateLimit429Count(ctx context.Context, accountID int64, windowSeconds int) (int64, error) {
	key := fmt.Sprintf("%s%d", rateLimit429CounterPrefix, accountID)

	ttlSeconds := windowSeconds
	if ttlSeconds < 60 {
		ttlSeconds = 60
	}

	result, err := rateLimit429CounterIncrScript.Run(ctx, c.rdb, []string{key}, ttlSeconds).Int64()
	if err != nil {
		return 0, fmt.Errorf("increment rate limit 429 count: %w", err)
	}
	return result, nil
}

func (c *rateLimit429CounterCache) ResetRateLimit429Count(ctx context.Context, accountID int64) error {
	key := fmt.Sprintf("%s%d", rateLimit429CounterPrefix, accountID)
	return c.rdb.Del(ctx, key).Err()
}
