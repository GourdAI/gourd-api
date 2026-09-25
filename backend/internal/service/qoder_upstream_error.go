package service

// qoder_upstream_error.go Qoder 上游业务错误码字典与账号状态分级处置。
//
// 背景（2026-09-22 排查坐实）：Qoder 上游的业务拒绝不走 HTTP 4xx/5xx，而是
// HTTP 200 + SSE 流内错误帧（信封 {"statusCodeValue":...} 或内层 {"code":"110",...}）。
// 请求转发路径此前只把错误帧规范化为 CC error 帧透传给客户端，不落任何账号
// 状态——账号被上游限额后管理面板仍显示「正常」，调度器持续选用，用户反复撞
// 「今日额度已用尽」（错误码 110）。本文件补齐请求路径的落状态处置，并与账号
// 测试路径共用同一分级口径。
//
// 错误码字典来源：Qoder 官方客户端内置文案表（app.asar，2026-09-22 提取），
// 与客户端弹窗文案逐字吻合：
//   - 110 今日额度已用尽 / 111 可用额度已用尽 / 119 今日免费额度已用尽 /
//     115 轻量模型月度额度已用尽 / 112·116·117·118 Credits 配额类
//     → 自动恢复：临时不可调度至次日 0 点（官方口径「请明天再试」）；
//   - 105 登录已过期 → 标记错误态（需重新登录）；
//   - 104/108/109 账户无访问资格 / 无可用许可证 / 应用被禁用
//     → 标记错误态（需管理员处理）。
//
// 其余码（101 签名、102 时间戳、103 重复请求、107 网络、113 使用限制、
// 114 试用限制等）为请求级/环境级语义，不落账号状态（仅 debug 日志）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// qoderUpstreamErrorClass 是 Qoder 业务错误码的处置分类。
type qoderUpstreamErrorClass string

const (
	qoderErrorClassNone     qoderUpstreamErrorClass = ""                   // 无码 / code "0"：不是业务错误
	qoderErrorClassUnknown  qoderUpstreamErrorClass = "unknown"            // 字典外错误码：不落状态
	qoderErrorClassQuota    qoderUpstreamErrorClass = "quota_exhausted"    // 额度/配额类：次日 0 点自动恢复
	qoderErrorClassSession  qoderUpstreamErrorClass = "session_expired"    // 105：需重新登录
	qoderErrorClassRestrict qoderUpstreamErrorClass = "account_restricted" // 104/108/109：需管理员处理
)

// qoderQuotaErrorCodes 额度/配额类错误码（自动恢复）。
var qoderQuotaErrorCodes = map[string]struct{}{
	"110": {}, // 今日额度已用尽（Daily usage limit reached）
	"111": {}, // 可用额度已用尽（Quota exhausted）
	"112": {}, // 可用 Credits 不足
	"115": {}, // 轻量模型月度额度已用尽
	"116": {}, // 配额已用尽（下个订阅周期重置）
	"117": {}, // 配额已用尽（管理员增购）
	"118": {}, // 可用 Credits 不足（升级订阅）
	"119": {}, // 今日免费额度已用尽
}

// qoderSessionErrorCodes 登录态类错误码（需重新 OAuth / 粘贴新令牌）。
var qoderSessionErrorCodes = map[string]struct{}{
	"105": {}, // 登录已过期（Session expired / Login expired）
}

// qoderAccountRestrictedErrorCodes 账户/许可类错误码（需管理员处理）。
var qoderAccountRestrictedErrorCodes = map[string]struct{}{
	"104": {}, // 账户未获得试用额度或访问资格
	"108": {}, // 暂无可用许可证
	"109": {}, // 应用已被禁用
}

// classifyQoderBusinessError 把上游业务错误码映射为处置分类。
func classifyQoderBusinessError(code string) qoderUpstreamErrorClass {
	c := strings.TrimSpace(code)
	if c == "" || c == "0" {
		return qoderErrorClassNone
	}
	if _, ok := qoderQuotaErrorCodes[c]; ok {
		return qoderErrorClassQuota
	}
	if _, ok := qoderSessionErrorCodes[c]; ok {
		return qoderErrorClassSession
	}
	if _, ok := qoderAccountRestrictedErrorCodes[c]; ok {
		return qoderErrorClassRestrict
	}
	return qoderErrorClassUnknown
}

// resolveQoderBusinessErrorCode 解析错误的“有效业务码”：优先用帧里直接携带的
// code；非字典码/空码时尝试从消息文本中提取内嵌业务码——信封级错误
// （statusCodeValue 非 200）时业务码可能藏在 body 字符串里，形如
// {"code":"110","message":"..."}（与“HTTP 200 信封包 403”同源的嵌套形态）。
// 返回 (有效码, 分类)；无法解析时码原样返回、分类为 unknown/none。
func resolveQoderBusinessErrorCode(code, message string) (string, qoderUpstreamErrorClass) {
	code = strings.TrimSpace(code)
	if class := classifyQoderBusinessError(code); class == qoderErrorClassQuota ||
		class == qoderErrorClassSession || class == qoderErrorClassRestrict {
		return code, class
	}
	if embedded := qoderExtractEmbeddedErrorCode(message); embedded != "" {
		if class := classifyQoderBusinessError(embedded); class != qoderErrorClassNone && class != qoderErrorClassUnknown {
			return embedded, class
		}
	}
	return code, classifyQoderBusinessError(code)
}

// qoderExtractEmbeddedErrorCode 从错误消息文本中提取内嵌业务码。仅接受
// JSON 对象形态的 {"code":...}，避免对普通文本误判。类型口径复用
// qoderInnerBusinessCode（字符串/数字等价），与帧解析层保持单一真相源。
func qoderExtractEmbeddedErrorCode(message string) string {
	msg := strings.TrimSpace(message)
	if msg == "" || !strings.HasPrefix(msg, "{") {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(msg), &obj); err != nil {
		return ""
	}
	return qoderInnerBusinessCode(obj)
}

// qoderQuotaRecoveryUntil 额度类错误的恢复时间：次日 0 点（跟随全局时区），
// 与官方文案「请明天再试」对齐。到期后账号自动恢复调度；若仍被限额，下一次
// 请求会再次触发标记（月度/周期类错误码同样按日探测，语义见各码注释）。
func qoderQuotaRecoveryUntil(now time.Time) time.Time {
	return timezone.StartOfDay(now).AddDate(0, 0, 1)
}

// qoderBusinessErrorDetail 拼装错误详情（空消息回落 code 文本，限长 200 rune）。
func qoderBusinessErrorDetail(code, message string) string {
	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = "code " + strings.TrimSpace(code)
	}
	return qoderTruncateReason(msg, 200)
}

// qoderTruncateReason 按 rune 截断（避免中文多字节截半）。
func qoderTruncateReason(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}

// applyQoderBusinessErrorAccountState 按业务错误码分级落账号状态（请求路径与账号
// 测试路径共用）：
//   - 额度/配额类 → 临时不可调度至次日 0 点（面板显示可恢复的回避期，自动到期，
//     也支持管理端手动清除）；
//   - 登录态 105 / 账户类 104·108·109 → 标记错误态（status=error，需人工处理）；
//   - 字典外错误码 → 不落状态（仅返回分类，由调用方决定可观测记录）。
//
// repo 为空（部分单测构造）时仅返回分类、不落状态；写失败只记 Warn 不阻断。
func applyQoderBusinessErrorAccountState(ctx context.Context, repo AccountRepository, account *Account, code, message string) qoderUpstreamErrorClass {
	resolvedCode, class := resolveQoderBusinessErrorCode(code, message)
	applyQoderBusinessErrorClass(ctx, repo, account, resolvedCode, class, message)
	return class
}

// applyQoderBusinessErrorClass 按已解析的分类落账号状态（不重复解析 code，
// 供调用方复用 resolve 结果）。
//
// 超时口径：本函数会被流式 reader 在下游 Read() 调用栈内**同步**调用
// （qoder_sse.go 的 onBizErr → pump → Read），而落状态包含多次 DB/Redis 往返
// （SetTempUnschedulable = UPDATE + scheduler outbox + GetByID + 调度快照）。
// 调用方给的 ctx 可能是无 deadline 的 WithoutCancel（qoder_client.go）或会被
// 客户端断开取消的入站 ctx（qoder_gateway.go 测试路径），两者都会出问题：
// 前者 DB 卡顿会直接挂住 SSE 转发（客户端读不到字节也断不开，goroutine 泄漏），
// 后者会在管理员关掉测试页面时静默丢标记。因此在此统一收敛到仓库既有的
// 账号状态写口径（openAIAccountStateContext = WithoutCancel + 5s 超时，
// 与 openai_gateway_grok.go / gateway_upstream_transport_error.go 同）。
func applyQoderBusinessErrorClass(ctx context.Context, repo AccountRepository, account *Account, resolvedCode string, class qoderUpstreamErrorClass, message string) {
	if account == nil || repo == nil || class == qoderErrorClassNone || class == qoderErrorClassUnknown {
		return
	}
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	detail := qoderBusinessErrorDetail(resolvedCode, message)
	switch class {
	case qoderErrorClassQuota:
		reason := fmt.Sprintf("Qoder upstream quota exhausted (code %s): %s", resolvedCode, detail)
		if err := repo.SetTempUnschedulable(stateCtx, account.ID, qoderQuotaRecoveryUntil(time.Now()), reason); err != nil {
			logger.L().Warn("qoder quota error set temp unschedulable failed",
				zap.Int64("account_id", account.ID), zap.String("code", resolvedCode), zap.Error(err))
		}
	case qoderErrorClassSession:
		// 文案与既有测试路径口径一致（登录态拒绝自修复以来即为此格式）。
		msg := fmt.Sprintf("Qoder upstream rejected the session: %s", detail)
		if err := repo.SetError(stateCtx, account.ID, msg); err != nil {
			logger.L().Warn("qoder session error mark failed",
				zap.Int64("account_id", account.ID), zap.String("code", resolvedCode), zap.Error(err))
		}
	case qoderErrorClassRestrict:
		msg := fmt.Sprintf("Qoder upstream account restricted (code %s): %s", resolvedCode, detail)
		if err := repo.SetError(stateCtx, account.ID, msg); err != nil {
			logger.L().Warn("qoder restricted error mark failed",
				zap.Int64("account_id", account.ID), zap.String("code", resolvedCode), zap.Error(err))
		}
	}
}

// qoderOpsBizErrorSeenKey 标记本请求已处置过某 (账号, 有效业务码)。
//
// 去重必要性：qoder_sse.go 的业务错误帧分支写完 CC error 帧后不终止流
// （保持修复前的帧透传行为），上游重发错误帧时回调会被触发多次；而
// SetError（105/104/108/109）没有 SetTempUnschedulable 那样的 SQL 级幂等守卫
// （temp_unschedulable_until < $1 条件 + affected<=0 短路），每帧都会做一次
// UPDATE + scheduler outbox INSERT + GetByID + Redis 写，全部串行阻塞在 SSE
// 读流热路径上；Ops 事件也会每帧 append 一次（入库上限
// opsUpstreamErrorsMaxEvents 从新往回保留，狂发错误帧的上游能把同请求内
// 其它账号的 failover 记录挤掉）。因此按请求粒度去重，只处置首帧。
const qoderOpsBizErrorSeenKey = "qoder_biz_error_marked"

// qoderMarkBizErrorOnce 报告本请求内该 (账号, 业务码) 是否首次处置。
// gin.Context 的 Keys 自带锁，且回调与请求处理同 goroutine，无竞态。
func qoderMarkBizErrorOnce(c *gin.Context, accountID int64, code string) bool {
	if c == nil {
		return true
	}
	key := fmt.Sprintf("%d:%s", accountID, code)
	set, _ := c.Get(qoderOpsBizErrorSeenKey)
	seen, _ := set.(map[string]struct{})
	if seen == nil {
		seen = map[string]struct{}{}
	}
	if _, dup := seen[key]; dup {
		return false
	}
	seen[key] = struct{}{}
	c.Set(qoderOpsBizErrorSeenKey, seen)
	return true
}

// markQoderAccountFromBusinessError 网关请求路径的错误帧处置入口：分级落账号状态
// 并写入 Ops 上游错误事件（管理页与排障可见）。同一请求内重复的错误帧只处置一次。
//
// 可观测口径：Qoder 业务错误的真实传输状态是 HTTP 200（错误藏在流内），
// 因此 UpstreamStatusCode 填 200 而非留 0——留 0 会让
// checkSkipMonitoringForUpstreamEvent 直接早退，而 PlatformQoder 已列入
// error_passthrough_rule 的平台白名单，管理员配的 skip_monitoring 规则将永久
// 不生效（这类失败一律计入 SLA 失败率）。Kind 用 stream_error，与
// gateway_forward.go 对 SSE 流内 error 事件的既有范式一致。
// 字典外码不落账号状态（请求级/环境级语义，避免误标），但仍需留 Ops 痕迹：
// 否则信封级错误（statusCodeValue=403/418/429/500）在面板与错误日志里完全
// 不可见，与 HTTP>=400 路径（无条件写 Ops）口径分叉。
func (s *OpenAIGatewayService) markQoderAccountFromBusinessError(ctx context.Context, c *gin.Context, account *Account, code, message string) {
	if s == nil || account == nil {
		return
	}
	resolvedCode, class := resolveQoderBusinessErrorCode(code, message)
	if class == qoderErrorClassNone {
		return
	}
	// 请求内去重闸门必须在任何副作用（写库 / 写 Ops）**之前**：上游重发错误帧
	// 时回调会被触发多次，SetError 没有 SQL 级幂等守卫（见 qoderOpsBizErrorSeenKey
	// 注释），且重复写库会串行阻塞 SSE 读流热路径。帧本身的透传不受影响
	// （qoder_sse.go 仍逐帧输出 error 帧，对外协议不变），仅“处置”去重。
	if !qoderMarkBizErrorOnce(c, account.ID, resolvedCode) {
		return
	}
	if class == qoderErrorClassUnknown {
		logger.L().Debug("qoder upstream business error frame not classified",
			zap.Int64("account_id", account.ID), zap.String("code", strings.TrimSpace(code)))
	} else {
		applyQoderBusinessErrorClass(ctx, s.accountRepo, account, resolvedCode, class, message)
	}
	if c == nil {
		return
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:     opsUpstreamProxyID(account),
		ProxyName:   opsUpstreamProxyName(account),
		Platform:    account.Platform,
		AccountID:   account.ID,
		AccountName: account.Name,
		// 真实传输码：业务错误藏在 HTTP 200 的流内。非 0 值才能让
		// error_passthrough_rule（skip_monitoring / 关键字匹配）命中。
		UpstreamStatusCode: http.StatusOK,
		UpstreamURL:        qoderChatPath,
		Kind:               "stream_error",
		Stage:              "qoder_upstream_frame",
		Reason:             string(class),
		Message:            fmt.Sprintf("qoder upstream business error code=%s", resolvedCode),
		// Detail 携带上游原文（限长），供 MatchRule 做关键字匹配。
		Detail: qoderBusinessErrorDetail(resolvedCode, message),
	})
}
