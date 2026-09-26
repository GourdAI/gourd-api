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
	"fmt"
	"net/http"
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

// traeBusinessErrorDetail 拼装错误详情（空消息回落 code 文本，按 rune 限长 200）。
func traeBusinessErrorDetail(code, message string) string {
	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = "code " + strings.TrimSpace(code)
	}
	runes := []rune(msg)
	if len(runes) > 200 {
		return string(runes[:200]) + "..."
	}
	return msg
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
		msg := fmt.Sprintf("Trae upstream rejected the session: %s", detail)
		if err := repo.SetError(stateCtx, account.ID, msg); err != nil {
			logger.L().Warn("trae session error mark failed",
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
