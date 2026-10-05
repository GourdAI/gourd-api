package service

import (
	"time"
)

// SubscriptionCacheData 是「用户订阅钱包」的缓存投影，按 user 单键存储。
//
// 为什么可以聚合成一份而不是每份订阅一个键：
// 跨订阅拆分消耗（AllocateSubscriptionUsage）在「准入判定」上与聚合等价——
// 只要所有有限额订阅都可自由消耗（先到期先消耗、允许单笔拆跨），则
//
//	Σ 可花金额 = Σ total_limit_usd − Σ total_usage_usd
//
// 因此缓存只需保存两个求和项即可复现判定结果，无需每份订阅一条键。
// 记账侧仍然逐订阅写入（IncrementUsage 按 subscription_id 累加），
// 聚合只发生在「够不够」这一次判断上。
type SubscriptionCacheData struct {
	// Status 钱包整体状态：至少一份生效订阅为 active，否则 expired/none。
	Status string
	// ExpiresAt 最晚到期时刻：代表钱包的最后可用时间。
	ExpiresAt time.Time
	// TotalLimit 所有有限额订阅的额度之和；0 表示没有有限额钱包。
	TotalLimit float64
	// TotalUsage 上述订阅的已用之和。
	TotalUsage float64
	// HasUnlimited 是否另持有不限额订阅；为 true 时额度判定直接放行。
	HasUnlimited bool
	Version      int64
}
