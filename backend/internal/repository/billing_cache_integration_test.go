//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

type BillingCacheSuite struct {
	IntegrationRedisSuite
}

func (s *BillingCacheSuite) TestUserBalance() {
	tests := []struct {
		name string
		fn   func(ctx context.Context, rdb *redis.Client, cache service.BillingCache)
	}{
		{
			name: "missing_key_returns_redis_nil",
			fn: func(ctx context.Context, rdb *redis.Client, cache service.BillingCache) {
				_, err := cache.GetUserBalance(ctx, 1)
				require.ErrorIs(s.T(), err, redis.Nil, "expected redis.Nil for missing balance key")
			},
		},
		{
			name: "deduct_on_nonexistent_is_noop",
			fn: func(ctx context.Context, rdb *redis.Client, cache service.BillingCache) {
				userID := int64(1)
				balanceKey := fmt.Sprintf("%s%d", billingBalanceKeyPrefix, userID)

				require.NoError(s.T(), cache.DeductUserBalance(ctx, userID, 1), "DeductUserBalance should not error")

				_, err := rdb.Get(ctx, balanceKey).Result()
				require.ErrorIs(s.T(), err, redis.Nil, "expected missing key after deduct on non-existent")
			},
		},
		{
			name: "set_and_get_with_ttl",
			fn: func(ctx context.Context, rdb *redis.Client, cache service.BillingCache) {
				userID := int64(2)
				balanceKey := fmt.Sprintf("%s%d", billingBalanceKeyPrefix, userID)

				require.NoError(s.T(), cache.SetUserBalance(ctx, userID, 10.5), "SetUserBalance")

				got, err := cache.GetUserBalance(ctx, userID)
				require.NoError(s.T(), err, "GetUserBalance")
				require.Equal(s.T(), 10.5, got, "balance mismatch")

				ttl, err := rdb.TTL(ctx, balanceKey).Result()
				require.NoError(s.T(), err, "TTL")
				s.AssertTTLWithin(ttl, 1*time.Second, billingCacheTTL)
			},
		},
		{
			name: "deduct_reduces_balance",
			fn: func(ctx context.Context, rdb *redis.Client, cache service.BillingCache) {
				userID := int64(3)

				require.NoError(s.T(), cache.SetUserBalance(ctx, userID, 10.5), "SetUserBalance")
				require.NoError(s.T(), cache.DeductUserBalance(ctx, userID, 2.25), "DeductUserBalance")

				got, err := cache.GetUserBalance(ctx, userID)
				require.NoError(s.T(), err, "GetUserBalance after deduct")
				require.Equal(s.T(), 8.25, got, "deduct mismatch")
			},
		},
		{
			name: "invalidate_removes_key",
			fn: func(ctx context.Context, rdb *redis.Client, cache service.BillingCache) {
				userID := int64(100)
				balanceKey := fmt.Sprintf("%s%d", billingBalanceKeyPrefix, userID)

				require.NoError(s.T(), cache.SetUserBalance(ctx, userID, 50.0), "SetUserBalance")

				exists, err := rdb.Exists(ctx, balanceKey).Result()
				require.NoError(s.T(), err, "Exists")
				require.Equal(s.T(), int64(1), exists, "expected balance key to exist")

				require.NoError(s.T(), cache.InvalidateUserBalance(ctx, userID), "InvalidateUserBalance")

				exists, err = rdb.Exists(ctx, balanceKey).Result()
				require.NoError(s.T(), err, "Exists after invalidate")
				require.Equal(s.T(), int64(0), exists, "expected balance key to be removed after invalidate")

				_, err = cache.GetUserBalance(ctx, userID)
				require.ErrorIs(s.T(), err, redis.Nil, "expected redis.Nil after invalidate")
			},
		},
		{
			name: "deduct_refreshes_ttl",
			fn: func(ctx context.Context, rdb *redis.Client, cache service.BillingCache) {
				userID := int64(103)
				balanceKey := fmt.Sprintf("%s%d", billingBalanceKeyPrefix, userID)

				require.NoError(s.T(), cache.SetUserBalance(ctx, userID, 100.0), "SetUserBalance")

				ttl1, err := rdb.TTL(ctx, balanceKey).Result()
				require.NoError(s.T(), err, "TTL before deduct")
				s.AssertTTLWithin(ttl1, 1*time.Second, billingCacheTTL)

				require.NoError(s.T(), cache.DeductUserBalance(ctx, userID, 25.0), "DeductUserBalance")

				balance, err := cache.GetUserBalance(ctx, userID)
				require.NoError(s.T(), err, "GetUserBalance")
				require.Equal(s.T(), 75.0, balance, "expected balance 75.0")

				ttl2, err := rdb.TTL(ctx, balanceKey).Result()
				require.NoError(s.T(), err, "TTL after deduct")
				s.AssertTTLWithin(ttl2, 1*time.Second, billingCacheTTL)
			},
		},
	}

	for _, tt := range tests {
		s.Run(tt.name, func() {
			rdb := testRedis(s.T())
			cache := NewBillingCache(rdb)
			ctx := context.Background()

			tt.fn(ctx, rdb, cache)
		})
	}
}

func (s *BillingCacheSuite) TestSubscriptionCache() {
	tests := []struct {
		name string
		fn   func(ctx context.Context, rdb *redis.Client, cache service.BillingCache)
	}{
		{
			name: "missing_key_returns_redis_nil",
			fn: func(ctx context.Context, rdb *redis.Client, cache service.BillingCache) {
				userID := int64(10)

				_, err := cache.GetSubscriptionCache(ctx, userID)
				require.ErrorIs(s.T(), err, redis.Nil, "expected redis.Nil for missing subscription key")
			},
		},
		{
			name: "update_usage_on_nonexistent_is_noop",
			fn: func(ctx context.Context, rdb *redis.Client, cache service.BillingCache) {
				userID := int64(11)
				subKey := fmt.Sprintf("%s%d", billingSubKeyPrefix, userID)

				require.NoError(s.T(), cache.UpdateSubscriptionUsage(ctx, userID, 1.0), "UpdateSubscriptionUsage should not error")

				exists, err := rdb.Exists(ctx, subKey).Result()
				require.NoError(s.T(), err, "Exists")
				require.Equal(s.T(), int64(0), exists, "expected missing subscription key after UpdateSubscriptionUsage on non-existent")
			},
		},
		{
			name: "set_and_get_with_ttl",
			fn: func(ctx context.Context, rdb *redis.Client, cache service.BillingCache) {
				userID := int64(12)
				subKey := fmt.Sprintf("%s%d", billingSubKeyPrefix, userID)

				data := &service.SubscriptionCacheData{
					Status:       "active",
					ExpiresAt:    time.Now().Add(1 * time.Hour),
					TotalLimit:   100.0,
					TotalUsage:   3.0,
					HasUnlimited: false,
					Version:      7,
				}
				require.NoError(s.T(), cache.SetSubscriptionCache(ctx, userID, data), "SetSubscriptionCache")

				gotSub, err := cache.GetSubscriptionCache(ctx, userID)
				require.NoError(s.T(), err, "GetSubscriptionCache")
				require.Equal(s.T(), "active", gotSub.Status)
				require.Equal(s.T(), int64(7), gotSub.Version)
				require.Equal(s.T(), 100.0, gotSub.TotalLimit)
				require.Equal(s.T(), 3.0, gotSub.TotalUsage)
				require.False(s.T(), gotSub.HasUnlimited)

				ttl, err := rdb.TTL(ctx, subKey).Result()
				require.NoError(s.T(), err, "TTL subKey")
				s.AssertTTLWithin(ttl, 1*time.Second, billingCacheTTL)
			},
		},
		{
			name: "set_and_get_preserves_has_unlimited",
			fn: func(ctx context.Context, rdb *redis.Client, cache service.BillingCache) {
				userID := int64(14)

				data := &service.SubscriptionCacheData{
					Status:       "active",
					ExpiresAt:    time.Now().Add(1 * time.Hour),
					TotalLimit:   50.0,
					TotalUsage:   10.0,
					HasUnlimited: true,
					Version:      2,
				}
				require.NoError(s.T(), cache.SetSubscriptionCache(ctx, userID, data), "SetSubscriptionCache")

				gotSub, err := cache.GetSubscriptionCache(ctx, userID)
				require.NoError(s.T(), err, "GetSubscriptionCache")
				require.True(s.T(), gotSub.HasUnlimited, "不限额标记应在往返读取后保留")
				require.Equal(s.T(), 50.0, gotSub.TotalLimit)
				require.Equal(s.T(), 10.0, gotSub.TotalUsage)
			},
		},
		{
			name: "update_usage_increments_total_usage",
			fn: func(ctx context.Context, rdb *redis.Client, cache service.BillingCache) {
				userID := int64(13)

				data := &service.SubscriptionCacheData{
					Status:     "active",
					ExpiresAt:  time.Now().Add(1 * time.Hour),
					TotalLimit: 10.0,
					TotalUsage: 3.0,
					Version:    1,
				}
				require.NoError(s.T(), cache.SetSubscriptionCache(ctx, userID, data), "SetSubscriptionCache")

				require.NoError(s.T(), cache.UpdateSubscriptionUsage(ctx, userID, 0.5), "UpdateSubscriptionUsage")

				gotSub, err := cache.GetSubscriptionCache(ctx, userID)
				require.NoError(s.T(), err, "GetSubscriptionCache after update")
				// 总额池只有一个累加项：用量累加，额度不变。
				require.Equal(s.T(), 3.5, gotSub.TotalUsage)
				require.Equal(s.T(), 10.0, gotSub.TotalLimit)
			},
		},
		{
			name: "invalidate_removes_key",
			fn: func(ctx context.Context, rdb *redis.Client, cache service.BillingCache) {
				userID := int64(101)
				subKey := fmt.Sprintf("%s%d", billingSubKeyPrefix, userID)

				data := &service.SubscriptionCacheData{
					Status:     "active",
					ExpiresAt:  time.Now().Add(1 * time.Hour),
					TotalLimit: 10.0,
					TotalUsage: 1.0,
					Version:    1,
				}
				require.NoError(s.T(), cache.SetSubscriptionCache(ctx, userID, data), "SetSubscriptionCache")

				exists, err := rdb.Exists(ctx, subKey).Result()
				require.NoError(s.T(), err, "Exists")
				require.Equal(s.T(), int64(1), exists, "expected subscription key to exist")

				require.NoError(s.T(), cache.InvalidateSubscriptionCache(ctx, userID), "InvalidateSubscriptionCache")

				exists, err = rdb.Exists(ctx, subKey).Result()
				require.NoError(s.T(), err, "Exists after invalidate")
				require.Equal(s.T(), int64(0), exists, "expected subscription key to be removed after invalidate")

				_, err = cache.GetSubscriptionCache(ctx, userID)
				require.ErrorIs(s.T(), err, redis.Nil, "expected redis.Nil after invalidate")
			},
		},
		{
			name: "missing_status_returns_parsing_error",
			fn: func(ctx context.Context, rdb *redis.Client, cache service.BillingCache) {
				userID := int64(102)
				subKey := fmt.Sprintf("%s%d", billingSubKeyPrefix, userID)

				fields := map[string]any{
					"expires_at":  time.Now().Add(1 * time.Hour).Unix(),
					"total_limit": 10.0,
					"total_usage": 1.0,
					"version":     1,
				}
				require.NoError(s.T(), rdb.HSet(ctx, subKey, fields).Err(), "HSet")

				_, err := cache.GetSubscriptionCache(ctx, userID)
				require.Error(s.T(), err, "expected error for missing status field")
				require.NotErrorIs(s.T(), err, redis.Nil, "expected parsing error, not redis.Nil")
				require.Equal(s.T(), "invalid cache: missing status", err.Error())
			},
		},
	}

	for _, tt := range tests {
		s.Run(tt.name, func() {
			rdb := testRedis(s.T())
			cache := NewBillingCache(rdb)
			ctx := context.Background()

			tt.fn(ctx, rdb, cache)
		})
	}
}

// TestDeductUserBalance_ErrorPropagation 验证 P2-12 修复：
// Redis 真实错误应传播，key 不存在（redis.Nil）应返回 nil。
func (s *BillingCacheSuite) TestDeductUserBalance_ErrorPropagation() {
	tests := []struct {
		name      string
		fn        func(ctx context.Context, cache service.BillingCache)
		expectErr bool
	}{
		{
			name: "key_not_exists_returns_nil",
			fn: func(ctx context.Context, cache service.BillingCache) {
				// key 不存在时，Lua 脚本返回 0（redis.Nil），应返回 nil 而非错误
				err := cache.DeductUserBalance(ctx, 99999, 1.0)
				require.NoError(s.T(), err, "DeductUserBalance on non-existent key should return nil")
			},
		},
		{
			name: "existing_key_deducts_successfully",
			fn: func(ctx context.Context, cache service.BillingCache) {
				require.NoError(s.T(), cache.SetUserBalance(ctx, 200, 50.0))
				err := cache.DeductUserBalance(ctx, 200, 10.0)
				require.NoError(s.T(), err, "DeductUserBalance should succeed")

				bal, err := cache.GetUserBalance(ctx, 200)
				require.NoError(s.T(), err)
				require.Equal(s.T(), 40.0, bal, "余额应为 40.0")
			},
		},
		{
			name: "cancelled_context_propagates_error",
			fn: func(ctx context.Context, cache service.BillingCache) {
				require.NoError(s.T(), cache.SetUserBalance(ctx, 201, 50.0))

				cancelCtx, cancel := context.WithCancel(ctx)
				cancel() // 立即取消

				err := cache.DeductUserBalance(cancelCtx, 201, 10.0)
				require.Error(s.T(), err, "cancelled context should propagate error")
			},
		},
	}

	for _, tt := range tests {
		s.Run(tt.name, func() {
			rdb := testRedis(s.T())
			cache := NewBillingCache(rdb)
			ctx := context.Background()
			tt.fn(ctx, cache)
		})
	}
}

// TestUpdateSubscriptionUsage_ErrorPropagation 验证 P2-12 修复：
// Redis 真实错误应传播，key 不存在（redis.Nil）应返回 nil。
func (s *BillingCacheSuite) TestUpdateSubscriptionUsage_ErrorPropagation() {
	s.Run("key_not_exists_returns_nil", func() {
		rdb := testRedis(s.T())
		cache := NewBillingCache(rdb)
		ctx := context.Background()

		err := cache.UpdateSubscriptionUsage(ctx, 88888, 1.0)
		require.NoError(s.T(), err, "UpdateSubscriptionUsage on non-existent key should return nil")
	})

	s.Run("cancelled_context_propagates_error", func() {
		rdb := testRedis(s.T())
		cache := NewBillingCache(rdb)
		ctx := context.Background()

		data := &service.SubscriptionCacheData{
			Status:     "active",
			ExpiresAt:  time.Now().Add(1 * time.Hour),
			TotalLimit: 10.0,
			TotalUsage: 1.0,
			Version:    1,
		}
		require.NoError(s.T(), cache.SetSubscriptionCache(ctx, 301, data))

		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()

		err := cache.UpdateSubscriptionUsage(cancelCtx, 301, 1.0)
		require.Error(s.T(), err, "cancelled context should propagate error")
	})
}

func TestBillingCacheSuite(t *testing.T) {
	suite.Run(t, new(BillingCacheSuite))
}
