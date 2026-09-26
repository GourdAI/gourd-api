package service

// trae_checkin_service.go Trae 每日签到周期任务。
//
// 行为（与 workbuddy/qoder 签到任务同构，默认每天 9:00 / 21:00 各触发一次）：
//  1. 到点后枚举全部 Trae 账号（跳过禁用账号与无凭据账号）；
//  2. 逐个串行执行签到（换票 → status → claim → 回查 status → 落快照），账号之间
//     固定间隔 traeCheckinAccountDelay，避免同一时刻批量请求多账号被上游风控识别；
//  3. 汇总日志（ok/already/fail/skipped）。
//
// 21:00 那次主要作兜底：上午因网络抖动/9074 失败的账号当晚还有机会。上游幂等
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

// traeCheckinBatchTimeout 单批签到的总时长上限（大批量账号下限保护）。
const traeCheckinBatchTimeout = 30 * time.Minute

// traeDefaultCheckinHours 默认签到小时（本地时区）：9 点 + 21 点兜底。
var traeDefaultCheckinHours = []int{9, 21}

// traeCheckinAccountDelay 账号间签到间隔（防批量风控；测试可置 0）。
var traeCheckinAccountDelay = 2 * time.Second

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
		"hours", hours, "account_delay", traeCheckinAccountDelay.String())
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

// loop 主循环：计算下一触发点 → 等待 → 执行一批签到，直到停止信号。
func (s *TraeCheckinService) loop(hours []int) {
	for {
		next := nextTraeCheckinFire(time.Now(), hours)
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

// nextTraeCheckinFire 返回 now 之后最近的整点触发时间（hours 为本地小时 0-23）。
func nextTraeCheckinFire(now time.Time, hours []int) time.Time {
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

// runOnce 执行一批签到：枚举全部 Trae 账号，串行逐个签到（账号间固定间隔）。
func (s *TraeCheckinService) runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), traeCheckinBatchTimeout)
	defer cancel()

	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformTrae)
	if err != nil {
		slog.Warn("trae checkin list accounts failed", "error", err)
		return
	}
	var okN, alreadyN, failN, skipN int
	attempted := 0
	for i := range accounts {
		if ctx.Err() != nil {
			break
		}
		account := &accounts[i]
		// 禁用账号不参与（签到不救禁用号）。
		if !account.IsTrae() || !account.IsActive() {
			skipN++
			continue
		}
		creds := account.GetTraeCredentials()
		// 只有 refreshToken 也可以：签到前会先换票拿到 access_token。
		if strings.TrimSpace(creds.AccessToken) == "" && strings.TrimSpace(creds.RefreshToken) == "" {
			skipN++
			continue
		}
		result := s.credits.CheckinForAccount(ctx, account)
		attempted++
		switch result.Status {
		case TraeCheckinStatusOK:
			okN++
			slog.Info("trae daily check-in claimed",
				"account_id", account.ID, "credits", result.Credits)
		case TraeCheckinStatusAlready:
			alreadyN++
		case TraeCheckinStatusSkipped:
			skipN++
			slog.Debug("trae checkin skipped", "account_id", account.ID, "detail", result.Detail)
		default:
			failN++
			slog.Warn("trae checkin failed",
				"account_id", account.ID, "status", result.Status, "detail", result.Detail)
		}
		// 账号间固定间隔：避免同一时间批量请求多账号被上游风控识别。
		if traeCheckinAccountDelay > 0 && i < len(accounts)-1 {
			timer := time.NewTimer(traeCheckinAccountDelay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
			case <-s.stopCh:
				timer.Stop()
				slog.Info("trae checkin batch interrupted",
					"attempted", attempted, "ok", okN, "already", alreadyN, "fail", failN, "skipped", skipN)
				return
			}
		}
	}
	if attempted > 0 || skipN < len(accounts) {
		slog.Info("trae checkin batch done",
			"total", len(accounts), "attempted", attempted, "ok", okN, "already", alreadyN, "fail", failN, "skipped", skipN)
	}
}
