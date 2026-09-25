package service

// workbuddy_credits_service.go WorkBuddy 账号积分查询与每日签到服务。
//
// 移植自 workbuddy2api 的 billing 域实现（internal/upstream/client.go 的
// getUserResourceBody / DailyCheckin / billingMeterPaths，以及 cmd/signin 的幂等判定），
// 传输层对齐 sub2api 的 CNProviderQuotaService（HTTPUpstream.Do + 出站 URL 白名单校验）。
//
// 上游接口（billing 域，与 chat 域分离）：
//   - 积分查询：POST {billing_base}/billing/meter/get-user-resource
//     body PageNumber/PageSize/ProductCode=p_tcaca/Status=[0,3]/到期时间范围；
//     响应 data.Response.Data.{TotalDosage, Accounts[].CycleCapacity*}。
//   - 每日签到：POST {billing_base}/billing/meter/daily-checkin（body {}）；
//     重复签到为幂等业务错误（code 10001/14001 或「已签到」文案），记 already 而非失败。
//
// 双域路径形态：CN 账号固定 /v2 前缀（现状零回归）；国际账号首选无 /v2 前缀，
// 404 时回落 /v2 变体（对齐参考实现 billingMeterPaths 的 R9 结论）。
// 签到仅 CN 账号有效——国际版无签到体系，直接记 skipped 不发上游请求。
//
// 结果快照写入 account.Extra（workbuddy_credits / workbuddy_checkin），供管理列表
// 免探测直接渲染；签到记录带当日内「成功优先」合并语义（后续失败重试不覆盖当日
// 已成功记录，避免把「今日已签到」误降级为失败）。
//
// 并发：同一账号的查询/签到分别被 singleflight 合并（列表页多组件自动探测、
// 手动连点均只打一次上游）；billing 请求带账号级 token 预刷新 + 401 刷新重试一次。

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
	// workbuddyBillingTimeout 单次 billing 上游调用（查询/签到）的总时长上限。
	workbuddyBillingTimeout = 20 * time.Second

	// workbuddyCheckinTimeout 单次签到编排（签到 + 后续积分刷新）的总时长上限。
	workbuddyCheckinTimeout = 60 * time.Second

	// workbuddyBillingMaxBodyBytes billing 响应体读取上限（套餐列表很小，1MB 足够）。
	workbuddyBillingMaxBodyBytes int64 = 1 << 20

	// workbuddyBillingProductCode get-user-resource 的产品码（对齐官方客户端）。
	workbuddyBillingProductCode = "p_tcaca"

	// workbuddyCreditsExtraKey / workbuddyCheckinExtraKey account.Extra 快照键。
	workbuddyCreditsExtraKey = "workbuddy_credits"
	workbuddyCheckinExtraKey = "workbuddy_checkin"
)

// 签到状态（与前端契约一致；skipped = 平台/账号形态不支持签到）。
const (
	WorkBuddyCheckinStatusOK      = "ok"
	WorkBuddyCheckinStatusAlready = "already"
	WorkBuddyCheckinStatusFail    = "fail"
	WorkBuddyCheckinStatusSkipped = "skipped"
)

// WorkBuddyCreditsPackage 单个积分套餐的明细（对应上游 Accounts[] 条目）。
type WorkBuddyCreditsPackage struct {
	PackageName  string `json:"package_name,omitempty"`
	Remain       int64  `json:"remain"`
	Used         int64  `json:"used"`
	Size         int64  `json:"size"`
	CycleEndTime string `json:"cycle_end_time,omitempty"`
}

// WorkBuddyCreditsResult 积分查询结果（管理端 + 前端单元格消费）。
type WorkBuddyCreditsResult struct {
	Success            bool                      `json:"success"`
	Realm              string                    `json:"realm"`
	Remain             int64                     `json:"remain"`
	Used               int64                     `json:"used"`
	Size               int64                     `json:"size"`
	Packs              int                       `json:"packs"`
	Packages           []WorkBuddyCreditsPackage `json:"packages,omitempty"`
	FetchedAt          int64                     `json:"fetched_at"`
	TodayCheckinStatus string                    `json:"today_checkin_status,omitempty"`
	// NotApplicable：企业成员账号在个人 billing 资源池里本就没有额度可查
	// （上游返回 code=0 但 Accounts=null），展示端据此显示「额度由企业后台管理」
	// 而不是把 0 当成真实余额。
	NotApplicable bool   `json:"not_applicable,omitempty"`
	Enterprise    bool   `json:"enterprise,omitempty"`
	Error         string `json:"error,omitempty"`
}

// WorkBuddyCheckinResult 签到结果（状态机：ok/already/fail/skipped）。
type WorkBuddyCheckinResult struct {
	Success   bool   `json:"success"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Credits   *int64 `json:"credits,omitempty"`
	Realm     string `json:"realm,omitempty"`
	CheckedAt int64  `json:"checked_at"`
}

// WorkBuddyCreditsSnapshot 写入 account.Extra 的积分快照（供列表免探测渲染）。
type WorkBuddyCreditsSnapshot struct {
	Remain    int64                     `json:"remain"`
	Used      int64                     `json:"used"`
	Size      int64                     `json:"size"`
	Packs     int                       `json:"packs"`
	Packages  []WorkBuddyCreditsPackage `json:"packages,omitempty"`
	FetchedAt int64                     `json:"fetched_at"`
	Realm     string                    `json:"realm"`
	// NotApplicable / Enterprise 与 WorkBuddyCreditsResult 同义（列表免探测渲染需要）。
	NotApplicable bool `json:"not_applicable,omitempty"`
	Enterprise    bool `json:"enterprise,omitempty"`
}

// WorkBuddyCheckinSnapshot 写入 account.Extra 的签到快照。
type WorkBuddyCheckinSnapshot struct {
	Date      string `json:"date"`
	Status    string `json:"status"`
	CheckedAt int64  `json:"checked_at"`
	Credits   *int64 `json:"credits,omitempty"`
}

// WorkBuddyCreditsService 查询 WorkBuddy 账号剩余积分并执行每日签到。
type WorkBuddyCreditsService struct {
	accountRepo  AccountRepository
	proxyRepo    ProxyRepository
	httpUpstream HTTPUpstream
	cfg          *config.Config
	flight       singleflight.Group
}

// NewWorkBuddyCreditsService 构造 WorkBuddy 积分/签到服务。
func NewWorkBuddyCreditsService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
) *WorkBuddyCreditsService {
	return &WorkBuddyCreditsService{
		accountRepo:  accountRepo,
		proxyRepo:    proxyRepo,
		httpUpstream: httpUpstream,
		cfg:          cfg,
	}
}

// workbuddyBillingError billing 域上游错误（HTTP >= 400 或业务 code != 0）。
// 与传输层/解析层错误严格区分：只有本类型参与「已签到」幂等判定（对齐参考实现
// IsAlreadyCheckin 的语义——网络抖动不得被记成幂等成功）。
type workbuddyBillingError struct {
	Status int // HTTP 状态码
	Code   int // 业务信封 code（HTTP 错误体里解析不出时为 0）
	Msg    string
}

func (e *workbuddyBillingError) Error() string {
	if e == nil {
		return "workbuddy billing error"
	}
	if e.Code != 0 {
		return fmt.Sprintf("workbuddy billing error: HTTP %d code=%d msg=%s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("workbuddy billing error: HTTP %d msg=%s", e.Status, e.Msg)
}

// workbuddyIsCheckinAlreadyError 报告错误是否表示「今天已签到」（上游幂等拒绝重复签到）。
// 只认带分类的 *workbuddyBillingError：业务码 10001/14001 或文案含「已签到」/「already」。
func workbuddyIsCheckinAlreadyError(err error) bool {
	var be *workbuddyBillingError
	if !errors.As(err, &be) {
		return false
	}
	switch be.Code {
	case 10001, 14001:
		return true
	}
	if strings.Contains(be.Msg, "已签到") {
		return true
	}
	return strings.Contains(strings.ToLower(be.Msg), "already")
}

// workbuddyIsUnauthorizedError 报告错误是否为 401（触发一次 token 刷新重试）。
func workbuddyIsUnauthorizedError(err error) bool {
	var be *workbuddyBillingError
	return errors.As(err, &be) && be.Status == http.StatusUnauthorized
}

// loadWorkbuddyAccount 加载并校验 WorkBuddy 账号（ForAccount 入口共用）。
func (s *WorkBuddyCreditsService) loadWorkbuddyAccount(ctx context.Context, accountID int64) (*Account, error) {
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusNotFound, "WORKBUDDY_ACCOUNT_NOT_FOUND", "account not found: %v", err)
	}
	if account == nil {
		return nil, infraerrors.New(http.StatusNotFound, "WORKBUDDY_ACCOUNT_NOT_FOUND", "account not found")
	}
	if !account.IsWorkbuddy() {
		return nil, infraerrors.New(http.StatusBadRequest, "WORKBUDDY_INVALID_PLATFORM", "account is not a workbuddy account")
	}
	return account, nil
}

// QueryCredits 查询指定账号的剩余积分（并落 extra 快照）。
// 同一账号的并发查询被 singleflight 合并。
func (s *WorkBuddyCreditsService) QueryCredits(ctx context.Context, accountID int64) (*WorkBuddyCreditsResult, error) {
	if s == nil || s.accountRepo == nil || s.httpUpstream == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "WORKBUDDY_CREDITS_NOT_CONFIGURED", "workbuddy credits service is not configured")
	}
	account, err := s.loadWorkbuddyAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	key := "workbuddy_credits:" + strconv.FormatInt(accountID, 10)
	resultCh := s.flight.DoChan(key, func() (any, error) {
		probeCtx, cancel := context.WithTimeout(context.Background(), workbuddyBillingTimeout+5*time.Second)
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
		result, ok := flightResult.Val.(*WorkBuddyCreditsResult)
		if !ok || result == nil {
			return nil, infraerrors.New(http.StatusInternalServerError, "WORKBUDDY_CREDITS_RESULT_INVALID", "invalid workbuddy credits result")
		}
		cloned := *result
		return &cloned, nil
	}
}

// Checkin 手动触发指定账号的每日签到（幂等；已签到返回 already）。
// 同一账号的并发签到被 singleflight 合并（手动连点只打一次上游）。
func (s *WorkBuddyCreditsService) Checkin(ctx context.Context, accountID int64) (*WorkBuddyCheckinResult, error) {
	if s == nil || s.accountRepo == nil || s.httpUpstream == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "WORKBUDDY_CREDITS_NOT_CONFIGURED", "workbuddy credits service is not configured")
	}
	account, err := s.loadWorkbuddyAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return s.CheckinForAccount(ctx, account), nil
}

// CheckinForAccount 对已加载账号执行签到（singleflight 合并；周期任务与手动入口共用）。
func (s *WorkBuddyCreditsService) CheckinForAccount(ctx context.Context, account *Account) *WorkBuddyCheckinResult {
	if account == nil {
		return &WorkBuddyCheckinResult{Status: WorkBuddyCheckinStatusFail, Detail: "account is nil", CheckedAt: time.Now().Unix()}
	}
	key := "workbuddy_checkin:" + strconv.FormatInt(account.ID, 10)
	resultCh := s.flight.DoChan(key, func() (any, error) {
		checkinCtx, cancel := context.WithTimeout(context.Background(), workbuddyCheckinTimeout)
		defer cancel()
		return s.checkinForAccount(checkinCtx, account), nil
	})
	select {
	case <-ctx.Done():
		return &WorkBuddyCheckinResult{Status: WorkBuddyCheckinStatusFail, Detail: ctx.Err().Error(), CheckedAt: time.Now().Unix()}
	case flightResult := <-resultCh:
		if flightResult.Err != nil {
			return &WorkBuddyCheckinResult{Status: WorkBuddyCheckinStatusFail, Detail: flightResult.Err.Error(), CheckedAt: time.Now().Unix()}
		}
		result, ok := flightResult.Val.(*WorkBuddyCheckinResult)
		if !ok || result == nil {
			return &WorkBuddyCheckinResult{Status: WorkBuddyCheckinStatusFail, Detail: "invalid workbuddy checkin result", CheckedAt: time.Now().Unix()}
		}
		cloned := *result
		return &cloned
	}
}

// queryCreditsForAccount 探测单个账号的积分并落快照。
// 上游/网络失败时不返回 Go error，而是回填 result.Error（管理端 200 + success=false），
// 与 CNProviderQuotaService 的探测结果口径一致。
func (s *WorkBuddyCreditsService) queryCreditsForAccount(ctx context.Context, account *Account) *WorkBuddyCreditsResult {
	realm := account.GetWorkbuddyRealm()
	creds := account.GetWorkbuddyCredentials()
	enterprise := strings.TrimSpace(creds.EnterpriseID) != ""
	result := &WorkBuddyCreditsResult{
		Realm:              realm,
		FetchedAt:          time.Now().Unix(),
		TodayCheckinStatus: workbuddyCheckinStatusToday(account),
		Enterprise:         enterprise,
	}
	if err := s.ensureWorkbuddyBillingToken(ctx, account); err != nil {
		result.Error = err.Error()
		return result
	}
	body := map[string]any{
		"PageNumber":  1,
		"PageSize":    100,
		"ProductCode": workbuddyBillingProductCode,
		"Status":      []int{0, 3},
	}
	now := time.Now()
	body["PackageEndTimeRangeBegin"] = now.Format(workbuddyBillingTimeLayout)
	body["PackageEndTimeRangeEnd"] = now.Add(365 * 101 * 24 * time.Hour).Format(workbuddyBillingTimeLayout)

	data, err := s.workbuddyBillingRequestWithAuthRetry(ctx, account, http.MethodPost, workbuddyBillingMeterPaths(account), body)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	var resp workbuddyUserResourceResp
	if err := json.Unmarshal(data, &resp); err != nil {
		result.Error = fmt.Sprintf("parse workbuddy resource response: %v", err)
		return result
	}
	remain, used, size, packs, packages := workbuddyAggregateResource(&resp)
	result.Success = true
	result.Remain = remain
	result.Used = used
	result.Size = size
	result.Packs = packs
	result.Packages = packages
	result.FetchedAt = time.Now().Unix()
	// 企业成员账号在个人 billing 资源池里本就无额度（实测 HTTP 200 + code=0
	// 但 Accounts=null / TotalDosage=0，且与请求参数无关）：不加这个标记的话，
	// 列表会把空结果当成「余额 0」展示，看起来就是「积分查不到」。
	result.NotApplicable = enterprise && packs == 0

	s.persistWorkbuddyCreditsSnapshot(ctx, account, result)
	return result
}

// checkinForAccount 对单账号执行签到编排：token 预刷新 → daily-checkin → 查积分 → 落快照。
func (s *WorkBuddyCreditsService) checkinForAccount(ctx context.Context, account *Account) *WorkBuddyCheckinResult {
	realm := account.GetWorkbuddyRealm()
	result := &WorkBuddyCheckinResult{
		Realm:     realm,
		CheckedAt: time.Now().Unix(),
	}
	// D4 门控：国际版无签到体系/任务中心，直接跳过（不发起任何上游调用，避免风控）。
	if realm != "cn" {
		result.Status = WorkBuddyCheckinStatusSkipped
		result.Detail = "global realm has no daily check-in; credits only"
		return result
	}
	// D5 门控：企业成员账号无个人签到体系（签到资格随企业席位，不在个人 billing 域），
	// 同样不发任何上游调用 —— 之前每轮都在给企业号打一个必然无效的 daily-checkin。
	if ent := strings.TrimSpace(account.GetWorkbuddyCredentials().EnterpriseID); ent != "" {
		result.Status = WorkBuddyCheckinStatusSkipped
		result.Detail = "enterprise member accounts have no personal daily check-in; quota is managed by the enterprise"
		return result
	}
	if err := s.ensureWorkbuddyBillingToken(ctx, account); err != nil {
		result.Status = WorkBuddyCheckinStatusFail
		result.Detail = err.Error()
		s.persistWorkbuddyCheckinSnapshot(ctx, account, result)
		return result
	}
	_, err := s.workbuddyBillingRequestWithAuthRetry(ctx, account, http.MethodPost, workbuddyCheckinPaths(account), map[string]any{})
	switch {
	case err == nil:
		result.Status = WorkBuddyCheckinStatusOK
	case workbuddyIsCheckinAlreadyError(err):
		// 「今天已签到」是幂等成功，不是错误：不填 detail，免得被误读成签到失败。
		result.Status = WorkBuddyCheckinStatusAlready
	default:
		result.Status = WorkBuddyCheckinStatusFail
		result.Detail = err.Error()
	}
	// 无论签到返回什么（含「已签到」），都尝试刷新积分：余额信息独立于签到结果。
	if credits := s.queryCreditsForAccount(ctx, account); credits.Success {
		result.Credits = &credits.Remain
	}
	result.Success = result.Status == WorkBuddyCheckinStatusOK || result.Status == WorkBuddyCheckinStatusAlready
	s.persistWorkbuddyCheckinSnapshot(ctx, account, result)
	return result
}

// ensureWorkbuddyBillingToken 确保账号持有可用 access_token：
// 临近过期且持有 refresh_token 时先刷新（失败但仍有旧 token 时宽容放行，
// 上游若拒绝会走 401 刷新重试路径）。
func (s *WorkBuddyCreditsService) ensureWorkbuddyBillingToken(ctx context.Context, account *Account) error {
	creds := account.GetWorkbuddyCredentials()
	if workbuddyTokenNeedsRefresh(creds, time.Now()) && strings.TrimSpace(creds.RefreshToken) != "" {
		if err := s.refreshWorkbuddyCreditsToken(ctx, account); err != nil {
			slog.Warn("workbuddy billing pre-flight token refresh failed",
				"account_id", account.ID, "error", err)
			creds = account.GetWorkbuddyCredentials()
			if strings.TrimSpace(creds.AccessToken) == "" {
				return fmt.Errorf("workbuddy account %d has no usable access token: %w", account.ID, err)
			}
		} else {
			creds = account.GetWorkbuddyCredentials()
		}
	}
	if strings.TrimSpace(creds.AccessToken) == "" {
		return fmt.Errorf("workbuddy account %d has no usable access token", account.ID)
	}
	return nil
}

// refreshWorkbuddyCreditsToken 积分/签到路径的 token 刷新：与网关 refreshWorkbuddyToken
// 同语义（账号级锁 + 锁内双检 + 凭据写回），传输层换成积分服务自己的 HTTPUpstream
// （与 AccountTestService.refreshWorkbuddyAccountToken 的「同语义、独立传输」先例一致）。
func (s *WorkBuddyCreditsService) refreshWorkbuddyCreditsToken(ctx context.Context, account *Account) error {
	if account == nil || !account.IsWorkbuddy() {
		return fmt.Errorf("workbuddy token refresh requires a workbuddy account")
	}
	snapshot := strings.TrimSpace(account.GetWorkbuddyCredentials().AccessToken)
	lock := workbuddyRefreshLock(account.ID)
	if err := lock.Lock(ctx); err != nil {
		return err
	}
	defer lock.Unlock()
	// 锁内双检：token 快照已被他人改写 → 并发刷新已完成，本次不重复刷新。
	if strings.TrimSpace(account.GetWorkbuddyCredentials().AccessToken) != snapshot {
		return nil
	}
	creds := account.GetWorkbuddyCredentials()
	refreshToken := strings.TrimSpace(creds.RefreshToken)
	if refreshToken == "" {
		return fmt.Errorf("workbuddy account %d has no refresh token", account.ID)
	}
	baseURL := account.GetWorkbuddyBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		return fmt.Errorf("workbuddy account %d missing base_url", account.ID)
	}
	targetURL := strings.TrimRight(baseURL, "/") + workbuddyRefreshPath
	validatedURL, err := cnValidateProbeURL(s.cfg, targetURL)
	if err != nil {
		return fmt.Errorf("invalid workbuddy base_url: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, workbuddyRefreshTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, validatedURL, nil)
	if err != nil {
		return fmt.Errorf("build workbuddy refresh request: %w", err)
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	applyWorkbuddyRefreshHeaders(req, creds, account, account.GetWorkbuddyRealm())
	resp, err := s.httpUpstream.Do(req, s.resolveWorkbuddyProxyURL(ctx, account), account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return fmt.Errorf("workbuddy token refresh transport error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, workbuddyUpstreamErrorBodyLimit))
	if err != nil {
		return fmt.Errorf("read workbuddy refresh response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("workbuddy token refresh failed: HTTP %d: %s", resp.StatusCode, workbuddyTruncateForError(string(raw)))
	}
	var env workbuddyRefreshResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("parse workbuddy refresh response: %w", err)
	}
	if env.Code != 0 {
		return fmt.Errorf("workbuddy token refresh failed: code=%d msg=%s", env.Code, strings.TrimSpace(env.Msg))
	}
	if strings.TrimSpace(env.Data.AccessToken) == "" {
		return fmt.Errorf("workbuddy token refresh failed: no accessToken in response — re-login required")
	}
	next := shallowCopyMap(account.Credentials)
	next["access_token"] = env.Data.AccessToken
	if rt := strings.TrimSpace(env.Data.RefreshToken); rt != "" {
		next["refresh_token"] = rt
	}
	if env.Data.ExpiresIn > 0 && time.Duration(env.Data.ExpiresIn)*time.Second < workbuddyRefreshExpiresInMax {
		next["expires_at"] = time.Now().Add(time.Duration(env.Data.ExpiresIn) * time.Second).Unix()
	}
	if d := strings.TrimSpace(env.Data.Domain); d != "" {
		next["domain"] = d
	}
	account.Credentials = next
	if err := persistAccountCredentials(ctx, s.accountRepo, account, account.Credentials); err != nil {
		return fmt.Errorf("persist workbuddy credentials: %w", err)
	}
	return nil
}

// workbuddyBillingMeterPaths 积分查询的 realm 路径候选：
// global → [无 /v2, 有 /v2]（404 回落）；cn → [有 /v2]（现状逐字，零回归）。
func workbuddyBillingMeterPaths(account *Account) []string {
	if account.GetWorkbuddyRealm() == "global" {
		return []string{workbuddyBillingMeterPath, workbuddyBillingMeterPathV2}
	}
	return []string{workbuddyBillingMeterPathV2}
}

// workbuddyCheckinPaths 签到的 realm 路径候选（当前仅 CN 实际发起签到）。
func workbuddyCheckinPaths(account *Account) []string {
	if account.GetWorkbuddyRealm() == "global" {
		return []string{workbuddyCheckinPath, workbuddyCheckinPathV2}
	}
	return []string{workbuddyCheckinPathV2}
}

// workbuddyBillingRequestWithAuthRetry 发送 billing 请求；401 且持有 refresh_token 时
// 刷新一次 token 后重试一次（与 chat 出站 401 重试同语义）。
func (s *WorkBuddyCreditsService) workbuddyBillingRequestWithAuthRetry(
	ctx context.Context,
	account *Account,
	method string,
	paths []string,
	body any,
) (json.RawMessage, error) {
	data, err := s.workbuddyBillingRequest(ctx, account, method, paths, body)
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
	if refreshErr := s.refreshWorkbuddyCreditsToken(ctx, account); refreshErr != nil {
		slog.Warn("workbuddy billing 401 retry token refresh failed",
			"account_id", account.ID, "error", refreshErr)
		return nil, err
	}
	return s.workbuddyBillingRequest(ctx, account, method, paths, body)
}

// workbuddyBillingRequest 按候选路径序列发送 billing 请求（404 时换下一候选路径）。
func (s *WorkBuddyCreditsService) workbuddyBillingRequest(
	ctx context.Context,
	account *Account,
	method string,
	paths []string,
	body any,
) (json.RawMessage, error) {
	var lastErr error
	for i, path := range paths {
		data, err := s.workbuddyBillingRequestOnce(ctx, account, method, path, body)
		if err == nil {
			return data, nil
		}
		lastErr = err
		var be *workbuddyBillingError
		if i < len(paths)-1 && errors.As(err, &be) && be.Status == http.StatusNotFound {
			continue // /billing/meter/* 404 → 换 /v2/billing/meter/*
		}
		return nil, err
	}
	return nil, lastErr
}

// workbuddyBillingRequestOnce 发送单次 billing 请求并解信封：
// HTTP >= 400 或业务 code != 0 返回 *workbuddyBillingError（幂等判定/路径回落的判据）；
// 传输层错误与解析错误返回普通 error（不参与幂等判定）。
func (s *WorkBuddyCreditsService) workbuddyBillingRequestOnce(
	ctx context.Context,
	account *Account,
	method string,
	path string,
	body any,
) (json.RawMessage, error) {
	baseURL := account.GetWorkbuddyBillingBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("workbuddy account %d missing billing base_url", account.ID)
	}
	targetURL := strings.TrimRight(baseURL, "/") + path
	// 探测发起前过出站 URL 安全策略（与网关转发/Grok 探测同一套校验）：
	// billing 域由账号 realm/凭据衍生，不得把凭据发往策略外主机。
	validatedURL, err := cnValidateProbeURL(s.cfg, targetURL)
	if err != nil {
		return nil, infraerrors.New(http.StatusForbidden, "WORKBUDDY_BILLING_URL_REJECTED", err.Error())
	}
	var bodyReader *bytes.Reader
	if body != nil {
		raw, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return nil, fmt.Errorf("marshal workbuddy billing body: %w", marshalErr)
		}
		bodyReader = bytes.NewReader(raw)
	}
	reqCtx, cancel := context.WithTimeout(ctx, workbuddyBillingTimeout)
	defer cancel()
	var reqBody io.Reader
	if bodyReader != nil {
		reqBody = bodyReader
	}
	req, err := http.NewRequestWithContext(reqCtx, method, validatedURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("build workbuddy billing request: %w", err)
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	applyWorkbuddyBillingHeaders(req, account.GetWorkbuddyCredentials(), account, account.GetWorkbuddyRealm())

	resp, err := s.httpUpstream.Do(req, s.resolveWorkbuddyProxyURL(ctx, account), account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return nil, fmt.Errorf("workbuddy billing transport error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, workbuddyBillingMaxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read workbuddy billing response: %w", err)
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
		return nil, fmt.Errorf("parse workbuddy billing response: %w (body: %s)", parseErr, workbuddyTruncateForError(string(raw)))
	}
	if env.Code != 0 {
		return nil, &workbuddyBillingError{Status: resp.StatusCode, Code: env.Code, Msg: workbuddyTruncateForError(env.Msg)}
	}
	return env.Data, nil
}

// resolveWorkbuddyProxyURL 解析账号代理（account.Proxy 未预载时经 proxyRepo 补载）。
func (s *WorkBuddyCreditsService) resolveWorkbuddyProxyURL(ctx context.Context, account *Account) string {
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

// persistWorkbuddyCreditsSnapshot 把积分快照写入 account.Extra（失败只告警不失败：
// 快照是观测数据，写入失败不应把一次成功查询变成错误）。
func (s *WorkBuddyCreditsService) persistWorkbuddyCreditsSnapshot(ctx context.Context, account *Account, result *WorkBuddyCreditsResult) {
	if s.accountRepo == nil || account == nil || result == nil || !result.Success {
		return
	}
	snapshot := WorkBuddyCreditsSnapshot{
		Remain:        result.Remain,
		Used:          result.Used,
		Size:          result.Size,
		Packs:         result.Packs,
		Packages:      result.Packages,
		FetchedAt:     result.FetchedAt,
		Realm:         result.Realm,
		NotApplicable: result.NotApplicable,
		Enterprise:    result.Enterprise,
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{workbuddyCreditsExtraKey: snapshot}); err != nil {
		slog.Warn("workbuddy credits snapshot persist failed", "account_id", account.ID, "error", err)
	}
}

// persistWorkbuddyCheckinSnapshot 把签到快照写入 account.Extra。
// 合并语义：当日已成功（ok/already）的既有记录不被后续失败重试覆盖（避免把
// 「今日已签到」误降级为失败）；skipped 不落盘。
func (s *WorkBuddyCreditsService) persistWorkbuddyCheckinSnapshot(ctx context.Context, account *Account, result *WorkBuddyCheckinResult) {
	if s.accountRepo == nil || account == nil || result == nil || result.Status == WorkBuddyCheckinStatusSkipped {
		return
	}
	next := WorkBuddyCheckinSnapshot{
		Date:      timezone.Now().Format("2006-01-02"),
		Status:    result.Status,
		CheckedAt: result.CheckedAt,
		Credits:   result.Credits,
	}
	if shouldKeepWorkbuddyCheckinSnapshot(readWorkbuddyCheckinSnapshot(account), next) {
		return
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{workbuddyCheckinExtraKey: next}); err != nil {
		slog.Warn("workbuddy checkin snapshot persist failed", "account_id", account.ID, "error", err)
	}
}

// shouldKeepWorkbuddyCheckinSnapshot 报告是否应保留既有签到快照（不写入 next）：
// 同日、既有记录为成功（ok/already）、新结果为失败时保留。
func shouldKeepWorkbuddyCheckinSnapshot(existing *WorkBuddyCheckinSnapshot, next WorkBuddyCheckinSnapshot) bool {
	if existing == nil || existing.Date == "" || existing.Date != next.Date {
		return false
	}
	prevSuccess := existing.Status == WorkBuddyCheckinStatusOK || existing.Status == WorkBuddyCheckinStatusAlready
	return prevSuccess && next.Status == WorkBuddyCheckinStatusFail
}

// readWorkbuddyCheckinSnapshot 从 account.Extra 解析签到快照（缺失/形态异常返回 nil）。
func readWorkbuddyCheckinSnapshot(account *Account) *WorkBuddyCheckinSnapshot {
	if account == nil || account.Extra == nil {
		return nil
	}
	raw, ok := account.Extra[workbuddyCheckinExtraKey]
	if !ok || raw == nil {
		return nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	snapshot := &WorkBuddyCheckinSnapshot{}
	if v, ok := obj["date"].(string); ok {
		snapshot.Date = v
	}
	if v, ok := obj["status"].(string); ok {
		snapshot.Status = v
	}
	snapshot.CheckedAt = workbuddyAnyInt64(obj["checked_at"])
	if credits, ok := obj["credits"]; ok && credits != nil {
		value := workbuddyAnyInt64(credits)
		snapshot.Credits = &value
	}
	return snapshot
}

// workbuddyCheckinStatusToday 返回账号当日签到状态（非当日/无记录返回空串）。
func workbuddyCheckinStatusToday(account *Account) string {
	snapshot := readWorkbuddyCheckinSnapshot(account)
	if snapshot == nil {
		return ""
	}
	if snapshot.Date != timezone.Now().Format("2006-01-02") {
		return ""
	}
	return snapshot.Status
}

// workbuddyAnyInt64 把 extra 快照里的数值（float64/int64/字符串数字）归一为 int64。
func workbuddyAnyInt64(raw any) int64 {
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

// workbuddyBillingTimeLayout 上游过滤串的时间格式（墙钟，对齐参考实现 packageEndLayout）。
const workbuddyBillingTimeLayout = "2006-01-02 15:04:05"

// workbuddyBillingEnvelope 上游 {code,msg,data} 业务信封。
type workbuddyBillingEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// workbuddyResourcePackage get-user-resource 响应中的单个套餐条目。
type workbuddyResourcePackage struct {
	PackageName         string `json:"PackageName"`
	CycleEndTime        string `json:"CycleEndTime"`
	CapacitySize        int64  `json:"CapacitySize"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
}

// workbuddyUserResourceResp get-user-resource 响应结构。
type workbuddyUserResourceResp struct {
	Response struct {
		Data struct {
			TotalDosage int64                      `json:"TotalDosage"`
			Accounts    []workbuddyResourcePackage `json:"Accounts"`
		} `json:"Data"`
	} `json:"Response"`
}

// workbuddyAggregateResource 聚合套餐列表为 remain/used/size/packs（对齐参考实现
// ResourceSummary 口径：TotalDosage 作 size 下限，已消耗的不该比总剂量小）。
func workbuddyAggregateResource(resp *workbuddyUserResourceResp) (remain, used, size int64, packs int, packages []WorkBuddyCreditsPackage) {
	if resp == nil {
		return 0, 0, 0, 0, nil
	}
	for _, pkg := range resp.Response.Data.Accounts {
		r, u, s := workbuddyPackageRemainUsed(pkg)
		remain += r
		used += u
		size += s
		packages = append(packages, WorkBuddyCreditsPackage{
			PackageName:  pkg.PackageName,
			Remain:       r,
			Used:         u,
			Size:         s,
			CycleEndTime: pkg.CycleEndTime,
		})
	}
	packs = len(resp.Response.Data.Accounts)
	if size > 0 {
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	if dosage := resp.Response.Data.TotalDosage; dosage > size {
		size = dosage
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	return remain, used, size, packs, packages
}

// workbuddyPackageRemainUsed 聚合单套餐的 remain/used/size（与参考实现
// packageRemainUsed 同口径）：Cycle 期套餐优先用 CycleCapacity 三字段，
// used 取 CycleUsed 与 size-remain 的较大者；否则回退 Capacity 三字段。
func workbuddyPackageRemainUsed(pkg workbuddyResourcePackage) (remain, used, size int64) {
	if pkg.CycleCapacitySize > 0 {
		remain = pkg.CycleCapacityRemain
		size = pkg.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if pkg.CycleCapacityUsed > used {
			used = pkg.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = pkg.CapacityRemain
	used = pkg.CapacityUsed
	size = pkg.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}
