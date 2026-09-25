package service

// qoder_campaign_service.go Qoder 账号积分查询与「每日领取活动 Credits」服务。
//
// 与 WorkBuddy 那套（workbuddy_credits_service.go）同构，但上游形态完全不同：
// Qoder 的活动体系是 campaigns 列表 + 按 campaignId 领取，而不是单一签到端点。
//
// 上游接口（openapi 域，纯 Bearer，无 COSY 请求签名）：
//   - 积分余额：GET {openapi}/api/v2/quota/usage
//     → {userQuota, addOnQuota, dedicatedResourcePackages[]}（数值为浮点，单位 credits）。
//   - 活动状态：GET {openapi}/sash/api/v1/me/campaigns
//     → {showCampaign, claimable, campaigns:[{campaignId, campaignKey, actionType,
//        claimStatus, startAt, endAt, benefit:{kind,amount,validity}, placements}]}。
//   - 领取活动权益：POST {openapi}/sash/api/v1/me/campaigns/{campaignId}/claim
//     → {grantId, status:"CLAIMED", replayed, benefit, claimedAt, expiresAt}。
//     上游对同一轮重复领取天然幂等（replayed=true 且 claimedAt 不变，不再发钱），
//     因此本模块不需要分布式锁即可安全多实例部署。
//
// ⚠ 客户端类型定向（实测差分矩阵，2026-09-24）：服务端按 cosy-clienttype 决定是否
// 下发活动——5（IDE，即 chat 签名链路使用的值）与 6 一律返回空 campaigns，只有 10
// （Qoder 桌面端）才下发「每日领取 100 Credits」。文档亦写明「领取渠道仅限桌面端」。
// 因此本文件用 applyQoderCampaignHeaders 单独发一套桌面端身份头，**不改** chat 的
// COSY 签名链路（qoderCosyClientType 保持 5），避免影响既有转发行为。
//
// 「轮次日」口径：活动每天 10:00（UTC+8）开放新一轮、窗口持续到次日 10:00，与
// WorkBuddy 的自然日（0 点分界）不同。快照 date 字段按 10 点分界归属（见
// qoderCampaignRoundDate），否则 0-10 点之间会把昨天的领取误判成今天没领。
//
// 结果快照写入 account.Extra（qoder_credits / qoder_checkin），供管理列表免探测
// 直接渲染；领取记录带当轮「成功优先」合并语义（后续失败重试不覆盖已成功记录）。
//
// 并发：同一账号的查询/领取分别由 singleflight 合并（列表页自动探测、手动连点、
// 定时任务三方并发时只打一次上游）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"golang.org/x/sync/singleflight"
)

const (
	// qoderCampaignTimeout 单次 openapi 上游调用（余额/活动/领取）的总时长上限。
	qoderCampaignTimeout = 20 * time.Second

	// qoderCheckinTimeout 单次领取编排（活动列表 → 领取 → 余额刷新）的总时长上限。
	qoderCheckinTimeout = 60 * time.Second

	// qoderCampaignMaxBodyBytes openapi 响应体读取上限（活动列表带双语文案，2MB 足够）。
	qoderCampaignMaxBodyBytes int64 = 2 << 20

	// qoderCreditsExtraKey / qoderCheckinExtraKey 是 account.Extra 的快照键。
	qoderCreditsExtraKey = "qoder_credits"
	qoderCheckinExtraKey = "qoder_checkin"

	// qoderCampaignClientType / qoderCampaignClientVersion 是活动接口的桌面端身份。
	// 10 = Qoder 桌面端（唯一能拿到每日活动的取值，见文件头差分矩阵）；
	// 版本号取自实测通过的桌面端发布版（version 本身不参与资格判定，仅需与桌面端
	// 形态自洽，避免 clientType=10 + IDE 版本这种矛盾组合被风控）。
	qoderCampaignClientType    = "10"
	qoderCampaignClientVersion = "0.3.4"
	// qoderCampaignMachineOS / Hostname 是补齐的桌面端环境头。服务端实测不校验其
	// 真实性（探针用固定值即通过），保持常量以让同一部署的指纹稳定。
	qoderCampaignMachineOS       = "Windows NT 10.0"
	qoderCampaignMachineHostname = "QoderDesktop"

	// qoderCampaignHour 活动刷新小时（UTC+8 每日 10:00 开放新一轮）。
	qoderCampaignHour = 10

	// qoderClaimStatusClaimable / qoderClaimStatusClaimed 是上游活动条目的领取态字面值。
	qoderClaimStatusClaimable = "CLAIMABLE"
	qoderClaimStatusClaimed   = "CLAIMED"

	// qoderActionTypeClaimBenefit 是可领取类活动（区别于 VIEW_DETAILS 纯展示）。
	qoderActionTypeClaimBenefit = "CLAIM_BENEFIT"
)

// 领取状态机（与前端契约一致）。
const (
	// QoderCheckinStatusOK 本轮领取成功（真实到账）。
	QoderCheckinStatusOK = "ok"
	// QoderCheckinStatusAlready 本轮已领取过（上游幂等或活动已是 CLAIMED）。
	QoderCheckinStatusAlready = "already"
	// QoderCheckinStatusFail 上游/网络失败（可重试）。
	QoderCheckinStatusFail = "fail"
	// QoderCheckinStatusSkipped 账号不具备参与资格或活动未下发（正常态，不重试）。
	QoderCheckinStatusSkipped = "skipped"
)

// ---------------------------------------------------------------------------
// 上游响应 DTO
// ---------------------------------------------------------------------------

// qoderQuotaUsageResp /api/v2/quota/usage 响应（camelCase，数值是浮点）。
type qoderQuotaUsageResp struct {
	UserID               string  `json:"userId"`
	UserType             string  `json:"userType"`
	UsageType            string  `json:"usageType"`
	TotalUsagePercentage float64 `json:"totalUsagePercentage"`
	IsQuotaExceeded      bool    `json:"isQuotaExceeded"`
	ExpiresAt            int64   `json:"expiresAt"`
	UserQuota            *struct {
		Total      float64 `json:"total"`
		Used       float64 `json:"used"`
		Remaining  float64 `json:"remaining"`
		Percentage float64 `json:"percentage"`
		Unit       string  `json:"unit"`
	} `json:"userQuota"`
	AddOnQuota *struct {
		Total      float64 `json:"total"`
		Used       float64 `json:"used"`
		Remaining  float64 `json:"remaining"`
		Percentage float64 `json:"percentage"`
		Unit       string  `json:"unit"`
		DetailURL  string  `json:"detailUrl"`
	} `json:"addOnQuota"`
	DedicatedResourcePackages []struct {
		ID         string  `json:"id"`
		Name       string  `json:"name"`
		Total      float64 `json:"total"`
		Used       float64 `json:"used"`
		Remaining  float64 `json:"remaining"`
		Unit       string  `json:"unit"`
		Status     string  `json:"status"`
		Available  bool    `json:"available"`
		ExpiresAt  int64   `json:"expiresAt"`
		Percentage float64 `json:"percentage"`
	} `json:"dedicatedResourcePackages"`
}

// qoderCampaignsResp /sash/api/v1/me/campaigns 响应。
type qoderCampaignsResp struct {
	UID          string             `json:"uid"`
	ShowCampaign bool               `json:"showCampaign"`
	Claimable    bool               `json:"claimable"`
	CampaignURL  string             `json:"campaignUrl"`
	Campaigns    []qoderCampaignDTO `json:"campaigns"`
}

// qoderCampaignDTO 单个活动条目。actionType 为 CLAIM_BENEFIT 的才可领取。
type qoderCampaignDTO struct {
	CampaignID  string `json:"campaignId"`
	CampaignKey string `json:"campaignKey"`
	ActionType  string `json:"actionType"`
	ClaimStatus string `json:"claimStatus"`
	StartAt     int64  `json:"startAt"`
	EndAt       int64  `json:"endAt"`
	Benefit     *struct {
		Kind     string `json:"kind"`
		Amount   int64  `json:"amount"`
		Validity *struct {
			Mode string `json:"mode"`
			Days int    `json:"days"`
		} `json:"validity"`
	} `json:"benefit"`
	Placements []struct {
		Type    string `json:"type"`
		Content map[string]struct {
			Title       string `json:"title"`
			Description string `json:"description"`
		} `json:"content"`
	} `json:"placements"`
}

// qoderClaimResp campaigns/{id}/claim 响应（裸 JSON，无业务信封）。
// replayed=true 表示上游识别为重复领取、本轮未再发钱。
type qoderClaimResp struct {
	GrantID     string `json:"grantId"`
	Status      string `json:"status"`
	Replayed    bool   `json:"replayed"`
	CampaignID  string `json:"campaignId"`
	CampaignKey string `json:"campaignKey"`
	ClaimedAt   string `json:"claimedAt"`
	ExpiresAt   string `json:"expiresAt"`
	Benefit     *struct {
		Kind   string `json:"kind"`
		Amount int64  `json:"amount"`
	} `json:"benefit"`
}

// ---------------------------------------------------------------------------
// 对外的结果 / 快照结构
// ---------------------------------------------------------------------------

// QoderCreditPack 单个专属资源包摘要（dedicatedResourcePackages 条目）。
type QoderCreditPack struct {
	Name       string  `json:"name,omitempty"`
	Remain     float64 `json:"remain"`
	Used       float64 `json:"used"`
	Total      float64 `json:"total"`
	Status     string  `json:"status,omitempty"`
	ExpiresAt  int64   `json:"expires_at,omitempty"`
	Available  bool    `json:"available"`
	Title      string  `json:"title,omitempty"`
	Expiration string  `json:"expiration,omitempty"`
}

// QoderCreditsResult 积分 + 活动查询结果（管理端与前端单元格消费）。
type QoderCreditsResult struct {
	Success  bool   `json:"success"`
	Realm    string `json:"realm"`
	UserType string `json:"user_type,omitempty"`

	// 汇总余额 = 套餐内 + 资源包（活动赠送计入资源包）。
	Remaining float64 `json:"remaining"`
	Used      float64 `json:"used"`
	Total     float64 `json:"total"`

	PlanRemain  float64 `json:"plan_remain"`
	PlanTotal   float64 `json:"plan_total"`
	AddOnRemain float64 `json:"addon_remain"`
	AddOnTotal  float64 `json:"addon_total"`

	Packs    int               `json:"packs"`
	Packages []QoderCreditPack `json:"packages,omitempty"`

	QuotaExceeded bool  `json:"quota_exceeded"`
	PlanExpiresAt int64 `json:"plan_expires_at,omitempty"`
	FetchedAt     int64 `json:"fetched_at"`

	// 活动侧观测
	Claimable        bool   `json:"claimable"`
	ClaimAmount      int64  `json:"claim_amount,omitempty"`
	ClaimCampaignKey string `json:"claim_campaign_key,omitempty"`
	ClaimCampaignEnd int64  `json:"claim_campaign_end_at,omitempty"`
	TodayClaimStatus string `json:"today_claim_status,omitempty"`
	Round            string `json:"round,omitempty"`

	Error string `json:"error,omitempty"`
}

// QoderCheckinResult 领取结果（状态机：ok/already/fail/skipped）。
type QoderCheckinResult struct {
	Success   bool    `json:"success"`
	Status    string  `json:"status"`
	Detail    string  `json:"detail,omitempty"`
	Credits   float64 `json:"credits,omitempty"`
	Amount    int64   `json:"amount,omitempty"`
	GrantID   string  `json:"grant_id,omitempty"`
	ExpiresAt string  `json:"expires_at,omitempty"`
	Realm     string  `json:"realm,omitempty"`
	Round     string  `json:"round,omitempty"`
	CheckedAt int64   `json:"checked_at"`
}

// QoderCreditsSnapshot 写入 account.Extra 的积分快照。
type QoderCreditsSnapshot struct {
	Remaining     float64           `json:"remain"`
	Used          float64           `json:"used"`
	Total         float64           `json:"total"`
	PlanRemain    float64           `json:"plan_remain"`
	PlanTotal     float64           `json:"plan_total"`
	AddOnRemain   float64           `json:"addon_remain"`
	AddOnTotal    float64           `json:"addon_total"`
	Packs         int               `json:"packs"`
	Packages      []QoderCreditPack `json:"packages,omitempty"`
	UserType      string            `json:"user_type,omitempty"`
	QuotaExceeded bool              `json:"quota_exceeded,omitempty"`
	FetchedAt     int64             `json:"fetched_at"`
	Realm         string            `json:"realm"`
}

// QoderCheckinSnapshot 写入 account.Extra 的领取快照（按活动轮次归属）。
type QoderCheckinSnapshot struct {
	Round     string  `json:"round"`
	Status    string  `json:"status"`
	CheckedAt int64   `json:"checked_at"`
	Credits   float64 `json:"credits,omitempty"`
	Amount    int64   `json:"amount,omitempty"`
	GrantID   string  `json:"grant_id,omitempty"`
}

// ---------------------------------------------------------------------------
// 服务
// ---------------------------------------------------------------------------

// QoderCreditsService 查询 Qoder 账号积分余额并领取每日活动 Credits。
type QoderCreditsService struct {
	accountRepo  AccountRepository
	proxyRepo    ProxyRepository
	httpUpstream HTTPUpstream
	cfg          *config.Config
	flight       singleflight.Group

	// endpointsOverride 是 openapi 端点解析的测试替身挂载点（生产恒为 nil，
	// 走 resolveQoderEndpoints）。openapi 域刻意不受 credentials.base_url 中转
	// 覆盖（见 qoder.go 注释），单测无法用 httptest 直接拦到，因此需要这个缝。
	endpointsOverride func(account *Account) qoderEndpoints
}

// NewQoderCreditsService 构造 Qoder 积分/活动领取服务。
func NewQoderCreditsService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
) *QoderCreditsService {
	return &QoderCreditsService{
		accountRepo:  accountRepo,
		proxyRepo:    proxyRepo,
		httpUpstream: httpUpstream,
		cfg:          cfg,
	}
}

// qoderCampaignError openapi 业务错误（HTTP >= 400）。与传输层/解析层错误严格
// 区分：只有本类型参与「登录态失效」判定，网络抖动不得被记成幂等成功。
type qoderCampaignError struct {
	Status int
	Body   string
}

func (e *qoderCampaignError) Error() string {
	if e == nil {
		return "qoder campaign error"
	}
	return fmt.Sprintf("qoder openapi error: HTTP %d %s", e.Status, e.Body)
}

// qoderIsUnauthorizedError 报告是否为 401/403（令牌失效，官方客户端据此 invalidate）。
func qoderIsUnauthorizedError(err error) bool {
	var ce *qoderCampaignError
	return errors.As(err, &ce) && (ce.Status == http.StatusUnauthorized || ce.Status == http.StatusForbidden)
}

// loadQoderAccount 加载并校验 Qoder 账号。
func (s *QoderCreditsService) loadQoderAccount(ctx context.Context, accountID int64) (*Account, error) {
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusNotFound, "QODER_ACCOUNT_NOT_FOUND", "account not found: %v", err)
	}
	if account == nil {
		return nil, infraerrors.New(http.StatusNotFound, "QODER_ACCOUNT_NOT_FOUND", "account not found")
	}
	if !account.IsQoderPlatform() {
		return nil, infraerrors.New(http.StatusBadRequest, "QODER_INVALID_PLATFORM", "account is not a qoder account")
	}
	return account, nil
}

// QueryCredits 查询账号积分与活动状态（并落 extra 快照）。
// 同一账号的并发查询由 singleflight 合并。
func (s *QoderCreditsService) QueryCredits(ctx context.Context, accountID int64) (*QoderCreditsResult, error) {
	if s == nil || s.accountRepo == nil || s.httpUpstream == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "QODER_CREDITS_NOT_CONFIGURED", "qoder credits service is not configured")
	}
	account, err := s.loadQoderAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return s.queryCredits(ctx, account)
}

func (s *QoderCreditsService) queryCredits(ctx context.Context, account *Account) (*QoderCreditsResult, error) {
	key := "qoder_credits:" + strconv.FormatInt(account.ID, 10)
	resultCh := s.flight.DoChan(key, func() (any, error) {
		probeCtx, cancel := context.WithTimeout(context.Background(), qoderCampaignTimeout*2+5*time.Second)
		defer cancel()
		return s.queryCreditsForAccount(probeCtx, account), nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case flightResult := <-resultCh:
		if flightResult.Err != nil {
			return nil, flightResult.Err
		}
		result, ok := flightResult.Val.(*QoderCreditsResult)
		if !ok || result == nil {
			return nil, infraerrors.New(http.StatusInternalServerError, "QODER_CREDITS_RESULT_INVALID", "invalid qoder credits result")
		}
		cloned := *result
		return &cloned, nil
	}
}

// queryCreditsForAccount 探测单个账号（余额 + 活动），上游失败不返回 Go error 而是
// 回填 result.Error（管理端 200 + success=false，与 WorkBuddy/CNProvider 口径一致）。
func (s *QoderCreditsService) queryCreditsForAccount(ctx context.Context, account *Account) *QoderCreditsResult {
	result := &QoderCreditsResult{
		Realm:     account.GetQoderRealm(),
		Round:     qoderCampaignRoundDate(time.Now()),
		FetchedAt: time.Now().Unix(),
	}
	token, endpoints, err := s.qoderCampaignToken(ctx, account)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	quota, err := s.qoderFetchQuota(ctx, account, endpoints, token)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	s.applyQoderQuota(result, quota)
	// 活动列表是增强信息：拉取失败不得把已拿到的余额判定为失败。
	campaigns, campErr := s.qoderFetchCampaigns(ctx, account, endpoints, token)
	if campErr != nil {
		slog.Debug("qoder campaigns probe failed", "account_id", account.ID, "error", campErr)
		// 上游抖动时回退本地快照，避免把「本轮已领」显示成「未领」。
		if status := qoderCheckinStatusToday(account); status != "" {
			result.TodayClaimStatus = status
		}
	} else {
		s.applyQoderCampaigns(result, campaigns)
	}
	result.Success = true
	s.persistQoderCreditsSnapshot(ctx, account, result)
	return result
}

// applyQoderQuota 把上游余额映射到结果字段。
func (s *QoderCreditsService) applyQoderQuota(result *QoderCreditsResult, resp *qoderQuotaUsageResp) {
	result.UserType = resp.UserType
	result.QuotaExceeded = resp.IsQuotaExceeded
	result.PlanExpiresAt = resp.ExpiresAt
	if resp.UserQuota != nil {
		result.PlanRemain = resp.UserQuota.Remaining
		result.PlanTotal = resp.UserQuota.Total
		result.Used += resp.UserQuota.Used
		result.Total += resp.UserQuota.Total
		result.Remaining += resp.UserQuota.Remaining
	}
	if resp.AddOnQuota != nil {
		result.AddOnRemain = resp.AddOnQuota.Remaining
		result.AddOnTotal = resp.AddOnQuota.Total
		result.Used += resp.AddOnQuota.Used
		result.Total += resp.AddOnQuota.Total
		result.Remaining += resp.AddOnQuota.Remaining
	}
	for _, pkg := range resp.DedicatedResourcePackages {
		result.Packs++
		result.Packages = append(result.Packages, QoderCreditPack{
			Name:      pkg.Name,
			Remain:    pkg.Remaining,
			Used:      pkg.Used,
			Total:     pkg.Total,
			Status:    pkg.Status,
			ExpiresAt: pkg.ExpiresAt,
			Available: pkg.Available,
		})
	}
}

// applyQoderCampaigns 把活动列表映射到结果字段：找出 actionType=CLAIM_BENEFIT 且
// 窗口内（未过期）的条目。多种活动并存时（如每日 100 + 节日礼包），优先以存在
// CLAIMABLE 条目为准——否则会被一个已领条目遮住另一个可领条目。
// TodayClaimStatus 统一用前端领取状态机值（already），Claimable 单独表达“可点”。
func (s *QoderCreditsService) applyQoderCampaigns(result *QoderCreditsResult, resp *qoderCampaignsResp) {
	now := time.Now().Unix()
	result.Claimable = false
	result.TodayClaimStatus = ""
	var claimable, claimed *qoderCampaignDTO
	for i := range resp.Campaigns {
		campaign := &resp.Campaigns[i]
		if campaign.ActionType != qoderActionTypeClaimBenefit {
			continue
		}
		if campaign.EndAt != 0 && campaign.EndAt <= now {
			continue // 本轮窗口已过
		}
		switch campaign.ClaimStatus {
		case qoderClaimStatusClaimable:
			if claimable == nil {
				claimable = campaign
			}
		case qoderClaimStatusClaimed:
			if claimed == nil {
				claimed = campaign
			}
		}
	}
	target := claimable
	if target == nil {
		target = claimed
	}
	if target == nil {
		// 无下发的活动条目：账号不符合资格（企业版/设备级规则）或活动未上线。
		result.Claimable = resp.Claimable
		return
	}
	result.ClaimCampaignKey = target.CampaignKey
	result.ClaimCampaignEnd = target.EndAt
	if target.Benefit != nil {
		result.ClaimAmount = target.Benefit.Amount
	}
	if claimable != nil {
		result.Claimable = true
		return
	}
	result.TodayClaimStatus = QoderCheckinStatusAlready
}

// Checkin 手动触发指定账号的本轮领取（幂等；已领取返回 already）。
func (s *QoderCreditsService) Checkin(ctx context.Context, accountID int64) (*QoderCheckinResult, error) {
	if s == nil || s.accountRepo == nil || s.httpUpstream == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "QODER_CREDITS_NOT_CONFIGURED", "qoder credits service is not configured")
	}
	account, err := s.loadQoderAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return s.CheckinForAccount(ctx, account), nil
}

// CheckinForAccount 对已加载账号执行领取（singleflight 合并；周期任务与手动入口共用）。
func (s *QoderCreditsService) CheckinForAccount(ctx context.Context, account *Account) *QoderCheckinResult {
	if account == nil {
		return &QoderCheckinResult{Status: QoderCheckinStatusFail, Detail: "account is nil", CheckedAt: time.Now().Unix()}
	}
	key := "qoder_checkin:" + strconv.FormatInt(account.ID, 10)
	resultCh := s.flight.DoChan(key, func() (any, error) {
		checkinCtx, cancel := context.WithTimeout(context.Background(), qoderCheckinTimeout)
		defer cancel()
		return s.checkinForAccount(checkinCtx, account), nil
	})
	select {
	case <-ctx.Done():
		return &QoderCheckinResult{Status: QoderCheckinStatusFail, Detail: ctx.Err().Error(), CheckedAt: time.Now().Unix()}
	case flightResult := <-resultCh:
		if flightResult.Err != nil {
			return &QoderCheckinResult{Status: QoderCheckinStatusFail, Detail: flightResult.Err.Error(), CheckedAt: time.Now().Unix()}
		}
		result, ok := flightResult.Val.(*QoderCheckinResult)
		if !ok || result == nil {
			return &QoderCheckinResult{Status: QoderCheckinStatusFail, Detail: "invalid qoder checkin result", CheckedAt: time.Now().Unix()}
		}
		cloned := *result
		return &cloned
	}
}

// checkinForAccount 领取编排：活动列表 → 挑本轮 CLAIMABLE → claim → 刷新余额 → 落快照。
// 「无 CLAIMABLE 活动」是正常态（账号不符合资格 / 活动未下发 / 企业版），记 skipped
// 且零写请求，绝不重试刷上游。
func (s *QoderCreditsService) checkinForAccount(ctx context.Context, account *Account) *QoderCheckinResult {
	round := qoderCampaignRoundDate(time.Now())
	result := &QoderCheckinResult{
		Realm:     account.GetQoderRealm(),
		Round:     round,
		CheckedAt: time.Now().Unix(),
	}
	token, endpoints, err := s.qoderCampaignToken(ctx, account)
	if err != nil {
		result.Status = QoderCheckinStatusFail
		result.Detail = err.Error()
		return result
	}
	campaigns, err := s.qoderFetchCampaigns(ctx, account, endpoints, token)
	if err != nil {
		result.Status = QoderCheckinStatusFail
		result.Detail = err.Error()
		s.persistQoderCheckinSnapshot(ctx, account, result)
		return result
	}
	now := time.Now().Unix()
	var target *qoderCampaignDTO
	var alreadySeen *qoderCampaignDTO
	for i := range campaigns.Campaigns {
		c := &campaigns.Campaigns[i]
		if c.ActionType != qoderActionTypeClaimBenefit {
			continue
		}
		if c.EndAt != 0 && c.EndAt <= now {
			continue
		}
		switch c.ClaimStatus {
		case qoderClaimStatusClaimable:
			if target == nil {
				cp := *c
				target = &cp
			}
		case qoderClaimStatusClaimed:
			if alreadySeen == nil {
				cp := *c
				alreadySeen = &cp
			}
		}
	}
	if target == nil {
		if alreadySeen != nil {
			// 本轮已领：幂等成功语义，不再发写请求。
			result.Status = QoderCheckinStatusAlready
			result.Success = true
			result.Amount = benefitAmount(alreadySeen)
			result.ExpiresAt = benefitExpiryText(alreadySeen, now)
			s.refreshQoderCreditsAfterCheckin(ctx, account, result)
			s.persistQoderCheckinSnapshot(ctx, account, result)
			return result
		}
		// 没有可领活动：资格/下发问题，属正常态。
		result.Status = QoderCheckinStatusSkipped
		result.Detail = "no claimable campaign"
		return result
	}
	result.Amount = benefitAmount(target)

	claim, err := s.qoderClaimCampaign(ctx, account, endpoints, token, target.CampaignID)
	if err != nil {
		result.Status = QoderCheckinStatusFail
		result.Detail = err.Error()
		s.persistQoderCheckinSnapshot(ctx, account, result)
		return result
	}
	result.Success = true
	result.GrantID = claim.GrantID
	result.ExpiresAt = claim.ExpiresAt
	if claim.Benefit != nil && claim.Benefit.Amount > 0 {
		result.Amount = claim.Benefit.Amount
	}
	switch {
	case claim.Replayed:
		// 上游识别为重复领取（同一轮已发过）：幂等成功，不算失败。
		result.Status = QoderCheckinStatusAlready
	case strings.EqualFold(claim.Status, "CLAIMED"):
		result.Status = QoderCheckinStatusOK
	default:
		// 2xx 但状态非预期：按成功处理但留痕，便于发现上游语义变化。
		result.Status = QoderCheckinStatusOK
		result.Detail = "unexpected claim status: " + claim.Status
		slog.Warn("qoder campaign claim returned unexpected status",
			"account_id", account.ID, "status", claim.Status, "grant_id", claim.GrantID)
	}
	s.refreshQoderCreditsAfterCheckin(ctx, account, result)
	s.persistQoderCheckinSnapshot(ctx, account, result)
	return result
}

// refreshQoderCreditsAfterCheckin 领取后刷新余额（余额独立于领取结果，失败只留痕）。
func (s *QoderCreditsService) refreshQoderCreditsAfterCheckin(ctx context.Context, account *Account, result *QoderCheckinResult) {
	token, endpoints, err := s.qoderCampaignToken(ctx, account)
	if err != nil {
		return
	}
	quota, err := s.qoderFetchQuota(ctx, account, endpoints, token)
	if err != nil {
		slog.Debug("qoder credits refresh after claim failed", "account_id", account.ID, "error", err)
		return
	}
	probe := &QoderCreditsResult{Realm: result.Realm, Round: result.Round, FetchedAt: time.Now().Unix()}
	s.applyQoderQuota(probe, quota)
	probe.Success = true
	value := probe.Remaining
	result.Credits = value
	s.persistQoderCreditsSnapshot(ctx, account, probe)
}

// ---------------------------------------------------------------------------
// 上游传输
// ---------------------------------------------------------------------------

// qoderCampaignToken 解析可直接用于 openapi 业务接口的 Bearer 令牌与端点集合。
// dt- 设备令牌直用；仅有 PAT(pt-) 的账号先做一次 jobToken 交换（与 chat 链路同一实现）。
func (s *QoderCreditsService) qoderCampaignToken(ctx context.Context, account *Account) (string, qoderEndpoints, error) {
	endpoints := s.campaignEndpoints(account)
	creds := account.GetQoderCredentials()
	token := strings.TrimSpace(creds.AccessToken)
	if token == "" && (strings.TrimSpace(creds.PersonalToken) != "" || strings.TrimSpace(creds.RefreshToken) != "") {
		if err := s.ensureQoderJobToken(ctx, account); err != nil {
			return "", endpoints, fmt.Errorf("qoder jobToken exchange failed: %w", err)
		}
		token = strings.TrimSpace(account.GetQoderCredentials().AccessToken)
	}
	if token == "" {
		return "", endpoints, fmt.Errorf("qoder account %d has no usable access token", account.ID)
	}
	return token, endpoints, nil
}

// campaignEndpoints 解析活动/余额端点集合（生产按 realm + base_url 覆盖规则）。
func (s *QoderCreditsService) campaignEndpoints(account *Account) qoderEndpoints {
	if s != nil && s.endpointsOverride != nil {
		return s.endpointsOverride(account)
	}
	return resolveQoderEndpoints(account.GetQoderRealm(), account.GetQoderBaseURL())
}

// ensureQoderJobToken 账号级锁 + 锁内双检的 PAT 交换（独立于网关那份实现，
// 语义一致：并发只换一次，成功后经 persistAccountCredentials 持久化）。
func (s *QoderCreditsService) ensureQoderJobToken(ctx context.Context, account *Account) error {
	snapshot := strings.TrimSpace(account.GetQoderCredentials().AccessToken)
	lock := qoderJobTokenLock(account.ID)
	if err := lock.Lock(ctx); err != nil {
		return err
	}
	defer lock.Unlock()
	if strings.TrimSpace(account.GetQoderCredentials().AccessToken) != snapshot {
		return nil // 他人已完成交换
	}
	creds := account.GetQoderCredentials()
	token := strings.TrimSpace(creds.PersonalToken)
	if token == "" {
		token = strings.TrimSpace(creds.RefreshToken)
	}
	if token == "" {
		return fmt.Errorf("qoder account %d has no personal token or refresh token", account.ID)
	}
	endpoints := resolveQoderEndpoints(account.GetQoderRealm(), account.GetQoderBaseURL())
	resp, err := QoderExchangeJobToken(qoderGatewayTransport(s.httpUpstream), endpoints, qoderFingerprintSeed(creds.UID, token), token)
	if err != nil {
		return err
	}
	next := shallowCopyMap(account.Credentials)
	next["access_token"] = resp.SecurityOauthToken
	if rt := strings.TrimSpace(resp.RefreshToken); rt != "" {
		next["refresh_token"] = rt
	}
	if resp.ID != "" && strings.TrimSpace(qoderCredentialValueString(next["uid"])) == "" {
		next["uid"] = resp.ID
	}
	if resp.UserType != "" {
		next["user_type"] = resp.UserType
	}
	return persistAccountCredentials(ctx, s.accountRepo, account, next)
}

func (s *QoderCreditsService) qoderFetchQuota(ctx context.Context, account *Account, endpoints qoderEndpoints, token string) (*qoderQuotaUsageResp, error) {
	raw, err := s.qoderOpenAPIRequest(ctx, account, endpoints.QuotaURL, http.MethodGet, token, nil)
	if err != nil {
		return nil, err
	}
	var resp qoderQuotaUsageResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse qoder quota response: %w", err)
	}
	return &resp, nil
}

func (s *QoderCreditsService) qoderFetchCampaigns(ctx context.Context, account *Account, endpoints qoderEndpoints, token string) (*qoderCampaignsResp, error) {
	raw, err := s.qoderOpenAPIRequest(ctx, account, endpoints.CampaignsURL, http.MethodGet, token, nil)
	if err != nil {
		return nil, err
	}
	var resp qoderCampaignsResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse qoder campaigns response: %w", err)
	}
	return &resp, nil
}

func (s *QoderCreditsService) qoderClaimCampaign(ctx context.Context, account *Account, endpoints qoderEndpoints, token, campaignID string) (*qoderClaimResp, error) {
	if strings.TrimSpace(campaignID) == "" {
		return nil, fmt.Errorf("qoder campaign id is empty")
	}
	target := endpoints.CampaignClaimBase + "/" + strings.Trim(campaignID, "/") + "/claim"
	raw, err := s.qoderOpenAPIRequest(ctx, account, target, http.MethodPost, token, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	var resp qoderClaimResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse qoder claim response: %w", err)
	}
	return &resp, nil
}

// qoderOpenAPIRequest 发送一次 openapi 域请求：纯 Bearer + 桌面端身份头，无 COSY 签名。
// 出站前过 URL 安全策略（与 WorkBuddy billing 同口径）；HTTP >= 400 归一为
// *qoderCampaignError（幂等/未授权判定的依据），传输与解析错误返回普通 error。
func (s *QoderCreditsService) qoderOpenAPIRequest(ctx context.Context, account *Account, targetURL, method, token string, body io.Reader) (json.RawMessage, error) {
	validatedURL, err := cnValidateProbeURL(s.cfg, targetURL)
	if err != nil {
		return nil, infraerrors.New(http.StatusForbidden, "QODER_OPENAPI_URL_REJECTED", err.Error())
	}
	reqCtx, cancel := context.WithTimeout(ctx, qoderCampaignTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, validatedURL, body)
	if err != nil {
		return nil, fmt.Errorf("build qoder openapi request: %w", err)
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	applyQoderCampaignHeaders(req, token)

	resp, err := s.httpUpstream.Do(req, s.resolveQoderProxyURL(ctx, account), account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return nil, fmt.Errorf("qoder openapi transport error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, qoderCampaignMaxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read qoder openapi response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &qoderCampaignError{Status: resp.StatusCode, Body: workbuddyTruncateForError(string(raw))}
	}
	// 空响应体（部分端点在无内容时返回 204/空）：交给上层按「无数据」处理。
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("{}"), nil
	}
	return raw, nil
}

// applyQoderCampaignHeaders 补齐 openapi 业务接口的桌面端身份头。
// 注意：这里刻意不复用 applyQoderChatHeaders——那套带 COSY 签名且 clientType=5，
// 服务端按 5 判定为 IDE 而不下发任何活动（实测）。
func applyQoderCampaignHeaders(req *http.Request, token string) {
	req.Header.Set("accept", "application/json")
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("cosy-clienttype", qoderCampaignClientType)
	req.Header.Set("cosy-version", qoderCampaignClientVersion)
	req.Header.Set("cosy-machineos", qoderCampaignMachineOS)
	req.Header.Set("cosy-machinehostname", qoderCampaignMachineHostname)
	if req.Header.Get("content-type") == "" {
		req.Header.Set("content-type", "application/json")
	}
}

// resolveQoderProxyURL 解析账号代理（account.Proxy 未预载时经 proxyRepo 补载）。
func (s *QoderCreditsService) resolveQoderProxyURL(ctx context.Context, account *Account) string {
	if account == nil || account.ProxyID == nil {
		return ""
	}
	if account.Proxy != nil {
		return account.Proxy.URL()
	}
	if s != nil && s.proxyRepo != nil {
		if proxy, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && proxy != nil {
			account.Proxy = proxy
			return proxy.URL()
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 快照读写
// ---------------------------------------------------------------------------

// persistQoderCreditsSnapshot 把积分快照写入 account.Extra（失败只告警：观测数据
// 的写入失败不应把一次成功查询变成错误）。
func (s *QoderCreditsService) persistQoderCreditsSnapshot(ctx context.Context, account *Account, result *QoderCreditsResult) {
	if s.accountRepo == nil || account == nil || result == nil || !result.Success {
		return
	}
	snapshot := QoderCreditsSnapshot{
		Remaining:     result.Remaining,
		Used:          result.Used,
		Total:         result.Total,
		PlanRemain:    result.PlanRemain,
		PlanTotal:     result.PlanTotal,
		AddOnRemain:   result.AddOnRemain,
		AddOnTotal:    result.AddOnTotal,
		Packs:         result.Packs,
		Packages:      result.Packages,
		UserType:      result.UserType,
		QuotaExceeded: result.QuotaExceeded,
		FetchedAt:     result.FetchedAt,
		Realm:         result.Realm,
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{qoderCreditsExtraKey: snapshot}); err != nil {
		slog.Warn("qoder credits snapshot persist failed", "account_id", account.ID, "error", err)
	}
}

// persistQoderCheckinSnapshot 把领取快照写入 account.Extra。合并语义：同一轮已
// 成功（ok/already）的记录不被后续失败重试覆盖；skipped 不落盘（未发生动作）。
func (s *QoderCreditsService) persistQoderCheckinSnapshot(ctx context.Context, account *Account, result *QoderCheckinResult) {
	if s.accountRepo == nil || account == nil || result == nil || result.Status == QoderCheckinStatusSkipped {
		return
	}
	next := QoderCheckinSnapshot{
		Round:     result.Round,
		Status:    result.Status,
		CheckedAt: result.CheckedAt,
		Credits:   result.Credits,
		Amount:    result.Amount,
		GrantID:   result.GrantID,
	}
	if shouldKeepQoderCheckinSnapshot(readQoderCheckinSnapshot(account), next) {
		return
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{qoderCheckinExtraKey: next}); err != nil {
		slog.Warn("qoder checkin snapshot persist failed", "account_id", account.ID, "error", err)
	}
}

// shouldKeepQoderCheckinSnapshot 报告是否保留既有领取快照（不写入 next）：
// 同轮、既有记录为成功态、新结果为失败时保留。
func shouldKeepQoderCheckinSnapshot(existing *QoderCheckinSnapshot, next QoderCheckinSnapshot) bool {
	if existing == nil || existing.Round == "" || existing.Round != next.Round {
		return false
	}
	prevSuccess := existing.Status == QoderCheckinStatusOK || existing.Status == QoderCheckinStatusAlready
	return prevSuccess && next.Status == QoderCheckinStatusFail
}

// readQoderCheckinSnapshot 从 account.Extra 解析领取快照（缺失/形态异常返回 nil）。
func readQoderCheckinSnapshot(account *Account) *QoderCheckinSnapshot {
	if account == nil || account.Extra == nil {
		return nil
	}
	raw, ok := account.Extra[qoderCheckinExtraKey]
	if !ok || raw == nil {
		return nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	snapshot := &QoderCheckinSnapshot{}
	if v, ok := obj["round"].(string); ok {
		snapshot.Round = v
	}
	if v, ok := obj["status"].(string); ok {
		snapshot.Status = v
	}
	if v, ok := obj["grant_id"].(string); ok {
		snapshot.GrantID = v
	}
	snapshot.CheckedAt = qoderAnyInt64(obj["checked_at"])
	snapshot.Credits = qoderAnyFloat64(obj["credits"])
	snapshot.Amount = qoderAnyInt64(obj["amount"])
	return snapshot
}

// qoderCheckinStatusToday 返回账号本轮的领取快照状态（非本轮/无记录返回空串）。
func qoderCheckinStatusToday(account *Account) string {
	snapshot := readQoderCheckinSnapshot(account)
	if snapshot == nil || snapshot.Round != qoderCampaignRoundDate(time.Now()) {
		return ""
	}
	return snapshot.Status
}

// ---------------------------------------------------------------------------
// 时间/数值 helper
// ---------------------------------------------------------------------------

// qoderCampaignRoundDate 返回活动「轮次」归属日期：每天 10:00（服务器本地时区，
// 部署为 UTC+8 即北京时间）开放新一轮，窗口持续到次日 10:00，因此 0-10 点仍属
// 前一天的轮次。用自然日会在这 10 小时内把已领取误判成未领取。
func qoderCampaignRoundDate(now time.Time) string {
	local := now.In(timezone.Location())
	if local.Hour() < qoderCampaignHour {
		local = local.AddDate(0, 0, -1)
	}
	return local.Format("2006-01-02")
}

// benefitAmount 取活动条目的赠送额度。
func benefitAmount(campaign *qoderCampaignDTO) int64 {
	if campaign == nil || campaign.Benefit == nil {
		return 0
	}
	return campaign.Benefit.Amount
}

// benefitExpiryText 按活动条目的有效期规则推算到期文案（RELATIVE_DAYS → now+days）。
func benefitExpiryText(campaign *qoderCampaignDTO, nowUnix int64) string {
	if campaign == nil || campaign.Benefit == nil || campaign.Benefit.Validity == nil {
		return ""
	}
	if campaign.Benefit.Validity.Mode == "RELATIVE_DAYS" && campaign.Benefit.Validity.Days > 0 {
		return time.Unix(nowUnix, 0).UTC().AddDate(0, 0, campaign.Benefit.Validity.Days).Format(time.RFC3339)
	}
	return ""
}

// qoderAnyInt64 把 extra 快照里的整数值归一为 int64（兼容 JSON float64/字符串数字）。
func qoderAnyInt64(raw any) int64 {
	switch v := raw.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	default:
		return int64(qoderAnyFloat64(raw))
	}
}

// qoderAnyFloat64 把 extra 快照里的数值归一为 float64。
func qoderAnyFloat64(raw any) float64 {
	switch v := raw.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		if n, err := v.Float64(); err == nil {
			return n
		}
	case string:
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return n
		}
	}
	return 0
}
