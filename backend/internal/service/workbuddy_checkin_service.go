package service

// workbuddy_checkin_service.go WorkBuddy 每日签到周期任务。
//
// 行为（对齐 workbuddy2api/internal/scheduler 的 CheckinAll，默认每天 9:00 / 21:00
// 各触发一次；21:00 那次主要作兜底——上午因网络抖动/token 问题失败后当晚还有机会，
// 重复签到上游幂等返回「今天已签到」，不会重复领取）：
//
//  1. 到点后枚举全部 WorkBuddy 账号（跳过禁用账号、国际版账号与无凭据账号）；
//  2. 逐个串行执行签到（token 预刷新 → daily-checkin → 积分刷新 → 落快照），
//     账号之间固定间隔 workbuddyCheckinAccountDelay，避免同一时间批量请求多账号
//     被上游风控识别（用户明确要求的防封控纪律）；
//  3. 汇总日志（ok/already/fail/skipped）。
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

// workbuddyCheckinBatchTimeout 单批签到的总时长上限（大批量账号下限保护）。
const workbuddyCheckinBatchTimeout = 30 * time.Minute

// workbuddyDefaultCheckinHours 默认签到小时（本地时区）：9 点 + 21 点兜底。
var workbuddyDefaultCheckinHours = []int{9, 21}

// workbuddyCheckinAccountDelay 账号间签到间隔（防批量风控；测试可置 0）。
var workbuddyCheckinAccountDelay = 2 * time.Second

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
		"hours", hours, "account_delay", workbuddyCheckinAccountDelay.String())
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

// loop 主循环：计算下一触发点 → 等待 → 执行一批签到，直到停止信号。
func (s *WorkBuddyCheckinService) loop(hours []int) {
	for {
		next := nextWorkbuddyCheckinFire(time.Now(), hours)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-timer.C:
			s.runOnce()
		case <-s.stopCh:
			timer.Stop()
			return
		}
	}
}

// nextWorkbuddyCheckinFire 返回 now 之后最近的整点触发时间（hours 为本地小时 0-23）。
func nextWorkbuddyCheckinFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
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

// runOnce 执行一批签到：枚举全部 WorkBuddy 账号，串行逐个签到（账号间固定间隔）。
func (s *WorkBuddyCheckinService) runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), workbuddyCheckinBatchTimeout)
	defer cancel()

	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformWorkbuddy)
	if err != nil {
		slog.Warn("workbuddy checkin list accounts failed", "error", err)
		return
	}
	var okN, alreadyN, failN, skipN int
	attempted := 0
	for i := range accounts {
		if ctx.Err() != nil {
			break
		}
		account := &accounts[i]
		// 禁用账号不参与（签到不救禁用号）；国际版无签到体系，直接跳过。
		if !account.IsWorkbuddy() || !account.IsActive() {
			skipN++
			continue
		}
		if account.GetWorkbuddyRealm() != "cn" {
			skipN++
			continue
		}
		creds := account.GetWorkbuddyCredentials()
		// 企业成员账号无个人签到体系（与 CheckinForAccount 的 D5 门控同口径），
		// 在本层直接跳过：不占用账号间隔，也不产生无意义的快照写入。
		if strings.TrimSpace(creds.EnterpriseID) != "" {
			skipN++
			continue
		}
		if strings.TrimSpace(creds.AccessToken) == "" && strings.TrimSpace(creds.RefreshToken) == "" {
			skipN++
			continue
		}
		result := s.credits.CheckinForAccount(ctx, account)
		attempted++
		switch result.Status {
		case WorkBuddyCheckinStatusOK:
			okN++
		case WorkBuddyCheckinStatusAlready:
			alreadyN++
		default:
			failN++
			slog.Warn("workbuddy checkin failed",
				"account_id", account.ID, "status", result.Status, "detail", result.Detail)
		}
		// 账号间固定间隔：避免同一时间批量请求多账号被上游风控识别。
		if workbuddyCheckinAccountDelay > 0 && i < len(accounts)-1 {
			timer := time.NewTimer(workbuddyCheckinAccountDelay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
			case <-s.stopCh:
				timer.Stop()
				slog.Info("workbuddy checkin batch interrupted",
					"attempted", attempted, "ok", okN, "already", alreadyN, "fail", failN, "skipped", skipN)
				return
			}
		}
	}
	if attempted > 0 || skipN < len(accounts) {
		slog.Info("workbuddy checkin batch done",
			"total", len(accounts), "attempted", attempted, "ok", okN, "already", alreadyN, "fail", failN, "skipped", skipN)
	}
}
