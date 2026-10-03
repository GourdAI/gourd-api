package service

import (
	"time"
)

// SubscriptionCacheData represents cached subscription data
type SubscriptionCacheData struct {
	Status       string
	ExpiresAt    time.Time
	DailyUsage   float64
	WeeklyUsage  float64
	MonthlyUsage float64
	Version      int64
	// GroupID 是该订阅自身的 group_id（个人订阅=0）。缓存槽位必须按它而不是按
	// 请求分组定位（请求分组可能回退命中个人订阅），因此它必须能过 Redis 往返；
	// 丢了就会让读回的数据 GroupID 恒为 0，下游任何按 GroupID 的判定皆错。
	GroupID int64
}
