package service

// workbuddy_spread_schedule.go WorkBuddy 三类周期任务（签到 / 活跃上报 / 猫猫旅行）
// 共用的「窗口内逐号推进」调度原语。
//
// 旧模型（改造前）：整点全量齐发，账号间固定 2s。两个已被代码证实的问题：
//  1. 整点准时齐发 + 固定间隔本身就是可识别的机械节律，账号一多即典型批量特征；
//  2. 「晚上兜底」那趟会把当天**已成功**的账号再全部重打一遍上游——签到只靠上游
//     幂等返回「今天已签到」，旅行先打 buddy/info + travel/status 两个 GET，活跃
//     上报把 N 条上报整组重发。幂等只保证了不重复发钱，没保证不重复暴露请求。
//
// 新模型：
//   - 每个触发整点打开一个长度为 Window 的执行窗口（不再要求「整点那一刻全部跑完」）；
//   - 窗口起点加一点随机抖动，首号不会精确落在 hh:00:00；
//   - 账号之间取 [GapMin, GapMax] 区间内的随机间隔（不是固定值）；
//   - 当日已完成的账号直接跳过（skip 谓词由各任务自己给，见各服务的 skip* 函数）；
//   - 跑完即收，到下一个整点再开新窗口；窗口用尽仍未轮到的账号留给下个窗口，
//     因此「晚上兜底」的语义从「全量重跑」变成「只补没跑成的」。
//
// 该原语只负责推进节奏与窗口边界，不碰账号筛选、不碰上游调用——那些仍留在
// 各任务自己的循环里（避免把三类任务的不同门控揉进一个公共函数）。

import (
	"context"
	"math/rand"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const (
	// workbuddyDefaultSpreadWindow 缺省执行窗口长度（12 小时）。
	workbuddyDefaultSpreadWindow = 12 * time.Hour

	// workbuddyDefaultAccountGapMin / workbuddyDefaultAccountGapMax 缺省账号间随机
	// 间隔区间：3~8 分钟。以 100 个账号估算约需 5~13 小时，刚好铺满 12h 窗口。
	workbuddyDefaultAccountGapMin = 3 * time.Minute
	workbuddyDefaultAccountGapMax = 8 * time.Minute

	// workbuddySpreadStartJitterMax 窗口起点随机抖动上限：首号不会精确落在 hh:00:00，
	// 消除「整点零秒开工」这个最容易识别的自动化特征。
	workbuddySpreadStartJitterMax = 90 * time.Second
)

// workbuddySpreadSettings 一次窗口推进的节奏参数（已归一，可直接使用）。
type workbuddySpreadSettings struct {
	Window time.Duration // 窗口长度
	GapMin time.Duration // 账号间随机间隔下界
	GapMax time.Duration // 账号间随机间隔上界（>= GapMin）
}

// workbuddySpreadSettingsFrom 从配置解析并归一错峰参数：
//   - 字段缺省（0）或负值 → 用默认值；
//   - GapMax < GapMin → 抬到 GapMin（区间退化为固定间隔，不会 panic 也不会取到负数）；
//   - Window 非正（归一后仍为 0）→ 兜底默认窗口。
//
// 归一在这里做而不是散在三个调用点：这三个参数由同一组配置键驱动，任何一处
// 漏判都会让某个任务单独退化成「0 间隔齐发」，那正是要修掉的形态。
func workbuddySpreadSettingsFrom(cfg *config.Config) workbuddySpreadSettings {
	s := workbuddySpreadSettings{
		Window: workbuddyDefaultSpreadWindow,
		GapMin: workbuddyDefaultAccountGapMin,
		GapMax: workbuddyDefaultAccountGapMax,
	}
	if cfg == nil {
		return s
	}
	wb := cfg.Gateway.Workbuddy
	if wb.SpreadWindowSeconds > 0 {
		s.Window = time.Duration(wb.SpreadWindowSeconds) * time.Second
	}
	if wb.AccountGapMinSeconds > 0 {
		s.GapMin = time.Duration(wb.AccountGapMinSeconds) * time.Second
	}
	if wb.AccountGapMaxSeconds > 0 {
		s.GapMax = time.Duration(wb.AccountGapMaxSeconds) * time.Second
	}
	if s.GapMax < s.GapMin {
		s.GapMax = s.GapMin
	}
	if s.Window <= 0 {
		s.Window = workbuddyDefaultSpreadWindow
	}
	return s
}

// workbuddySpreadGapFn 返回账号间的实际等待时长。做成可替换的函数变量是为了让
// 单测把等待置 0（与改造前 workbuddyTasksAccountDelay / workbuddyCheckinAccountDelay
// 的测试用法等价，但那两个是固定值，表达不了区间语义）。
var workbuddySpreadGapFn = func(s workbuddySpreadSettings) time.Duration {
	if s.GapMax <= s.GapMin {
		return s.GapMin
	}
	return s.GapMin + time.Duration(rand.Int63n(int64(s.GapMax-s.GapMin)))
}

// workbuddyStartJitterFn 返回窗口起点抖动。同样可替换（测试置 0）。
var workbuddyStartJitterFn = func() time.Duration {
	return time.Duration(rand.Int63n(int64(workbuddySpreadStartJitterMax)))
}

// workbuddyRunOutcome 单账号执行结果分类（决定统计口径，不参与业务判定）。
type workbuddyRunOutcome int

const (
	// workbuddyRunOK 执行成功（含上游幂等「今天已签到」这类成功态）。
	workbuddyRunOK workbuddyRunOutcome = iota
	// workbuddyRunFail 执行失败（会记 WARN，留给下个窗口重试）。
	workbuddyRunFail
	// workbuddyRunSkipped 执行了但无动作（traveling / daily limit / ctx 取消等）。
	workbuddyRunSkipped
)

// workbuddySpreadStats 一个窗口的推进统计（用于收尾日志）。
type workbuddySpreadStats struct {
	Candidates int // 过滤后进入本轮候选的账号数
	Already    int // 当日已完成、被 skip 谓词跳过的账号数
	Attempted  int // 实际打了上游的账号数
	OK         int
	Fail       int
	Skipped    int
	Remaining  int // 窗口用尽/被中断而未轮到的账号数（下个窗口继续）
}

// workbuddySpreadRunner 在单个窗口内逐号推进的执行器。
//
// 节奏参数全部放在结构体上（而不是包级变量）：签到 / 活跃上报 / 旅行三个 loop
// 是独立 goroutine，并发跑着，任何共享可变状态都是相互干扰的隐患。
type workbuddySpreadRunner struct {
	label  string        // 日志标签：checkin / activity / travel
	window time.Duration // 窗口长度
	gapMin time.Duration // 账号间随机间隔下界（<=0 时用缺省值）
	gapMax time.Duration // 账号间随机间隔上界（<gapMin 时抬到 gapMin）
}

// run 在窗口内按随机间隔逐个推进 accounts。
//
// 参数：
//   - ctx：窗口级上下文（调用方给的批超时；本函数另外用 window 兜住）。
//   - stopCh：进程停机信号，命中即中断（剩余账号留给下个窗口）。
//   - accounts：**已过滤**的候选账号（门控留在各任务自己那边）。
//   - skip：当日已完成判定，返回 true 的账号不占用间隔、不打上游。
//   - exec：单账号执行。
//
// 等待语义：账号间隔在「下一次真正要执行之前」才 sleep，因此尾部不会再白等一个
// 间隔；skip 命中的账号也不消耗间隔——否则一批全已完成的账号会被无意义地拖满窗口。
func (r workbuddySpreadRunner) run(
	ctx context.Context,
	stopCh <-chan struct{},
	accounts []*Account,
	skip func(*Account) bool,
	exec func(context.Context, *Account) workbuddyRunOutcome,
) workbuddySpreadStats {
	stats := workbuddySpreadStats{Candidates: len(accounts)}
	if len(accounts) == 0 {
		return stats
	}
	window := r.window
	if window <= 0 {
		window = workbuddyDefaultSpreadWindow
	}
	settings := workbuddySpreadSettings{Window: window, GapMin: r.gapMin, GapMax: r.gapMax}
	if settings.GapMin <= 0 {
		settings.GapMin = workbuddyDefaultAccountGapMin
	}
	if settings.GapMax < settings.GapMin {
		settings.GapMax = settings.GapMin
	}
	startJitter := workbuddyStartJitterFn()
	if startJitter > 0 {
		// 起点抖动同样受窗口/停机约束：窗口比抖动还短时直接放弃抖动。
		if startJitter > window/2 {
			startJitter = window / 2
		}
		if !workbuddySpreadWait(ctx, stopCh, startJitter) {
			stats.Remaining = len(accounts)
			return stats
		}
	}
	deadline := time.Now().Add(window - startJitter)
	executed := 0
	for i, account := range accounts {
		if account == nil {
			stats.Skipped++
			continue
		}
		if workbuddySpreadDone(stopCh) || ctx.Err() != nil {
			stats.Remaining = len(accounts) - i
			return stats
		}
		if time.Now().After(deadline) {
			stats.Remaining = len(accounts) - i
			break
		}
		if skip != nil && skip(account) {
			stats.Already++
			continue
		}
		if executed > 0 {
			gap := workbuddySpreadGapFn(settings)
			if remaining := time.Until(deadline); gap > remaining {
				gap = remaining
			}
			if gap > 0 && !workbuddySpreadWait(ctx, stopCh, gap) {
				stats.Remaining = len(accounts) - i
				return stats
			}
		}
		executed++
		stats.Attempted++
		switch exec(ctx, account) {
		case workbuddyRunOK:
			stats.OK++
		case workbuddyRunFail:
			stats.Fail++
		default:
			stats.Skipped++
		}
	}
	return stats
}

// workbuddySpreadWait 可中断等待：ctx 取消或停机立即返回 false。
func workbuddySpreadWait(ctx context.Context, stopCh <-chan struct{}, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	case <-stopCh:
		return false
	}
}

// workbuddySpreadDone 报告是否已收到停机信号（非阻塞）。
func workbuddySpreadDone(stopCh <-chan struct{}) bool {
	if stopCh == nil {
		return false
	}
	select {
	case <-stopCh:
		return true
	default:
		return false
	}
}
