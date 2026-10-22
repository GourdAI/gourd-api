package service

// trae_checkin_service.go Trae 每日签到周期任务。
//
// 行为（改造后：窗口内逐号推进，与 workbuddy/qoder 同构）：
//
//  1. hours 列表里的每个整点打开一个长度为 spread_window_seconds 的执行窗口；
//  2. 窗口内逐个枚举 Trae 账号（跳过禁用账号与无凭据账号）；
//  3. **当日已签到成功（ok/already）的账号直接跳过**——旧实现只靠上游幂等返回
//     「今天已签到」，等于把当天已完成的号在 21 点兜底那趟全部重走一遍
//     status → claim → 回查（至少 2 个请求），白白多暴露一倍的请求；
//  4. 账号之间取 [account_gap_min_seconds, account_gap_max_seconds] 区间内的随机
//     间隔，而不是固定值（固定节律本身就是自动化特征）；
//  5. 跑完即收；窗口用尽仍未轮到的账号留给下一个窗口——于是「晚上兜底」的语义从
//     「全量重跑」变成「只补没签到成功的」。
//
// 21:00 窗口主要作兜底：上午因网络抖动/9074 失败的账号当晚还有机会。上游幂等
// 保证（code=0 重复调用不再发钱、9095=今日已签到），故**签到本身**多实例重复执行
// 不会重复发钱，无需分布式锁。
//
// 但【不等于多副本部署安全】：签到链路开头会换票（ensureTraeBillingToken →
// traeTokenRefresher），而账号级刷新锁是**进程内** sync.Map（与 workbuddy/qoder
// 同构、口径一致，非本平台引入）。两个副本同时刷同一账号时，各自拿到不同的新
// refreshToken 并相互覆盖落库，未胜出的那份 refreshToken 下次刷新会失败。因此多副本
// 部署时，必须只在**一个**副本上开定时签到（该副本配 gateway.trae.checkin_enabled=true，
// 其余副本置 false），或整体单副本运行。
//
// 触发时刻按服务端本地时区（config.timezone 已在启动时落到 time.Local）整点判定；
// 每次到点先算下一个触发点再等待，进程跨日/跨重启自然对齐。与 singleflight 配合：
// 手动签到（管理端点）与定时任务对同一账号并发时只打一次上游。

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// TraeCheckinService 驱动 Trae 账号的每日批量签到。
type TraeCheckinService struct {
	credits     *TraeCreditsService
	accountRepo AccountRepository
	cfg         *config.Config
	stopCh      chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup
}

// traeCheckinBatchTimeoutFloor 单批签到的**硬上限**（防呆）。
//
// 注意：真正的节奏由错峰窗口（spread_window_seconds）决定，本值只是防止某个号
// 卡死在 9074 重试/代理层时整批永不退出；取 1 小时，与窗口相加后作为批超时，
// 保证正常窗口不会被这个兜底超时提前截断。
//
// 历史：改造前固定 30 分钟，单账号含 9074 重试最坏可达 7 分钟（traeCheckinTimeout
// =420s），30 分钟只够 4 个账号，早已不够用。
const traeCheckinBatchTimeoutFloor = time.Hour

// traeDefaultCheckinHours 默认签到小时（本地时区）：9 点 + 21 点兜底窗口。
var traeDefaultCheckinHours = []int{9, 21}

// NewTraeCheckinService 构造每日签到任务。
func NewTraeCheckinService(
	credits *TraeCreditsService,
	accountRepo AccountRepository,
	cfg *config.Config,
) *TraeCheckinService {
	return &TraeCheckinService{
		credits:     credits,
		accountRepo: accountRepo,
		cfg:         cfg,
		stopCh:      make(chan struct{}),
	}
}

// traeCheckinHours 解析并归一签到小时列表：配置缺省/空用默认值；越界值丢弃；
// 去重后为空（全部非法）返回 nil（视为关闭）。
func traeCheckinHours(cfg *config.Config) []int {
	hours := traeDefaultCheckinHours
	if cfg != nil && len(cfg.Gateway.Trae.CheckinHours) > 0 {
		hours = cfg.Gateway.Trae.CheckinHours
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

// Start 启动签到循环；显式关闭（checkin_enabled=false）或小时列表全非法时不启动。
func (s *TraeCheckinService) Start() {
	if s == nil || s.credits == nil || s.accountRepo == nil || s.stopCh == nil {
		return
	}
	if s.cfg != nil && !s.cfg.Gateway.Trae.CheckinEnabled {
		return
	}
	hours := traeCheckinHours(s.cfg)
	if len(hours) == 0 {
		return
	}
	slog.Info("trae checkin service started",
		"hours", hours,
		// 打印实际生效窗口（已按相邻触发点间隔夹逼），而非配置原始值。
		"spread_window", s.effectiveWindow().String(),
		"account_gap_min", traeSpreadSettingsFrom(s.cfg).GapMin.String(),
		"account_gap_max", traeSpreadSettingsFrom(s.cfg).GapMax.String())
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(hours)
	}()
}

// Stop 停止签到循环（幂等；等待当前批次收尾）。
// stopCh 为 nil（未经构造函数创建的零值实例，如单测/禁用分支）时不得 panic。
func (s *TraeCheckinService) Stop() {
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
func (s *TraeCheckinService) loop(hours []int) {
	window := spreadEffectiveWindow(traeSpreadSettingsFrom(s.cfg).Window, hours)
	spreadTriggerLoop(s.stopCh, hours, window, "trae-checkin", s.runOnce)
}

// nextTraeCheckinFire 返回 now 之后最近的整点触发时间。
//
// 实现委托给共享的 nextSpreadFire（四个平台/任务同源，避免语义漂移）。
func nextTraeCheckinFire(now time.Time, hours []int) time.Time {
	return nextSpreadFire(now, hours)
}

// traeCheckinAlreadyDoneToday 报告该账号当日是否已签到成功（ok/already）。
//
// 用于「晚上兜底窗口不再重打已成功的号」这个短路。判据只用本地快照（免探测），
// 与前端展示的「今日已签到」同源（traeCheckinSnapshot 的 date 已按配置时区写入，
// 且 shouldKeepTraeCheckinSnapshot 保证当日成功后不会被后续失败覆盖）。
// 快照缺失（新号/刚上线）一律返回 false——宁可多试一次，不可漏领。
func traeCheckinAlreadyDoneToday(account *Account) bool {
	status := traeCheckinStatusToday(account)
	return status == TraeCheckinStatusOK || status == TraeCheckinStatusAlready
}

// effectiveWindow 返回经「相邻触发点间隔」夹逼后的实际窗口长度。
//
// 存在的理由：runOnce 无参（也供手动/测试直接调用），拿不到 loop 的 hours，
// 因此自己从配置重算一次小时列表并夹逼，与 loop 保持同一语义。
func (s *TraeCheckinService) effectiveWindow() time.Duration {
	return spreadEffectiveWindow(traeSpreadSettingsFrom(s.cfg).Window, traeCheckinHours(s.cfg))
}

// runOnce 开启一个执行窗口，在窗口内逐号推进签到。
//
// 与改造前的差别（均已实证）：
//   - 不再整点全量齐发；窗口内随机起点 + 随机间隔推进；
//   - 当日已成功的号直接跳过，不再重打上游只为了拿一句「今天已签到」；
//   - 窗口用尽仍未轮到的号留给下个窗口（Remaining 写进收尾日志，可观测）。
func (s *TraeCheckinService) runOnce() {
	settings := traeSpreadSettingsFrom(s.cfg)
	// 窗口长度必须夹到「相邻触发点间隔 - 余量」以内，否则 9 点窗口会跑过 21 点
	// 把兜底吞掉（默认 [9,21] + 12h 窗口恰好相等，必现）。这里与 loop 用同一个
	// 夹逼函数，但 loop 拿不到 hours（runOnce 无参），因此两边都在用同一语义：
	// loop 在构造时夹逼，runOnce 自己也夹一次，避免直接调 runOnce（测试/手动）时超窗。
	window := s.effectiveWindow()
	// 窗口级 ctx：窗口长度 + 1 小时硬上限（仅防卡死，正常窗口不会撞到）。
	ctx, cancel := context.WithTimeout(context.Background(), window+traeCheckinBatchTimeoutFloor)
	defer cancel()

	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformTrae)
	if err != nil {
		slog.Warn("trae checkin list accounts failed", "error", err)
		return
	}
	// 门控在进入窗口前一次性过滤完，避免把永远不会执行的号排进候选列表后
	// 白白占用错峰间隔。
	candidates := make([]*Account, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		// 禁用账号不参与（签到不救禁用号）。
		if !account.IsTrae() || !account.IsActive() {
			continue
		}
		creds := account.GetTraeCredentials()
		// 只有 refreshToken 也可以：签到前会先换票拿到 access_token。
		if strings.TrimSpace(creds.AccessToken) == "" && strings.TrimSpace(creds.RefreshToken) == "" {
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
	stats := runner.run(ctx, s.stopCh, candidates, traeCheckinAlreadyDoneToday,
		func(runCtx context.Context, account *Account) spreadRunOutcome {
			result := s.credits.CheckinForAccount(runCtx, account)
			if result == nil {
				return spreadRunFail
			}
			switch result.Status {
			case TraeCheckinStatusOK:
				slog.Info("trae daily check-in claimed",
					"account_id", account.ID, "credits", result.Credits)
				return spreadRunOK
			case TraeCheckinStatusAlready:
				return spreadRunOK
			case TraeCheckinStatusSkipped:
				slog.Debug("trae checkin skipped", "account_id", account.ID, "detail", result.Detail)
				return spreadRunSkipped
			default:
				slog.Warn("trae checkin failed",
					"account_id", account.ID, "status", result.Status, "detail", result.Detail)
				return spreadRunFail
			}
		})

	if stats.Attempted > 0 || stats.Already > 0 || stats.Remaining > 0 {
		slog.Info("trae checkin window done",
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
