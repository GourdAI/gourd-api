package service

// workbuddy_checkin_service.go WorkBuddy 每日签到周期任务。
//
// 行为（改造后：窗口内逐号推进）：
//
//  1. hours 列表里的每个整点打开一个长度为 spread_window_seconds 的执行窗口；
//  2. 窗口内逐个枚举 WorkBuddy 账号（跳过禁用账号、国际版账号、企业成员与无凭据账号）；
//  3. **当日已签到成功（ok/already）的账号直接跳过**——旧实现只靠上游幂等返回「今天已签到」，
//     等于把当天已完成的号在 21 点兜底那趟全部重打一遍上游，白白多暴露一倍的请求；
//  4. 账号之间取 [account_gap_min_seconds, account_gap_max_seconds] 区间内的随机间隔，
//     而不是固定值（固定节律本身就是自动化特征）；
//  5. 跑完即收；窗口用尽仍未轮到的账号留给下一个窗口——於是「晚上兜底」的语义从
//     「全量重跑」变成「只补没签到成功的」。
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

type WorkBuddyCheckinService struct {
	credits     *WorkBuddyCreditsService
	accountRepo AccountRepository
	cfg         *config.Config
	stopCh      chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup
}

// workbuddyCheckinBatchTimeout 单批签到的**硬上限**（防呆）。
//
// 注意：真正的节奏由错峰窗口（spread_window_seconds）决定，本值只是防止某个号
// 卡死在 DLL/代理层时整批永不退出；取窗口长度 + 1 小时，保证正常窗口不会
// 被这个兜底超时提前截断。
const workbuddyCheckinBatchTimeoutFloor = time.Hour

// workbuddyDefaultCheckinHours 默认签到小时（本地时区）：9 点 + 21 点兜底窗口。
var workbuddyDefaultCheckinHours = []int{9, 21}

// NewWorkBuddyCheckinService 构造每日签到任务。
func NewWorkBuddyCheckinService(
	credits *WorkBuddyCreditsService,
	accountRepo AccountRepository,
	cfg *config.Config,
) *WorkBuddyCheckinService {
	return &WorkBuddyCheckinService{
		credits:     credits,
		accountRepo: accountRepo,
		cfg:         cfg,
		stopCh:      make(chan struct{}),
	}
}

// workbuddyCheckinHours 解析并归一签到小时列表：配置缺省/空用默认值；越界值丢弃；
// 去重后为空（全部非法）返回 nil（视为关闭）。
func workbuddyCheckinHours(cfg *config.Config) []int {
	hours := workbuddyDefaultCheckinHours
	if cfg != nil && len(cfg.Gateway.Workbuddy.CheckinHours) > 0 {
		hours = cfg.Gateway.Workbuddy.CheckinHours
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
func (s *WorkBuddyCheckinService) Start() {
	if s == nil || s.credits == nil || s.accountRepo == nil {
		return
	}
	if s.cfg != nil && !s.cfg.Gateway.Workbuddy.CheckinEnabled {
		return
	}
	hours := workbuddyCheckinHours(s.cfg)
	if len(hours) == 0 {
		return
	}
	slog.Info("workbuddy checkin service started",
		"hours", hours,
		// 打印实际生效窗口（已按相邻触发点间隔夹逼），而非配置原始值。
		"spread_window", s.effectiveWindow(hours).String(),
		"account_gap_min", workbuddySpreadSettingsFrom(s.cfg).GapMin.String(),
		"account_gap_max", workbuddySpreadSettingsFrom(s.cfg).GapMax.String())
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(hours)
	}()
}

// Stop 停止签到循环（幂等；等待当前批次收尾）。
func (s *WorkBuddyCheckinService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

// loop 主循环：到点开窗 → 窗口内逐号推进，直到停止信号。
//
// 委托给 spreadTriggerLoop：它以「上次服务过的触发点」为基准算下一个点，
// 因此不会因为上一窗口跑满时长而跳过 21 点兜底窗口。
func (s *WorkBuddyCheckinService) loop(hours []int) {
	window := s.effectiveWindow(hours)
	spreadTriggerLoop(s.stopCh, hours, window, "workbuddy-checkin", s.runOnce)
}

// effectiveWindow 返回经「相邻触发点间隔」夹逼后的实际窗口长度。
func (s *WorkBuddyCheckinService) effectiveWindow(hours []int) time.Duration {
	return spreadEffectiveWindow(workbuddySpreadSettingsFrom(s.cfg).Window, hours)
}

// nextWorkbuddyCheckinFire 返回 now 之后最近的整点触发时间。
//
// 实现委托给共享的 nextSpreadFire：四个平台/任务的触发点计算必须同源，
// 否则会再次出现「某个平台的 loop 把下一个触发点吞掉」这类差异。
func nextWorkbuddyCheckinFire(now time.Time, hours []int) time.Time {
	return nextSpreadFire(now, hours)
}

// workbuddyCheckinAlreadyDoneToday 报告该账号当日是否已签到成功（ok/already）。
//
// 用于「晚上兜底窗口不再重打已成功的号」这个短路。判据只用本地快照（免探测），
// 与前端展示的「今日已签到」同源（workbuddyCheckinSnapshot 的 date 已按配置时区写入，
// 且 shouldKeepWorkbuddyCheckinSnapshot 保证当日成功后不会被后续失败覆盖）。
// 快照缺失（新号/刚上线）一律返回 false——宁可多试一次，不可漏领。
func workbuddyCheckinAlreadyDoneToday(account *Account) bool {
	status := workbuddyCheckinStatusToday(account)
	return status == WorkBuddyCheckinStatusOK || status == WorkBuddyCheckinStatusAlready
}

// runOnce 开启一个执行窗口，在窗口内逐号推进签到。
//
// 与旧实现的差别（均已实证）：
//   - 不再整点全量齐发；窗口内随机起点 + 随机间隔推进；
//   - 当日已成功的号直接跳过，不再重打上游只为了拿一句「今天已签到」；
//   - 窗口用尽仍未轮到的号留给下个窗口（Remaining 写进收尾日志，可观测）。
func (s *WorkBuddyCheckinService) runOnce() {
	settings := workbuddySpreadSettingsFrom(s.cfg)
	// 窗口夹到相邻触发点间隔 - 余量以内：默认 [9,21] 间隔恰好 12h、窗口也 12h，
	// 不夹则会跑过 21 点把兜底窗口吞掉。
	window := s.effectiveWindow(workbuddyCheckinHours(s.cfg))
	// 窗口级 ctx：窗口长度 + 1 小时硬上限（仅防卡死，正常窗口不会撞到）。
	ctx, cancel := context.WithTimeout(context.Background(), window+workbuddyCheckinBatchTimeoutFloor)
	defer cancel()

	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformWorkbuddy)
	if err != nil {
		slog.Warn("workbuddy checkin list accounts failed", "error", err)
		return
	}
	// 门控与旧实现同口径（禁用/非 workbuddy/国际版/企业成员/无凭据），在进入窗口前一次性过滤完，
	// 避免把永远不会执行的号排进候选列表后白白占用错峰间隔。
	candidates := make([]*Account, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		if !account.IsWorkbuddy() || !account.IsActive() {
			continue
		}
		if account.GetWorkbuddyRealm() != "cn" {
			continue
		}
		creds := account.GetWorkbuddyCredentials()
		// 企业成员账号无个人签到体系（与 CheckinForAccount 的 D5 门控同口径）。
		if strings.TrimSpace(creds.EnterpriseID) != "" {
			continue
		}
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
	stats := runner.run(ctx, s.stopCh, candidates, workbuddyCheckinAlreadyDoneToday,
		func(runCtx context.Context, account *Account) spreadRunOutcome {
			result := s.credits.CheckinForAccount(runCtx, account)
			if result == nil {
				return spreadRunFail
			}
			switch result.Status {
			case WorkBuddyCheckinStatusOK, WorkBuddyCheckinStatusAlready:
				return spreadRunOK
			case WorkBuddyCheckinStatusSkipped:
				return spreadRunSkipped
			default:
				slog.Warn("workbuddy checkin failed",
					"account_id", account.ID, "status", result.Status, "detail", result.Detail)
				return spreadRunFail
			}
		})

	if stats.Attempted > 0 || stats.Already > 0 || stats.Remaining > 0 {
		slog.Info("workbuddy checkin window done",
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
