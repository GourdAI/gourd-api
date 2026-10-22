//go:build unit

package service

// workbuddy_spread_schedule_test.go 错峰调度原语 + 「当日已完成跳过」谓词的单测。
//
// 覆盖目标（全部是本次改造引入的新语义，旧测试无法覆盖）：
//  1. 参数归一：缺省/越界/区间倒置不会产出 0 间隔或 0 窗口；
//  2. 间隔取自区间且两账号间不消耗间隔（skip 命中者不占间隔）；
//  3. 窗口用尽后剩余账号被记为 Remaining（下个窗口继续），不是静默丢弃；
//  4. 停机信号立即中断，剩余同样记 Remaining；
//  5. 三个 AlreadyDoneToday 谓词的边界（当日/跨日、成功/失败、traveling 必须继续跑）。

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

// spreadZeroWaits 把起点抖动与账号间隔都置 0，让用例不真的等。
//
// 【纪律】本函数改写进程级可替换函数（spreadStartJitterFn / spreadGapFn）。
// 引用它的用例**一律不得**加 t.Parallel()：所有并行用例共享同一组包级变量，
// 一旦并发就会相互踩踏（某个用例刚置 0，另一个用例又把它改回真实值）。
func spreadZeroWaits(t *testing.T) {
	t.Helper()
	oldJitter, oldGap := spreadStartJitterFn, spreadGapFn
	spreadStartJitterFn = func() time.Duration { return 0 }
	spreadGapFn = func(spreadSettings) time.Duration { return 0 }
	t.Cleanup(func() {
		spreadStartJitterFn, spreadGapFn = oldJitter, oldGap
	})
}

// spreadZeroWaitsFor 把等待置 0 并从 now 开始返回一个永不耗尽的 deadline。
// 用于验证「窗口内实际推进顺序与统计」的用例；窗口耗尽本身由专门用例覆盖。

// TestSpreadSettingsNormalizeDefaults 缺省配置得到 12h 窗口 + 3~8 分钟区间。
func TestSpreadSettingsNormalizeDefaults(t *testing.T) {
	s := normalizeSpreadSettings(spreadTuning{})
	require.Equal(t, 12*time.Hour, s.Window)
	require.Equal(t, 3*time.Minute, s.GapMin)
	require.Equal(t, 8*time.Minute, s.GapMax)
}

// TestSpreadSettingsPerPlatformDefaults 三个平台在空配置下必须得到完全一致的缺省节奏。
//
// 这条用例锁的是「新增平台接入时不能自带另一套缺省值」：三家都是 12h / 3~8min，
// 否则同一时刻仍在跑出不同波形。
func TestSpreadSettingsPerPlatformDefaults(t *testing.T) {
	empty := &config.Config{}
	wb := workbuddySpreadSettingsFrom(empty)
	tr := traeSpreadSettingsFrom(empty)
	qd := qoderSpreadSettingsFrom(empty)
	require.Equal(t, wb, tr, "Trae 缺省节奏必须与 WorkBuddy 一致")
	require.Equal(t, wb, qd, "Qoder 缺省节奏必须与 WorkBuddy 一致")
	require.Equal(t, workbuddySpreadSettingsFrom(nil), wb, "nil 配置必须与空配置同结果")
	require.Equal(t, workbuddySpreadSettingsFrom(nil), traeSpreadSettingsFrom(nil))
	require.Equal(t, workbuddySpreadSettingsFrom(nil), qoderSpreadSettingsFrom(nil))
}

// TestSpreadSettingsPerPlatformHonorsOwnConfig 各平台只读自己的配置键，不得串到别人身上。
//
// 这是复制粘贴最容易出的错：把 traeSpreadSettingsFrom 写成读 cfg.Gateway.Workbuddy。
func TestSpreadSettingsPerPlatformHonorsOwnConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.Trae.SpreadWindowSeconds = 3600
	cfg.Gateway.Trae.AccountGapMinSeconds = 60
	cfg.Gateway.Trae.AccountGapMaxSeconds = 120
	cfg.Gateway.Qoder.SpreadWindowSeconds = 7200
	cfg.Gateway.Qoder.AccountGapMinSeconds = 90
	cfg.Gateway.Qoder.AccountGapMaxSeconds = 150
	// WorkBuddy 保持缺省，用作对照。

	tr := traeSpreadSettingsFrom(cfg)
	require.Equal(t, time.Hour, tr.Window, "Trae 必须读自己的 spread_window_seconds")
	require.Equal(t, time.Minute, tr.GapMin)
	require.Equal(t, 2*time.Minute, tr.GapMax)

	qd := qoderSpreadSettingsFrom(cfg)
	require.Equal(t, 2*time.Hour, qd.Window, "Qoder 必须读自己的 spread_window_seconds")
	require.Equal(t, 90*time.Second, qd.GapMin)
	require.Equal(t, 150*time.Second, qd.GapMax)

	wb := workbuddySpreadSettingsFrom(cfg)
	require.Equal(t, 12*time.Hour, wb.Window, "改 Trae/Qoder 配置不得影响 WorkBuddy")
	require.Equal(t, 3*time.Minute, wb.GapMin)
	require.Equal(t, 8*time.Minute, wb.GapMax)
}

// TestSpreadSettingsFromNormalizes 越界与倒置区间被夹逼，绝不产出 0/负间隔。
func TestSpreadSettingsFromNormalizes(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.Workbuddy.SpreadWindowSeconds = -5
	cfg.Gateway.Workbuddy.AccountGapMinSeconds = 600
	cfg.Gateway.Workbuddy.AccountGapMaxSeconds = 120 // 小于 min
	s := workbuddySpreadSettingsFrom(cfg)
	require.Equal(t, 12*time.Hour, s.Window, "负窗口回落默认")
	require.Equal(t, 10*time.Minute, s.GapMin)
	require.Equal(t, 10*time.Minute, s.GapMax, "max<min 时抬到 min，退化为固定间隔而不是负数")

	cfg.Gateway.Workbuddy.SpreadWindowSeconds = 3600
	cfg.Gateway.Workbuddy.AccountGapMinSeconds = 0
	cfg.Gateway.Workbuddy.AccountGapMaxSeconds = 0
	s = workbuddySpreadSettingsFrom(cfg)
	require.Equal(t, time.Hour, s.Window)
	require.Equal(t, 3*time.Minute, s.GapMin, "0 视为缺省而不是 0 间隔")
	require.Equal(t, 8*time.Minute, s.GapMax)
}

// TestWorkbuddySpreadGapFnStaysInRange 随机间隔恒落在 [min,max) 内且会取到多个不同值。
func TestWorkbuddySpreadGapFnStaysInRange(t *testing.T) {
	settings := spreadSettings{GapMin: 3 * time.Minute, GapMax: 8 * time.Minute}
	seen := make(map[time.Duration]struct{})
	for i := 0; i < 200; i++ {
		g := spreadGapFn(settings)
		require.GreaterOrEqual(t, g, settings.GapMin)
		require.Less(t, g, settings.GapMax, "上界为开区间（min + randn(span)）")
		seen[g] = struct{}{}
	}
	require.Greater(t, len(seen), 1, "必须是随机区间，不能退化成固定值")

	fixed := spreadGapFn(spreadSettings{GapMin: 5 * time.Minute, GapMax: 5 * time.Minute})
	require.Equal(t, 5*time.Minute, fixed, "上下界相等时退化为固定间隔")
}

// TestWorkbuddySpreadRunnerSkipsWithoutConsumingGap 已完成的账号不占间隔、不打上游。
func TestWorkbuddySpreadRunnerSkipsWithoutConsumingGap(t *testing.T) {
	spreadZeroWaits(t)
	var gapCalls int64
	spreadGapFn = func(spreadSettings) time.Duration {
		atomic.AddInt64(&gapCalls, 1)
		return 0
	}
	now := time.Now()
	accounts := []*Account{
		{ID: 1, Extra: map[string]any{workbuddyTasksActivityExtraKey: map[string]any{"date": workbuddyTasksToday(now), "ok": true}}},
		{ID: 2, Extra: map[string]any{workbuddyTasksActivityExtraKey: map[string]any{"date": workbuddyTasksToday(now), "ok": true}}},
		{ID: 3},
	}
	var execd []int64
	runner := spreadRunner{label: "activity", window: time.Hour}
	stats := runner.run(context.Background(), nil, accounts, workbuddyActivityAlreadyDoneToday,
		func(_ context.Context, a *Account) spreadRunOutcome {
			execd = append(execd, a.ID)
			return spreadRunOK
		})
	require.Equal(t, []int64{3}, execd, "两个当日已完成的号必须被跳过")
	require.Equal(t, 3, stats.Candidates)
	require.Equal(t, 2, stats.Already)
	require.Equal(t, 1, stats.Attempted)
	require.Equal(t, 1, stats.OK)
	require.Zero(t, atomic.LoadInt64(&gapCalls), "跳过的号不得消耗账号间隔")
}

// TestWorkbuddySpreadRunnerWindowExhaustedLeavesRemaining 窗口用尽后剩余账号记 Remaining。
//
// 构造必须确定性：用「首个账号的执行本身把窗口耗光」来触发过期检查，
// 而不是把窗口设成 1ns——Windows 时钟精度约 15ms，纳秒级构造会因两次 Now()
// 返回同一值而根本触发不到过期分支（假通过）。
func TestWorkbuddySpreadRunnerWindowExhaustedLeavesRemaining(t *testing.T) {
	spreadZeroWaits(t)
	var n int64
	accounts := make([]*Account, 0, 10)
	for i := 0; i < 10; i++ {
		accounts = append(accounts, &Account{ID: int64(i + 1)})
	}
	// 窗口 = 1 小时是常态值；靠 exec 里的 sleep 把它耗掉不现实，
	// 因此改用短窗口 + exec 内 sleep 超过窗口的方式：第 1 个账号跑完时已过期。
	runner := spreadRunner{label: "checkin", window: 60 * time.Millisecond}
	stats := runner.run(context.Background(), nil, accounts, nil,
		func(_ context.Context, _ *Account) spreadRunOutcome {
			atomic.AddInt64(&n, 1)
			time.Sleep(80 * time.Millisecond) // 超过整个窗口
			return spreadRunOK
		})
	require.Equal(t, int64(1), atomic.LoadInt64(&n), "窗口过期后不得再执行")
	require.Equal(t, 1, stats.Attempted)
	require.Equal(t, 9, stats.Remaining, "剩余账号必须被记账，留给下个窗口")
}

// TestWorkbuddySpreadRunnerStopSignalInterrupts 停机信号立即中断并记 Remaining。
func TestWorkbuddySpreadRunnerStopSignalInterrupts(t *testing.T) {
	spreadZeroWaits(t)
	stopCh := make(chan struct{})
	close(stopCh)
	var n int64
	accounts := []*Account{{ID: 1}, {ID: 2}, {ID: 3}}
	runner := spreadRunner{label: "travel", window: time.Hour}
	stats := runner.run(context.Background(), stopCh, accounts, nil,
		func(_ context.Context, _ *Account) spreadRunOutcome {
			atomic.AddInt64(&n, 1)
			return spreadRunOK
		})
	require.Zero(t, atomic.LoadInt64(&n), "停机后不得再执行任何账号")
	require.Equal(t, 3, stats.Remaining)
}

// TestWorkbuddyCheckinAlreadyDoneToday 签到跳过谓词：只认当日 ok/already。
func TestWorkbuddyCheckinAlreadyDoneToday(t *testing.T) {
	today := timezone.Now().Format("2006-01-02")
	yesterday := timezone.Now().AddDate(0, 0, -1).Format("2006-01-02")

	cases := []struct {
		name    string
		account *Account
		want    bool
	}{
		{"nil 账号", nil, false},
		{"无 Extra", &Account{}, false},
		{"当日 ok", &Account{Extra: map[string]any{workbuddyCheckinExtraKey: map[string]any{"date": today, "status": "ok"}}}, true},
		{"当日 already", &Account{Extra: map[string]any{workbuddyCheckinExtraKey: map[string]any{"date": today, "status": "already"}}}, true},
		{"当日 fail 必须重试", &Account{Extra: map[string]any{workbuddyCheckinExtraKey: map[string]any{"date": today, "status": "fail"}}}, false},
		{"昨日 ok 必须重签", &Account{Extra: map[string]any{workbuddyCheckinExtraKey: map[string]any{"date": yesterday, "status": "ok"}}}, false},
		{"当日 skipped 不落盘等价缺失", &Account{Extra: map[string]any{workbuddyCheckinExtraKey: map[string]any{"date": today, "status": "skipped"}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, workbuddyCheckinAlreadyDoneToday(tc.account))
		})
	}
}

// TestWorkbuddyActivityAlreadyDoneToday 活跃上报跳过谓词：当日 + ok 才跳过。
func TestWorkbuddyActivityAlreadyDoneToday(t *testing.T) {
	today := workbuddyTasksToday(time.Now())
	yesterday := workbuddyTasksToday(time.Now().AddDate(0, 0, -1))

	cases := []struct {
		name    string
		account *Account
		want    bool
	}{
		{"nil 账号", nil, false},
		{"无 Extra", &Account{}, false},
		{"当日成功", &Account{Extra: map[string]any{workbuddyTasksActivityExtraKey: map[string]any{"date": today, "ok": true}}}, true},
		{"当日失败必须重试", &Account{Extra: map[string]any{workbuddyTasksActivityExtraKey: map[string]any{"date": today, "ok": false}}}, false},
		{"昨日成功必须重跑", &Account{Extra: map[string]any{workbuddyTasksActivityExtraKey: map[string]any{"date": yesterday, "ok": true}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, workbuddyActivityAlreadyDoneToday(tc.account))
		})
	}
}

// TestWorkbuddyTravelAlreadyDoneToday 旅行跳过谓词：只跳过「当日已领奖」，
// traveling / fail / 派出上限以外的态都必须继续巡检（否则永远等不到领奖）。
func TestWorkbuddyTravelAlreadyDoneToday(t *testing.T) {
	today := workbuddyTasksToday(time.Now())
	yesterday := workbuddyTasksToday(time.Now().AddDate(0, 0, -1))

	cases := []struct {
		name    string
		account *Account
		want    bool
	}{
		{"nil 账号", nil, false},
		{"无 Extra", &Account{}, false},
		{"当日已领奖", &Account{Extra: map[string]any{workbuddyTasksTravelExtraKey: map[string]any{"date": today, "action": "claim", "ok": true}}}, true},
		{"当日领奖失败必须重试", &Account{Extra: map[string]any{workbuddyTasksTravelExtraKey: map[string]any{"date": today, "action": "claim", "ok": false}}}, false},
		{"当日旅行中必须继续巡检", &Account{Extra: map[string]any{workbuddyTasksTravelExtraKey: map[string]any{"date": today, "action": "skip", "state": "traveling", "ok": false}}}, false},
		{"当日已派出必须继续等领奖", &Account{Extra: map[string]any{workbuddyTasksTravelExtraKey: map[string]any{"date": today, "action": "depart", "ok": true}}}, false},
		{"昨日已领奖必须重跑", &Account{Extra: map[string]any{workbuddyTasksTravelExtraKey: map[string]any{"date": yesterday, "action": "claim", "ok": true}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, workbuddyTravelAlreadyDoneToday(tc.account))
		})
	}
}

// TestWorkbuddySpreadRunnerTailDoesNotWaitAfterLastAccount
// 尾部不得为「最后一个账号之后」白等一个间隔：executed 计数控制的是「下一次执行前」。
func TestWorkbuddySpreadRunnerTailDoesNotWaitAfterLastAccount(t *testing.T) {
	oldJitter := spreadStartJitterFn
	spreadStartJitterFn = func() time.Duration { return 0 }
	t.Cleanup(func() { spreadStartJitterFn = oldJitter })

	var gapCalls int64
	oldGap := spreadGapFn
	spreadGapFn = func(spreadSettings) time.Duration {
		atomic.AddInt64(&gapCalls, 1)
		return 0 // 记录调用次数但不等
	}
	t.Cleanup(func() { spreadGapFn = oldGap })

	accounts := []*Account{{ID: 1}, {ID: 2}, {ID: 3}}
	runner := spreadRunner{label: "checkin", window: time.Hour}
	stats := runner.run(context.Background(), nil, accounts, nil,
		func(_ context.Context, _ *Account) spreadRunOutcome { return spreadRunOK })
	require.Equal(t, 3, stats.Attempted)
	// 首个账号不等待；第 2、3 个之前各等一次 → 恰好 2 次。
	require.Equal(t, int64(2), atomic.LoadInt64(&gapCalls), "尾部不得额外等待一个间隔")
}

// ---------------------------------------------------------------------------
// 时序纪律：窗口不得跨过下一个触发点（否则兜底窗口被静默吞掉）
// ---------------------------------------------------------------------------

// TestSpreadEffectiveWindowNeverReachesNextTrigger 窗口必须夹到触发间隔以内。
//
// 【这是本次发现的真实缺陷】改造前 loop 是串行的：runOnce 跑满一个窗口才回到
// 等待，然后以 now 重算下一个触发点。默认 [9,21] 的间隔恰好 12h、窗口也 12h，
// 于是 9 点窗口会一直跑到 21:00:00 之后，重算时 21 点已过 → 顺延到次日 9 点，
// **21 点兜底窗口被永久跳过**，与「晚上兜底只补没跑成的」这个需求直接冲突。
func TestSpreadEffectiveWindowNeverReachesNextTrigger(t *testing.T) {
	cases := []struct {
		name   string
		window time.Duration
		hours  []int
		// 断言：返回窗口 + 余量 <= 最小相邻触发间隔
		minGapHours int
	}{
		{"Trae/WB 签到 [9,21] 12h 窗口", 12 * time.Hour, []int{9, 21}, 12},
		{"Qoder [10,21] 12h 窗口", 12 * time.Hour, []int{10, 21}, 11},
		{"WB 活跃 [10] 单点 12h 窗口", 12 * time.Hour, []int{10}, 24},
		{"WB 旅行 [9,21] 12h 窗口", 12 * time.Hour, []int{9, 21}, 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := spreadEffectiveWindow(tc.window, tc.hours)
			gap := time.Duration(tc.minGapHours) * time.Hour
			require.LessOrEqual(t, got+spreadFireGapMargin, gap,
				"窗口 + 余量必须 <= 相邻触发点最小间隔，否则会跑过下一个触发点")
			require.Greater(t, got, time.Duration(0), "窗口恒为正")
		})
	}
}

// TestSpreadEffectiveWindowShrinksShorterIntervals 密集触发小时也要正确夹逼。
//
// [9,12] 间隔只有 3h：窗口必须缩到 3h - 余量，而不是继续用 12h。
func TestSpreadEffectiveWindowShrinksShorterIntervals(t *testing.T) {
	got := spreadEffectiveWindow(12*time.Hour, []int{9, 12})
	require.Equal(t, 3*time.Hour-spreadFireGapMargin, got,
		"窗口必须缩到最小相邻间隔 - 余量")
}

// TestSpreadEffectiveWindowDenseHoursFallBackToHalfGap 触发间隔小于余量时退化一半。
//
// [9,9] 这类同点（归一后实际只剩一个）与极端密集（[0,1] 等）不得产出负窗口。
func TestSpreadEffectiveWindowDenseHoursFallBackToHalfGap(t *testing.T) {
	got := spreadEffectiveWindow(time.Hour, []int{0, 1})
	require.Equal(t, time.Hour-spreadFireGapMargin, got, "间隔 1h 减去 5min 余量")
	require.Greater(t, got, time.Duration(0))

	// 重复小时（归一后不应出现）不得产出 0 窗口：0 窗口会让 runner 回退到缺省 12h，
	// 重新引入「窗口跨过下一个触发点」的缺陷。
	dup := spreadEffectiveWindow(12*time.Hour, []int{9, 9})
	require.Greater(t, dup, time.Duration(0), "重复小时不得产出 0 窗口")
	require.GreaterOrEqual(t, dup, time.Minute, "退化为分钟级下限")

	// 空列表不得 panic，且回到缺省语义。
	require.Greater(t, spreadEffectiveWindow(2*time.Hour, nil), time.Duration(0))
}

// TestSpreadTriggerLoopServesEveryTriggerOnce 核心时序行为：每个触发点恰好服务一次。
//
// 构造方式：把触发点设成「刚过去的新鲜时刻」，并让 run 本身耗时跨过下一个点，
// 验证用 served 基准重算时不会漏掉任何一个触发点。
//
// 由于 spreadTriggerLoop 内部用真实时钟，这里用极短路径：hours 只有一个点且
// 该点刚过 → nextSpreadFire 会顺延到次日，wait 很长，故改用 stopCh 立即中断，
// 只验证「不 panic + 停机立即返回」。真正的跨窗口覆盖由
// TestSpreadTriggerLoopDoesNotSkipAfterLongRun 覆盖。
func TestSpreadTriggerLoopStopsImmediately(t *testing.T) {
	stopCh := make(chan struct{})
	close(stopCh)
	called := 0
	spreadTriggerLoop(stopCh, []int{9, 21}, time.Hour, "test", func() { called++ })
	require.Zero(t, called, "已停机的 loop 不得执行任何一批")
}

// TestSpreadTriggerLoopDoesNotSkipAfterLongRun 驱动真实 loop，证明长窗口后不跳过触发点。
//
// 【这是缺陷的直接回归用例】用假时钟 + 在 run 内把时钟推进 12h（模拟一个跑满的
// 窗口），验证 loop 仍然服务 21 点，而不是顺延到次日 9 点。
//
// 若把 nextSpreadFire(served, ...) 改回 nextSpreadFire(time.Now(), ...)，
// 本用例会得到 9:00 而非 21:00，当场 FAIL。
func TestSpreadTriggerLoopDoesNotSkipAfterLongRun(t *testing.T) {
	loc := time.Local
	// 假时钟：起点 9:00:00 前一点点，让首个触发点是当日 9:00。
	clock := time.Date(2026, 10, 22, 8, 59, 59, 0, loc)
	oldClock := spreadClock
	spreadClock = func() time.Time { return clock }
	t.Cleanup(func() { spreadClock = oldClock })

	stopCh := make(chan struct{})
	var fired []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		spreadTriggerLoop(stopCh, []int{9, 21}, 12*time.Hour, "test", func() {
			// 记录「本次服务的是哪个触发点」：用时钟对齐到当前小时来表示。
			// 不能用 clock.Format：run 在 trigger 时刻被调用，但时钟可能已经被
			// 上一次 run 推进过，这里只关心「服务了 9 点还是 21 点」这个事实。
			fired = append(fired, clock.Round(time.Hour).Format("15:04"))
			if len(fired) >= 2 {
				close(stopCh) // 收满两次就停机，避免真的等
				return
			}
			// 模拟「窗口跑满 12 小时」：时钟直接推进到 21:00:01（已过 21 点 1 秒）。
			clock = time.Date(2026, 10, 22, 21, 0, 1, 0, loc)
		})
	}()

	// 超时保护：若基准退化成 time.Now()，loop 会去等次日 9:00（约 12 小时），
	// 这里 3 秒内收不到两次就直接判失败，避免用例挂死。
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("loop 未在 3 秒内服务第二个触发点：基准很可能退回了 time.Now()，" +
			"导致 21 点兜底被跳过、去等次日 9 点")
	}

	require.Equal(t, []string{"09:00", "21:00"}, fired,
		"跑满 12h 的窗口后，loop 必须仍服务 21 点兜底，而不是顺延到次日 9 点")
}

// TestSpreadNextFireSharedAcrossPlatforms 四个 next*Fire 必须同源（同一语义）。
func TestSpreadNextFireSharedAcrossPlatforms(t *testing.T) {
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.Local)
	want := nextSpreadFire(at, []int{9, 21})
	require.Equal(t, want, nextTraeCheckinFire(at, []int{9, 21}))
	require.Equal(t, want, nextWorkbuddyCheckinFire(at, []int{9, 21}))
	require.Equal(t, want, nextWorkbuddyTasksFire(at, []int{9, 21}))

	qWant := nextSpreadFire(at, []int{10, 21})
	require.Equal(t, qWant, nextQoderCheckinFire(at, []int{10, 21}))
}
