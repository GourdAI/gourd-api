package service

import (
	"context"
	"time"
)

// RateLimit429CounterCache 追踪账号在窗口内连续触发「429 无重置时间兜底回避」的次数。
//
// 存在的理由：上游 429 若无法解析出重置时间，现有逻辑只应用一次固定的秒级回避
// （默认 5s）。若账号持续被上游拒绝（例如真实配额耗尽但响应体格式变化、
// 或被风控软限制），5s 回避会让调度器把请求反复打回同一账号——既烧掉 failover
// 预算，又向风控暴露「被拒绝后立即重试」的机器行为。
//
// 参照 anti-api 的分级冷却（60s → 5min → 30min → 2h）：连续兜底事件按次数递增
// 回避时长，成功请求或账号恢复后清零。
type RateLimit429CounterCache interface {
	// IncrementRateLimit429Count 原子递增 429 兜底计数并返回当前值。
	IncrementRateLimit429Count(ctx context.Context, accountID int64, windowSeconds int) (int64, error)
	// ResetRateLimit429Count 成功后清零计数器。
	ResetRateLimit429Count(ctx context.Context, accountID int64) error
}

// 429 兜底回避的分级阈值（连续事件次数 → 回避时长）。
// 第 1 次保持配置的默认回避（默认 5s，容忍瞬时抖动）；
// 第 2 次起按阶梯递增，避免连续重试触发上游风控。
var rateLimit429EscalationTiers = []struct {
	MinCount int
	Cooldown time.Duration
}{
	{MinCount: 2, Cooldown: 60 * time.Second},
	{MinCount: 3, Cooldown: 5 * time.Minute},
	{MinCount: 4, Cooldown: 30 * time.Minute},
	{MinCount: 5, Cooldown: 2 * time.Hour},
}

// rateLimit429EscalationCooldown 返回连续 count 次事件时应采用的分级回避时长。
// count <= 1 返回 0（不升级，使用配置值）。
func rateLimit429EscalationCooldown(count int64) time.Duration {
	var out time.Duration
	for _, tier := range rateLimit429EscalationTiers {
		if count >= int64(tier.MinCount) {
			out = tier.Cooldown
		}
	}
	return out
}

// rateLimit429CounterWindowSeconds 是 429 兜底计数的滑动窗口长度（秒）。
// 实现语义：每次递增都会把 TTL 续期到该窗口长度，连续事件期间计数保持累加；
// 最后一次事件后窗口内未再触发，则计数自然过期（等价于「事件停止后冷却归零」）。
const rateLimit429CounterWindowSeconds = 1800
