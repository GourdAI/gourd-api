package repository

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// errUpstreamGateTimeout 闸门等待超过 upstream_gate_wait_timeout_ms 上限。
//
// 与 context.DeadlineExceeded 分开是为了可诊断：前者意味着「出站槽位被抢完」
// （要查泄漏或调大并发上限），后者意味着请求本身的预算用完了。两者在日志里
// 长得一样时，运维无法判断该改配置还是该查代码。
var errUpstreamGateTimeout = errors.New("upstream gate wait timeout")

// upstreamGlobalGate 是**进程级**出站闸门（最小间隔 + 并发上限）。
//
// 设计动机（防封加固·对齐 anti-api 的两层节奏控制）：
// 账号级 pacer（account_pacer.go）只约束"同一账号"的节奏，它管不到两类事实：
//  1. 跨账号聚合流量。同一个出口 IP 上，N 个账号各自 200ms 间隔，聚合起来
//     对该 IP 就是高频突发 —— 上游风控最常见的封禁维度恰恰是 IP/ASN 而非账号。
//  2. 无账号上下文的出站（换票、OAuth、探针常传 accountID=0），pacer 对它们直接放行。
//
// 与 anti-api 的差异（重要，别照抄）：anti-api 是本地单人代理，可以硬编码
// `Semaphore(1)` + 全局 1000ms（src/lib/rate-limiter.ts:95、rust-proxy MIN_REQUEST_INTERVAL_MS）。
// 本仓是多租户网关，同一种做法会把**整个实例**的吞吐钉在 1 req/s。因此两层都做成
// 可配置且默认关闭（0=禁用），运维按出口 IP 数量与账号规模自行权衡。
//
// 实现要点：
//   - 两项能力都为 0 时 newUpstreamGlobalGate 返回 nil，调用方 nil 分支零开销放行。
//   - 间隔采用"预约槽位"语义（与 accountPacer 一致）：先把 nextAt 推进再等待，
//     并发请求按间隔均匀排开，而不是全部挤在同一时刻被唤醒（唤醒风暴本身是机器特征）。
//   - 并发上限用带缓冲 channel 当信号量；获取与释放都必须尊重 ctx 取消，
//     否则客户端断开会把槽位永久吃掉，最终整个闸门死锁。
//   - 等待顺序：先拿并发槽位，再等最小间隔。反过来的话，已预约的间隔槽位会
//     在长时间排队中被无谓消耗。
type upstreamGlobalGate struct {
	minInterval time.Duration
	jitter      time.Duration
	// maxWait 是排队（抢槽位 + 等间隔）的**独立上限**，0=不限。
	// 默认不限，因为正常请求应该由请求自身预算支配；启用上限只为了一个场景：
	// 出站槽位被某个不归还的调用方长期占用时，把「实例级永久阻塞」降级为
	// 「可观察、可恢复的 5xx」，避免整个网关被拖死且只能重启。
	maxWait time.Duration

	mu     sync.Mutex
	nextAt time.Time

	slots chan struct{}
}

// newUpstreamGlobalGate 创建进程级闸门。两项参数都 <=0 时返回 nil（禁用）。
func newUpstreamGlobalGate(minInterval, jitter time.Duration, maxConcurrent int) *upstreamGlobalGate {
	if minInterval <= 0 && jitter <= 0 && maxConcurrent <= 0 {
		return nil
	}
	if jitter < 0 {
		jitter = 0
	}
	g := &upstreamGlobalGate{
		minInterval: minInterval,
		jitter:      jitter,
	}
	if maxConcurrent > 0 {
		g.slots = make(chan struct{}, maxConcurrent)
	}
	return g
}

// acquire 在出站前排队：先占并发槽位，再等最小间隔。
// 返回的释放函数**恒非 nil**（未启用并发上限时为 no-op），调用方可无条件 defer/调用。
// ctx 取消/超时时返回错误，调用方应按上游失败处理。
func (g *upstreamGlobalGate) acquire(ctx context.Context) (func(), error) {
	if g == nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if g.maxWait <= 0 {
		return g.acquireWithin(ctx)
	}

	// 包一层超时 ctx。注意不能把 waitCtx 交给调用方：它只用于排队阶段，
	// 一旦 cancel，已拿到的槽位仍需由 release 显式归还（两者不绑定）。
	waitCtx, cancel := context.WithTimeout(ctx, g.maxWait)
	defer cancel()

	release, err := g.acquireWithin(waitCtx)
	if err != nil && waitCtx.Err() != nil && ctx.Err() == nil {
		// 排队上限触发，而调用方自己的 ctx 仍然健康：归因给闸门。
		// 判定必须发生在 cancel() 之前（上面 return 表达式中的错误判断早于 defer）。
		// 同时包住 sentinel 与原始错误：两个 %w 使 errors.Is 对
		// errUpstreamGateTimeout 和 context.DeadlineExceeded 都成立，不打破调用方已有判据。
		// （多 %w 需 Go 1.20+，本仓 Go 1.27。）
		return func() {}, fmt.Errorf("%w (upstream gate queued longer than %s): %w", errUpstreamGateTimeout, g.maxWait, err)
	}
	return release, err
}

// acquireWithin 执行实际的「先抢槽位、再等间隔」。
func (g *upstreamGlobalGate) acquireWithin(ctx context.Context) (func(), error) {
	acquired, err := g.acquireSlot(ctx)
	if err != nil {
		return func() {}, err
	}
	release := onceRelease(acquired)
	if err := g.waitInterval(ctx); err != nil {
		release()
		return func() {}, err
	}
	return release, nil
}

// onceRelease 把释放函数包成幂等：重复调用只生效一次。
// 调用方在多条出口（请求出错 / 响应体关闭 / 响应体为 nil 的兜底）重复调用是
// 常见写法，若不幂等会多还一个没持有的槽位，使并发上限形同虚设。
func onceRelease(fn func()) func() {
	if fn == nil {
		return func() {}
	}
	var once sync.Once
	return func() { once.Do(fn) }
}

// acquireSlot 占一个并发槽位；未配置上限时直接返回空释放函数。
func (g *upstreamGlobalGate) acquireSlot(ctx context.Context) (func(), error) {
	if g.slots == nil {
		return nil, nil
	}
	select {
	case g.slots <- struct{}{}:
		return func() { <-g.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// waitInterval 预约并等待全局最小间隔。间隔为 0 时立即返回。
func (g *upstreamGlobalGate) waitInterval(ctx context.Context) error {
	if g.minInterval <= 0 {
		return nil
	}

	now := time.Now()
	step := g.minInterval
	if g.jitter > 0 {
		step += time.Duration(rand.Int63n(int64(g.jitter)))
	}

	g.mu.Lock()
	target := now
	if !g.nextAt.IsZero() && g.nextAt.After(now) {
		target = g.nextAt
	}
	g.nextAt = target.Add(step)
	g.mu.Unlock()

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
