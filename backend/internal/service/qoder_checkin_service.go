package service

// qoder_checkin_service.go Qoder「每日领取活动 Credits」周期任务。
//
// 行为（与 workbuddy_checkin_service.go 同构，差异在活动刷新时刻与筛选条件）：
//  1. 到点后枚举全部 Qoder 账号（跳过禁用账号与无凭据账号）；
//  2. 逐个串行执行领取（活动列表 → claim → 余额刷新 → 落快照），账号之间固定间隔
//     qoderCheckinAccountDelay，避免同一时刻批量请求多账号被上游风控识别；
//  3. 汇总日志（ok/already/skipped/fail）。
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

// qoderCheckinBatchTimeout 单批领取的总时长上限（大批量账号下限保护）。
const qoderCheckinBatchTimeout = 30 * time.Minute

// qoderDefaultCheckinHours 默认领取小时（本地时区）：10 点开放即领 + 21 点兜底。
var qoderDefaultCheckinHours = []int{10, 21}

// qoderCheckinAccountDelay 账号间领取间隔（防批量风控；测试可置 0）。
var qoderCheckinAccountDelay = 2 * time.Second

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
		"hours", hours, "account_delay", qoderCheckinAccountDelay.String())
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

// loop 主循环：计算下一触发点 → 等待 → 执行一批领取，直到停止信号。
func (s *QoderCheckinService) loop(hours []int) {
	for {
		next := nextQoderCheckinFire(time.Now(), hours)
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

// nextQoderCheckinFire 返回 now 之后最近的整点触发时间（hours 为本地小时 0-23）。
func nextQoderCheckinFire(now time.Time, hours []int) time.Time {
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

// runOnce 执行一批领取：枚举全部 Qoder 账号，串行逐个领取（账号间固定间隔）。
func (s *QoderCheckinService) runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), qoderCheckinBatchTimeout)
	defer cancel()

	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformQoder)
	if err != nil {
		slog.Warn("qoder checkin list accounts failed", "error", err)
		return
	}
	var okN, alreadyN, failN, skipN int
	attempted := 0
	for i := range accounts {
		if ctx.Err() != nil {
			break
		}
		account := &accounts[i]
		// 禁用账号不参与（领取不救禁用号）。
		if !account.IsQoderPlatform() || !account.IsActive() {
			skipN++
			continue
		}
		creds := account.GetQoderCredentials()
		if strings.TrimSpace(creds.AccessToken) == "" &&
			strings.TrimSpace(creds.PersonalToken) == "" &&
			strings.TrimSpace(creds.RefreshToken) == "" {
			skipN++
			continue
		}
		result := s.credits.CheckinForAccount(ctx, account)
		attempted++
		switch result.Status {
		case QoderCheckinStatusOK:
			okN++
			slog.Info("qoder daily credits claimed",
				"account_id", account.ID, "amount", result.Amount,
				"grant_id", result.GrantID, "remaining", result.Credits)
		case QoderCheckinStatusAlready:
			alreadyN++
		case QoderCheckinStatusSkipped:
			skipN++
			slog.Debug("qoder checkin skipped",
				"account_id", account.ID, "detail", result.Detail)
		default:
			failN++
			slog.Warn("qoder checkin failed",
				"account_id", account.ID, "status", result.Status, "detail", result.Detail)
		}
		// 账号间固定间隔：避免同一时间批量请求多账号被上游风控识别。
		if qoderCheckinAccountDelay > 0 && i < len(accounts)-1 {
			timer := time.NewTimer(qoderCheckinAccountDelay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
			case <-s.stopCh:
				timer.Stop()
				slog.Info("qoder checkin batch interrupted",
					"attempted", attempted, "ok", okN, "already", alreadyN, "fail", failN, "skipped", skipN)
				return
			}
		}
	}
	if attempted > 0 || skipN < len(accounts) {
		slog.Info("qoder checkin batch done",
			"total", len(accounts), "attempted", attempted, "ok", okN, "already", alreadyN, "fail", failN, "skipped", skipN)
	}
}
