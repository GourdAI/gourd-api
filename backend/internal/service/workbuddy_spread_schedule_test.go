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

// workbuddySpreadZeroWaits 把起点抖动与账号间隔都置 0，让用例不真的等。
func workbuddySpreadZeroWaits(t *testing.T) {
	t.Helper()
	oldJitter, oldGap := workbuddyStartJitterFn, workbuddySpreadGapFn
	workbuddyStartJitterFn = func() time.Duration { return 0 }
	workbuddySpreadGapFn = func(workbuddySpreadSettings) time.Duration { return 0 }
	t.Cleanup(func() {
		workbuddyStartJitterFn, workbuddySpreadGapFn = oldJitter, oldGap
	})
}

// TestWorkbuddySpreadSettingsFromDefaults 缺省配置得到 12h 窗口 + 3~8 分钟区间。
func TestWorkbuddySpreadSettingsFromDefaults(t *testing.T) {
	s := workbuddySpreadSettingsFrom(nil)
	require.Equal(t, 12*time.Hour, s.Window)
	require.Equal(t, 3*time.Minute, s.GapMin)
	require.Equal(t, 8*time.Minute, s.GapMax)

	require.Equal(t, s, workbuddySpreadSettingsFrom(&config.Config{}), "空配置必须与 nil 同结果")
}

// TestWorkbuddySpreadSettingsFromNormalizes 越界与倒置区间被夹逼，绝不产出 0/负间隔。
func TestWorkbuddySpreadSettingsFromNormalizes(t *testing.T) {
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
	settings := workbuddySpreadSettings{GapMin: 3 * time.Minute, GapMax: 8 * time.Minute}
	seen := make(map[time.Duration]struct{})
	for i := 0; i < 200; i++ {
		g := workbuddySpreadGapFn(settings)
		require.GreaterOrEqual(t, g, settings.GapMin)
		require.Less(t, g, settings.GapMax, "上界为开区间（min + randn(span)）")
		seen[g] = struct{}{}
	}
	require.Greater(t, len(seen), 1, "必须是随机区间，不能退化成固定值")

	fixed := workbuddySpreadGapFn(workbuddySpreadSettings{GapMin: 5 * time.Minute, GapMax: 5 * time.Minute})
	require.Equal(t, 5*time.Minute, fixed, "上下界相等时退化为固定间隔")
}

// TestWorkbuddySpreadRunnerSkipsWithoutConsumingGap 已完成的账号不占间隔、不打上游。
func TestWorkbuddySpreadRunnerSkipsWithoutConsumingGap(t *testing.T) {
	workbuddySpreadZeroWaits(t)
	var gapCalls int64
	workbuddySpreadGapFn = func(workbuddySpreadSettings) time.Duration {
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
	runner := workbuddySpreadRunner{label: "activity", window: time.Hour}
	stats := runner.run(context.Background(), nil, accounts, workbuddyActivityAlreadyDoneToday,
		func(_ context.Context, a *Account) workbuddyRunOutcome {
			execd = append(execd, a.ID)
			return workbuddyRunOK
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
	workbuddySpreadZeroWaits(t)
	var n int64
	accounts := make([]*Account, 0, 10)
	for i := 0; i < 10; i++ {
		accounts = append(accounts, &Account{ID: int64(i + 1)})
	}
	// 窗口 = 1 小时是常态值；靠 exec 里的 sleep 把它耗掉不现实，
	// 因此改用短窗口 + exec 内 sleep 超过窗口的方式：第 1 个账号跑完时已过期。
	runner := workbuddySpreadRunner{label: "checkin", window: 60 * time.Millisecond}
	stats := runner.run(context.Background(), nil, accounts, nil,
		func(_ context.Context, _ *Account) workbuddyRunOutcome {
			atomic.AddInt64(&n, 1)
			time.Sleep(80 * time.Millisecond) // 超过整个窗口
			return workbuddyRunOK
		})
	require.Equal(t, int64(1), atomic.LoadInt64(&n), "窗口过期后不得再执行")
	require.Equal(t, 1, stats.Attempted)
	require.Equal(t, 9, stats.Remaining, "剩余账号必须被记账，留给下个窗口")
}

// TestWorkbuddySpreadRunnerStopSignalInterrupts 停机信号立即中断并记 Remaining。
func TestWorkbuddySpreadRunnerStopSignalInterrupts(t *testing.T) {
	workbuddySpreadZeroWaits(t)
	stopCh := make(chan struct{})
	close(stopCh)
	var n int64
	accounts := []*Account{{ID: 1}, {ID: 2}, {ID: 3}}
	runner := workbuddySpreadRunner{label: "travel", window: time.Hour}
	stats := runner.run(context.Background(), stopCh, accounts, nil,
		func(_ context.Context, _ *Account) workbuddyRunOutcome {
			atomic.AddInt64(&n, 1)
			return workbuddyRunOK
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
	oldJitter := workbuddyStartJitterFn
	workbuddyStartJitterFn = func() time.Duration { return 0 }
	t.Cleanup(func() { workbuddyStartJitterFn = oldJitter })

	var gapCalls int64
	oldGap := workbuddySpreadGapFn
	workbuddySpreadGapFn = func(workbuddySpreadSettings) time.Duration {
		atomic.AddInt64(&gapCalls, 1)
		return 0 // 记录调用次数但不等
	}
	t.Cleanup(func() { workbuddySpreadGapFn = oldGap })

	accounts := []*Account{{ID: 1}, {ID: 2}, {ID: 3}}
	runner := workbuddySpreadRunner{label: "checkin", window: time.Hour}
	stats := runner.run(context.Background(), nil, accounts, nil,
		func(_ context.Context, _ *Account) workbuddyRunOutcome { return workbuddyRunOK })
	require.Equal(t, 3, stats.Attempted)
	// 首个账号不等待；第 2、3 个之前各等一次 → 恰好 2 次。
	require.Equal(t, int64(2), atomic.LoadInt64(&gapCalls), "尾部不得额外等待一个间隔")
}
