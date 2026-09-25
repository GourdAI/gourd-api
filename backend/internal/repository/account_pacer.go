package repository

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

// accountPacer 为每个账号提供「最小请求间隔 + 抖动」的出站节奏控制。
//
// 设计动机（对齐 anti-api 的账号级节流）：上游风控不仅看单个请求的伪装程度，
// 也看请求节奏。同一账号在极短时间内连续发出多个请求（尤其多用户共享订阅账号时），
// 是自动化流量最明显的特征之一。为每个账号维持一个最小发出间隔，可以让请求
// 节奏更接近真实用户的使用模式。
//
// 实现要点：
//   - 无锁热路径：minInterval 为 0 时直接放行（默认关闭，零行为变更）。
//   - 槽位预约：等待逻辑不是「睡到目标时间后再放行」，而是先把 lastAt 推进到
//     目标时间（预约槽位），再等待。这样并发请求会按 minInterval 均匀排开，
//     而不是全部挤在同一时刻。
//   - 抖动：每次等待叠加一个 [0, jitter) 的随机增量，避免多个账号在同一
//     毫秒刻度上发出请求（周期性节拍也是机器特征）。
//   - 上下文取消：等待期间尊重 ctx 取消/超时并提前返回；已预约的槽位不回滚
//     （取消的请求仍占用一个间隔），避免槽位竞态。
//   - 状态增长：lastAt 条目数受进程内出现过的 accountID 数约束（与账号表规模同阶），
//     不做主动清理；账号删除/重建等需要回收槽位状态的场景可调用 forget。
type accountPacer struct {
	minInterval time.Duration
	jitter      time.Duration

	mu     sync.Mutex
	lastAt map[int64]time.Time
}

// newAccountPacer 创建账号节奏控制器。minInterval <= 0 时返回 nil（禁用）。
func newAccountPacer(minInterval, jitter time.Duration) *accountPacer {
	if minInterval <= 0 {
		return nil
	}
	if jitter < 0 {
		jitter = 0
	}
	return &accountPacer{
		minInterval: minInterval,
		jitter:      jitter,
		lastAt:      make(map[int64]time.Time),
	}
}

// wait 在需要时等待，直到该账号到达下一个可发送时点。
// accountID <= 0（无账号上下文）或未启用时直接返回 nil。
func (p *accountPacer) wait(ctx context.Context, accountID int64) error {
	if p == nil || accountID <= 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	now := time.Now()
	extra := time.Duration(0)
	if p.jitter > 0 {
		extra = time.Duration(rand.Int63n(int64(p.jitter)))
	}
	step := p.minInterval + extra

	p.mu.Lock()
	last, ok := p.lastAt[accountID]
	target := now
	if ok {
		target = last.Add(step)
		if !target.After(now) {
			target = now
		}
	}
	// 预约槽位：无论是否真正等待，都把该账号的下一个可发送时点推进到 target。
	// 注意：取消时不回滚槽位（保持实现简单且无竞态）。
	p.lastAt[accountID] = target
	p.mu.Unlock()

	delay := time.Until(target)
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// forget 丢弃指定账号的节奏状态（账号删除/代理切换等场景可选调用）。
//
// 与 lastAt 的增长约束配套：lastAt 条目数受进程内出现过的 accountID 数约束
// （与账号表规模同阶），不做主动清理；账号删除/重建等需要回收槽位状态的场景调用本方法即可。
func (p *accountPacer) forget(accountID int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.lastAt, accountID)
	p.mu.Unlock()
}
