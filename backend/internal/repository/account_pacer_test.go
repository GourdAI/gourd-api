package repository

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"
)

// TestNewAccountPacerDisabled 验证未配置最小间隔时不启用（零行为变更）。
func TestNewAccountPacerDisabled(t *testing.T) {
	if p := newAccountPacer(0, time.Second); p != nil {
		t.Fatalf("minInterval=0 must disable the pacer")
	}
	if p := newAccountPacer(-1, time.Second); p != nil {
		t.Fatalf("negative minInterval must disable the pacer")
	}
}

// TestAccountPacerWaitNoAccount 验证无账号上下文时零开销放行。
func TestAccountPacerWaitNoAccount(t *testing.T) {
	p := newAccountPacer(time.Second, 0)
	if err := p.wait(context.Background(), 0); err != nil {
		t.Fatalf("accountID=0 must pass through, got %v", err)
	}
	if err := p.wait(context.Background(), -1); err != nil {
		t.Fatalf("negative accountID must pass through, got %v", err)
	}
}

// TestAccountPacerFirstRequestPassesImmediately 首请求不等待。
func TestAccountPacerFirstRequestPassesImmediately(t *testing.T) {
	p := newAccountPacer(200*time.Millisecond, 0)
	start := time.Now()
	if err := p.wait(context.Background(), 7); err != nil {
		t.Fatalf("first wait must pass, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("first request should not wait, elapsed=%v", elapsed)
	}
}

// TestAccountPacerSerializesSameAccount 同账号连续请求按最小间隔排开。
func TestAccountPacerSerializesSameAccount(t *testing.T) {
	p := newAccountPacer(150*time.Millisecond, 0)
	ctx := context.Background()

	if err := p.wait(ctx, 1); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	start := time.Now()
	if err := p.wait(ctx, 1); err != nil {
		t.Fatalf("second wait: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 130*time.Millisecond {
		t.Fatalf("second request must wait ~150ms, elapsed=%v", elapsed)
	}
}

// TestAccountPacerAccountsAreIndependent 不同账号互不影响。
func TestAccountPacerAccountsAreIndependent(t *testing.T) {
	p := newAccountPacer(time.Second, 0)
	ctx := context.Background()

	if err := p.wait(ctx, 1); err != nil {
		t.Fatalf("account 1 wait: %v", err)
	}
	start := time.Now()
	if err := p.wait(ctx, 2); err != nil {
		t.Fatalf("account 2 wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("different accounts must not block each other, elapsed=%v", elapsed)
	}
}

// TestAccountPacerContextCancellation 等待期间取消应尽快返回。
func TestAccountPacerContextCancellation(t *testing.T) {
	p := newAccountPacer(time.Second, 0)
	ctx := context.Background()
	if err := p.wait(ctx, 3); err != nil {
		t.Fatalf("first wait: %v", err)
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := p.wait(cancelCtx, 3)
	if err == nil {
		t.Fatalf("cancelled wait must return an error")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("cancellation should abort the wait promptly, elapsed=%v", elapsed)
	}
}

// TestAccountPacerConcurrentSlotsAreSpaced 并发请求按最小间隔均匀预约槽位。
func TestAccountPacerConcurrentSlotsAreSpaced(t *testing.T) {
	const interval = 60 * time.Millisecond
	p := newAccountPacer(interval, 0)
	ctx := context.Background()

	const n = 4
	var wg sync.WaitGroup
	times := make([]time.Time, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			_ = p.wait(ctx, 9)
			times[idx] = time.Now()
		}(i)
	}
	wg.Wait()

	// 排序后逐对断言相邻间隔：槽位预约必须让 4 个请求按 interval 均匀排开。
	// 仅断言「首尾总跨度」无法发现「3 个请求挤在一起、第 4 个独自等到最后」的
	// 退化情形——这正是并发场景下最可能出现的槽位竞态表现。
	sorted := append([]time.Time(nil), times...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Before(sorted[j]) })

	// 下限 = interval 的 80%，容忍 goroutine 调度与 timer 触发精度误差。
	minGap := time.Duration(float64(interval) * 0.8)
	for i := 1; i < len(sorted); i++ {
		if gap := sorted[i].Sub(sorted[i-1]); gap < minGap {
			t.Fatalf("adjacent slots must be spaced by >=%v, gap #%d=%v (sorted=%v)", minGap, i, gap, sorted)
		}
	}

	// 保留原断言：总跨度至少 (n-1)*interval 的 70%（避免 CI 抖动导致误报）。
	spanFactor := 0.7
	minSpan := time.Duration(float64(n-1) * float64(interval) * spanFactor)
	if span := sorted[len(sorted)-1].Sub(sorted[0]); span < minSpan {
		t.Fatalf("concurrent slots must be spaced, span=%v want >=%v", span, minSpan)
	}
}

// TestAccountPacerForget 丢弃账号状态后下一请求立即放行。
func TestAccountPacerForget(t *testing.T) {
	p := newAccountPacer(time.Second, 0)
	ctx := context.Background()
	if err := p.wait(ctx, 5); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	p.forget(5)
	start := time.Now()
	if err := p.wait(ctx, 5); err != nil {
		t.Fatalf("wait after forget: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("forget must reset pacing state, elapsed=%v", elapsed)
	}
}

// TestAccountPacerCanceledSlotStillConsumed 验证被取消的等待仍然消耗槽位（不回滚）。
//
// 语义：wait 在进入等待前就已预约槽位（推进 lastAt），因此 ctx 取消提前返回后，
// 该账号的下一个可发送时点不会回退——取消的请求同样占用一个间隔，避免槽位竞态。
func TestAccountPacerCanceledSlotStillConsumed(t *testing.T) {
	p := newAccountPacer(200*time.Millisecond, 0)
	ctx := context.Background()

	// 第一次 wait：正常 ctx，首请求立即放行并预约槽位。
	if err := p.wait(ctx, 21); err != nil {
		t.Fatalf("first wait: %v", err)
	}

	// 第二次 wait：短超时 ctx，在等待期间被取消而提前返回（槽位不回滚）。
	canceledCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := p.wait(canceledCtx, 21); err == nil {
		t.Fatalf("canceled wait must return an error")
	} else if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("canceled wait should abort promptly, elapsed=%v", elapsed)
	}

	// 第三次 wait：正常 ctx，仍需等待约一个完整间隔，证明被取消的请求消耗了槽位。
	start = time.Now()
	if err := p.wait(ctx, 21); err != nil {
		t.Fatalf("wait after canceled slot: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("canceled request must still consume a slot, elapsed=%v", elapsed)
	}
}

// TestAccountPacerJitterRange 验证抖动后相邻请求间隔落在 [minInterval, minInterval+jitter) 内。
func TestAccountPacerJitterRange(t *testing.T) {
	const (
		minInterval = 100 * time.Millisecond
		jitter      = 100 * time.Millisecond
	)
	p := newAccountPacer(minInterval, jitter)
	ctx := context.Background()

	marks := make([]time.Time, 0, 3)
	for i := 0; i < 3; i++ {
		if err := p.wait(ctx, 33); err != nil {
			t.Fatalf("wait #%d: %v", i+1, err)
		}
		marks = append(marks, time.Now())
	}

	// 下限 90ms 容忍调度/计时器误差；上限 = minInterval + jitter + 20ms 余量。
	const lower = 90 * time.Millisecond
	const upper = minInterval + jitter + 20*time.Millisecond
	for i := 1; i < len(marks); i++ {
		gap := marks[i].Sub(marks[i-1])
		if gap < lower || gap > upper {
			t.Fatalf("gap #%d out of jitter range: got %v, want [%v, %v]", i, gap, lower, upper)
		}
	}
}
