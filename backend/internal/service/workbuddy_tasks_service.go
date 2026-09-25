package service

// workbuddy_tasks_service.go WorkBuddy「任务三件套」后端核心服务：
// 对话活跃上报 + 连登奖励链（补签/礼包/补偿/兑换/抽奖）+ 猫猫旅行。
//
// 移植自 workbuddy2api/internal/scheduler（runActivity / claimGrowthRewards /
// runTravel）+ internal/upstream（report.go / travel.go / growth_bonus.go /
// growth_reward.go），传输层对齐本仓库 WorkBuddyCreditsService（HTTPUpstream.Do +
// cnValidateProbeURL 出站校验 + applyWorkbuddyBillingHeaders + 401 刷新重试）。
//
// 上游端点（路径逐字对齐参考实现，不得自创）：
//
//	POST {billingBase}/v2/report                           对话活跃上报（数组 body）
//	GET  {chatBase}/activity/growth/streak                 连登天数 + 补签卡 + 兑换状态
//	GET  {chatBase}/activity/growth/heatmap                活跃地图热力格
//	POST {chatBase}/activity/growth/makeup-cards/use       补签
//	POST {billingBase}/billing/meter/claim-gift            新手礼包
//	POST {billingBase}/billing/meter/claim-compensation    活动补偿
//	POST {chatBase}/activity/growth/redeem                 连登奖励兑换
//	GET  {chatBase}/activity/growth/lottery/chances        抽奖次数
//	POST {chatBase}/activity/growth/lottery/draw           抽奖一次
//	GET  {chatBase}/activity/growth/buddy/info             猫档案
//	POST {chatBase}/activity/growth/buddy/agreement        同意协议
//	POST {chatBase}/activity/growth/buddy/first            领养
//	GET  {chatBase}/activity/growth/buddy/travel/status    旅行状态
//	POST {chatBase}/activity/growth/buddy/travel/depart    派出
//	POST {chatBase}/activity/growth/buddy/travel/claim     领奖
//
// 域路由：billing 域 = account.GetWorkbuddyBillingBaseURL()（CN → www.codebuddy.cn）；
// chat 域 = account.GetWorkbuddyBaseURL()（CN → copilot.tencent.com）。
// 两域统一 applyWorkbuddyBillingHeaders（billing 白名单头组）出站。
//
// 业务编排语义（对齐参考实现）：
//  1. 活跃上报每号 N 条同 conversationId（sub2api-<UnixMilli>），requestId 各条独立；
//     全部成功才做 streak 自检（days==0 记 WARN "silent drop?"）→ 领养豁免重试 → 奖励链。
//  2. 奖励链仅 CN：礼包/补偿（静默）→ 读 streak/redemption → 补签（昨日漏签且有卡，
//     补签后重读状态）→ 挑最高达标未领档 redeem → 抽奖（chances>0 才 draw）。
//  3. 旅行仅 CN：无猫领养（受按日防抖）→ 有猫按状态机单趟最多一个动作。
//
// 幂等：adoptTried / rewardClaimed 按 CST 自然日（uid → "2006-01-02"）进程内存记录，
// 重启清零；领取类写操作按天幂等，上游 409/403/400 正常态静默。
//
// 快照写入 account.Extra（workbuddy_activity / workbuddy_travel），供管理列表渲染。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"golang.org/x/sync/singleflight"
)

const (
	// 上游端点路径（逐字对齐参考实现，不得自创）。
	//
	// billing 域（account.GetWorkbuddyBillingBaseURL）——路径为参考实现固定形态，不做 /v2 变体回落：
	//   /v2/report 活跃上报（CN+global 都发）；claim-gift / claim-compensation 为 billing
	//   白名单域固定形态（参考实现 claimGiftPath/claimCompensationPath 逐字）。
	workbuddyTasksReportPath            = "/v2/report"
	workbuddyTasksClaimGiftPath         = "/billing/meter/claim-gift"
	workbuddyTasksClaimCompensationPath = "/billing/meter/claim-compensation"

	// chat 域（account.GetWorkbuddyBaseURL）growth 端点。
	workbuddyTasksStreakPath         = "/activity/growth/streak"
	workbuddyTasksHeatmapPath        = "/activity/growth/heatmap"
	workbuddyTasksMakeupCardUsePath  = "/activity/growth/makeup-cards/use"
	workbuddyTasksRedeemPath         = "/activity/growth/redeem"
	workbuddyTasksLotteryChancesPath = "/activity/growth/lottery/chances"
	workbuddyTasksLotteryDrawPath    = "/activity/growth/lottery/draw"
	workbuddyTasksBuddyInfoPath      = "/activity/growth/buddy/info"
	workbuddyTasksBuddyAgreementPath = "/activity/growth/buddy/agreement"
	workbuddyTasksBuddyFirstPath     = "/activity/growth/buddy/first"
	workbuddyTasksTravelStatusPath   = "/activity/growth/buddy/travel/status"
	workbuddyTasksTravelDepartPath   = "/activity/growth/buddy/travel/depart"
	workbuddyTasksTravelClaimPath    = "/activity/growth/buddy/travel/claim"

	// workbuddyTasksTravelLocationID 派出地点固定 4（古镇客栈）：4 个地点收益/时长区间相同。
	workbuddyTasksTravelLocationID = 4

	// 猫猫旅行状态枚举（对齐参考实现 travelState* 常量）。
	workbuddyTasksTravelStateIdle      = "idle"
	workbuddyTasksTravelStateTraveling = "traveling"
	workbuddyTasksTravelStateArrived   = "arrived"

	// account.Extra 快照键（下游接线/前端依赖，字段名必须一致）。
	workbuddyTasksActivityExtraKey = "workbuddy_activity"
	workbuddyTasksTravelExtraKey   = "workbuddy_travel"

	// workbuddyTasksClaimExtraKey 「按日幂等标记」的持久化键（与上面两个「展示快照」
	// 不同：它是资金安全语义的闸门，不是给前端看的）。值为 {scope: "YYYY-MM-DD"}。
	workbuddyTasksClaimExtraKey = "workbuddy_task_claims"

	// 幂等标记作用域。reward：连登奖励链（礼包/补偿/redeem/抽奖）整体按日一次；
	// makeup：补签卡消耗单独一道闸门——它吃的是不可再生资产，且发生在挑档之前，
	// 绝不能因为「今日无达标档可领」这条 return 路径而当日可重入。
	workbuddyClaimScopeReward = "reward"
	workbuddyClaimScopeMakeup = "makeup"

	// workbuddyTasksActivityTimeout 单账号活跃上报编排（N 条上报 + 自检 + 奖励链）总时长上限。
	workbuddyTasksActivityTimeout = 90 * time.Second

	// workbuddyTasksTravelTimeout 单账号旅行巡检（状态机单趟）总时长上限。
	workbuddyTasksTravelTimeout = 30 * time.Second

	// workbuddyTasksBatchTimeout 单批批量任务（全量账号遍历）总时长上限。
	workbuddyTasksBatchTimeout = 30 * time.Minute

	// workbuddyTasksPersistTimeout 快照/幂等标记落库的独立超时预算：必须脱离调度 ctx
	// （见 persistDetachedCtx），否则停机/超时会把「已发放」的客观事实一并掉。
	workbuddyTasksPersistTimeout = 5 * time.Second
)

// workbuddyTasksReportGap 同一账号内连续上报之间的间隔：N 连发模拟同一会话多轮对话，
// 秒发易触发风控，故 1.5s 一条（对齐参考实现 activityReportGap）。测试可置 0。
var workbuddyTasksReportGap = 1500 * time.Millisecond

// workbuddyTasksAccountDelay 账号间固定间隔（防批量风控；对齐参考实现
// activityAccountDelay/travelAccountDelay 与本仓库 workbuddyCheckinAccountDelay）。测试可置 0。
var workbuddyTasksAccountDelay = 2 * time.Second

// workbuddyTasksCST 上游自然日口径：CST（Asia/Shanghai）固定 +8，不依赖容器 tzdata
// （与参考实现 cstZone/travelDay 同口径）。
var workbuddyTasksCST = time.FixedZone("CST", 8*60*60)

// workbuddyTasksDefaultActivityHours / workbuddyTasksDefaultTravelHours 小时列表缺省值
// （activity 默认每天 10 点一次；travel 默认 9 点派出 + 21 点兜底领奖）。
var (
	workbuddyTasksDefaultActivityHours = []int{10}
	workbuddyTasksDefaultTravelHours   = []int{9, 21}
)

// WorkBuddyActivityRunResult 单账号活跃上报（含连登奖励链）执行结果
// （管理端点/前端消费；字段名与 JSON 键为下游契约，不得改动）。
type WorkBuddyActivityRunResult struct {
	Success          bool   `json:"success"`
	Realm            string `json:"realm"`
	Reports          int    `json:"reports"`
	ReportsRequested int    `json:"reports_requested"`
	StreakDays       int    `json:"streak_days"`
	StreakChecked    bool   `json:"streak_checked"`
	RedeemTier       string `json:"redeem_tier,omitempty"`
	CreditGranted    int64  `json:"credit_granted,omitempty"`
	LotteryDrawn     bool   `json:"lottery_drawn"`
	Detail           string `json:"detail,omitempty"`
	RanAt            int64  `json:"ran_at"`
}

// WorkBuddyTravelRunResult 单账号猫猫旅行巡检执行结果（字段名与 JSON 键为下游契约）。
type WorkBuddyTravelRunResult struct {
	Success      bool   `json:"success"`
	Realm        string `json:"realm"`
	Action       string `json:"action"`
	Detail       string `json:"detail,omitempty"`
	BuddyName    string `json:"buddy_name,omitempty"`
	State        string `json:"state,omitempty"`
	RewardCredit int64  `json:"reward_credit,omitempty"`
	RanAt        int64  `json:"ran_at"`
}

// workbuddyActivitySnapshot 写入 account.Extra 的活跃上报快照（管理列表免探测渲染）。
type workbuddyActivitySnapshot struct {
	Date          string `json:"date"`
	StreakDays    int    `json:"streak_days"`
	Reports       int    `json:"reports"`
	RewardTier    string `json:"reward_tier"`
	CreditGranted int64  `json:"credit_granted"`
	LotteryDrawn  bool   `json:"lottery_drawn"`
	RanAt         int64  `json:"ran_at"`
	OK            bool   `json:"ok"`
}

// workbuddyTravelSnapshot 写入 account.Extra 的旅行巡检快照。
type workbuddyTravelSnapshot struct {
	Date         string `json:"date"`
	Action       string `json:"action"`
	State        string `json:"state"`
	BuddyName    string `json:"buddy_name"`
	RewardCredit int64  `json:"reward_credit"`
	RanAt        int64  `json:"ran_at"`
	OK           bool   `json:"ok"`
}

// workbuddyChatRequestEvent 客户端 chat_request_send 事件完整形状（31 字段照抄参考实现
// report.go 的 chatRequestEvent，勿用最小字段，防上游后续加严）。
// userId 为必填字段（= 账号 uid），缺失则服务端 200 但静默丢弃。
type workbuddyChatRequestEvent struct {
	EventCode             string `json:"eventCode"`
	Timestamp             int64  `json:"timestamp"`
	ReportDelay           int    `json:"reportDelay"`
	Mode                  string `json:"mode"`
	ConversationID        string `json:"conversationId"`
	RequestID             string `json:"requestId"`
	InputLength           int    `json:"inputLength"`
	RequestModelID        string `json:"requestModelId"`
	RequestModelName      string `json:"requestModelName"`
	IsPlan                bool   `json:"isPlan"`
	IsAutoExecuteTerminal bool   `json:"isAutoExecuteTerminal"`
	IsAutoModify          bool   `json:"isAutoModify"`
	CodebaseEnable        bool   `json:"codebaseEnable"`
	MaxToken              int    `json:"maxToken"`
	MaxSteps              int    `json:"maxSteps"`
	Temperature           int    `json:"temperature"`
	MaxRetries            int    `json:"maxRetries"`
	MentionContexts       []any  `json:"mentionContexts"`
	KnowledgeID           []any  `json:"knowledgeId"`
	KnowledgeName         []any  `json:"knowledgeName"`
	CodebaseID            string `json:"codebaseId"`
	MentionContextCount   int    `json:"mentionContextCount"`
	Command               string `json:"command"`
	ExpertID              string `json:"expertId"`
	RecommendID           string `json:"recommendId"`
	SkillID               string `json:"skillId"`
	SkillCount            int    `json:"skillCount"`
	TotalCount            int    `json:"totalCount"`
	FileURI               string `json:"fileUri"`
	PresentAt             int64  `json:"presentAt"`
	TraceID               string `json:"traceId"`
	RootRequestID         string `json:"rootRequestId"`
	ParentConversationID  string `json:"parentConversationId"`
	AgentName             string `json:"agentName"`
	AgentType             string `json:"agentType"`
	UserID                string `json:"userId"`
}

// workbuddyBuddy 账号当前猫档案；nil（data.buddy 为 null）表示无猫。
type workbuddyBuddy struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// workbuddyTravelState 猫猫旅行状态（对齐参考实现 TravelState）。
type workbuddyTravelState struct {
	State             string `json:"state"`               // idle / traveling / arrived
	DailyLimitReached bool   `json:"daily_limit_reached"` // 今日已派出过（CST 00:00 重置）
	RecordID          int64  `json:"record_id"`           // 在途/到站记录 id，claim 必带
	RewardCredit      int64  `json:"reward_credit"`       // 到站可领奖励积分
}

// workbuddyMakeupCards 补签卡余额（streak 响应的 makeup_cards 段）。
type workbuddyMakeupCards struct {
	Balance int `json:"balance"`
	Max     int `json:"max"`
}

// workbuddyHeatmapCell 活跃地图热力格（一日一格；date 取前 10 位比较）。
type workbuddyHeatmapCell struct {
	Date  string `json:"date"`
	Score int    `json:"score"`
}

// workbuddyTierSpec 连登奖励档位配置（redemption_status.tiers 数组元素）。
type workbuddyTierSpec struct {
	Tier    string `json:"tier"`
	Days    int    `json:"days"`
	Credit  int    `json:"credit"`
	Energy  int    `json:"energy"`
	Cards   int    `json:"cards"`
	Chances int    `json:"chances"`
}

// workbuddyRedemptionStatus 连登奖励兑换状态（streak.redemption_status）。
type workbuddyRedemptionStatus struct {
	Tier7dStatus  string              `json:"tier_7d_status"`
	Tier14dStatus string              `json:"tier_14d_status"`
	Tier28dStatus string              `json:"tier_28d_status"`
	Tiers         []workbuddyTierSpec `json:"tiers"`
	RemainingDays int                 `json:"remaining_days"`
}

// claimed 报告指定档位本月是否已领（status=="claimed"）。
func (r *workbuddyRedemptionStatus) claimed(tier string) bool {
	if r == nil {
		return false
	}
	switch tier {
	case "7d":
		return r.Tier7dStatus == "claimed"
	case "14d":
		return r.Tier14dStatus == "claimed"
	case "28d":
		return r.Tier28dStatus == "claimed"
	}
	return false
}

// workbuddyGrowthState 连登天数 + 补签卡 + 兑换状态的整体快照（一次 GET streak 读完）。
type workbuddyGrowthState struct {
	Streak struct {
		Days int `json:"days"`
	} `json:"streak"`
	MakeupCards workbuddyMakeupCards      `json:"makeup_cards"`
	Redemption  workbuddyRedemptionStatus `json:"redemption_status"`
}

// days 返回连登天数（data.streak.days 同口径）。
func (s *workbuddyGrowthState) days() int {
	if s == nil {
		return 0
	}
	return s.Streak.Days
}

// workbuddyRedeemResult 领奖回执（POST /activity/growth/redeem 成功态 data）。
type workbuddyRedeemResult struct {
	CardsGranted   int `json:"cards_granted"`
	CardsOverflow  int `json:"cards_overflow"`
	CreditGranted  int `json:"credit_granted"`
	EnergyGranted  int `json:"energy_granted"`
	ChancesGranted int `json:"chances_granted"`
}

// WorkBuddyTasksService WorkBuddy「任务三件套」服务：
// 活跃上报（含连登奖励链）与猫猫旅行两套周期循环，均支持手动单账号触发。
type WorkBuddyTasksService struct {
	credits     *WorkBuddyCreditsService
	accountRepo AccountRepository
	cfg         *config.Config
	flight      singleflight.Group

	// mu/adoptTried 领养当日失败记录（uid → CST 自然日）：门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；mu/rewardClaimed 连登奖励按日领取标记（uid → 当日日期）。
	// 两者是**进程内快路径**（避免每账号每趟多一次 DB 写）；幂等的**权威记录在
	// account.Extra[workbuddy_task_claims]**（见 workbuddyTaskDayClaimed），重启不丢、
	// 不依赖单进程假设；两者语义差异见下方两个方法的注释。
	mu            sync.Mutex
	adoptTried    map[string]string
	rewardClaimed map[string]string

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewWorkBuddyTasksService 构造任务服务。credits 用于复用其 token 预刷新、401 重试
// 与代理解析链路（同包直接调用私有方法），accountRepo 用于账号枚举与快照落库。
func NewWorkBuddyTasksService(credits *WorkBuddyCreditsService, accountRepo AccountRepository, cfg *config.Config) *WorkBuddyTasksService {
	return &WorkBuddyTasksService{
		credits:       credits,
		accountRepo:   accountRepo,
		cfg:           cfg,
		adoptTried:    make(map[string]string),
		rewardClaimed: make(map[string]string),
		stopCh:        make(chan struct{}),
	}
}

// Start 启动任务循环：activity 与 travel 各一个 goroutine，按各自小时列表唤醒。
// 门控：对应 Enabled=false 或小时列表全非法时不启动对应 loop。
func (s *WorkBuddyTasksService) Start() {
	if s == nil || s.credits == nil || s.accountRepo == nil {
		return
	}
	if s.cfg == nil || s.cfg.Gateway.Workbuddy.ActivityEnabled {
		if hours := workbuddyTasksHours(s.cfg, "activity"); len(hours) > 0 {
			slog.Info("workbuddy activity task started",
				"hours", hours, "report_count", workbuddyTasksReportCount(s.cfg),
				"account_delay", workbuddyTasksAccountDelay.String())
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.loop(hours, s.runActivityOnce)
			}()
		}
	}
	if s.cfg == nil || s.cfg.Gateway.Workbuddy.TravelEnabled {
		if hours := workbuddyTasksHours(s.cfg, "travel"); len(hours) > 0 {
			slog.Info("workbuddy travel task started",
				"hours", hours, "account_delay", workbuddyTasksAccountDelay.String())
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.loop(hours, s.runTravelOnce)
			}()
		}
	}
}

// Stop 停止任务循环（幂等；等待当前批次收尾）。
func (s *WorkBuddyTasksService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

// loop 单个任务的主循环：计算下一触发点 → 等待 → 执行一批，直到停止信号。
func (s *WorkBuddyTasksService) loop(hours []int, run func(context.Context)) {
	for {
		next := nextWorkbuddyTasksFire(time.Now(), hours)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-timer.C:
			run(s.batchContext())
		case <-s.stopCh:
			timer.Stop()
			return
		}
	}
}

// batchContext 单批任务的 ctx：独立于 loop 的取消，由 stopCh 与批超时双闸控制。
func (s *WorkBuddyTasksService) batchContext() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), workbuddyTasksBatchTimeout)
	go func() {
		defer cancel()
		select {
		case <-s.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx
}

// workbuddyTasksHours 解析并归一某类任务的小时列表：配置缺省/空用默认值；越界值丢弃；
// 去重后为空（全部非法）返回 nil（视为关闭）。kind 取 "activity" / "travel"。
func workbuddyTasksHours(cfg *config.Config, kind string) []int {
	hours := workbuddyTasksDefaultActivityHours
	if kind == "travel" {
		hours = workbuddyTasksDefaultTravelHours
	}
	if cfg != nil {
		configured := cfg.Gateway.Workbuddy.ActivityHours
		if kind == "travel" {
			configured = cfg.Gateway.Workbuddy.TravelHours
		}
		if len(configured) > 0 {
			hours = configured
		}
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

// workbuddyTasksReportCount 每号每次上报条数：<=0 归 1（兼容旧行为）。
func workbuddyTasksReportCount(cfg *config.Config) int {
	if cfg == nil || cfg.Gateway.Workbuddy.ActivityReportCount <= 0 {
		return 1
	}
	return cfg.Gateway.Workbuddy.ActivityReportCount
}

// nextWorkbuddyTasksFire 返回 now 之后最近的整点触发时间（hours 为本地小时 0-23）。
func nextWorkbuddyTasksFire(now time.Time, hours []int) time.Time {
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

// workbuddyTasksRequest 发送单次上游请求并解信封（billing/chat 双域共用）。
//
// 各步骤与 WorkBuddyCreditsService.workbuddyBillingRequestOnce 对齐：
//  1. baseURL 由调用方按域给出（billing → GetWorkbuddyBillingBaseURL，chat → GetWorkbuddyBaseURL）；
//  2. cnValidateProbeURL 出站安全校验（与网关转发同一套运营者策略）；
//  3. HTTPUpstream.Do + WithHTTPUpstreamProfile(OpenAI) + applyWorkbuddyBillingHeaders；
//  4. HTTP >= 400 / 业务 code != 0 → *workbuddyBillingError（幂等判定只认该类型）；
//     传输层与解析错误返回普通 error（不参与幂等判定）。
func (s *WorkBuddyTasksService) workbuddyTasksRequest(
	ctx context.Context,
	account *Account,
	method string,
	baseURL string,
	path string,
	body any,
) (json.RawMessage, error) {
	if account == nil {
		return nil, fmt.Errorf("workbuddy tasks request requires an account")
	}
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("workbuddy account %d missing base_url for %s", account.ID, path)
	}
	targetURL := strings.TrimRight(baseURL, "/") + path
	validatedURL, err := cnValidateProbeURL(s.cfg, targetURL)
	if err != nil {
		return nil, infraerrors.New(http.StatusForbidden, "WORKBUDDY_TASKS_URL_REJECTED", err.Error())
	}
	return s.workbuddyTasksSend(ctx, account, method, validatedURL, body)
}

// workbuddyTasksRequestWithAuthRetry 发送请求；401 且持有 refresh_token 时刷新一次
// token 后重试一次（与积分服务 workbuddyBillingRequestWithAuthRetry 同语义）。
func (s *WorkBuddyTasksService) workbuddyTasksRequestWithAuthRetry(
	ctx context.Context,
	account *Account,
	method string,
	baseURL string,
	path string,
	body any,
) (json.RawMessage, error) {
	data, err := s.workbuddyTasksRequest(ctx, account, method, baseURL, path, body)
	if err == nil {
		return data, nil
	}
	if !workbuddyIsUnauthorizedError(err) {
		return nil, err
	}
	creds := account.GetWorkbuddyCredentials()
	if strings.TrimSpace(creds.RefreshToken) == "" {
		return nil, err
	}
	if refreshErr := s.credits.refreshWorkbuddyCreditsToken(ctx, account); refreshErr != nil {
		slog.Warn("workbuddy tasks 401 retry token refresh failed",
			"account_id", account.ID, "error", refreshErr)
		return nil, err
	}
	return s.workbuddyTasksRequest(ctx, account, method, baseURL, path, body)
}

// workbuddyTasksSend 构建并执行已验证 URL 的单次请求（出站头统一 billing 白名单头组）。
func (s *WorkBuddyTasksService) workbuddyTasksSend(
	ctx context.Context,
	account *Account,
	method string,
	validatedURL string,
	body any,
) (json.RawMessage, error) {
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal workbuddy tasks body: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	reqCtx, cancel := context.WithTimeout(ctx, workbuddyBillingTimeout)
	defer cancel()
	var reqBody io.Reader
	if reader != nil {
		reqBody = reader
	}
	req, err := http.NewRequestWithContext(reqCtx, method, validatedURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("build workbuddy tasks request: %w", err)
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	applyWorkbuddyBillingHeaders(req, account.GetWorkbuddyCredentials(), account, account.GetWorkbuddyRealm())
	resp, err := s.credits.httpUpstream.Do(req, s.credits.resolveWorkbuddyProxyURL(ctx, account), account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return nil, fmt.Errorf("workbuddy tasks transport error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, workbuddyBillingMaxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read workbuddy tasks response: %w", err)
	}
	var env workbuddyBillingEnvelope
	parseErr := json.Unmarshal(raw, &env)
	if resp.StatusCode >= 400 {
		be := &workbuddyBillingError{Status: resp.StatusCode}
		if parseErr == nil {
			be.Code = env.Code
			be.Msg = workbuddyTruncateForError(env.Msg)
		}
		if strings.TrimSpace(be.Msg) == "" {
			be.Msg = workbuddyTruncateForError(string(raw))
		}
		return nil, be
	}
	if parseErr != nil {
		return nil, fmt.Errorf("parse workbuddy tasks response: %w (body: %s)", parseErr, workbuddyTruncateForError(string(raw)))
	}
	if env.Code != 0 {
		return nil, &workbuddyBillingError{Status: resp.StatusCode, Code: env.Code, Msg: workbuddyTruncateForError(env.Msg)}
	}
	return env.Data, nil
}

// workbuddyTasksIsErrMarker 判定 err 是否 *workbuddyBillingError 且 HTTP 状态码与任一
// marker 命中（大小写不敏感，对齐参考实现 isErrMarker 四态判定）。
// 网络层/解析层错误返回 false（不当作幂等正常态）。
func workbuddyTasksIsErrMarker(err error, status int, markers ...string) bool {
	if err == nil {
		return false
	}
	var be *workbuddyBillingError
	if !errors.As(err, &be) || be.Status != status {
		return false
	}
	lower := strings.ToLower(be.Msg)
	for _, m := range markers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// workbuddyTasksIsRedeemAlreadyClaimed 报告是否「本月已领取」幂等态（HTTP 409 + duplicate/已领取）。
func workbuddyTasksIsRedeemAlreadyClaimed(err error) bool {
	return workbuddyTasksIsErrMarker(err, http.StatusConflict, "duplicate", "已领取")
}

// workbuddyTasksIsRedeemNotEnoughDays 报告是否「连续登录天数不足」（HTTP 403）。
func workbuddyTasksIsRedeemNotEnoughDays(err error) bool {
	return workbuddyTasksIsErrMarker(err, http.StatusForbidden, "连续登录天数不足")
}

// workbuddyTasksIsLotteryNoChance 报告是否「无抽奖次数」（HTTP 400 insufficient）。
func workbuddyTasksIsLotteryNoChance(err error) bool {
	return workbuddyTasksIsErrMarker(err, http.StatusBadRequest, "insufficient lottery chance balance")
}

// workbuddyTasksIsLotteryDisabled 报告是否「抽奖未开启」（HTTP 400 lottery disabled）。
func workbuddyTasksIsLotteryDisabled(err error) bool {
	return workbuddyTasksIsErrMarker(err, http.StatusBadRequest, "lottery disabled")
}

// workbuddyTasksIsBuddyTaskIncomplete 判定「领养门槛未达标」（HTTP 400 + first_buddy 关键词）。
// 仅该错误当日不重试（避免对上游重试轰炸）。
func workbuddyTasksIsBuddyTaskIncomplete(err error) bool {
	return workbuddyTasksIsErrMarker(err, http.StatusBadRequest, "first_buddy task not completed yet")
}

// workbuddyTasksClientToken 生成幂等键：`<prefix>-<32hex>`（32hex 用 workbuddyNewMessageID）。
// 每次调用新键：上游按 client_token 去重，复用旧键会被静默吞掉。
func workbuddyTasksClientToken(prefix string) string {
	return prefix + "-" + workbuddyNewMessageID()
}

// workbuddyTasksToday 返回 t 所属的 CST 自然日（2006-01-02）。
func workbuddyTasksToday(t time.Time) string {
	return t.In(workbuddyTasksCST).Format("2006-01-02")
}

// workbuddyTasksYesterday 昨日的 CST 自然日。必须先 In(CST) 再 AddDate：AddDate 按
// 入参 Time 所在时区做日历日减法，容器时区含夏令时时会把瞬时点挪 1 小时、CST 日期错位。
func workbuddyTasksYesterday(t time.Time) string {
	return t.In(workbuddyTasksCST).AddDate(0, 0, -1).Format("2006-01-02")
}

// workbuddyTasksSleepCtx 可取消等待：ctx 取消立即返回 false（优雅停机不等睡满），
// d<=0 立即放行（测试把延迟置 0 时不白等）。
func workbuddyTasksSleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// workbuddyReportChatActivity 发送一条对话活跃上报（chat_request_send，body 为单元素数组）。
// conversationID 由调用方生成（sub2api-<UnixMilli>），同号 N 条共享；requestID 各条独立。
// 一条上报同时点亮连登并解锁 first_buddy 任务（领养前置）。
func (s *WorkBuddyTasksService) workbuddyReportChatActivity(ctx context.Context, account *Account, conversationID, requestID string) error {
	if requestID == "" {
		requestID = conversationID
	}
	now := time.Now().UnixMilli()
	ev := workbuddyChatRequestEvent{
		EventCode:             "chat_request_send",
		Timestamp:             now,
		ReportDelay:           0,
		Mode:                  "craft",
		ConversationID:        conversationID,
		RequestID:             requestID,
		InputLength:           12,
		RequestModelID:        "deepseek-v4-flash",
		RequestModelName:      "DeepSeek V4 Flash",
		IsPlan:                false,
		IsAutoExecuteTerminal: false,
		IsAutoModify:          false,
		CodebaseEnable:        false,
		MaxToken:              0,
		MaxSteps:              0,
		Temperature:           0,
		MaxRetries:            0,
		MentionContexts:       []any{},
		KnowledgeID:           []any{},
		KnowledgeName:         []any{},
		CodebaseID:            "",
		MentionContextCount:   0,
		Command:               "",
		ExpertID:              "",
		RecommendID:           "",
		SkillID:               "",
		SkillCount:            0,
		TotalCount:            0,
		FileURI:               "",
		PresentAt:             now,
		TraceID:               "",
		RootRequestID:         conversationID,
		ParentConversationID:  conversationID,
		AgentName:             "default",
		AgentType:             "conversation",
		UserID:                workbuddyStableUID(account.GetWorkbuddyCredentials(), account),
	}
	raw, err := json.Marshal([]workbuddyChatRequestEvent{ev})
	if err != nil {
		return err
	}
	_, err = s.workbuddyTasksRequestWithAuthRetry(ctx, account, http.MethodPost,
		account.GetWorkbuddyBillingBaseURL(), workbuddyTasksReportPath, json.RawMessage(raw))
	return err
}

// workbuddyGrowthState 读取连登天数 + 补签卡余额 + 各档兑换状态（一次 GET streak 全量解析）。
func (s *WorkBuddyTasksService) workbuddyGrowthState(ctx context.Context, account *Account) (*workbuddyGrowthState, error) {
	data, err := s.workbuddyTasksRequestWithAuthRetry(ctx, account, http.MethodGet,
		account.GetWorkbuddyBaseURL(), workbuddyTasksStreakPath, nil)
	if err != nil {
		return nil, err
	}
	var state workbuddyGrowthState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse workbuddy growth state: %w", err)
	}
	return &state, nil
}

// workbuddyGrowthHeatmap 读取活跃地图热力格列表（GET heatmap）。
func (s *WorkBuddyTasksService) workbuddyGrowthHeatmap(ctx context.Context, account *Account) ([]workbuddyHeatmapCell, error) {
	data, err := s.workbuddyTasksRequestWithAuthRetry(ctx, account, http.MethodGet,
		account.GetWorkbuddyBaseURL(), workbuddyTasksHeatmapPath, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Cells []workbuddyHeatmapCell `json:"cells"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse workbuddy heatmap: %w", err)
	}
	return resp.Cells, nil
}

// workbuddyHeatmapDayScore 返回 cells 中 date（2006-01-02，取前 10 位比较）当日 score；
// 无该日格返回 (0, false)（无判据，不动）。
func workbuddyHeatmapDayScore(cells []workbuddyHeatmapCell, date string) (int, bool) {
	for _, c := range cells {
		if len(c.Date) >= 10 && c.Date[:10] == date {
			return c.Score, true
		}
	}
	return 0, false
}

// workbuddyGrowthUseMakeupCard 对指定日期使用补签卡（target_date 格式 2006-01-02，CST 自然日）。
// 无卡 / 该日无漏签 / 已补过 → 上游业务错误，调用方静默跳过。
func (s *WorkBuddyTasksService) workbuddyGrowthUseMakeupCard(ctx context.Context, account *Account, targetDate string) error {
	_, err := s.workbuddyTasksRequestWithAuthRetry(ctx, account, http.MethodPost,
		account.GetWorkbuddyBaseURL(), workbuddyTasksMakeupCardUsePath,
		map[string]any{"target_date": targetDate})
	return err
}

// workbuddyGrowthClaimGift 领取新手礼包（每号一次）；已领返回业务错误，调用方静默。
func (s *WorkBuddyTasksService) workbuddyGrowthClaimGift(ctx context.Context, account *Account) (int64, error) {
	return s.workbuddyClaimBillingCredit(ctx, account, workbuddyTasksClaimGiftPath)
}

// workbuddyGrowthClaimCompensation 领取活动补偿（有则领）；无可领返回业务错误，调用方静默。
func (s *WorkBuddyTasksService) workbuddyGrowthClaimCompensation(ctx context.Context, account *Account) (int64, error) {
	return s.workbuddyClaimBillingCredit(ctx, account, workbuddyTasksClaimCompensationPath)
}

// workbuddyClaimBillingCredit billing 域领取类公共实现：POST path → data.credit。
// 回执字段缺失不视为失败（调用方按 0 记日志）。
func (s *WorkBuddyTasksService) workbuddyClaimBillingCredit(ctx context.Context, account *Account, path string) (int64, error) {
	data, err := s.workbuddyTasksRequestWithAuthRetry(ctx, account, http.MethodPost,
		account.GetWorkbuddyBillingBaseURL(), path, map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

// workbuddyGrowthRedeem 兑换指定档位连登奖励；clientToken 每次调用必须新键。
// 409 duplicate（本月已领）/ 403 天数不足（未达标）为正常态，由调用方静默。
func (s *WorkBuddyTasksService) workbuddyGrowthRedeem(ctx context.Context, account *Account, tier, clientToken string) (*workbuddyRedeemResult, error) {
	if clientToken == "" {
		clientToken = workbuddyTasksClientToken("redeem-" + tier)
	}
	data, err := s.workbuddyTasksRequestWithAuthRetry(ctx, account, http.MethodPost,
		account.GetWorkbuddyBaseURL(), workbuddyTasksRedeemPath,
		map[string]any{"tier": tier, "client_token": clientToken})
	if err != nil {
		return nil, err
	}
	var res workbuddyRedeemResult
	if len(data) > 0 {
		_ = json.Unmarshal(data, &res) // 回执字段缺失不视为失败
	}
	return &res, nil
}

// workbuddyGrowthLotteryChances 查询当前抽奖次数余额（GET lottery/chances → data.balance）。
func (s *WorkBuddyTasksService) workbuddyGrowthLotteryChances(ctx context.Context, account *Account) (int, error) {
	data, err := s.workbuddyTasksRequestWithAuthRetry(ctx, account, http.MethodGet,
		account.GetWorkbuddyBaseURL(), workbuddyTasksLotteryChancesPath, nil)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Balance int `json:"balance"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("parse workbuddy lottery chances: %w", err)
	}
	return resp.Balance, nil
}

// workbuddyGrowthLotteryDraw 抽一次奖（clientToken 为空时自动新生成）。
// 无次数（400 insufficient）/ 未开启（400 disabled）为正常态，调用方静默。
func (s *WorkBuddyTasksService) workbuddyGrowthLotteryDraw(ctx context.Context, account *Account, clientToken string) (string, error) {
	if clientToken == "" {
		clientToken = workbuddyTasksClientToken("draw")
	}
	data, err := s.workbuddyTasksRequestWithAuthRetry(ctx, account, http.MethodPost,
		account.GetWorkbuddyBaseURL(), workbuddyTasksLotteryDrawPath,
		map[string]any{"client_token": clientToken})
	if err != nil {
		return "", err
	}
	var res struct {
		PrizeCode    string `json:"prize_code"`
		PrizeName    string `json:"prize_name"`
		PrizeType    string `json:"prize_type"`
		CreditAmount int    `json:"credit_amount"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &res) // 奖品字段缺失不视为失败
	}
	return res.PrizeName, nil
}

// workbuddyBuddyInfo 查询当前猫档案；返回 (nil, nil) 表示无猫（data.buddy 为 null/缺失）。
func (s *WorkBuddyTasksService) workbuddyBuddyInfo(ctx context.Context, account *Account) (*workbuddyBuddy, error) {
	data, err := s.workbuddyTasksRequestWithAuthRetry(ctx, account, http.MethodGet,
		account.GetWorkbuddyBaseURL(), workbuddyTasksBuddyInfoPath, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse workbuddy buddy info: %w", err)
	}
	trimmed := strings.TrimSpace(string(resp.Buddy))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var buddy workbuddyBuddy
	if err := json.Unmarshal(resp.Buddy, &buddy); err != nil {
		return nil, fmt.Errorf("parse workbuddy buddy: %w", err)
	}
	return &buddy, nil
}

// workbuddyBuddyAgreement 同意领养协议（幂等，重复调用无副作用）。
func (s *WorkBuddyTasksService) workbuddyBuddyAgreement(ctx context.Context, account *Account) error {
	_, err := s.workbuddyTasksRequestWithAuthRetry(ctx, account, http.MethodPost,
		account.GetWorkbuddyBaseURL(), workbuddyTasksBuddyAgreementPath,
		map[string]any{"agree": true})
	return err
}

// workbuddyBuddyFirst 领养第一只猫。无猫且已过 conversation 门槛时送 300 分；
// 门槛未达标返回 HTTP 400（见 workbuddyTasksIsBuddyTaskIncomplete），属预期行为。
func (s *WorkBuddyTasksService) workbuddyBuddyFirst(ctx context.Context, account *Account) error {
	_, err := s.workbuddyTasksRequestWithAuthRetry(ctx, account, http.MethodPost,
		account.GetWorkbuddyBaseURL(), workbuddyTasksBuddyFirstPath, map[string]any{})
	return err
}

// workbuddyTravelStatus 查询猫猫旅行状态。
func (s *WorkBuddyTasksService) workbuddyTravelStatus(ctx context.Context, account *Account) (*workbuddyTravelState, error) {
	data, err := s.workbuddyTasksRequestWithAuthRetry(ctx, account, http.MethodGet,
		account.GetWorkbuddyBaseURL(), workbuddyTasksTravelStatusPath, nil)
	if err != nil {
		return nil, err
	}
	var st workbuddyTravelState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse workbuddy travel status: %w", err)
	}
	return &st, nil
}

// workbuddyTravelDepart 派出猫旅行；locationID 实测 1~4（收益/时长区间相同）。
func (s *WorkBuddyTasksService) workbuddyTravelDepart(ctx context.Context, account *Account, locationID int) error {
	_, err := s.workbuddyTasksRequestWithAuthRetry(ctx, account, http.MethodPost,
		account.GetWorkbuddyBaseURL(), workbuddyTasksTravelDepartPath,
		map[string]any{"location_id": locationID})
	return err
}

// workbuddyTravelClaim 领取到站奖励，返回 reward_credit。
func (s *WorkBuddyTasksService) workbuddyTravelClaim(ctx context.Context, account *Account, recordID int64) (int64, error) {
	data, err := s.workbuddyTasksRequestWithAuthRetry(ctx, account, http.MethodPost,
		account.GetWorkbuddyBaseURL(), workbuddyTasksTravelClaimPath,
		map[string]any{"record_id": recordID})
	if err != nil {
		return 0, err
	}
	var resp struct {
		RewardCredit int64 `json:"reward_credit"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &resp) // 奖励字段缺失不视为失败
	}
	return resp.RewardCredit, nil
}

// RunActivity 手动触发指定账号的活跃上报 + 连登奖励链（管理端点入口）。
// 同一账号并发调用被 singleflight 合并；账号不存在/平台不对返回 infraerrors；
// 上游失败回填 result.Detail 不返 Go error（对齐积分服务口径）。
func (s *WorkBuddyTasksService) RunActivity(ctx context.Context, accountID int64) (*WorkBuddyActivityRunResult, error) {
	if s == nil || s.credits == nil || s.accountRepo == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "WORKBUDDY_TASKS_NOT_CONFIGURED", "workbuddy tasks service is not configured")
	}
	account, err := s.credits.loadWorkbuddyAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	key := workbuddyActivityFlightKey(accountID)
	resultCh := s.flight.DoChan(key, func() (any, error) {
		probeCtx, cancel := context.WithTimeout(context.Background(), workbuddyTasksActivityTimeout)
		defer cancel()
		return s.runActivityForAccount(probeCtx, account), nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case flightResult := <-resultCh:
		if flightResult.Err != nil {
			return nil, flightResult.Err
		}
		result, ok := flightResult.Val.(*WorkBuddyActivityRunResult)
		if !ok || result == nil {
			return nil, infraerrors.New(http.StatusInternalServerError, "WORKBUDDY_ACTIVITY_RESULT_INVALID", "invalid workbuddy activity result")
		}
		cloned := *result
		return &cloned, nil
	}
}

// RunTravel 手动触发指定账号的猫猫旅行巡检（管理端点入口）。
// 同一账号并发调用被 singleflight 合并；账号不存在/平台不对返回 infraerrors。
func (s *WorkBuddyTasksService) RunTravel(ctx context.Context, accountID int64) (*WorkBuddyTravelRunResult, error) {
	if s == nil || s.credits == nil || s.accountRepo == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "WORKBUDDY_TASKS_NOT_CONFIGURED", "workbuddy tasks service is not configured")
	}
	account, err := s.credits.loadWorkbuddyAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	key := workbuddyTravelFlightKey(accountID)
	resultCh := s.flight.DoChan(key, func() (any, error) {
		probeCtx, cancel := context.WithTimeout(context.Background(), workbuddyTasksTravelTimeout)
		defer cancel()
		return s.runTravelForAccount(probeCtx, account), nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case flightResult := <-resultCh:
		if flightResult.Err != nil {
			return nil, flightResult.Err
		}
		result, ok := flightResult.Val.(*WorkBuddyTravelRunResult)
		if !ok || result == nil {
			return nil, infraerrors.New(http.StatusInternalServerError, "WORKBUDDY_TRAVEL_RESULT_INVALID", "invalid workbuddy travel result")
		}
		cloned := *result
		return &cloned, nil
	}
}

// runActivityForAccount 单账号活跃上报编排：token 预刷新 → N 条上报（同 cid，各条独立 rid，
// 条间 workbuddyTasksReportGap）→ 全部成功才 streak 自检 → 领养豁免重试 → 连登奖励链。
// 任一上报失败立即 break 本号剩余条数（未发满则自检与领养均无意义）。
func (s *WorkBuddyTasksService) runActivityForAccount(ctx context.Context, account *Account) *WorkBuddyActivityRunResult {
	count := workbuddyTasksReportCount(s.cfg)
	result := &WorkBuddyActivityRunResult{
		Realm:            account.GetWorkbuddyRealm(),
		ReportsRequested: count,
		RanAt:            time.Now().Unix(),
	}
	if err := s.credits.ensureWorkbuddyBillingToken(ctx, account); err != nil {
		result.Detail = err.Error()
		s.persistWorkbuddyActivitySnapshot(ctx, account, result)
		return result
	}
	cid := fmt.Sprintf("sub2api-%d", time.Now().UnixMilli())
	sent := 0
	for i := 1; i <= count; i++ {
		rid := fmt.Sprintf("%s-r%d", cid, i)
		if err := s.workbuddyReportChatActivity(ctx, account, cid, rid); err != nil {
			slog.Warn("workbuddy activity report failed",
				"account_id", account.ID, "report", strconv.Itoa(i)+"/"+strconv.Itoa(count), "error", err)
			result.Detail = fmt.Sprintf("report %d/%d: %v", i, count, err)
			break
		}
		sent++
		if i < count {
			if !workbuddyTasksSleepCtx(ctx, workbuddyTasksReportGap) {
				result.Detail = ctx.Err().Error()
				result.Reports = sent
				s.persistWorkbuddyActivitySnapshot(ctx, account, result)
				return result
			}
		}
	}
	result.Reports = sent
	if sent < count {
		// 未发满：自检与奖励链均跳过（与参考实现同口径）。
		s.persistWorkbuddyActivitySnapshot(ctx, account, result)
		return result
	}
	// 全部发满 → 回读 streak 自检（只读 oracle，发现「200 但静默丢弃」）。
	result.StreakChecked = true
	if state, err := s.workbuddyGrowthState(ctx, account); err != nil {
		slog.Warn("workbuddy activity streak check failed (report OK)", "account_id", account.ID, "error", err)
	} else {
		result.StreakDays = state.days()
		if result.StreakDays == 0 {
			slog.Warn("workbuddy activity report OK but streak.days=0 (silent drop?)", "account_id", account.ID)
		} else {
			slog.Info("workbuddy activity streak checked", "account_id", account.ID, "days", result.StreakDays)
		}
	}
	// 无猫账号对话量刚补满 → 立即重试领养（豁免当日防抖，就地闭环 first_buddy 门槛）。
	s.workbuddyTravelAdoptForce(ctx, account)
	// 连登奖励链（CN-only；失败不拖累上报：异常只记日志）。
	s.claimGrowthRewards(ctx, account, result)
	result.Success = true
	s.persistWorkbuddyActivitySnapshot(ctx, account, result)
	return result
}

// claimGrowthRewards 连登奖励链（仅 CN，global 整链跳过）：
// 礼包/补偿（静默）→ 读 streak/redemption 状态 → 补签（昨日漏签且有卡，补后重读状态）→
// 挑最高达标未领档 redeem → 按 CST 自然日标记 → 抽奖（chances>0 才 draw）。
func (s *WorkBuddyTasksService) claimGrowthRewards(ctx context.Context, account *Account, result *WorkBuddyActivityRunResult) {
	// global 门控：国际版连登链证据不足（新账号 streak 返回 500），不发起任何领取类调用。
	if account.GetWorkbuddyRealm() != "cn" {
		return
	}
	if s.rewardClaimedToday(account) {
		return // 当日已领过一轮，跳过（按天幂等）
	}
	// 0. 礼包/补偿（每号一次 / 有则领的幂等写，业务错误静默）。
	s.claimGrowthBonus(ctx, account, result)
	state, err := s.workbuddyGrowthState(ctx, account)
	if err != nil {
		slog.Warn("workbuddy activity reward-state failed", "account_id", account.ID, "error", err)
		return
	}
	// 0.5 补签保连登：昨日漏签且有卡 → 补签后重读状态（吃恢复后的天数挑档）。
	if s.makeupYesterday(ctx, account) {
		if refreshed, err := s.workbuddyGrowthState(ctx, account); err == nil {
			state = refreshed
		} else {
			slog.Warn("workbuddy activity reward-state after makeup failed", "account_id", account.ID, "error", err)
		}
	}
	days := state.days()
	if result.StreakDays == 0 {
		result.StreakDays = days // 自检失败时用奖励链读数补齐回执
	}
	tier := workbuddyEligibleTier(days, &state.Redemption)
	if tier == "" {
		// 无新达标档位：不动写接口（正常态，不刷 WARN），也不标记——下次上行照旧可领。
		s.claimGrowthLottery(ctx, account, result)
		return
	}
	res, err := s.workbuddyGrowthRedeem(ctx, account, tier, workbuddyTasksClientToken("redeem-"+tier))
	switch {
	case err == nil:
		result.RedeemTier = tier
		if res != nil {
			// 累加而非覆盖：礼包/补偿到账已在前面累计（同日多来源积分合一回执）。
			result.CreditGranted += int64(res.CreditGranted)
		}
		slog.Info("workbuddy activity redeem ok",
			"account_id", account.ID, "tier", tier, "credit", result.CreditGranted)
	case workbuddyTasksIsRedeemAlreadyClaimed(err) || workbuddyTasksIsRedeemNotEnoughDays(err):
		slog.Info("workbuddy activity redeem skipped (already claimed or days not enough)",
			"account_id", account.ID, "tier", tier)
	default:
		slog.Warn("workbuddy activity redeem failed", "account_id", account.ID, "tier", tier, "error", err)
	}
	// 无论 redeem 成败都标记当日已处理：领取类各状态当日不再重试，避免对上游重复写
	// （成功→无需再领；失败→次日自然日重置/上游幂等兜底）。
	s.markRewardClaimed(ctx, account, workbuddyClaimScopeReward)
	s.claimGrowthLottery(ctx, account, result)
}

// claimGrowthBonus 新手礼包 + 活动补偿领取：两者都是幂等写（已领/无可领返回业务错误），
// 错误静默不刷 WARN（绝大多数号早已领过，无法与真错误可靠区分）；成功到账累计入回执。
func (s *WorkBuddyTasksService) claimGrowthBonus(ctx context.Context, account *Account, result *WorkBuddyActivityRunResult) {
	if credit, err := s.workbuddyGrowthClaimGift(ctx, account); err == nil && credit > 0 {
		slog.Info("workbuddy activity gift claimed", "account_id", account.ID, "credit", credit)
		result.CreditGranted += credit
	}
	if credit, err := s.workbuddyGrowthClaimCompensation(ctx, account); err == nil && credit > 0 {
		slog.Info("workbuddy activity compensation claimed", "account_id", account.ID, "credit", credit)
		result.CreditGranted += credit
	}
}

// makeupYesterday 昨日漏签且有补签卡时自动补签（保住连登连续天数）。
// 判据链：heatmap 昨日格 score==0（且该日格存在）→ makeup_cards.balance>0 →
// POST makeup-cards/use {"target_date":昨日}。无卡/无漏签/无该日格/查询失败均静默返回 false；
// 补签成功返回 true（调用方重读 streak 天数挑档）。
//
// 幂等：补签吃的是不可再生资产（补签卡），且判据是「只读的 heatmap」——写后
// 上游聚合有延迟时，同一自然日内第二次进入会再吃一张卡。因此补签成功后必须把
// 「昨日已补过」持久化到 Extra（scope=makeup，值=昨日日期）并前置短路；不依赖
// 奖励链的按日标记（tier=="" 分支会不标记就 return，两者语义不同）。
func (s *WorkBuddyTasksService) makeupYesterday(ctx context.Context, account *Account) bool {
	yesterday := workbuddyTasksYesterday(time.Now())
	if workbuddyTaskScopeClaimed(account, workbuddyClaimScopeMakeup, yesterday) {
		return false // 昨日已补过（内存/Extra 持久标记）：不再消耗补签卡
	}
	cells, err := s.workbuddyGrowthHeatmap(ctx, account)
	if err != nil {
		return false // 只读判据失败：静默（每日重试，无写风险）
	}
	score, ok := workbuddyHeatmapDayScore(cells, yesterday)
	if !ok || score != 0 {
		return false // 昨日有分或无判据：无需补签
	}
	state, err := s.workbuddyGrowthState(ctx, account)
	if err != nil || state.MakeupCards.Balance <= 0 {
		return false // 无卡或查询失败：静默（次日再判）
	}
	if err := s.workbuddyGrowthUseMakeupCard(ctx, account, yesterday); err != nil {
		slog.Warn("workbuddy activity makeup failed", "account_id", account.ID, "date", yesterday, "error", err)
		return false
	}
	// 标记必须在返回之前：写操作已发生，哪怕调用方后续抛错也不能重入。
	s.persistTaskClaimMark(ctx, account, workbuddyClaimScopeMakeup, yesterday)
	slog.Info("workbuddy activity makeup ok", "account_id", account.ID, "date", yesterday)
	return true
}

// workbuddyEligibleTier 按当前连登天数挑选「尚未领取且达标」的最高档位。
// tiers 按 days 升序（上游形态），从高到低挑第一个 days>=spec.days && !claimed。
// 返回 "" 表示无可领档（未达标或全部已领），调用方据此跳过 redeem（正常态）。
func workbuddyEligibleTier(days int, rs *workbuddyRedemptionStatus) string {
	if rs == nil {
		return ""
	}
	for i := len(rs.Tiers) - 1; i >= 0; i-- {
		spec := rs.Tiers[i]
		if days >= spec.Days && !rs.claimed(spec.Tier) {
			return spec.Tier
		}
	}
	return ""
}

// claimGrowthLottery 消耗连登奖励赠与的抽奖次数：chances>0 才 draw；无次数跳过
// （400 insufficient 正常态静默）；抽奖未开启（400 lottery disabled）静默。
func (s *WorkBuddyTasksService) claimGrowthLottery(ctx context.Context, account *Account, result *WorkBuddyActivityRunResult) {
	chances, err := s.workbuddyGrowthLotteryChances(ctx, account)
	if err != nil {
		slog.Warn("workbuddy activity lottery chances failed", "account_id", account.ID, "error", err)
		return
	}
	if chances <= 0 {
		return // 无次数不消耗、不刷 WARN（正常态）
	}
	prize, err := s.workbuddyGrowthLotteryDraw(ctx, account, workbuddyTasksClientToken("draw"))
	switch {
	case err == nil:
		result.LotteryDrawn = true
		slog.Info("workbuddy activity lottery drawn", "account_id", account.ID, "prize", prize)
	case workbuddyTasksIsLotteryNoChance(err) || workbuddyTasksIsLotteryDisabled(err):
		slog.Info("workbuddy activity lottery skipped (no chances or disabled)", "account_id", account.ID)
	default:
		slog.Warn("workbuddy activity lottery draw failed", "account_id", account.ID, "error", err)
	}
}

// runTravelForAccount 单账号旅行巡检状态机：buddy/info → 无猫走领养（受按日防抖）→
// 有猫查 status：arrived→claim（record_id==0 跳过）、idle→depart（daily_limit_reached 跳过）、
// traveling→skip、其他→skip。单趟最多一个动作。
func (s *WorkBuddyTasksService) runTravelForAccount(ctx context.Context, account *Account) *WorkBuddyTravelRunResult {
	result := &WorkBuddyTravelRunResult{
		Realm: account.GetWorkbuddyRealm(),
		RanAt: time.Now().Unix(),
	}
	// global 门控：国际版无猫猫旅行体系，不发起任何上游调用（避免风控）。
	if result.Realm != "cn" {
		result.Action = "skip"
		result.Detail = "global realm has no buddy travel system"
		return result
	}
	if err := s.credits.ensureWorkbuddyBillingToken(ctx, account); err != nil {
		result.Action = "skip"
		result.Detail = err.Error()
		s.persistWorkbuddyTravelSnapshot(ctx, account, result)
		return result
	}
	buddy, err := s.workbuddyBuddyInfo(ctx, account)
	if err != nil {
		result.Action = "skip"
		result.Detail = "buddy-info: " + err.Error()
		slog.Warn("workbuddy travel buddy-info failed", "account_id", account.ID, "error", err)
		s.persistWorkbuddyTravelSnapshot(ctx, account, result)
		return result
	}
	if buddy == nil {
		result.Action = "skip"
		s.workbuddyTravelAdopt(ctx, account, result)
		s.persistWorkbuddyTravelSnapshot(ctx, account, result)
		return result
	}
	result.BuddyName = buddy.Name
	state, err := s.workbuddyTravelStatus(ctx, account)
	if err != nil {
		result.Action = "skip"
		result.Detail = "status: " + err.Error()
		slog.Warn("workbuddy travel status failed", "account_id", account.ID, "error", err)
		s.persistWorkbuddyTravelSnapshot(ctx, account, result)
		return result
	}
	result.State = state.State
	switch state.State {
	case workbuddyTasksTravelStateArrived:
		result.Action = "claim"
		s.workbuddyTravelClaimOne(ctx, account, state, result)
	case workbuddyTasksTravelStateIdle:
		result.Action = "depart"
		s.workbuddyTravelDepartOne(ctx, account, state, result)
	case workbuddyTasksTravelStateTraveling:
		result.Action = "skip"
		result.Detail = fmt.Sprintf("traveling record=%d", state.RecordID)
	default:
		result.Action = "skip"
		result.Detail = fmt.Sprintf("unknown state %q", state.State)
	}
	s.persistWorkbuddyTravelSnapshot(ctx, account, result)
	return result
}

// workbuddyTravelAdopt 旅行巡检时领养：先同意协议（幂等）再 buddy/first；
// 受 adoptTried 当日防抖约束（门槛未达的账号当日不再重试）。
func (s *WorkBuddyTasksService) workbuddyTravelAdopt(ctx context.Context, account *Account, result *WorkBuddyTravelRunResult) {
	if s.adoptTriedToday(account.ID) {
		result.Detail = "adopt skipped (tried today)"
		return
	}
	if err := s.workbuddyBuddyAgreement(ctx, account); err != nil {
		result.Detail = "agreement: " + err.Error()
		slog.Warn("workbuddy travel agreement failed", "account_id", account.ID, "error", err)
		return
	}
	if err := s.workbuddyBuddyFirst(ctx, account); err != nil {
		if workbuddyTasksIsBuddyTaskIncomplete(err) {
			s.markAdoptTried(account.ID)
			result.Detail = "adopt skipped (conversation threshold not reached, retry tomorrow)"
			return
		}
		result.Detail = "adopt: " + err.Error()
		slog.Warn("workbuddy travel adopt failed", "account_id", account.ID, "error", err)
		return
	}
	result.Action = "adopt"
	slog.Info("workbuddy travel adopt ok", "account_id", account.ID)
}

// workbuddyTravelAdoptForce 活跃上报补满对话量后领养：豁免 adoptTried 当日防抖
// （门槛刚达成的新状态，不算对上游重试轰炸）；有猫时直接跳过。
// 仅 400 + first_buddy 门槛错误置 adoptTried（其余失败不标记，允许后续重试）。
func (s *WorkBuddyTasksService) workbuddyTravelAdoptForce(ctx context.Context, account *Account) {
	buddy, err := s.workbuddyBuddyInfo(ctx, account)
	if err != nil {
		slog.Warn("workbuddy activity buddy-info failed", "account_id", account.ID, "error", err)
		return
	}
	if buddy != nil {
		return // 已有猫，无需领养
	}
	if err := s.workbuddyBuddyAgreement(ctx, account); err != nil {
		slog.Warn("workbuddy activity agreement failed", "account_id", account.ID, "error", err)
		return
	}
	err = s.workbuddyBuddyFirst(ctx, account)
	switch {
	case err == nil:
		slog.Info("workbuddy activity adopt ok (+300 credits)", "account_id", account.ID)
	case workbuddyTasksIsBuddyTaskIncomplete(err):
		s.markAdoptTried(account.ID)
		slog.Info("workbuddy activity adopt skipped (conversation threshold not reached)", "account_id", account.ID)
	default:
		slog.Warn("workbuddy activity adopt failed", "account_id", account.ID, "error", err)
	}
}

// workbuddyTravelDepartOne 空闲且未达当日上限时派出（每日 1 次，CST 00:00 重置）。
func (s *WorkBuddyTasksService) workbuddyTravelDepartOne(ctx context.Context, account *Account, state *workbuddyTravelState, result *WorkBuddyTravelRunResult) {
	if state.DailyLimitReached {
		result.Action = "skip"
		result.Detail = "daily limit reached"
		return
	}
	if err := s.workbuddyTravelDepart(ctx, account, workbuddyTasksTravelLocationID); err != nil {
		result.Detail = "depart: " + err.Error()
		slog.Warn("workbuddy travel depart failed", "account_id", account.ID, "error", err)
		return
	}
	result.Success = true
	slog.Info("workbuddy travel depart ok", "account_id", account.ID, "location", workbuddyTasksTravelLocationID)
}

// workbuddyTravelClaimOne 到站领奖（必须带 record_id；record_id==0 跳过）。
func (s *WorkBuddyTasksService) workbuddyTravelClaimOne(ctx context.Context, account *Account, state *workbuddyTravelState, result *WorkBuddyTravelRunResult) {
	if state.RecordID == 0 {
		result.Action = "skip"
		result.Detail = "arrived but no record_id"
		return
	}
	reward, err := s.workbuddyTravelClaim(ctx, account, state.RecordID)
	if err != nil {
		result.Detail = fmt.Sprintf("claim record=%d: %v", state.RecordID, err)
		slog.Warn("workbuddy travel claim failed", "account_id", account.ID, "record", state.RecordID, "error", err)
		return
	}
	result.Success = true
	result.RewardCredit = reward
	slog.Info("workbuddy travel claim ok", "account_id", account.ID, "record", state.RecordID, "reward", reward)
}

// persistWorkbuddyActivitySnapshot 把活跃上报快照写入 account.Extra（失败只告警不阻断）。
func (s *WorkBuddyTasksService) persistWorkbuddyActivitySnapshot(ctx context.Context, account *Account, result *WorkBuddyActivityRunResult) {
	if s.accountRepo == nil || account == nil || result == nil {
		return
	}
	snapshot := workbuddyActivitySnapshot{
		Date:          workbuddyTasksToday(time.Now()),
		StreakDays:    result.StreakDays,
		Reports:       result.Reports,
		RewardTier:    result.RedeemTier,
		CreditGranted: result.CreditGranted,
		LotteryDrawn:  result.LotteryDrawn,
		RanAt:         result.RanAt,
		OK:            result.Success,
	}
	// 快照必须在「调度 ctx 已取消」时仍能落库：redeem 可能已经在上游成功发放，此时
	// 丢记录等于丢掉「已领过」的唯一本地痕迹（stopCh 一关、批量预算一到就会踩到）。
	persistCtx, cancel := context.WithTimeout(persistDetachedCtx(ctx), workbuddyTasksPersistTimeout)
	defer cancel()
	if err := s.accountRepo.UpdateExtra(persistCtx, account.ID, map[string]any{workbuddyTasksActivityExtraKey: snapshot}); err != nil {
		slog.Warn("workbuddy activity snapshot persist failed", "account_id", account.ID, "error", err)
	}
}

// persistWorkbuddyTravelSnapshot 把旅行巡检快照写入 account.Extra（失败只告警不阻断）。
func (s *WorkBuddyTasksService) persistWorkbuddyTravelSnapshot(ctx context.Context, account *Account, result *WorkBuddyTravelRunResult) {
	if s.accountRepo == nil || account == nil || result == nil {
		return
	}
	snapshot := workbuddyTravelSnapshot{
		Date:         workbuddyTasksToday(time.Now()),
		Action:       result.Action,
		State:        result.State,
		BuddyName:    result.BuddyName,
		RewardCredit: result.RewardCredit,
		RanAt:        result.RanAt,
		OK:           result.Success,
	}
	// 同上：旅行快照（含已领奖 record 的事实）不得随调度 ctx 一起被掉。
	persistCtx, cancel := context.WithTimeout(persistDetachedCtx(ctx), workbuddyTasksPersistTimeout)
	defer cancel()
	if err := s.accountRepo.UpdateExtra(persistCtx, account.ID, map[string]any{workbuddyTasksTravelExtraKey: snapshot}); err != nil {
		slog.Warn("workbuddy travel snapshot persist failed", "account_id", account.ID, "error", err)
	}
}

// runActivityOnce 批量活跃上报：枚举全部 WorkBuddy 账号逐个执行（账号间固定间隔），
// 禁用/无凭据账号跳过；单账号失败只记 WARN 不影响遍历。
func (s *WorkBuddyTasksService) runActivityOnce(ctx context.Context) {
	s.runBatch(ctx, false)
}

// runTravelOnce 批量旅行巡检（仅 CN 账号实际执行）。
func (s *WorkBuddyTasksService) runTravelOnce(ctx context.Context) {
	s.runBatch(ctx, true)
}

// runBatch 批量遍历公共实现：travel=false 走活跃上报，travel=true 走旅行巡检。
// 账号筛选对齐签到服务（禁用/非 workbuddy/国际版/无凭据跳过）；
// 账号间 workbuddyTasksAccountDelay 可取消等待（停机不睡满，剩余账号下轮再跑）。
func (s *WorkBuddyTasksService) runBatch(ctx context.Context, travel bool) {
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformWorkbuddy)
	if err != nil {
		slog.Warn("workbuddy tasks list accounts failed", "travel", travel, "error", err)
		return
	}
	attempted, okN, failN, skipN := 0, 0, 0, 0
	first := true
	for i := range accounts {
		if ctx.Err() != nil {
			break
		}
		account := &accounts[i]
		if !account.IsWorkbuddy() || !account.IsActive() {
			skipN++
			continue
		}
		// 活跃上报 CN+global 都发（国际版 /v2/report 可用，点亮连登）；
		// 旅行仅 CN（国际版无猫猫旅行体系）。
		if travel && account.GetWorkbuddyRealm() != "cn" {
			skipN++
			continue
		}
		creds := account.GetWorkbuddyCredentials()
		if strings.TrimSpace(creds.AccessToken) == "" && strings.TrimSpace(creds.RefreshToken) == "" {
			skipN++
			continue
		}
		if !first {
			if !workbuddyTasksSleepCtx(ctx, workbuddyTasksAccountDelay) {
				break // 优雅停机：不等限速睡满，剩余账号下轮再跑
			}
		}
		first = false
		attempted++
		// 必须与手动入口（RunActivity/RunTravel）共用同一 singleflight key：否则
		// 「定时批次」与「管理员点击」之间、activity 与 travel 两个 loop 之间零互斥，
		// 同账号同时刻会双发 redeem/派出的领奖（旧实现里只有手动包了 flight）。
		if travel {
			result := s.runTravelFlight(ctx, account)
			switch {
			case result == nil:
				skipN++ // ctx 已取消：不计失败，剩余账号下轮再跑
			case result.Success:
				okN++
			case result.Detail != "":
				failN++
			}
			continue
		}
		result := s.runActivityFlight(ctx, account)
		switch {
		case result == nil:
			skipN++
		case result.Success:
			okN++
		default:
			failN++
		}
	}
	if attempted > 0 {
		slog.Info("workbuddy tasks batch done",
			"task", map[bool]string{true: "travel", false: "activity"}[travel],
			"total", len(accounts), "attempted", attempted, "ok", okN, "fail", failN, "skipped", skipN)
	}
}

// runActivityFlight 批次路径下的活跃上报：与手动入口 RunActivity **共用同一
// singleflight key**，从而消除「定时批次 vs 管理员点击 vs 两个 loop 之间」零互斥
// 导致同账号同时刻双发 redeem/领奖的窗口（旧实现里只有手动包了 flight，runBatch
// 直接调 *ForAccount）。
// 调用方 ctx 取消/超时时返回 nil（批次会因此提前跳出，不记成失败）。
func (s *WorkBuddyTasksService) runActivityFlight(ctx context.Context, account *Account) *WorkBuddyActivityRunResult {
	resultCh := s.flight.DoChan(workbuddyActivityFlightKey(account.ID), func() (any, error) {
		runCtx, cancel := context.WithTimeout(ctx, workbuddyTasksActivityTimeout)
		defer cancel()
		return s.runActivityForAccount(runCtx, account), nil
	})
	select {
	case <-ctx.Done():
		return nil
	case flightResult := <-resultCh:
		result, _ := flightResult.Val.(*WorkBuddyActivityRunResult)
		return result
	}
}

// runTravelFlight 批次路径下的旅行巡检：与 RunTravel 共用 key，理由同上。
func (s *WorkBuddyTasksService) runTravelFlight(ctx context.Context, account *Account) *WorkBuddyTravelRunResult {
	resultCh := s.flight.DoChan(workbuddyTravelFlightKey(account.ID), func() (any, error) {
		runCtx, cancel := context.WithTimeout(ctx, workbuddyTasksTravelTimeout)
		defer cancel()
		return s.runTravelForAccount(runCtx, account), nil
	})
	select {
	case <-ctx.Done():
		return nil
	case flightResult := <-resultCh:
		result, _ := flightResult.Val.(*WorkBuddyTravelRunResult)
		return result
	}
}

// 两套任务的 singleflight key（手动与批次必须一致，否则互斥失效）。
func workbuddyActivityFlightKey(accountID int64) string {
	return "workbuddy_activity:" + strconv.FormatInt(accountID, 10)
}

func workbuddyTravelFlightKey(accountID int64) string {
	return "workbuddy_travel:" + strconv.FormatInt(accountID, 10)
}

// adoptTriedToday 该账号当日是否已判定领养门槛未达（CST 自然日）。
func (s *WorkBuddyTasksService) adoptTriedToday(accountID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adoptTried[strconv.FormatInt(accountID, 10)] == workbuddyTasksToday(time.Now())
}

// markAdoptTried 记录该账号当日已尝试领养且未过门槛。
func (s *WorkBuddyTasksService) markAdoptTried(accountID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adoptTried[strconv.FormatInt(accountID, 10)] = workbuddyTasksToday(time.Now())
}

// rewardClaimedToday 该账号当日是否已处理过连登奖励领取（CST 自然日）。
//
// 判定顺序：内存快路径 → account.Extra 持久化标记。仅靠内存会在发布/重启/
// OOM 后归零，导致同一自然日内第二次跑完整奖励链（redeem/礼包/抽奖/补签），
// 幂等责任全部外包给上游 409；持久化标记让「当日已处理」这一事实在进程重启后
// 仍然成立。内存仍然保留：它防的是「同一次批次内 Extra 是旧读数」的回跳。
func (s *WorkBuddyTasksService) rewardClaimedToday(account *Account) bool {
	if account == nil {
		return false
	}
	today := workbuddyTasksToday(time.Now())
	s.mu.Lock()
	inMemory := s.rewardClaimed[strconv.FormatInt(account.ID, 10)] == today
	s.mu.Unlock()
	if inMemory {
		return true
	}
	return workbuddyTaskScopeClaimed(account, workbuddyClaimScopeReward, today)
}

// markRewardClaimed 记录该账号当日已处理连登奖励领取（内存 + Extra 双写）。
func (s *WorkBuddyTasksService) markRewardClaimed(ctx context.Context, account *Account, scope string) {
	if account == nil {
		return
	}
	today := workbuddyTasksToday(time.Now())
	if scope == "" {
		scope = workbuddyClaimScopeReward
	}
	if scope == workbuddyClaimScopeReward {
		s.mu.Lock()
		s.rewardClaimed[strconv.FormatInt(account.ID, 10)] = today
		s.mu.Unlock()
	}
	s.persistTaskClaimMark(ctx, account, scope, today)
}

// persistTaskClaimMark 把「作用域 → 自然日」标记写进 account.Extra。
//
// 必须用脱离调度 ctx 的 context（persistDetachedCtx）：这个标记是「已经打过上游
// 写接口」的客观事实，如果随 stopCh/超时一起被掉，下次运行就会重发，直接踩回
// 重复领取；写失败只告警（不阻断任务主流程，与快照口径一致）。
func (s *WorkBuddyTasksService) persistTaskClaimMark(ctx context.Context, account *Account, scope, day string) {
	if s == nil || s.accountRepo == nil || account == nil || scope == "" {
		return
	}
	claims := readWorkbuddyTaskClaims(account)
	if claims[scope] == day {
		return // 已是目标值：不产生无意义的 DB 写。
	}
	claims[scope] = day
	persistCtx, cancel := context.WithTimeout(persistDetachedCtx(ctx), workbuddyTasksPersistTimeout)
	defer cancel()
	if err := s.accountRepo.UpdateExtra(persistCtx, account.ID, map[string]any{workbuddyTasksClaimExtraKey: claims}); err != nil {
		slog.Warn("workbuddy task claim mark persist failed",
			"account_id", account.ID, "scope", scope, "date", day, "error", err)
		return
	}
	// 回写内存对象，使同一批次内的后续读取能看到最新标记（避免同腿重发）。
	if account.Extra == nil {
		account.Extra = map[string]any{}
	}
	account.Extra[workbuddyTasksClaimExtraKey] = claims
}

// readWorkbuddyTaskClaims 从 account.Extra 读幂等标记表（缺失/格式异常返回可写副本）。
func readWorkbuddyTaskClaims(account *Account) map[string]string {
	claims := make(map[string]string, 2)
	if account == nil || account.Extra == nil {
		return claims
	}
	raw, ok := account.Extra[workbuddyTasksClaimExtraKey]
	if !ok || raw == nil {
		return claims
	}
	switch typed := raw.(type) {
	case map[string]string:
		for k, v := range typed {
			claims[k] = v
		}
	case map[string]any:
		for k, v := range typed {
			if day, isStr := v.(string); isStr {
				claims[k] = day
			}
		}
	}
	return claims
}

// workbuddyTaskScopeClaimed 该作用域在指定自然日是否已标记过。
func workbuddyTaskScopeClaimed(account *Account, scope, day string) bool {
	if account == nil || scope == "" || day == "" {
		return false
	}
	return readWorkbuddyTaskClaims(account)[scope] == day
}

// persistDetachedCtx 给「必须落库的客观事实」用：脱离取消链，仅受自身超时约束。
func persistDetachedCtx(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}
