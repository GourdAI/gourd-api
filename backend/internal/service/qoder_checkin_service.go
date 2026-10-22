package service

// qoder_checkin_service.go Qoder「每日领取活动 Credits」周期任务。
//
// 行为（改造后：窗口内逐号推进，与 workbuddy/trae 同构）：
//
//  1. hours 列表里的每个整点打开一个长度为 spread_window_seconds 的执行窗口；
//  2. 窗口内逐个枚举 Qoder 账号（跳过禁用账号与无凭据账号）；
//  3. **本轮已领取成功（ok/already）的账号直接跳过**——旧实现只靠上游幂等，
//     等于把当天已领的号在 21 点兜底那趟重新 GET campaigns，且已领分支还会
//     额外刷一次 quota/usage（refreshQoderCreditsAfterCheckin），每号每晚多花 2 个 GET；
//  4. 账号之间取 [account_gap_min_seconds, account_gap_max_seconds] 区间内的随机
//     间隔，而不是固定值；
//  5. 跑完即收；窗口用尽仍未轮到的账号留给下一个窗口——「晚上兜底」的语义从
//     「全量重跑」变成「只补没领取成功的」。
//
// 触发小时默认 [10, 21]：活动每天 10:00（UTC+8）开放新一轮，10 点为首发，21 点为
// 兜底重试（上午因网络抖动/令牌问题失败的账号当晚还有机会）。重复领取由上游幂等
// 保证（claim 响应 replayed=true，不再发钱），因此多实例部署无需分布式锁。
//
// 「skipped」是正常态且占比可能很高：官方存在未写进文档的设备级资格规则（如同一
// 设备仅一次试用）、企业/团队版不适用，这类账号服务端根本不下发活动。skipped 不写
// 快照、不重试，避免无意义地刷上游。
//
// 触发时刻按服务端本地时区（config.timezone 已在启动时落到 time.Local）整点判定；
// 每次到点先算下一个触发点再等待，进程跨日/跨重启自然对齐。与 singleflight 配合：
// 手动领取（管理端点）与定时任务对同一账号并发时只打一次上游。

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// QoderCheckinService 驱动 Qoder 每日活动 Credits 的批量领取。
type QoderCheckinService struct {
	credits     *QoderCreditsService
	accountRepo AccountRepository
	cfg         *config.Config
	stopCh      chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup
}

// qoderCheckinBatchTimeoutFloor 单批领取的**硬上限**（防呆）。
//
// 注意：真正的节奏由错峰窗口（spread_window_seconds）决定，本值只是防止某个号
// 卡死在代理层时整批永不退出；取 1 小时，与窗口相加后作为批超时，
// 保证正常窗口不会被这个兜底超时提前截断。
const qoderCheckinBatchTimeoutFloor = time.Hour

// qoderDefaultCheckinHours 默认领取小时（本地时区）：10 点开放即领 + 21 点兜底。
var qoderDefaultCheckinHours = []int{10, 21}

// NewQoderCheckinService 构造每日领取任务。
func NewQoderCheckinService(
	credits *QoderCreditsService,
	accountRepo AccountRepository,
	cfg *config.Config,
) *QoderCheckinService {
	return &QoderCheckinService{
		credits:     credits,
		accountRepo: accountRepo,
		cfg:         cfg,
		stopCh:      make(chan struct{}),
	}
}

// qoderCheckinHours 解析并归一领取小时列表：配置缺省/空用默认值；越界值丢弃；
// 去重后为空（全部非法）返回 nil（视为关闭）。
func qoderCheckinHours(cfg *config.Config) []int {
	hours := qoderDefaultCheckinHours
	if cfg != nil && len(cfg.Gateway.Qoder.CheckinHours) > 0 {
		hours = cfg.Gateway.Qoder.CheckinHours
	}
	seen := make(map[int]struct{}, len(hours))
	out := make([]int, 0, len(hours))
	for _, h := range hours {
		if h < 0 || h > 23 {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Ints(out)
	return out
}

// Start 启动领取循环；显式关闭（checkin_enabled=false）或小时列表全非法时不启动。
func (s *QoderCheckinService) Start() {
	if s == nil || s.credits == nil || s.accountRepo == nil || s.stopCh == nil {
		return
	}
	if s.cfg != nil && !s.cfg.Gateway.Qoder.CheckinEnabled {
		return
	}
	hours := qoderCheckinHours(s.cfg)
	if len(hours) == 0 {
		return
	}
	slog.Info("qoder checkin service started",
		"hours", hours,
		// 打印实际生效窗口（已按相邻触发点间隔夹逼），而非配置原始值。
		"spread_window", s.effectiveWindow().String(),
		"account_gap_min", qoderSpreadSettingsFrom(s.cfg).GapMin.String(),
		"account_gap_max", qoderSpreadSettingsFrom(s.cfg).GapMax.String())
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(hours)
	}()
}

// Stop 停止领取循环（幂等；等待当前批次收尾）。
// stopCh 为 nil（未经构造函数创建的零值实例，如单测/禁用分支）时不得 panic。
func (s *QoderCheckinService) Stop() {
	if s == nil || s.stopCh == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

// loop 主循环：到点开窗 → 窗口内逐号推进，直到停止信号。
//
// 委托给 spreadTriggerLoop：它以「上次服务过的触发点」为基准算下一个点，
// 因此不会因为上一窗口跑满时长而跳过 21 点兜底窗口（详见该函数注释）。
func (s *QoderCheckinService) loop(hours []int) {
	window := spreadEffectiveWindow(qoderSpreadSettingsFrom(s.cfg).Window, hours)
	spreadTriggerLoop(s.stopCh, hours, window, "qoder-checkin", s.runOnce)
}

// nextQoderCheckinFire 返回 now 之后最近的整点触发时间。
//
// 实现委托给共享的 nextSpreadFire（四个平台/任务同源，避免语义漂移）。
func nextQoderCheckinFire(now time.Time, hours []int) time.Time {
	return nextSpreadFire(now, hours)
}

// qoderCheckinAlreadyDoneToday 报告该账号本轮是否已领取成功（ok/already）。
//
// 用于「晚上兜底窗口不再重打已领的号」这个短路。判据只用本地快照（免探测），
// 与前端展示的「今日已领取」同源（qoderCheckinSnapshot 的 Round 已按活动轮次
// 口径写入，且 shouldKeepQoderCheckinSnapshot 保证同轮成功后不会被后续失败覆盖）。
// 快照缺失（新号/刚上线）一律返回 false——宁可多试一次，不可漏领。
func qoderCheckinAlreadyDoneToday(account *Account) bool {
	status := qoderCheckinStatusToday(account)
	return status == QoderCheckinStatusOK || status == QoderCheckinStatusAlready
}

// effectiveWindow 返回经「相邻触发点间隔」夹逼后的实际窗口长度。
//
// 存在的理由：runOnce 无参（也供手动/测试直接调用），拿不到 loop 的 hours，
// 因此自己从配置重算一次小时列表并夹逼，与 loop 保持同一语义。
func (s *QoderCheckinService) effectiveWindow() time.Duration {
	return spreadEffectiveWindow(qoderSpreadSettingsFrom(s.cfg).Window, qoderCheckinHours(s.cfg))
}

// runOnce 开启一个执行窗口，在窗口内逐号推进领取。
//
// 与改造前的差别（均已实证）：
//   - 不再整点全量齐发；窗口内随机起点 + 随机间隔推进；
//   - 本轮已成功的号直接跳过，不再重打上游只为了拿一句 replayed=true；
//   - 窗口用尽仍未轮到的号留给下个窗口（Remaining 写进收尾日志，可观测）。
func (s *QoderCheckinService) runOnce() {
	settings := qoderSpreadSettingsFrom(s.cfg)
	// 窗口夹到相邻触发点间隔 - 余量以内（默认 [10,21] 间隔 11h < 12h 窗口，必现超窗）。
	window := s.effectiveWindow()
	// 窗口级 ctx：窗口长度 + 1 小时硬上限（仅防卡死，正常窗口不会撞到）。
	ctx, cancel := context.WithTimeout(context.Background(), window+qoderCheckinBatchTimeoutFloor)
	defer cancel()

	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformQoder)
	if err != nil {
		slog.Warn("qoder checkin list accounts failed", "error", err)
		return
	}
	// 门控在进入窗口前一次性过滤完，避免把永远不会执行的号排进候选列表后
	// 白白占用错峰间隔。
	candidates := make([]*Account, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		// 禁用账号不参与（领取不救禁用号）。
		if !account.IsQoderPlatform() || !account.IsActive() {
			continue
		}
		creds := account.GetQoderCredentials()
		if strings.TrimSpace(creds.AccessToken) == "" &&
			strings.TrimSpace(creds.PersonalToken) == "" &&
			strings.TrimSpace(creds.RefreshToken) == "" {
			continue
		}
		candidates = append(candidates, account)
	}

	runner := spreadRunner{
		label:  "checkin",
		window: window,
		gapMin: settings.GapMin,
		gapMax: settings.GapMax,
	}
	stats := runner.run(ctx, s.stopCh, candidates, qoderCheckinAlreadyDoneToday,
		func(runCtx context.Context, account *Account) spreadRunOutcome {
			result := s.credits.CheckinForAccount(runCtx, account)
			if result == nil {
				return spreadRunFail
			}
			switch result.Status {
			case QoderCheckinStatusOK:
				slog.Info("qoder daily credits claimed",
					"account_id", account.ID, "amount", result.Amount,
					"grant_id", result.GrantID, "remaining", result.Credits)
				return spreadRunOK
			case QoderCheckinStatusAlready:
				return spreadRunOK
			case QoderCheckinStatusSkipped:
				slog.Debug("qoder checkin skipped",
					"account_id", account.ID, "detail", result.Detail)
				return spreadRunSkipped
			default:
				slog.Warn("qoder checkin failed",
					"account_id", account.ID, "status", result.Status, "detail", result.Detail)
				return spreadRunFail
			}
		})

	if stats.Attempted > 0 || stats.Already > 0 || stats.Remaining > 0 {
		slog.Info("qoder checkin window done",
			"task", runner.label,
			"total", len(accounts),
			"candidates", stats.Candidates,
			"already_done", stats.Already,
			"attempted", stats.Attempted,
			"ok", stats.OK,
			"fail", stats.Fail,
			"skipped", stats.Skipped,
			"remaining", stats.Remaining)
	}
}
