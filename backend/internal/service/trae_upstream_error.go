package service

// trae_upstream_error.go Trae 上游业务错误码字典与账号状态分级处置。
//
// 背景：Trae 上游的**主要拒绝形态不是 HTTP 4xx，而是 HTTP 200 + SSE 流内
// event:error 帧**（模型不在版本表内 4001、额度耗尽 1005、令牌失效 1001、风控
// 4010/4015 全部走这一条）。若归一层只把错误帧转成 CC error 透传，就会出现：
// 账号被上游限额后管理面板仍显示「正常」、调度器持续选用、用户反复撞空回复，
// 且这一整次请求被记为成功并按 0 token 出账。本文件补齐请求路径的落状态处置。
//
// 结构与口径对齐 qoder_upstream_error.go（同仓库内的先例）：分级 → 幂等落状态 →
// 写 Ops 事件；字典外错误码不落账号状态（避免把请求级/环境级问题误标成账号坏了）。
//
// 错误码语义来源：Trae 官方错误码文档（docs.trae.ai/ide/error-codes）与参考实现
// 分类（1005 套餐/额度限制、401 会话失效）。凡不确定的一律归 unknown 不落状态。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// traeUpstreamErrorClass 是 Trae 业务错误码的处置分类。
type traeUpstreamErrorClass string

const (
	traeErrorClassNone     traeUpstreamErrorClass = ""                   // 无码 / code "0"：不是业务错误
	traeErrorClassUnknown  traeUpstreamErrorClass = "unknown"            // 字典外错误码：不落状态
	traeErrorClassQuota    traeUpstreamErrorClass = "quota_exhausted"    // 额度/积分类：次日 0 点自动恢复
	traeErrorClassSession  traeUpstreamErrorClass = "session_expired"    // 令牌/登录态失效：需重新登录
	traeErrorClassRestrict traeUpstreamErrorClass = "account_restricted" // 风控限制：需人工处理
)

// traeSessionRecoveryCooldown 会话类错误（1001/401）叠加 refresh_token 时的冷却时长。
//
// 为何是 10 分钟而不是更长：Trae 后台刷新周期的间隔远大于它，而请求路径的预刷新
// 会在下一次请求时自行换票成功。冷却这么久只是为了避开「同一批并发请求连环撞 401」
// 和「刷新刚成功但缓存未失效」两个窗口；与 rate_limit.oauth_401_cooldown_minutes
// 的默认值（10）保持一致，避免同一平台的两条恢复路径给出不同冷却时长。
const traeSessionRecoveryCooldown = 10 * time.Minute

// traeQuotaErrorCodes 额度/积分类错误码（可自动恢复）。
var traeQuotaErrorCodes = map[string]struct{}{
	"1005": {}, // 套餐/积分额度耗尽（plan or credits limit）
}

// traeSessionErrorCodes 登录态类错误码：access token 已不可用，需重新登录换取。
var traeSessionErrorCodes = map[string]struct{}{
	"1001": {}, // 会话失效（本仓库 UG 域实测同码，见 trae_credits_service.go）
	"401":  {}, // HTTP 401 的字符串形态
}

// traeAccountRestrictedErrorCodes 风控/资格类错误码（官方口径需人工处理或等待解除）。
var traeAccountRestrictedErrorCodes = map[string]struct{}{
	"4010": {}, // 检测到风险账号，已自动登出
	"4015": {}, // 账户/IP 存在风险，请求被阻止（官方口径约 24h 自动解除）
}

// classifyTraeBusinessError 把上游业务错误码映射为处置分类。
func classifyTraeBusinessError(code string) traeUpstreamErrorClass {
	c := strings.TrimSpace(code)
	if c == "" || c == "0" {
		return traeErrorClassNone
	}
	if _, ok := traeQuotaErrorCodes[c]; ok {
		return traeErrorClassQuota
	}
	if _, ok := traeSessionErrorCodes[c]; ok {
		return traeErrorClassSession
	}
	if _, ok := traeAccountRestrictedErrorCodes[c]; ok {
		return traeErrorClassRestrict
	}
	return traeErrorClassUnknown
}

// traeQuotaRecoveryUntil 额度类错误的恢复时间：次日 0 点（跟随全局时区）。
// Trae 积分按自然日/自然月重置，与官方「明天再试」口径对齐；到期自动恢复调度，
// 若仍被限额，下一次请求会再次触发标记。
func traeQuotaRecoveryUntil(now time.Time) time.Time {
	return timezone.StartOfDay(now).AddDate(0, 0, 1)
}

// traeBusinessErrorDetail 拼装管理端可见的错误详情（空消息回落 code 文本，上游原文
// 按 rune 限长 200，再追加本仓处置提示）。
//
// 截断与追加的顺序不能倒：hint 自身就有 183 字符，先拼后截会把提示的尾部（恰好是
// 「该怎么做」那半句）削成 `...`；先截上游原文再追加，才能保证提示完整送达。
// 本函数结果是 Ops 事件 Detail / 账号状态 reason 的唯一加工作台，不外发。
func traeBusinessErrorDetail(code, message string) string {
	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = "code " + strings.TrimSpace(code)
	}
	runes := []rune(msg)
	if len(runes) > 200 {
		msg = string(runes[:200]) + "..."
	}
	return msg + traeUpstreamHint(code)
}

// traeOpsBizErrorSeenKey 标记本请求已处置过某 (账号, 业务码)。
//
// 去重必要性：落状态含多次 DB/Redis 往返，而回调运行在 SSE 读流热路径内；上游重发
// 错误帧会让回调触发多次，SetError 又没有 SQL 级幂等守卫，故按请求粒度只处置首帧。
// 帧本身的透传不受影响（对外协议不变），仅「处置」去重。
const traeOpsBizErrorSeenKey = "trae_biz_error_marked"

// traeMarkBizErrorOnce 报告本请求内该 (账号, 业务码) 是否首次处置。
// gin.Context 的 Keys 自带锁，且回调与请求处理同 goroutine，无竞态。
func traeMarkBizErrorOnce(c *gin.Context, accountID int64, code string) bool {
	if c == nil {
		return true
	}
	key := fmt.Sprintf("%d:%s", accountID, code)
	set, _ := c.Get(traeOpsBizErrorSeenKey)
	seen, _ := set.(map[string]struct{})
	if seen == nil {
		seen = map[string]struct{}{}
	}
	if _, dup := seen[key]; dup {
		return false
	}
	seen[key] = struct{}{}
	c.Set(traeOpsBizErrorSeenKey, seen)
	return true
}

// applyTraeBusinessErrorClass 按已解析的分类落账号状态。
//
// ctx 收敛口径与 qoder 同源：本函数会被流式 reader 在下游 Read() 调用栈内同步调用，
// 而调用方给的 ctx 要么是脱离取消链的 WithoutCancel（DB 卡顿会挂住 SSE 转发），要么
// 是会被客户端断开取消的入站 ctx（管理员关掉页面即静默丢标记）。因此统一收敛到仓库
// 既有的账号状态写口径（openAIAccountStateContext = WithoutCancel + 5s 超时）。
func applyTraeBusinessErrorClass(ctx context.Context, repo AccountRepository, account *Account, code string, class traeUpstreamErrorClass, message string) {
	if account == nil || repo == nil || class == traeErrorClassNone || class == traeErrorClassUnknown {
		return
	}
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	detail := traeBusinessErrorDetail(code, message)
	switch class {
	case traeErrorClassQuota:
		reason := fmt.Sprintf("Trae upstream quota exhausted (code %s): %s", code, detail)
		if err := repo.SetTempUnschedulable(stateCtx, account.ID, traeQuotaRecoveryUntil(time.Now()), reason); err != nil {
			logger.L().Warn("trae quota error set temp unschedulable failed",
				zap.Int64("account_id", account.ID), zap.String("code", code), zap.Error(err))
		}
	case traeErrorClassSession:
		// 【不得永久 SetError（2026-10-27 续期排查）】1001/401 在本平台是「access token
		// 过期或会话失活」，而 Trae 持有可轮换的 refresh_token（apikey 类型但凭据语义与
		// OAuth 同构）。永久 SetError 会写 status=error + schedulable=false，后台刷新器
		// 的候选查询带 `status = 'active'` 条件（account_repo.ListOAuthRefreshCandidatePage）
		// ⇒ 账号再也不会被刷新，只能管理员手工重登 —— 与 Qoder 签到 TOKEN_EXPIRE、
		// WorkBuddy 全量 401 完全同形的坑。
		//
		// 改为临时不可调度：冷却期内不被选中，到期后由后台刷新器或请求路径预刷新自愈。
		// 无 refresh_token 的号确实不可自愈，仍走永久 SetError（否则冷却结束后只是再撞一次）。
		//
		// 【判据必须与 HTTP 401 路径同源（2026-10-30 自查修复）】这里原来只调
		// isRefreshableAPIKeyCredential（内含 Type==APIKey 门），而 401 分支用的是泛化判据。
		// 同一个 Trae 账号、同样撞令牌过期，只因 Type 不同就一个冷却、一个永久下线：
		// 临时探针实测 Type=oauth 在此分支落到 setError=1 / tempUnsched=0。
		// 现在统一走 canSelfHealSessionFailure（= 可自愈类型族 ∧ 真的拿得到票）。
		// 反向护栏：把本行改回 !isRefreshableAPIKeyCredential(account)，
		// TestApplyTraeBusinessErrorClass_SessionOAuthTypeCoolsDown 必须 FAIL。
		if !canSelfHealSessionFailure(account) {
			msg := fmt.Sprintf("Trae upstream rejected the session (code %s, no refresh_token): %s", code, detail)
			if err := repo.SetError(stateCtx, account.ID, msg); err != nil {
				logger.L().Warn("trae session error mark failed",
					zap.Int64("account_id", account.ID), zap.String("code", code), zap.Error(err))
			}
			break
		}
		msg := fmt.Sprintf("Trae session expired (code %s): refresh_token present, cooling down for automatic recovery: %s", code, detail)
		until := time.Now().Add(traeSessionRecoveryCooldown)
		if err := repo.SetTempUnschedulable(stateCtx, account.ID, until, msg); err != nil {
			logger.L().Warn("trae session error set temp unschedulable failed",
				zap.Int64("account_id", account.ID), zap.String("code", code), zap.Error(err))
		}
	case traeErrorClassRestrict:
		msg := fmt.Sprintf("Trae upstream account restricted (code %s): %s", code, detail)
		if err := repo.SetError(stateCtx, account.ID, msg); err != nil {
			logger.L().Warn("trae restricted error mark failed",
				zap.Int64("account_id", account.ID), zap.String("code", code), zap.Error(err))
		}
	}
}

// traeFailoverErrorClass 报告一个流内业务错误是否应让本请求换号重试。
//
// 判定直接复用 classifyTraeBusinessError 的分级（不另建一套口径，避免两处对同一
// 错误码给出矛盾结论）：
//   - quota / session / restrict 都是**这一张号**的局部问题（额度耗尽、票失效、
//     被风控），换号能直接解决 → 换；
//   - unknown（典型 4001）是**请求级/通道级**确定失败：所有 Trae 号共用同一出站
//     function，换号只会把同一个错再打一遍，白耗一次上游往返。
//   - none（无码/code "0"）不是错误 → 不换。
func traeFailoverErrorClass(class traeUpstreamErrorClass) bool {
	switch class {
	case traeErrorClassQuota, traeErrorClassSession, traeErrorClassRestrict:
		return true
	default:
		return false
	}
}

// traeFailoverableBusinessError 报告该流内错误是否值得在「尚未向客户端写出任何
// 字节」的窗口里换号重试（见 traeSSEReader.traeProbeFirstFrame）。
func traeFailoverableBusinessError(err error) bool {
	var streamErr *traeStreamError
	if !errors.As(err, &streamErr) || streamErr == nil {
		return false
	}
	return traeFailoverErrorClass(classifyTraeBusinessError(streamErr.Code))
}

// newTraeStreamFailoverError 把流内业务错误包成换号错误。
//
// 调用前提：本请求尚未向客户端写出任何字节（由 probe 保证），否则换号会重放输出。
// 账号状态与 Ops 事件已由 markTraeAccountFromBusinessError 落过，本函数不再重复记录。
//
// 字段取向（与 handler 消费端对齐，openai_chat_completions.go 的 failover 分支）：
//   - RetryableOnSameAccount=false：1005/1001/4015 对**这张号**是确定态，同账号重试
//     只是多花一次上游往返，直接换号；也因此不会误触发 TempUnscheduleRetryableError。
//   - Stage=Inference（非 AccountAuth）：避开 credentialFailoverClientResponse 把文案
//     改写成与 Trae 无关的 Grok 口径，走 mapUpstreamError 的语义映射。
//   - StatusCode 按分类取 429/401/403，耗尽后下发给客户端的文案与排障语义对齐；
//     ResponseBody 仍携带上游原文（经 sanitize）供错误透传规则匹配与 Ops 记录。
func newTraeStreamFailoverError(err error, now time.Time) *UpstreamFailoverError {
	var streamErr *traeStreamError
	if !errors.As(err, &streamErr) || streamErr == nil {
		return nil
	}
	code := strings.TrimSpace(streamErr.Code)
	class := classifyTraeBusinessError(code)
	if !traeFailoverErrorClass(class) {
		return nil
	}

	statusCode := http.StatusBadGateway
	switch class {
	case traeErrorClassQuota:
		statusCode = http.StatusTooManyRequests
	case traeErrorClassSession:
		statusCode = http.StatusUnauthorized
	case traeErrorClassRestrict:
		statusCode = http.StatusForbidden
	}

	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(streamErr.Message))
	if message == "" {
		message = "Trae upstream returned an in-stream error"
	}
	if code != "" {
		message = message + " (code=" + code + ")"
	}

	headers := http.Header{}
	if class == traeErrorClassQuota {
		// 额度按自然日/自然月重置，与 SetTempUnschedulable 的次日 0 点同口径；
		// 上限 7 天由 handler 的 isSafeRetryAfter 把关，本值不会超 24h。
		if delta := traeQuotaRecoveryUntil(now).Sub(now); delta > 0 {
			headers.Set("Retry-After", strconv.FormatInt(int64(math.Ceil(delta.Seconds())), 10))
		}
	}

	body, marshalErr := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "upstream_error",
			"code":    code,
			"message": message,
		},
	})
	if marshalErr != nil {
		body = []byte(`{"error":{"type":"upstream_error","message":"Trae upstream error"}}`)
	}

	return &UpstreamFailoverError{
		StatusCode:        statusCode,
		ResponseBody:      body,
		ResponseHeaders:   headers,
		Stage:             GatewayFailureStageInference,
		Scope:             GatewayFailureScopeAccount,
		Reason:            GatewayFailureReason("trae_" + string(class)),
		NextAccountAction: NextAccountRetry,
	}
}

// markTraeAccountFromBusinessError 网关请求路径的流内错误处置入口：分级落账号状态
// 并写入 Ops 上游错误事件（管理页与排障可见）。同一请求内重复错误帧只处置一次。
//
// 可观测口径：业务错误的真实传输状态是 HTTP 200（错误藏在流内），故
// UpstreamStatusCode 填 200 而非留 0——留 0 会让
// checkSkipMonitoringForUpstreamEvent 直接早退，管理员配的 skip_monitoring 规则将
// 永久不生效。Kind 用 stream_error，与 gateway_forward.go 对 SSE 流内 error 事件
// 的既有范式一致。字典外码不落账号状态（4001 一类请求级语义，避免误标好号），
// 但仍留 Ops 痕迹，否则面板与错误日志完全不可见。
func (s *OpenAIGatewayService) markTraeAccountFromBusinessError(ctx context.Context, c *gin.Context, account *Account, code, message string) {
	if s == nil || account == nil {
		return
	}
	class := classifyTraeBusinessError(code)
	if class == traeErrorClassNone {
		return
	}
	if !traeMarkBizErrorOnce(c, account.ID, strings.TrimSpace(code)) {
		return
	}
	if class == traeErrorClassUnknown {
		logger.L().Debug("trae upstream business error frame not classified",
			zap.Int64("account_id", account.ID), zap.String("code", strings.TrimSpace(code)))
	} else {
		applyTraeBusinessErrorClass(ctx, s.accountRepo, account, strings.TrimSpace(code), class, message)
	}
	if c == nil {
		return
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: http.StatusOK,
		UpstreamURL:        traeChatPath,
		Kind:               "stream_error",
		Stage:              "trae_upstream_frame",
		Reason:             string(class),
		Message:            fmt.Sprintf("trae upstream business error code=%s", strings.TrimSpace(code)),
		Detail:             traeBusinessErrorDetail(code, message),
	})
}
