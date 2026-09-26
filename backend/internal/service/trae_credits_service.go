package service

// trae_credits_service.go Trae 账号积分查询与每日签到服务。
//
// 上游接口（UG 域 api.trae.cn，与聊天域分离，且客户端指纹不同，见 trae_headers.go）：
//   - 签到状态 + 当前总积分：POST /trae/api/v2/ug/checkin_credits/status
//     body {"req_source":N} → {code, checked_in, credits, enable, message}
//   - 每日签到领取：POST /trae/api/v2/ug/checkin_credits/claim → 只有 code/message，
//     **领取到的积分不在响应里**，必须回查 status 才拿得到新余额；
//   - 权益包用量（真正的余额明细）：POST /trae/api/v2/pay/ide_user_ent_usage
//     body {"require_usage":true,"req_source":2} → user_entitlement_pack_list[]
//     {entitlement_id, entitlement_base_info:{quota:{credits_limit}, end_time},
//      usage:{credits_amount}}。
//
// 四条实测纪律（照抄参考实现，别自己发明）：
//  1. UG 域 HTTP 一律 200，成败只看 body 的 code；
//  2. claim 的业务码 9095 = 今日已签到，属幂等成功而非失败；code=0 也幂等（重复
//     调用不再发钱），故**不能拿积分变化当签到凭据**，必须回查 checked_in；
//  3. 9074（"当前参与用户太多"）是**账号级稳定拒绝**（换 deviceId/token/UA 均无效），
//     只重试 1 次即判失败；
//  4. 聚合积分必须跳过已过期权益包：签到积分当日发放、31 天后过期，不过滤会显示
//     "越签越多、永远用不完"。
//
// 结果快照写入 account.Extra（trae_credits / trae_checkin），供管理列表免探测直接
// 渲染；签到记录带当日内「成功优先」合并语义（后续失败重试不覆盖当日已成功记录）。
//
// 并发：同一账号的查询/签到分别被 singleflight 合并（列表页多组件自动探测、手动连点
// 均只打一次上游）；UG 请求带账号级 token 预刷新 + 401 刷新重试一次。

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
	// traeBillingTimeout 单次 UG 上游调用（查询/签到）的总时长上限。
	traeBillingTimeout = 20 * time.Second
	// traeCheckinTimeout 单次签到编排（状态 → 领取 → 回查积分）的总时长上限。
	traeCheckinTimeout = 60 * time.Second
	// traeBillingMaxBodyBytes UG 响应体读取上限（权益包列表很小）。
	traeBillingMaxBodyBytes int64 = 1 << 20
	// traeCreditsExtraKey / traeCheckinExtraKey account.Extra 快照键。
	traeCreditsExtraKey = "trae_credits"
	traeCheckinExtraKey = "trae_checkin"
	// traeDefaultReqSource UG 请求来源默认值：1=Trae CN IDE（同一账号积分互通）。
	traeDefaultReqSource = 1
	// traeUsageReqSource 权益包用量接口的来源值：2 才返回完整 usage 字段。
	traeUsageReqSource = 2
)

// Trae 签到业务码（实测语义）。
const (
	traeCodeOK             = 0
	traeCodeAlreadyChecked = 9095 // 今日已签到（幂等成功）
	traeCodeBusy           = 9074 // 当前参与用户太多：账号级稳定拒绝
	traeCodeSessionDead    = 1001 // 令牌/会话失效
	traeCodeActivityOff    = 9090 // 活动暂不可用
	traeCodeBadParams      = 9004 // 参数错误（典型：缺设备头）
)

// 签到状态（与前端契约一致；skipped = 账号形态不支持签到）。
const (
	TraeCheckinStatusOK      = "ok"
	TraeCheckinStatusAlready = "already"
	TraeCheckinStatusFail    = "fail"
	TraeCheckinStatusSkipped = "skipped"
)

// TraeCreditsPackage 单个权益包的积分明细。
type TraeCreditsPackage struct {
	EntitlementID string  `json:"entitlement_id,omitempty"`
	Kind          string  `json:"kind,omitempty"` // checkin / plan / 其他（按 entitlement_id 前缀归类）
	Limit         float64 `json:"limit"`
	Used          float64 `json:"used"`
	Remain        float64 `json:"remain"`
	ExpireAt      int64   `json:"expire_at,omitempty"`
	Expired       bool    `json:"expired,omitempty"`
}

// TraeCreditsResult 积分查询结果（管理端 + 前端单元格消费）。
// 积分带小数（上游 credits 是 float），故用 float64 而非 int64，避免整型截断。
type TraeCreditsResult struct {
	Success   bool    `json:"success"`
	Realm     string  `json:"realm"`
	Credits   float64 `json:"credits"`   // status 接口口径的当前总积分
	Remain    float64 `json:"remain"`    // 未过期权益包聚合剩余
	Used      float64 `json:"used"`      // 已用
	Size      float64 `json:"size"`      // 总额度
	Packs     int     `json:"packs"`     // 未过期权益包数
	Checkable bool    `json:"checkable"` // 签到入口是否可用（status.enable）
	// CheckedIn 今日是否已签到（status.checked_in，上游权威读数）。
	CheckedIn bool `json:"checked_in"`
	// TokenExpiresAt / RefreshExpiresAt 为凭据到期时刻（epoch 秒，0=未知）。
	// 不走上游探测，直接从 credentials 读出：Trae 没有「重新登录即可自动续期」的
	// 旁路——refreshToken 一过期就必须人工到 Trae 客户端重登并重新取凭据，因此
	// 「还能用多久」必须在积分旁边就可见，否则账号会悄悄变成不可用。
	TokenExpiresAt     int64                `json:"token_expires_at,omitempty"`
	RefreshExpiresAt   int64                `json:"refresh_expires_at,omitempty"`
	Packages           []TraeCreditsPackage `json:"packages,omitempty"`
	FetchedAt          int64                `json:"fetched_at"`
	TodayCheckinStatus string               `json:"today_checkin_status,omitempty"`
	Error              string               `json:"error,omitempty"`
}

// TraeCheckinResult 签到结果（状态机：ok/already/fail/skipped）。
type TraeCheckinResult struct {
	Success   bool    `json:"success"`
	Status    string  `json:"status"`
	Detail    string  `json:"detail,omitempty"`
	Credits   float64 `json:"credits"`
	HasCredit bool    `json:"has_credits,omitempty"`
	Realm     string  `json:"realm,omitempty"`
	CheckedAt int64   `json:"checked_at"`
}

// TraeCreditsSnapshot 写入 account.Extra 的积分快照（供列表免探测渲染）。
type TraeCreditsSnapshot struct {
	Credits   float64              `json:"credits"`
	Remain    float64              `json:"remain"`
	Used      float64              `json:"used"`
	Size      float64              `json:"size"`
	Packs     int                  `json:"packs"`
	Packages  []TraeCreditsPackage `json:"packages,omitempty"`
	FetchedAt int64                `json:"fetched_at"`
	Realm     string               `json:"realm"`
	Checkable bool                 `json:"checkable,omitempty"`
	CheckedIn bool                 `json:"checked_in,omitempty"`
	// 凭据到期时刻（epoch 秒）：随积分快照一同落 extra，列表页免探测即可告警。
	TokenExpiresAt   int64 `json:"token_expires_at,omitempty"`
	RefreshExpiresAt int64 `json:"refresh_expires_at,omitempty"`
}

// TraeCheckinSnapshot 写入 account.Extra 的签到快照。
type TraeCheckinSnapshot struct {
	Date      string  `json:"date"`
	Status    string  `json:"status"`
	CheckedAt int64   `json:"checked_at"`
	Credits   float64 `json:"credits,omitempty"`
}

// TraeCreditsService 查询 Trae 账号剩余积分并执行每日签到。
type TraeCreditsService struct {
	accountRepo  AccountRepository
	proxyRepo    ProxyRepository
	httpUpstream HTTPUpstream
	cfg          *config.Config
	flight       singleflight.Group
}

// NewTraeCreditsService 构造 Trae 积分/签到服务。
func NewTraeCreditsService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
) *TraeCreditsService {
	return &TraeCreditsService{
		accountRepo:  accountRepo,
		proxyRepo:    proxyRepo,
		httpUpstream: httpUpstream,
		cfg:          cfg,
	}
}

// traeBillingError UG 域上游错误（body code != 0）。
// 与传输层/解析层错误严格区分：只有本类型参与「已签到」幂等判定。
type traeBillingError struct {
	Status int
	Code   int
	Msg    string
}

func (e *traeBillingError) Error() string {
	if e == nil {
		return "trae billing error"
	}
	return fmt.Sprintf("trae billing error: HTTP %d code=%d msg=%s", e.Status, e.Code, e.Msg)
}

// traeIsCheckinAlreadyError 报告错误是否表示「今天已签到」。
func traeIsCheckinAlreadyError(err error) bool {
	var be *traeBillingError
	if !errors.As(err, &be) {
		return false
	}
	if be.Code == traeCodeAlreadyChecked {
		return true
	}
	return strings.Contains(be.Msg, "已签到") || strings.Contains(strings.ToLower(be.Msg), "already")
}

// traeIsSessionDeadError 报告错误是否为令牌/会话失效（触发一次刷新重试）。
func traeIsSessionDeadError(err error) bool {
	var be *traeBillingError
	if !errors.As(err, &be) {
		return false
	}
	return be.Status == http.StatusUnauthorized || be.Code == traeCodeSessionDead
}

// traeIsBusyError 报告错误是否为上游 9074（账号级稳定拒绝，只重试一次）。
func traeIsBusyError(err error) bool {
	var be *traeBillingError
	return errors.As(err, &be) && be.Code == traeCodeBusy
}

// loadTraeAccount 加载并校验 Trae 账号（ForAccount 入口共用）。
func (s *TraeCreditsService) loadTraeAccount(ctx context.Context, accountID int64) (*Account, error) {
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusNotFound, "TRAE_ACCOUNT_NOT_FOUND", "account not found: %v", err)
	}
	if account == nil {
		return nil, infraerrors.New(http.StatusNotFound, "TRAE_ACCOUNT_NOT_FOUND", "account not found")
	}
	if !account.IsTrae() {
		return nil, infraerrors.New(http.StatusBadRequest, "TRAE_INVALID_PLATFORM", "account is not a trae account")
	}
	return account, nil
}

// QueryCredits 查询指定账号的剩余积分（并落 extra 快照）。同一账号并发查询合并。
func (s *TraeCreditsService) QueryCredits(ctx context.Context, accountID int64) (*TraeCreditsResult, error) {
	if s == nil || s.accountRepo == nil || s.httpUpstream == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "TRAE_CREDITS_NOT_CONFIGURED", "trae credits service is not configured")
	}
	account, err := s.loadTraeAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	key := "trae_credits:" + strconv.FormatInt(accountID, 10)
	resultCh := s.flight.DoChan(key, func() (any, error) {
		probeCtx, cancel := context.WithTimeout(context.Background(), traeBillingTimeout+5*time.Second)
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
		result, ok := flightResult.Val.(*TraeCreditsResult)
		if !ok || result == nil {
			return nil, infraerrors.New(http.StatusInternalServerError, "TRAE_CREDITS_RESULT_INVALID", "invalid trae credits result")
		}
		cloned := *result
		return &cloned, nil
	}
}

// Checkin 手动触发指定账号的每日签到（幂等；已签到返回 already）。
func (s *TraeCreditsService) Checkin(ctx context.Context, accountID int64) (*TraeCheckinResult, error) {
	if s == nil || s.accountRepo == nil || s.httpUpstream == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "TRAE_CREDITS_NOT_CONFIGURED", "trae credits service is not configured")
	}
	account, err := s.loadTraeAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return s.CheckinForAccount(ctx, account), nil
}

// CheckinForAccount 对已加载账号执行签到（singleflight 合并；周期任务与手动入口共用）。
func (s *TraeCreditsService) CheckinForAccount(ctx context.Context, account *Account) *TraeCheckinResult {
	if account == nil {
		return &TraeCheckinResult{Status: TraeCheckinStatusFail, Detail: "account is nil", CheckedAt: time.Now().Unix()}
	}
	key := "trae_checkin:" + strconv.FormatInt(account.ID, 10)
	resultCh := s.flight.DoChan(key, func() (any, error) {
		checkinCtx, cancel := context.WithTimeout(context.Background(), traeCheckinTimeout)
		defer cancel()
		return s.checkinForAccount(checkinCtx, account), nil
	})
	select {
	case <-ctx.Done():
		return &TraeCheckinResult{Status: TraeCheckinStatusFail, Detail: ctx.Err().Error(), CheckedAt: time.Now().Unix()}
	case flightResult := <-resultCh:
		if flightResult.Err != nil {
			return &TraeCheckinResult{Status: TraeCheckinStatusFail, Detail: flightResult.Err.Error(), CheckedAt: time.Now().Unix()}
		}
		result, ok := flightResult.Val.(*TraeCheckinResult)
		if !ok || result == nil {
			return &TraeCheckinResult{Status: TraeCheckinStatusFail, Detail: "invalid trae checkin result", CheckedAt: time.Now().Unix()}
		}
		cloned := *result
		return &cloned
	}
}

// queryCreditsForAccount 探测单账号积分并落快照。上游/网络失败不返回 Go error，
// 而是回填 result.Error（管理端 200 + success=false），与 workbuddy 口径一致。
func (s *TraeCreditsService) queryCreditsForAccount(ctx context.Context, account *Account) *TraeCreditsResult {
	result := &TraeCreditsResult{
		Realm:              account.GetTraeRealm(),
		FetchedAt:          time.Now().Unix(),
		TodayCheckinStatus: traeCheckinStatusToday(account),
	}
	// 凭据到期时刻取自本地 credentials（由换票回写或用户粘贴时写入），不花一次上游请求。
	tcreds := account.GetTraeCredentials()
	result.TokenExpiresAt = tcreds.ExpiresAt
	result.RefreshExpiresAt = tcreds.RefreshExpireAt
	if err := s.ensureTraeBillingToken(ctx, account); err != nil {
		result.Error = err.Error()
		return result
	}
	status, statusErr := s.traeCheckinStatus(ctx, account)
	if statusErr == nil && status != nil {
		result.Credits = status.Credits
		result.CheckedIn = status.CheckedIn
		result.Checkable = status.Enable
	}
	// 权益包明细失败不致命：退回 status 的总积分读数。
	if packs, usageErr := s.traeEntitlementUsage(ctx, account); usageErr == nil {
		remain, used, size := traeAggregatePacks(packs)
		result.Remain = remain
		result.Used = used
		result.Size = size
		result.Packs = len(packs)
		result.Packages = packs
	} else if statusErr != nil {
		result.Error = fmt.Sprintf("%v; usage: %v", statusErr, usageErr)
		return result
	}
	if statusErr != nil {
		result.Error = statusErr.Error()
		return result
	}
	result.Success = true
	result.FetchedAt = time.Now().Unix()
	s.persistTraeCreditsSnapshot(ctx, account, result)
	return result
}

// checkinForAccount 单账号签到编排：预刷新 → status → claim → 回查 status → 落快照。
func (s *TraeCreditsService) checkinForAccount(ctx context.Context, account *Account) *TraeCheckinResult {
	result := &TraeCheckinResult{
		Realm:     account.GetTraeRealm(),
		CheckedAt: time.Now().Unix(),
	}
	if err := s.ensureTraeBillingToken(ctx, account); err != nil {
		result.Status = TraeCheckinStatusFail
		result.Detail = err.Error()
		s.persistTraeCheckinSnapshot(ctx, account, result)
		return result
	}
	status, err := s.traeCheckinStatus(ctx, account)
	if err != nil {
		result.Status = TraeCheckinStatusFail
		result.Detail = err.Error()
		s.persistTraeCheckinSnapshot(ctx, account, result)
		return result
	}
	if status != nil {
		result.Credits = status.Credits
		result.HasCredit = true
		// 上游已确认今日签过：不再发 claim（重复请求无收益，且徒增风控画像）。
		if status.CheckedIn {
			result.Status = TraeCheckinStatusAlready
			result.Success = true
			s.persistTraeCheckinSnapshot(ctx, account, result)
			s.refreshCreditsSnapshot(ctx, account)
			return result
		}
		if !status.Enable {
			result.Status = TraeCheckinStatusSkipped
			result.Detail = "check-in activity is not available for this account"
			return result
		}
	}
	claimErr := s.traeCheckinClaim(ctx, account)
	switch {
	case claimErr == nil:
		result.Status = TraeCheckinStatusOK
	case traeIsCheckinAlreadyError(claimErr):
		// 「今天已签到」是幂等成功，不填 detail，免得被误读成失败。
		result.Status = TraeCheckinStatusAlready
	case traeIsBusyError(claimErr):
		// 9074（「当前参与用户太多」）字面看像拥堵，实测却是账号级稳定拒绝：同一账号
		// 40+ 次重试（间隔 0~15s）全部 9074，刷新 token、换全新 deviceId、换 UA /
		// region / 换请求体（含 req_source）均无效。对照实验：把失败账号的 deviceId
		// 借给成功账号仍 code=0 —— 失败跟着账号走，不跟着设备或参数走。
		// 故此处不做任何参数变体重试（旧的「改用 req_source=2 再试一次」已被上述实测
		// 否证，只会多一次无效且风控可见的请求），也不谎报成功：如实落 fail 并保留
		// 上游原文，让管理面板看到真实缺签。
		result.Status = TraeCheckinStatusFail
		result.Detail = claimErr.Error()
	default:
		result.Status = TraeCheckinStatusFail
		result.Detail = claimErr.Error()
	}
	// 签到接口不回传积分数：回查 status 取新余额。
	// 关于「claim 报成功但回查仍 checked_in=false」：不以它推翻结论。上游 code
	// 是唯一权威判定，而 checked_in 是带设备侧缓存延迟的读数（实测：换 deviceId 后
	// 会短暂回 false）。若把它当否据，配合「当日成功优先不覆盖」的快照合并语义，
	// 一次真实成功的签到会被长期误报为失败，代价高于反过来。只记录待确认。
	if after, queryErr := s.traeCheckinStatus(ctx, account); queryErr == nil && after != nil {
		result.Credits = after.Credits
		result.HasCredit = true
		if result.Status == TraeCheckinStatusOK && !after.CheckedIn {
			result.Detail = "claim accepted (code=0) but check-in status not yet reflected upstream"
			slog.Info("trae check-in accepted but status not yet reflected",
				"account_id", account.ID, "credits", after.Credits)
		}
	}
	result.Success = result.Status == TraeCheckinStatusOK || result.Status == TraeCheckinStatusAlready
	s.persistTraeCheckinSnapshot(ctx, account, result)
	if result.Success {
		s.refreshCreditsSnapshot(ctx, account)
	}
	return result
}

// refreshCreditsSnapshot 签到后刷新积分快照（失败只告警：余额观测不影响签到结论）。
func (s *TraeCreditsService) refreshCreditsSnapshot(ctx context.Context, account *Account) {
	if credits := s.queryCreditsForAccount(ctx, account); !credits.Success {
		slog.Debug("trae credits refresh after checkin failed",
			"account_id", account.ID, "error", credits.Error)
	}
}

// ensureTraeBillingToken 确保账号持有可用 access_token：临近过期且持有 refresh_token
// 时先换票（失败但仍有旧 token 时宽容放行，上游若拒绝会走刷新重试路径）。
func (s *TraeCreditsService) ensureTraeBillingToken(ctx context.Context, account *Account) error {
	creds := account.GetTraeCredentials()
	if traeTokenNeedsRefresh(creds, time.Now()) && strings.TrimSpace(creds.RefreshToken) != "" {
		if err := s.refreshTraeBillingToken(ctx, account); err != nil {
			slog.Warn("trae billing pre-flight token refresh failed", "account_id", account.ID, "error", err)
			creds = account.GetTraeCredentials()
			if strings.TrimSpace(creds.AccessToken) == "" {
				return fmt.Errorf("trae account %d has no usable access token: %w", account.ID, err)
			}
		} else {
			creds = account.GetTraeCredentials()
		}
	}
	if strings.TrimSpace(creds.AccessToken) == "" {
		return fmt.Errorf("trae account %d has no usable access token", account.ID)
	}
	return nil
}

// refreshTraeBillingToken UG 路径的换票：与网关 refreshTraeToken 同语义，传输层换成
// 本服务的 HTTPUpstream（模式对齐 refreshWorkbuddyCreditsToken）。
func (s *TraeCreditsService) refreshTraeBillingToken(ctx context.Context, account *Account) error {
	refresher := &traeTokenRefresher{
		repo:     s.accountRepo,
		validate: func(rawURL string) (string, error) { return cnValidateProbeURL(s.cfg, rawURL) },
		proxyURL: func(account *Account) string { return s.resolveTraeProxyURL(ctx, account) },
		do: func(ctx context.Context, req *http.Request, account *Account) (*http.Response, error) {
			return s.httpUpstream.Do(req, s.resolveTraeProxyURL(ctx, account), account.ID, maxInt(account.Concurrency, 1))
		},
	}
	return refresher.refresh(ctx, account)
}

// traeCheckinStatusBody status 接口响应（HTTP 恒 200，成败看 code）。
type traeCheckinStatusBody struct {
	Code      int     `json:"code"`
	Message   string  `json:"message"`
	Msg       string  `json:"msg"`
	CheckedIn bool    `json:"checked_in"`
	Credits   float64 `json:"credits"`
	Enable    bool    `json:"enable"`
}

// traeCheckinStatus 查询签到状态（含当前总积分）。
func (s *TraeCreditsService) traeCheckinStatus(ctx context.Context, account *Account) (*traeCheckinStatusBody, error) {
	data, err := s.traeBillingRequestWithAuthRetry(ctx, account, traeCheckinStatusPath, map[string]any{
		"req_source": traeReqSource(account),
	})
	if err != nil {
		return nil, err
	}
	var body traeCheckinStatusBody
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("parse trae checkin status response: %w", err)
	}
	if body.Code != traeCodeOK {
		return nil, &traeBillingError{Status: http.StatusOK, Code: body.Code, Msg: traeFirstNonEmpty(body.Message, body.Msg)}
	}
	return &body, nil
}

// traeCheckinClaim 发起每日签到领取。错误原样上抛：由调用方区分「幂等已成功
// （9095=今日已签到）」与「真的失败」——在此就地吞掉会把「未领到积分」误报成
// 「刚刚领到」（前端按钮文案也不同：一个是「早就签过了」，一个是「刚领到」）。
func (s *TraeCreditsService) traeCheckinClaim(ctx context.Context, account *Account) error {
	_, err := s.traeBillingRequestWithAuthRetry(ctx, account, traeCheckinClaimPath, map[string]any{
		"req_source": traeReqSource(account),
	})
	return err
}

// traeEntitlementResp 权益包用量接口的**内层**载荷（信封的 data 已由
// traeBillingRequestOnce 剖出，部分上游版本则直接平铺）：两种形态都兼容。
type traeEntitlementResp struct {
	Packs []traeEntitlementPack `json:"user_entitlement_pack_list"`
	Data  struct {
		Packs []traeEntitlementPack `json:"user_entitlement_pack_list"`
	} `json:"data"`
}

// traeEntitlementRespPacks 取两份形态里非空的一份。
func traeEntitlementRespPacks(resp *traeEntitlementResp) []traeEntitlementPack {
	if resp == nil {
		return nil
	}
	if len(resp.Packs) > 0 {
		return resp.Packs
	}
	return resp.Data.Packs
}

// traeEntitlementPack 单个权益包（上游实样字段）。
type traeEntitlementPack struct {
	EntitlementID   string `json:"entitlement_id"`
	EntitlementBase struct {
		Quota struct {
			CreditsLimit float64 `json:"credits_limit"`
		} `json:"quota"`
		EndTime int64 `json:"end_time"`
	} `json:"entitlement_base_info"`
	ExpireTime int64 `json:"expire_time"`
	Usage      struct {
		CreditsAmount float64 `json:"credits_amount"`
	} `json:"usage"`
}

// traeEntitlementUsage 查询权益包明细并聚合出 remain/used/size（跳过已过期包）。
func (s *TraeCreditsService) traeEntitlementUsage(ctx context.Context, account *Account) ([]TraeCreditsPackage, error) {
	data, err := s.traeBillingRequestWithAuthRetry(ctx, account, traeEntUsagePath, map[string]any{
		"require_usage": true,
		"req_source":    traeUsageReqSource,
	})
	if err != nil {
		return nil, err
	}
	var resp traeEntitlementResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse trae entitlement usage response: %w", err)
	}
	now := time.Now().Unix()
	all := traeEntitlementRespPacks(&resp)
	packs := make([]TraeCreditsPackage, 0, len(all))
	for _, pkg := range all {
		expireAt := pkg.EntitlementBase.EndTime
		if expireAt <= 0 {
			expireAt = pkg.ExpireTime
		}
		expired := expireAt > 0 && expireAt < now
		limit := pkg.EntitlementBase.Quota.CreditsLimit
		used := pkg.Usage.CreditsAmount
		remain := limit - used
		if remain < 0 {
			remain = 0
		}
		if expired {
			remain = 0
		}
		packs = append(packs, TraeCreditsPackage{
			EntitlementID: pkg.EntitlementID,
			Kind:          traePackageKind(pkg.EntitlementID),
			Limit:         limit,
			Used:          used,
			Remain:        remain,
			ExpireAt:      traeNormalizeEpochSeconds(expireAt),
			Expired:       expired,
		})
	}
	return packs, nil
}

// traeAggregatePacks 聚合未过期权益包为 remain/used/size（过期包不计入，见文件头纪律 4）。
func traeAggregatePacks(packs []TraeCreditsPackage) (remain, used, size float64) {
	for _, pkg := range packs {
		if pkg.Expired {
			continue
		}
		remain += pkg.Remain
		used += pkg.Used
		size += pkg.Limit
	}
	return remain, used, size
}

// traePackageKind 按 entitlement_id 前缀归类权益包（签到包命名 checkin_YYYYMMDD_<uid>）。
func traePackageKind(entitlementID string) string {
	id := strings.ToLower(strings.TrimSpace(entitlementID))
	switch {
	case id == "":
		return ""
	case strings.HasPrefix(id, "checkin"):
		return "checkin"
	case strings.HasPrefix(id, "plan"), strings.HasPrefix(id, "subscription"):
		return "plan"
	default:
		return "other"
	}
}

// traeReqSource 账号实际使用的 req_source（凭据覆盖优先，缺省 1）。
func traeReqSource(account *Account) int {
	if account != nil {
		if v := account.GetTraeCredentials().ReqSource; v == 1 || v == 2 {
			return v
		}
	}
	return traeDefaultReqSource
}

// traeBillingEnvelope UG 域响应信封：{code, message/msg, data}。
// 注意 status/claim 是**平铺**结构（checked_in 与 code 同级），故此处把未知字段
// 一并交给 json.RawMessage 承载：解出 code 后原样回传 data 供上层二次解析。
type traeBillingEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Msg     string          `json:"msg"`
	Data    json.RawMessage `json:"data"`
}

// traeBillingRequestWithAuthRetry 发送 UG 请求；401/code=1001 且持有 refresh_token 时
// 换票一次后重试一次。
func (s *TraeCreditsService) traeBillingRequestWithAuthRetry(ctx context.Context, account *Account, path string, body any) (json.RawMessage, error) {
	data, err := s.traeBillingRequestOnce(ctx, account, path, body)
	if err == nil {
		return data, nil
	}
	if !traeIsSessionDeadError(err) {
		return nil, err
	}
	if strings.TrimSpace(account.GetTraeCredentials().RefreshToken) == "" {
		return nil, err
	}
	if refreshErr := s.refreshTraeBillingToken(ctx, account); refreshErr != nil {
		slog.Warn("trae billing token refresh failed", "account_id", account.ID, "error", refreshErr)
		return nil, err
	}
	return s.traeBillingRequestOnce(ctx, account, path, body)
}

// traeBillingRequestOnce 发送单次 UG 请求：HTTP >=400 或业务 code != 0 都返回
// *traeBillingError（幂等/会话失效判据）；传输与解析错误返回普通 error。
func (s *TraeCreditsService) traeBillingRequestOnce(ctx context.Context, account *Account, path string, body any) (json.RawMessage, error) {
	baseURL := account.GetTraeBillingBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("trae account %d missing billing base_url", account.ID)
	}
	targetURL := strings.TrimRight(baseURL, "/") + path
	// 出站前过安全策略（与网关转发/workbuddy 探测同一套校验）：UG 域由账号 realm
	// 衍生，不得把凭据发往策略外主机。
	validatedURL, err := cnValidateProbeURL(s.cfg, targetURL)
	if err != nil {
		return nil, infraerrors.New(http.StatusForbidden, "TRAE_BILLING_URL_REJECTED", err.Error())
	}
	var reqBody io.Reader
	if body != nil {
		raw, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return nil, fmt.Errorf("marshal trae billing body: %w", marshalErr)
		}
		reqBody = bytes.NewReader(raw)
	}
	reqCtx, cancel := context.WithTimeout(ctx, traeBillingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, validatedURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("build trae billing request: %w", err)
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	applyTraeBillingHeaders(req, account.GetTraeCredentials(), account)

	resp, err := s.httpUpstream.Do(req, s.resolveTraeProxyURL(ctx, account), account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return nil, fmt.Errorf("trae billing transport error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, traeBillingMaxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read trae billing response: %w", err)
	}
	var env traeBillingEnvelope
	parseErr := json.Unmarshal(raw, &env)
	if resp.StatusCode >= 400 {
		be := &traeBillingError{Status: resp.StatusCode}
		if parseErr == nil {
			be.Code = env.Code
			be.Msg = traeTruncateForError(traeFirstNonEmpty(env.Message, env.Msg))
		}
		if strings.TrimSpace(be.Msg) == "" {
			be.Msg = traeTruncateForError(string(raw))
		}
		return nil, be
	}
	if parseErr != nil {
		return nil, fmt.Errorf("parse trae billing response: %w (body: %s)", parseErr, traeTruncateForError(string(raw)))
	}
	if env.Code != traeCodeOK {
		return nil, &traeBillingError{Status: resp.StatusCode, Code: env.Code, Msg: traeTruncateForError(traeFirstNonEmpty(env.Message, env.Msg))}
	}
	// status/claim 平铺：env.data 为空时用整个 body 作为 data 交给上层解析。
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return json.RawMessage(raw), nil
	}
	return env.Data, nil
}

// resolveTraeProxyURL 解析账号代理（account.Proxy 未预载时经 proxyRepo 补载）。
func (s *TraeCreditsService) resolveTraeProxyURL(ctx context.Context, account *Account) string {
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

// persistTraeCreditsSnapshot 把积分快照写入 account.Extra（失败只告警：快照是观测
// 数据，写入失败不应把一次成功查询变成错误）。
func (s *TraeCreditsService) persistTraeCreditsSnapshot(ctx context.Context, account *Account, result *TraeCreditsResult) {
	if s.accountRepo == nil || account == nil || result == nil || !result.Success {
		return
	}
	snapshot := TraeCreditsSnapshot{
		Credits:   result.Credits,
		Remain:    result.Remain,
		Used:      result.Used,
		Size:      result.Size,
		Packs:     result.Packs,
		Packages:  result.Packages,
		FetchedAt: result.FetchedAt,
		Realm:     result.Realm,
		Checkable: result.Checkable,
		CheckedIn: result.CheckedIn,
	}
	// 到期时刻随快照落库，使列表页不请求上游也能告警。
	snapshot.TokenExpiresAt = result.TokenExpiresAt
	snapshot.RefreshExpiresAt = result.RefreshExpiresAt
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{traeCreditsExtraKey: snapshot}); err != nil {
		slog.Warn("trae credits snapshot persist failed", "account_id", account.ID, "error", err)
	}
}

// persistTraeCheckinSnapshot 把签到快照写入 account.Extra。
// 合并语义：当日已成功（ok/already）的既有记录不被后续失败重试覆盖；skipped 不落盘。
func (s *TraeCreditsService) persistTraeCheckinSnapshot(ctx context.Context, account *Account, result *TraeCheckinResult) {
	if s.accountRepo == nil || account == nil || result == nil || result.Status == TraeCheckinStatusSkipped {
		return
	}
	next := TraeCheckinSnapshot{
		Date:      timezone.Now().Format("2006-01-02"),
		Status:    result.Status,
		CheckedAt: result.CheckedAt,
		Credits:   result.Credits,
	}
	if shouldKeepTraeCheckinSnapshot(readTraeCheckinSnapshot(account), next) {
		return
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{traeCheckinExtraKey: next}); err != nil {
		slog.Warn("trae checkin snapshot persist failed", "account_id", account.ID, "error", err)
	}
}

// shouldKeepTraeCheckinSnapshot 报告是否应保留既有签到快照（不写入 next）：同日、
// 既有记录为成功（ok/already）、新结果为失败时保留。
func shouldKeepTraeCheckinSnapshot(existing *TraeCheckinSnapshot, next TraeCheckinSnapshot) bool {
	if existing == nil || existing.Date == "" || existing.Date != next.Date {
		return false
	}
	prevSuccess := existing.Status == TraeCheckinStatusOK || existing.Status == TraeCheckinStatusAlready
	return prevSuccess && next.Status == TraeCheckinStatusFail
}

// readTraeCheckinSnapshot 从 account.Extra 解析签到快照（缺失/形态异常返回 nil）。
func readTraeCheckinSnapshot(account *Account) *TraeCheckinSnapshot {
	if account == nil || account.Extra == nil {
		return nil
	}
	raw, ok := account.Extra[traeCheckinExtraKey]
	if !ok || raw == nil {
		return nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	snapshot := &TraeCheckinSnapshot{}
	if v, ok := obj["date"].(string); ok {
		snapshot.Date = v
	}
	if v, ok := obj["status"].(string); ok {
		snapshot.Status = v
	}
	snapshot.CheckedAt = traeAnyInt64(obj["checked_at"])
	snapshot.Credits = traeAnyFloat64(obj["credits"])
	return snapshot
}

// traeCheckinStatusToday 返回账号当日签到状态（非当日/无记录返回空串）。
func traeCheckinStatusToday(account *Account) string {
	snapshot := readTraeCheckinSnapshot(account)
	if snapshot == nil {
		return ""
	}
	if snapshot.Date != timezone.Now().Format("2006-01-02") {
		return ""
	}
	return snapshot.Status
}

// traeAnyInt64 把 extra 快照里的数值归一为 int64。
func traeAnyInt64(raw any) int64 {
	switch v := raw.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// traeAnyFloat64 把 extra 快照里的数值归一为 float64（积分带小数）。
func traeAnyFloat64(raw any) float64 {
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
		if f, err := v.Float64(); err == nil {
			return f
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return 0
}

// traeFirstNonEmpty 返回第一个非空串（去首尾空白后的值）。
func traeFirstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
