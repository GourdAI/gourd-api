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
	"errors"
	"fmt"
	"io"
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

// qoderUpstreamErrorClass 是 Qoder 业务错误码的处置分类。
type qoderUpstreamErrorClass string

const (
	qoderErrorClassNone     qoderUpstreamErrorClass = ""                   // 无码 / code "0"：不是业务错误
	qoderErrorClassUnknown  qoderUpstreamErrorClass = "unknown"            // 字典外错误码：不落状态
	qoderErrorClassQuota    qoderUpstreamErrorClass = "quota_exhausted"    // 额度/配额类：次日 0 点自动恢复
	qoderErrorClassQueued   qoderUpstreamErrorClass = "account_queued"     // 10605：上游排队，短时冷却（按上游给的秒数）
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

// qoderQueuedErrorCodes 上游排队/背压类错误码（短时自愈，非账号级损坏）。
//
// 实证样本（2026-10-20 用户线上报错原文，HTTP 200 + 信封 statusCodeValue=403，
// 业务码内嵌在 body 文本里）：
//
//	{"code":"10605","message":"AIIsQueued","isQueued":true,"modelKey":"dfmodel",
//	 "queueCount":95,"queueType":"p3","retryAfterSeconds":30,
//	 "serviceAvailable":true,"waitTime":6}
//
// 与额度类（110/111/…）的语义差别至关重要：额度是**这张号当天不再可用**（恢复
// 到次日 0 点），排队是**这张号此刻排不上**（上游自带 retryAfterSeconds，且明确
// serviceAvailable=true）。因此冷却时长取上游给的秒数，绝不按日拉黑——把排队号
// 拉到次日 0 点会在一轮拥塞里误杀整批可用号。
var qoderQueuedErrorCodes = map[string]struct{}{
	"10605": {}, // AIIsQueued：模型排队中（queueCount/retryAfterSeconds 随行）
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
	if _, ok := qoderQueuedErrorCodes[c]; ok {
		return qoderErrorClassQueued
	}
	if _, ok := qoderSessionErrorCodes[c]; ok {
		return qoderErrorClassSession
	}
	if _, ok := qoderAccountRestrictedErrorCodes[c]; ok {
		return qoderErrorClassRestrict
	}
	return qoderErrorClassUnknown
}

// qoderQueuedCooldownBounds 排队类冷却的夹逼区间。
//
// 下限 5s：上游给的 retryAfterSeconds 可能是 0/1（拥塞时它更希望你立刻重试），
// 但我们的调度器换号是**零退避**的，同一秒内回灌同一张号只会把队列继续顶高。
// 上限 10min：queueCount=95 这种深队列上游可能给出很大的值，而排队是瞬时状态，
// 长时间停调会把整池号在拥塞窗口里锁死（号被停调后不再收流量，也就没有路径
// 观测到它已经排上了）。
const (
	qoderQueuedCooldownMin = 5 * time.Second
	qoderQueuedCooldownMax = 10 * time.Minute
)

// qoderParseRetryAfterSeconds 从错误消息文本里取 retryAfterSeconds（仅接受 JSON 对象形态）。
func qoderParseRetryAfterSeconds(message string) (int, bool) {
	msg := strings.TrimSpace(message)
	if msg == "" || !strings.HasPrefix(msg, "{") {
		return 0, false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(msg), &obj); err != nil {
		return 0, false
	}
	v := qoderInnerRetryAfterSeconds(obj)
	if v == nil {
		return 0, false
	}
	return *v, true
}

// qoderQueuedCooldown 排队类错误的避让时长（已夹逼到 [5s, 10min]）。
//
// 【优先用结构化值】retryAfterSeconds 与 code 同层，而 Message 只存 message **字段**；
// 只靠文本解析会把上游明说的等待值丢成回落值（实测：86400 变成 30s）。
// 【必须区分「字段缺失」与「上游显式给 0」】
//   - 有值（包括 0）→ 直接用，再夹逼（下限 5s：调度换号零退避，不给冷却会秒级
//     回灌同一队列）；
//   - 完全缺失/不可解析 → 回落 30s（实证样本值）。
func qoderQueuedCooldown(retryAfterSeconds *int, message string) time.Duration {
	seconds, present := 0, false
	if retryAfterSeconds != nil {
		seconds, present = *retryAfterSeconds, true
	} else if parsed, ok := qoderParseRetryAfterSeconds(message); ok {
		seconds, present = parsed, true
	}
	if !present {
		return 30 * time.Second
	}
	d := time.Duration(seconds) * time.Second
	if d < qoderQueuedCooldownMin {
		return qoderQueuedCooldownMin
	}
	if d > qoderQueuedCooldownMax {
		return qoderQueuedCooldownMax
	}
	return d
}

// resolveQoderBusinessErrorCode 解析错误的“有效业务码”：优先用帧里直接携带的
// code；非字典码/空码时尝试从消息文本中提取内嵌业务码——信封级错误
// （statusCodeValue 非 200）时业务码可能藏在 body 字符串里，形如
// {"code":"110","message":"..."}（与“HTTP 200 信封包 403”同源的嵌套形态）。
// 返回 (有效码, 分类)；无法解析时码原样返回、分类为 unknown/none。
func resolveQoderBusinessErrorCode(code, message string) (string, qoderUpstreamErrorClass) {
	code = strings.TrimSpace(code)
	if class := classifyQoderBusinessError(code); class == qoderErrorClassQuota ||
		class == qoderErrorClassQueued ||
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

// qoderTruncateReason 按 rune 截断（避免中文多字节截半）。截断前先把片段压成单行
// （见 flattenUpstreamSnippet）：Qoder 上游会把「写给模型看」的护栏提示当作正常
// 内容回吐，原样拼进错误信息会让管理员看到与自己操作无关的多行英文长提示。
func qoderTruncateReason(s string, max int) string {
	s = flattenUpstreamSnippet(s)
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
	applyQoderBusinessErrorClass(ctx, repo, account, resolvedCode, class, message, nil)
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
func applyQoderBusinessErrorClass(ctx context.Context, repo AccountRepository, account *Account, resolvedCode string, class qoderUpstreamErrorClass, message string, retryAfterSeconds *int) {
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
	case qoderErrorClassQueued:
		// 排队类：只按上游给的秒数短时避让，**绝不按日拉黑**。serviceAvailable=true
		// 说明这张号本身健康，只是此刻排不上；拉到次日 0 点会在一轮拥塞里误杀整池号。
		cooldown := qoderQueuedCooldown(retryAfterSeconds, message)
		reason := fmt.Sprintf("Qoder upstream queued the account (code %s): %s", resolvedCode, detail)
		if err := repo.SetTempUnschedulable(stateCtx, account.ID, time.Now().Add(cooldown), reason); err != nil {
			logger.L().Warn("qoder queued error set temp unschedulable failed",
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

// qoderFailoverErrorClass 报告一个业务错误分类是否应让本请求换号重试。
//
// 判定直接复用上面的分级（不另建一套口径，避免两处对同一错误码给出矛盾结论）：
//   - quota / queued / session / restrict 都是**这一张号**的局部问题（额度耗尽、
//     此刻排不上、登录态失效、无访问资格），换号能直接解决 → 换；
//   - unknown（101 签名/102 时间戳/103 重复请求/107 网络/113 使用限制/114 试用限制）
//     为请求级/环境级语义：所有 Qoder 号都会以同一形态被拒，换号只会把同一个错
//     再打一遍，白耗一次上游往返 → 不换。
func qoderFailoverErrorClass(class qoderUpstreamErrorClass) bool {
	switch class {
	case qoderErrorClassQuota, qoderErrorClassQueued, qoderErrorClassSession, qoderErrorClassRestrict:
		return true
	default:
		return false
	}
}

// qoderFailoverableBusinessError 报告该流内错误是否值得在「尚未向客户端写出任何
// 字节」的窗口里换号重试（见 qoderSSEReader.qoderProbeFirstFrame）。
//
// 【必须用 resolve 而不是直接 classify】信封级错误（statusCodeValue 非 200）时，帧里
// 的 code 就是那个 HTTP 码（如 "403"），**真正的业务码内嵌在 message 文本里**（形如
// {"code":"110",...}）。直接 classify("403") 会归 unknown 而不换号，恰好漏掉 Qoder
// 上游表达「账号被拒」的主要形态之一；resolveQoderBusinessErrorCode 已为这一形态
// 做过内嵌码提取，与落状态链路严格同源。
func qoderFailoverableBusinessError(err error) bool {
	var env *errQoderUpstreamEnvelope
	if !errors.As(err, &env) || env == nil {
		return false
	}
	_, class := resolveQoderBusinessErrorCode(env.Code, env.Message)
	return qoderFailoverErrorClass(class)
}

// newQoderStreamFailoverError 把流内业务错误包成换号错误；不可换号时返回 nil
// （调用方据此维持「错误帧原样下发」的旧行为）。
//
// 调用前提：本请求尚未向客户端写出任何字节（由 probe 保证），否则换号会重放输出。
// 账号状态与 Ops 事件已由 markQoderAccountFromBusinessError 落过，本函数不重复记录。
//
// 字段取向（与 newTraeStreamFailoverError 同，对齐 handler 消费端
// openai_chat_completions.go 的 failover 分支）：
//   - RetryableOnSameAccount=false：110/105 一类对**这张号**是确定态，同账号重试
//     只是多花一次上游往返，直接换号；也因此不会误触发 TempUnscheduleRetryableError。
//   - Stage=Inference（非 AccountAuth）：避开 credentialFailoverClientResponse 把文案
//     改写成与 Qoder 无关的 Grok 口径，走 mapUpstreamError 的语义映射。
//   - StatusCode 按分类取 429/401/403，使换号耗尽后下发给客户端的文案与排障语义对齐。
func newQoderStreamFailoverError(err error, now time.Time) *UpstreamFailoverError {
	var env *errQoderUpstreamEnvelope
	if !errors.As(err, &env) || env == nil {
		return nil
	}
	code, class := resolveQoderBusinessErrorCode(env.Code, env.Message)
	if !qoderFailoverErrorClass(class) {
		return nil
	}

	statusCode := http.StatusBadGateway
	switch class {
	case qoderErrorClassQuota:
		statusCode = http.StatusTooManyRequests
	case qoderErrorClassQueued:
		// 排队在客户端语义上就是「稍后重试」，与额度共用 429 而不是 502。
		statusCode = http.StatusTooManyRequests
	case qoderErrorClassSession:
		statusCode = http.StatusUnauthorized
	case qoderErrorClassRestrict:
		statusCode = http.StatusForbidden
	}

	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(env.Message))
	if message == "" {
		message = "Qoder upstream returned an in-stream error"
	}
	if code != "" {
		message = message + " (code=" + code + ")"
	}

	headers := http.Header{}
	switch class {
	case qoderErrorClassQuota:
		// 额度按自然日重置，与 SetTempUnschedulable 的次日 0 点同口径；
		// 上限 7 天由 handler 的 isSafeRetryAfter 把关，本值不会超过 24h。
		if delta := qoderQuotaRecoveryUntil(now).Sub(now); delta > 0 {
			headers.Set("Retry-After", strconv.FormatInt(int64(math.Ceil(delta.Seconds())), 10))
		}
	case qoderErrorClassQueued:
		// 排队类直接把等待时长透下去，与落库的冷却时长同源（qoderQueuedCooldown 取同一个
		// 结构化值，不会因 message 不含该字段而退到回落值）。
		headers.Set("Retry-After", strconv.FormatInt(int64(qoderQueuedCooldown(env.RetryAfterSeconds, env.Message).Seconds()), 10))
	}

	body, marshalErr := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "upstream_error",
			"code":    code,
			"message": message,
		},
	})
	if marshalErr != nil {
		body = []byte(`{"error":{"type":"upstream_error","message":"Qoder upstream error"}}`)
	}

	return &UpstreamFailoverError{
		StatusCode:        statusCode,
		ResponseBody:      body,
		ResponseHeaders:   headers,
		Stage:             GatewayFailureStageInference,
		Scope:             GatewayFailureScopeAccount,
		Reason:            GatewayFailureReason("qoder_" + string(class)),
		NextAccountAction: NextAccountRetry,
	}
}

// qoderStreamFailoverError 同时是「可换号的 UpstreamFailoverError」与「上游信封错误」。
//
// 为何需要这一层：非流式聚合路径的 aggErr 原本就是 *errQoderUpstreamEnvelope，
// 既有调用方与回归用例靠 isQoderUpstreamEnvelopeError(err) 区分「上游业务错」与
// 「本地读错」（qoder_upstream_error_test.go:412）。直接换成裸 UpstreamFailoverError
// 会让那条判据当场失效（错误分类与可观测不应因换号而退化）。
//
// 【实测过的机制，别改回嵌入】嵌入 UpstreamFailoverError 是**错的**：errors.As 只沿
// Unwrap 链走、不扫描结构体字段（Go 1.27 实测：嵌入值与嵌入指针两种写法都返回 false，
// 注释 promoteUnwraps 只适用于 Unwrap 方法本身的提升）。靠 Unwrap() []error 多路递出
// 两个分支，errors.As 两种判据才同时成立（已探针验证）。
type qoderStreamFailoverError struct {
	fo  UpstreamFailoverError
	env *errQoderUpstreamEnvelope
}

// Unwrap 多路递出：既让 errors.As 能取到 UpstreamFailoverError（handler 据此换号），
// 也能取到原始信封错误（错误分类与透传规则判据）。
func (e *qoderStreamFailoverError) Unwrap() []error { return []error{&e.fo, e.env} }

// Error 沿用信封文案：嵌入的 UpstreamFailoverError 只给
// 「upstream error: 429 (failover)」，会丢掉 code 与上游原文，排查时更难定位。
func (e *qoderStreamFailoverError) Error() string {
	if e.env != nil {
		return e.env.Error()
	}
	return e.fo.Error()
}

// qoderSSEDeliveryReader 包装规范化读取器，在**被消费到末尾**时补记一次「流内错误
// 已交付客户端」——用于「先正文、后错误帧」这种首帧预读无法短路的形态。
//
// 【为何不能直接用 qoderSSEReader】归一化读取器写完错误帧后不终止流，且 finish()
// 恒补一个 [DONE]，上层 streamRawChatCompletions 只认终止信号、不认识「带内 error
// 帧」这个概念 → 本次以 forwardErr=nil 收尾、被记为成功。只有在流真的读完（客户端
// 中途断开时不记）才能确定错误帧已交付。
type qoderSSEDeliveryReader struct {
	*qoderSSEReader
	onDelivered func()
	marked      bool
}

func (r *qoderSSEDeliveryReader) Read(p []byte) (int, error) {
	n, err := r.qoderSSEReader.Read(p)
	if err == io.EOF && !r.marked {
		r.marked = true
		// 走到这里意味着错误帧**确实已交付客户端**（本 reader 负责把流读到尾）。
		// 不区分 contentFrames：「首帧即错但不可换号」与「先正文后错」两条形态都
		// 要记失败；而「首帧即错且可换号」的尝试在调用方已 close+return，本 reader
		// 永不消费到 EOF，因此不会把已恢复的尝试误记为失败。
		if r.onDelivered != nil && r.bizErr != nil {
			r.onDelivered()
		}
	}
	return n, err
}

// newQoderSSEDeliveryReader 用交付观察包装器包住规范化读取器。
func newQoderSSEDeliveryReader(r *qoderSSEReader, onDelivered func()) *qoderSSEDeliveryReader {
	return &qoderSSEDeliveryReader{qoderSSEReader: r, onDelivered: onDelivered}
}

// qoderMarkDeliveredInBandError 在「错误帧即将交付客户端」这一刻补记 SLA 失败。
//
// 【为何需要这一层】上游 200 + 流内错误帧是 Qoder 的主要拒绝形态，而
// streamRawChatCompletions 的成功判据是 [DONE]/usage/finish_reason 任一出现，
// 归一化读取器又**恒补一个 [DONE]**（qoder_sse.go finish）——于是错误流被伪装成
// 正常完成：forwardErr=nil → handler 走成功分支并上报账号成功，运维侧无任何
// ops_error_logs 行（带内错误要靠 MarkOpsStreamFailure 才补行），面板永远显示成功。
//
// 【为何在交付点、而不在错误帧回调里】首帧预读命中可换号错误时，本 reader 会被丢弃
// 并重试下一个号——那时在回调里标记会把**已恢复的尝试**记成客户端可见失败（与
// logOpsRecoveredUpstream 的「恢复尝试不算失败」口径相冲）。走到本函数就意味着
// 错误帧确实会下发给用户。每个尝试各自持有一个 reader，因此不存在跨账号污染。
//
// 不区分分类：字典外 unknown 码也记失败（用户已拍板）——不认识不等于成功。
// 【客户端已断开不算本次失败】raw 路径把 clientDisconnected / context.Canceled /
// DeadlineExceeded 归为 clientAborted 并且不计上游故障（
// openai_gateway_chat_completions_raw.go:412-419），此处同口径：入站 ctx 已取消说明
// 错误帧并没有真正被用户读到（只是被排空到 EOF），记了会虚抬错误率。
func (s *OpenAIGatewayService) qoderMarkDeliveredInBandError(c *gin.Context, account *Account, reader *qoderSSEReader) {
	if c == nil || reader == nil || reader.bizErr == nil {
		return
	}
	if c.Request != nil {
		if err := c.Request.Context().Err(); err != nil {
			return
		}
	}
	code, class := resolveQoderBusinessErrorCode(reader.bizErr.Code, reader.bizErr.Message)
	status := http.StatusBadGateway
	switch class {
	case qoderErrorClassQuota, qoderErrorClassQueued:
		status = http.StatusTooManyRequests
	case qoderErrorClassSession:
		status = http.StatusUnauthorized
	case qoderErrorClassRestrict:
		status = http.StatusForbidden
	}
	message := qoderBusinessErrorDetail(code, reader.bizErr.Message)
	if message == "" {
		message = "Qoder upstream returned an in-stream error"
	}
	if code != "" {
		message = fmt.Sprintf("%s (code=%s)", message, code)
	}
	MarkOpsStreamFailure(c, "upstream_error", code, message, status)
	if account != nil {
		logger.L().Warn("qoder in-band stream error delivered to client",
			zap.Int64("account_id", account.ID),
			zap.String("code", code),
			zap.String("class", string(class)),
			zap.Int("logic_status", status))
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
func (s *OpenAIGatewayService) markQoderAccountFromBusinessError(ctx context.Context, c *gin.Context, account *Account, code, message string, retryAfterSeconds *int) {
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
		applyQoderBusinessErrorClass(ctx, s.accountRepo, account, resolvedCode, class, message, retryAfterSeconds)
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
