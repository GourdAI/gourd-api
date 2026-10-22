package service

// spread_schedule.go 签到 / 任务类周期任务共用的「窗口内逐号推进」调度原语。
//
// 使用者：WorkBuddy（签到 / 活跃上报 / 猫猫旅行）、Trae（签到）、Qoder（每日领取）。
//
// 旧模型（改造前，三家同构）：整点全量齐发，账号间固定 2s。三个已被代码证实的问题：
//  1. 整点准时齐发 + 固定间隔本身就是可识别的机械节律，账号一多即典型批量特征；
//  2. 「晚上兜底」那趟会把当天**已成功**的账号再全部重打一遍上游——签到只靠上游
//     幂等返回「今天已签到」，旅行先打 buddy/info + travel/status 两个 GET，活跃
//     上报把 N 条上报整组重发，Qoder 已领号还要多刷一次 quota。幂等只保证了不重复
//     发钱，没保证不重复暴露请求；
//  3. 触发点恒为 hh:00:00，无任何抖动。
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
// 各任务自己的循环里（避免把不同任务的门控揉进一个公共函数）。
//
// 平台无关性纪律：归一逻辑（normalizeSpreadSettings）**不得**引用任何平台配置。
// 本文件确实包含三个 workbuddy/trae/qoderSpreadSettingsFrom 折算函数，但它们是
// 「读哪个配置键」的**唯一入口**（写在下方「各平台配置折算」一节），职责只是把
// cfg 折算成 spreadTuning 后交给 normalize，不含任何归一规则。新增平台接入时
// 只加一个同构的 XxxSpreadSettingsFrom，归一逻辑与本文件其余部分均不动。

import (
	"context"
	"log/slog"
	"math/rand"
	"sort"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const (
	// spreadDefaultWindow 缺省执行窗口长度（12 小时）。
	spreadDefaultWindow = 12 * time.Hour

	// spreadDefaultAccountGapMin / spreadDefaultAccountGapMax 缺省账号间随机
	// 间隔区间：3~8 分钟。以 100 个账号估算约需 5~13 小时，刚好铺满 12h 窗口。
	spreadDefaultAccountGapMin = 3 * time.Minute
	spreadDefaultAccountGapMax = 8 * time.Minute

	// spreadStartJitterMax 窗口起点随机抖动上限：首号不会精确落在 hh:00:00，
	// 消除「整点零秒开工」这个最容易识别的自动化特征。
	spreadStartJitterMax = 90 * time.Second
)

// spreadSettings 一次窗口推进的节奏参数（已归一，可直接使用）。
type spreadSettings struct {
	Window time.Duration // 窗口长度
	GapMin time.Duration // 账号间随机间隔下界
	GapMax time.Duration // 账号间随机间隔上界（>= GapMin）
}

// spreadTuning 某平台（或某个任务）原始配置值的秒数载体，零值表示「未配置」。
//
// 引入这一层是为了把「读哪个配置键」与「如何归一」分开：
// 归一规则只写一份（normalize），各平台自己负责把配置折算成 spreadTuning。
// 否则每接一个平台都要复制一遍夹逼逻辑，而任何一处漏判都会让该平台单独
// 退化成「0 间隔齐发」——那正是本原语要修掉的形态。
type spreadTuning struct {
	WindowSeconds int
	GapMinSeconds int
	GapMaxSeconds int
}

// normalizeSpreadSettings 把原始秒数归一为可直接使用的节奏参数：
//   - 字段缺省（0）或负值 → 用默认值；
//   - GapMax < GapMin → 抬到 GapMin（区间退化为固定间隔，不会 panic 也不会取到负数）；
//   - Window 非正（归一后仍为 0）→ 兜底默认窗口。
func normalizeSpreadSettings(t spreadTuning) spreadSettings {
	s := spreadSettings{
		Window: spreadDefaultWindow,
		GapMin: spreadDefaultAccountGapMin,
		GapMax: spreadDefaultAccountGapMax,
	}
	if t.WindowSeconds > 0 {
		s.Window = time.Duration(t.WindowSeconds) * time.Second
	}
	if t.GapMinSeconds > 0 {
		s.GapMin = time.Duration(t.GapMinSeconds) * time.Second
	}
	if t.GapMaxSeconds > 0 {
		s.GapMax = time.Duration(t.GapMaxSeconds) * time.Second
	}
	if s.GapMax < s.GapMin {
		s.GapMax = s.GapMin
	}
	if s.Window <= 0 {
		s.Window = spreadDefaultWindow
	}
	return s
}

// spreadGapFn 返回账号间的实际等待时长。做成可替换的函数变量是为了让
// 单测把等待置 0（与改造前 workbuddyTasksAccountDelay / workbuddyCheckinAccountDelay
// 的测试用法等价，但那两个是固定值，表达不了区间语义）。
var spreadGapFn = func(s spreadSettings) time.Duration {
	if s.GapMax <= s.GapMin {
		return s.GapMin
	}
	return s.GapMin + time.Duration(rand.Int63n(int64(s.GapMax-s.GapMin)))
}

// spreadStartJitterFn 返回窗口起点抖动。同样可替换（测试置 0）。
var spreadStartJitterFn = func() time.Duration {
	return time.Duration(rand.Int63n(int64(spreadStartJitterMax)))
}

// --- 各平台配置折算 ---------------------------------------------------------
//
// 以下函数是「读哪个配置键」的唯一入口。新增平台接入时只需加一个同构函数，
// 归一逻辑（normalizeSpreadSettings）不动。

// workbuddySpreadSettingsFrom 从 gateway.workbuddy.* 折算错峰参数。
func workbuddySpreadSettingsFrom(cfg *config.Config) spreadSettings {
	if cfg == nil {
		return normalizeSpreadSettings(spreadTuning{})
	}
	wb := cfg.Gateway.Workbuddy
	return normalizeSpreadSettings(spreadTuning{
		WindowSeconds: wb.SpreadWindowSeconds,
		GapMinSeconds: wb.AccountGapMinSeconds,
		GapMaxSeconds: wb.AccountGapMaxSeconds,
	})
}

// traeSpreadSettingsFrom 从 gateway.trae.* 折算错峰参数。
func traeSpreadSettingsFrom(cfg *config.Config) spreadSettings {
	if cfg == nil {
		return normalizeSpreadSettings(spreadTuning{})
	}
	tr := cfg.Gateway.Trae
	return normalizeSpreadSettings(spreadTuning{
		WindowSeconds: tr.SpreadWindowSeconds,
		GapMinSeconds: tr.AccountGapMinSeconds,
		GapMaxSeconds: tr.AccountGapMaxSeconds,
	})
}

// qoderSpreadSettingsFrom 从 gateway.qoder.* 折算错峰参数。
func qoderSpreadSettingsFrom(cfg *config.Config) spreadSettings {
	if cfg == nil {
		return normalizeSpreadSettings(spreadTuning{})
	}
	q := cfg.Gateway.Qoder
	return normalizeSpreadSettings(spreadTuning{
		WindowSeconds: q.SpreadWindowSeconds,
		GapMinSeconds: q.AccountGapMinSeconds,
		GapMaxSeconds: q.AccountGapMaxSeconds,
	})
}

// spreadRunOutcome 单账号执行结果分类（决定统计口径，不参与业务判定）。
type spreadRunOutcome int

const (
	// spreadRunOK 执行成功（含上游幂等「今天已签到」这类成功态）。
	spreadRunOK spreadRunOutcome = iota
	// spreadRunFail 执行失败（会记 WARN，留给下个窗口重试）。
	spreadRunFail
	// spreadRunSkipped 执行了但无动作（traveling / daily limit / ctx 取消等）。
	spreadRunSkipped
)

// spreadStats 一个窗口的推进统计（用于收尾日志）。
type spreadStats struct {
	Candidates int // 过滤后进入本轮候选的账号数
	Already    int // 当日已完成、被 skip 谓词跳过的账号数
	Attempted  int // 实际打了上游的账号数
	OK         int
	Fail       int
	Skipped    int
	Remaining  int // 窗口用尽/被中断而未轮到的账号数（下个窗口继续）
}

// spreadRunner 在单个窗口内逐号推进的执行器。
//
// 节奏参数全部放在结构体上（而不是包级变量）：签到 / 活跃上报 / 旅行三个 loop
// 是独立 goroutine，并发跑着，任何共享可变状态都是相互干扰的隐患。
type spreadRunner struct {
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
func (r spreadRunner) run(
	ctx context.Context,
	stopCh <-chan struct{},
	accounts []*Account,
	skip func(*Account) bool,
	exec func(context.Context, *Account) spreadRunOutcome,
) spreadStats {
	stats := spreadStats{Candidates: len(accounts)}
	if len(accounts) == 0 {
		return stats
	}
	window := r.window
	if window <= 0 {
		window = spreadDefaultWindow
	}
	settings := spreadSettings{Window: window, GapMin: r.gapMin, GapMax: r.gapMax}
	if settings.GapMin <= 0 {
		settings.GapMin = spreadDefaultAccountGapMin
	}
	if settings.GapMax < settings.GapMin {
		settings.GapMax = settings.GapMin
	}
	startJitter := spreadStartJitterFn()
	if startJitter > 0 {
		// 起点抖动同样受窗口/停机约束：窗口比抖动还短时直接放弃抖动。
		if startJitter > window/2 {
			startJitter = window / 2
		}
		if !spreadWait(ctx, stopCh, startJitter) {
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
		if spreadDone(stopCh) || ctx.Err() != nil {
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
			gap := spreadGapFn(settings)
			if remaining := time.Until(deadline); gap > remaining {
				gap = remaining
			}
			if gap > 0 && !spreadWait(ctx, stopCh, gap) {
				stats.Remaining = len(accounts) - i
				return stats
			}
		}
		executed++
		stats.Attempted++
		switch exec(ctx, account) {
		case spreadRunOK:
			stats.OK++
		case spreadRunFail:
			stats.Fail++
		default:
			stats.Skipped++
		}
	}
	return stats
}

// spreadWait 可中断等待：ctx 取消或停机立即返回 false。
func spreadWait(ctx context.Context, stopCh <-chan struct{}, d time.Duration) bool {
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

// spreadDone 报告是否已收到停机信号（非阻塞）。
func spreadDone(stopCh <-chan struct{}) bool {
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

// spreadFireGapMargin 窗口与「相邻触发点间隔」之间必须留出的余量。
//
// 理由：loop 是**串行**的（runOnce 跑完才回到等待）。若窗口长度 >= 触发间隔，
// 一个窗口会一直跑到下一个触发点之后才返回，而返回后若以 now 重算下一个触发点，
// 那个刚刚错过的触发点就被永久跳过了（默认 [9,21] + 12h 窗口恰好相等，必现）。
// 留余量使窗口总在下个触发点之前收底，从时序根源上避免「兜底窗口被吞」。
const spreadFireGapMargin = 5 * time.Minute

// spreadEffectiveWindow 把窗口夹到「相邻触发点间隔 - 余量」以内。
//
// hours 不足 2 个时（如单点 [10]）不存在相邻触发点，间隔按 24h 计。
// 返回的窗口恒为正：余量过大时退化为间隔的一半。
func spreadEffectiveWindow(window time.Duration, hours []int) time.Duration {
	if window <= 0 {
		window = spreadDefaultWindow
	}
	if len(hours) < 2 {
		gap := 24 * time.Hour
		if window > gap-spreadFireGapMargin {
			return gap - spreadFireGapMargin
		}
		return window
	}
	// 升序后最小间隔出现在首尾环相邻处或相邻两项之间；取全量最小。
	sorted := make([]int, 0, len(hours))
	for _, h := range hours {
		if h >= 0 && h <= 23 {
			sorted = append(sorted, h)
		}
	}
	if len(sorted) < 2 {
		gap := 24 * time.Hour
		if window > gap-spreadFireGapMargin {
			return gap - spreadFireGapMargin
		}
		return window
	}
	sort.Ints(sorted)
	gap := time.Duration(sorted[0]+24-sorted[len(sorted)-1]) * time.Hour
	for i := 1; i < len(sorted); i++ {
		if d := time.Duration(sorted[i]-sorted[i-1]) * time.Hour; d < gap {
			gap = d
		}
	}
	limit := gap - spreadFireGapMargin
	if limit <= 0 {
		// hours 含重复值（归一后不应出现，但本函数也被测/手动调用）时 gap 可能为 0，
		// 若直接 return limit 会得到 0 窗口，runner 会回退成缺省 12h 从而重新引入
		// 「窗口跨过下一个触发点」的缺陷。这里退化到间隔一半；仍非正则给一个分钟级下限。
		limit = gap / 2
	}
	if limit <= 0 {
		limit = time.Minute
	}
	if window > limit {
		return limit
	}
	return window
}

// nextSpreadFire 返回 now 之后最近的整点触发时间（hours 为本地小时 0-23）。
func nextSpreadFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		if h < 0 || h > 23 {
			continue
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// spreadClock 可替换的时钟（测试注入假时钟）。
//
// 存在的理由：spreadTriggerLoop 的「不跳过触发点」语义是完全时序性的，
// 用真实时钟无法在单测里触发（需要真的等过小时），而那正是之前漏测、
// 导致缺陷直接进了默认配置的原因。
var spreadClock = time.Now

// spreadTriggerLoop 统一的「到点开窗 → 窗口内逐号推进 → 等下一个点」循环。
//
// 四个周期任务（WorkBuddy 签到/活跃上报/旅行、Trae 签到、Qoder 领取）共用，
// 保证「不跳过触发点」这个时序纪律只写一份。
//
// 关键差异（相对改造前的 loop）：**以「上次服务过的触发点」而非 now 为基准算下一个点**。
// 改造前的写法是：
//
//	timer → runOnce()（可能跑很久） → nextFire(time.Now()) → 若已过则顺延 24h
//
// 于是只要一个窗口的耗时溢过了下一个触发点（默认 [9,21] 间隔 12h、窗口也 12h，
// 恰好相等 ⇒ 必现），21 点兜底就被静默跳过了。现在的写法让每个触发点**恰好服务一次**，
// 即使上一窗口跨过了它（窗口已由 spreadEffectiveWindow 夹到间隔以内，正常不会发生），
// 也会立即补跑并记 WARN，而不是静默丢掉。
func spreadTriggerLoop(stopCh <-chan struct{}, hours []int, window time.Duration, label string, run func()) {
	if len(hours) == 0 || run == nil {
		return
	}
	var served time.Time // 零值表示尚未服务过任何触发点
	for {
		var next time.Time
		if served.IsZero() {
			next = nextSpreadFire(spreadClock(), hours)
		} else {
			// nextSpreadFire 的语义是「严格晚于参数」，故传 served 即得下一个未服务的点。
			next = nextSpreadFire(served, hours)
		}
		if next.IsZero() {
			return
		}
		if wait := next.Sub(spreadClock()); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-stopCh:
				timer.Stop()
				return
			}
		} else if lag := -wait; lag > spreadFireGapMargin {
			// 上一窗口越过了本触发点（不应发生，留作可观测兜底）。
			slog.Warn("spread task trigger ran late; previous window overran its slot",
				"task", label, "trigger", next.Format("15:04"), "late_by", lag.String(),
				"window", window.String())
		}
		served = next
		run()
		if spreadDone(stopCh) {
			return
		}
	}
}
