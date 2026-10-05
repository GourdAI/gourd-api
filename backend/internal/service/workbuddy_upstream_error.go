package service

// workbuddy_upstream_error.go —— WorkBuddy 上游错误的分级处置与账号冷却。
//
// 【本文件解决的问题】
// WorkBuddy 是 Trae / Qoder / WorkBuddy 三平台里**唯一没有任何事后冷却路径**的：
//   - Trae：trae_upstream_error.go（流内 event:error 帧 → 分类 → SetTempUnschedulable）
//   - Qoder：qoder_upstream_error.go（同结构，110-119 等码表）
//   - WorkBuddy：全仓 grep 只有 workbuddy_gateway.go:235 一处 SetError，而且那在
//     **管理员「测试连接」路径**（testWorkbuddyAccountConnection），不在网关热路径。
// 结果：号子撞上「今日额度已用尽」之后，除了等下一次积分探测刷新快照，没有任何
// 东西会让它退出调度 —— 而探测是手动/签到触发的，耗尽号又不再收流量，形成空窗。
//
// 【两个接口域，形态不同，必须分开处理】
// 这不是同一处的歧义，而是两个真实不同的上游接口（均有实测样本）：
//   A. billing 域（/v2/billing/meter/get-user-resource 等）：HTTP 200 + **顶层** code
//      且为 int。实测样本 {"code":110,"msg":"今日额度已用尽"}
//      （workbuddy_credits_service_test.go:220，测试同时断言了请求路径 :211）。
//      该域错误已被归一成 *workbuddyBillingError（workbuddy_credits_service.go:168-182，
//      字段 Status int / Code int / Msg string）。
//   B. chat 域（/v2/chat/completions）：SSE 流内 **error.code** 且为 string。
//      实测样本 {"error":{"message":"rate limited","code":"6004"}}
//      （workbuddy_sse_test.go:90/:94）。
//
// 【码表纪律 —— 为什么两张表几乎是空的】
// trae_upstream_error.go:14-15 定下的规矩：错误码语义必须有权威来源，
// 「凡不确定的一律归 unknown 不落状态」。A 侧只填 110（有真实上游样本 + 明确中文
// 文案，语义无歧义）；B 侧**一张都不填**：6004 的字面语义是 rate limited（请求级/
// 瞬时），既没有额度耗尽的证据，也没有官方文档，把它当"账号没额度了"停调到次日
// 就是误杀健康号。因此 B 侧现在只做两件事：解析出 code/msg 写 Ops 事件（管理员
// 可见、可据此回传真实样本），以及留好接码表的骨架。
// 字典外的码一律不落账号状态，这条不许放宽。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// workbuddyUpstreamErrorClass 是 WorkBuddy 上游错误的处置分类。
type workbuddyUpstreamErrorClass string

const (
	workbuddyErrorClassNone    workbuddyUpstreamErrorClass = ""                // 非业务错误（code 0 / 缺失）
	workbuddyErrorClassUnknown workbuddyUpstreamErrorClass = "unknown"         // 字典外：只留 Ops 痕迹，不落状态
	workbuddyErrorClassQuota   workbuddyUpstreamErrorClass = "quota_exhausted" // 额度耗尽：次日 0 点自动恢复
	workbuddyErrorClassSession workbuddyUpstreamErrorClass = "session_expired" // 登录态失效：需人工重登
)

// workbuddyBillingQuotaErrorCodes billing 域额度耗尽码（int，顶层 code）。
var workbuddyBillingQuotaErrorCodes = map[int]struct{}{
	110: {}, // 实测：{"code":110,"msg":"今日额度已用尽"}
}

// workbuddyBillingSessionErrorCodes billing 域登录态失效码（int，顶层 code）。
// 401 有实测样本（workbuddy_credits_service_test.go:463）；但注意 **HTTP 401 本身
// 不在这里处理** —— 它由 workbuddy_client.go 的重试路径与 billing 请求层的 token
// 刷新负责，重复落 SetError 会把「刷新失败」这类瞬时问题标成账号坏了。
var workbuddyBillingSessionErrorCodes = map[int]struct{}{
	401: {},
}

// classifyWorkbuddyBillingError 把 billing 域业务码映射为处置分类。
func classifyWorkbuddyBillingError(code int) workbuddyUpstreamErrorClass {
	if code == 0 {
		return workbuddyErrorClassNone
	}
	if _, ok := workbuddyBillingQuotaErrorCodes[code]; ok {
		return workbuddyErrorClassQuota
	}
	if _, ok := workbuddyBillingSessionErrorCodes[code]; ok {
		return workbuddyErrorClassSession
	}
	return workbuddyErrorClassUnknown
}

// classifyWorkbuddyChatError 把 chat 域流内 error.code（string）映射为处置分类。
//
// 当前恒返回 unknown：没有任何 chat 域码有权威语义来源（见文件头「码表纪律」）。
// 这不是「没写完」，而是刻意的缺省 —— 拿到真实耗尽样本前，往这里加码就是赌博。
func classifyWorkbuddyChatError(code string) workbuddyUpstreamErrorClass {
	c := strings.TrimSpace(code)
	if c == "" || c == "0" {
		return workbuddyErrorClassNone
	}
	// 码表就位后在此追加：if _, ok := workbuddyChatQuotaErrorCodes[c]; ok { ... }
	return workbuddyErrorClassUnknown
}

// workbuddyQuotaRecoveryUntil 额度类错误的恢复时刻：次日 0 点（跟随全局时区）。
// 与 traeQuotaRecoveryUntil / qoderQuotaRecoveryUntil / creditSnapshotRecoveryUntil
// 三处现行口径完全一致（同一命名空间的 reason/window 也让 filterStats 可归并）。
// 刻意不用 workbuddyTasksCST（固定 +8）：那是签到/出行任务的「上游自然日」口径，
// 账号状态写这边要与另两平台同口径，否则同一时刻两类号会错开一天恢复。
func workbuddyQuotaRecoveryUntil(now time.Time) time.Time {
	return timezone.StartOfDay(now).AddDate(0, 0, 1)
}

// workbuddyUpstreamHint 处置提示（追加在上游原文之后，供管理端看到该怎么做）。
func workbuddyUpstreamHint(code string) string {
	return fmt.Sprintf(" [WorkBuddy: code %s]", strings.TrimSpace(code))
}

// workbuddyErrorDetail 上游原文（按 rune 限长 200）+ 处置提示。
//
// 顺序与 traeBusinessErrorDetail 一致：先截原文再追加提示，否则提示尾部（恰好是
// 「该怎么做」那半句）会被削成省略号。
func workbuddyErrorDetail(code, message string) string {
	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = "code " + strings.TrimSpace(code)
	}
	runes := []rune(msg)
	if len(runes) > 200 {
		msg = string(runes[:200]) + "..."
	}
	return msg + workbuddyUpstreamHint(code)
}

// workbuddyOpsBizErrorSeenKey 标记本请求已处置过某 (账号, 错误码)。
//
// 去重必要性同 trae：落状态含多次 DB/Redis 往返，而流式回调运行在 SSE 读流热路径
// 内，上游重发错误帧会让回调触发多次。帧本身的透传不受影响（对外协议不变）。
const workbuddyOpsBizErrorSeenKey = "workbuddy_biz_error_marked"

// workbuddyMarkBizErrorOnce 报告本请求内该 (账号, 码) 是否首次处置。
// c==nil（无请求上下文，如后台签到任务）时视为首次 —— 由下游 SQL 幂等守卫兜底。
func workbuddyMarkBizErrorOnce(c *gin.Context, accountID int64, code string) bool {
	if c == nil {
		return true
	}
	key := fmt.Sprintf("%d:%s", accountID, code)
	set, _ := c.Get(workbuddyOpsBizErrorSeenKey)
	seen, _ := set.(map[string]struct{})
	if seen == nil {
		seen = map[string]struct{}{}
	}
	if _, dup := seen[key]; dup {
		return false
	}
	seen[key] = struct{}{}
	c.Set(workbuddyOpsBizErrorSeenKey, seen)
	return true
}

// applyWorkbuddyErrorClass 按已解析的分类落账号状态。
//
// ctx 收敛口径与 trae/qoder 同源：本函数可能从 SSE 读流栈内被同步调用，入站 ctx
// 要么被客户端断开取消（静默丢标记），要么是脱离取消链的 WithoutCancel（DB 卡顿
// 挂住流转发）。统一收敛到仓库既有的账号状态写口径（WithoutCancel + 5s 超时）。
//
// unknown 不落任何状态（只由调用方写 Ops 痕迹）。
func applyWorkbuddyErrorClass(ctx context.Context, repo AccountRepository, account *Account, code string, class workbuddyUpstreamErrorClass, message string) {
	if account == nil || repo == nil {
		return
	}
	if class == workbuddyErrorClassNone || class == workbuddyErrorClassUnknown {
		return
	}
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	detail := workbuddyErrorDetail(code, message)
	switch class {
	case workbuddyErrorClassQuota:
		reason := fmt.Sprintf("WorkBuddy upstream quota exhausted (code %s): %s", code, detail)
		if err := repo.SetTempUnschedulable(stateCtx, account.ID, workbuddyQuotaRecoveryUntil(time.Now()), reason); err != nil {
			logger.L().Warn("workbuddy quota error set temp unschedulable failed",
				zap.Int64("account_id", account.ID), zap.String("code", code), zap.Error(err))
		}
	case workbuddyErrorClassSession:
		msg := fmt.Sprintf("WorkBuddy upstream rejected the session: %s", detail)
		if err := repo.SetError(stateCtx, account.ID, msg); err != nil {
			logger.L().Warn("workbuddy session error mark failed",
				zap.Int64("account_id", account.ID), zap.String("code", code), zap.Error(err))
		}
	}
}

// markWorkbuddyAccountFromBillingError 是 A 侧入口：billing 探测（查积分/签到）撞到
// 业务错误时的账号处置。
//
// 为什么值得做：额度耗尽时 billing 接口返回的正是 HTTP 200 + code 110，而
// persistWorkbuddyCreditsSnapshot 在 result.Success==false 时**早退不覆盖旧快照**
// （这是防误杀的正确设计）。两者叠加的结果是：上游已经明确说"今日额度已用尽"，
// 但调度层看到的还是一个新鲜的、有余额的旧快照 —— 这个号会继续被选中打上游。
// 本函数把这条**权威读数**转成冷却，是对硬闸门（它只能读本地快照）的必要补充。
//
// 无 gin.Context（后台签到/手动探测）故不写 Ops 事件，只落状态 + 日志。
func markWorkbuddyAccountFromBillingError(ctx context.Context, repo AccountRepository, account *Account, err error) {
	if account == nil || repo == nil || err == nil {
		return
	}
	var be *workbuddyBillingError
	if !errors.As(err, &be) || be == nil {
		return
	}
	class := classifyWorkbuddyBillingError(be.Code)
	if class == workbuddyErrorClassNone {
		return
	}
	code := fmt.Sprintf("%d", be.Code)
	if class == workbuddyErrorClassUnknown {
		logger.L().Debug("workbuddy billing business error not classified",
			zap.Int64("account_id", account.ID), zap.Int("code", be.Code), zap.String("msg", be.Msg))
		return
	}
	// 此处**不做**请求级去重（workbuddyMarkBizErrorOnce 在无 gin.Context 时恒返 true，
	// 写了也是死逻辑）：billing 探测不是 SSE 热路径，调用频率受手动探测/签到任务限制；
	// 而额度类写入已由 SetTempUnschedulable 的 SQL 守卫兜住（account_repo.go：
	// `temp_unschedulable_until IS NULL OR temp_unschedulable_until < $1`），重复调用
	// 不会反复刷 reason。session 类走 SetError（无同构守卫），但同一错误文案重复写入
	// 同样不改变账号可用性，可接受。
	applyWorkbuddyErrorClass(ctx, repo, account, code, class, be.Msg)
}

// workbuddyStreamErrorHandler 返回一个可注入 SSE 规范化读取器的错误帧回调
// （见 workbuddy_sse.go 的 newWorkbuddySSEReaderWithHook）。
//
// ctx 必须单独传入，不能从 c.Request.Context() 取：本回调可能在 c==nil 时被调用
// （无请求上下文的内部转发），而早退点在分类之后 —— 先读 c.Request 会 nil 解引用。
// 闭包只持有 account 与 c，以便在读流栈内落状态与写 Ops 事件。返回的回调必须
// 廉价且不得阻塞过久：它在 SSE 热路径上被同步调用。
func (s *OpenAIGatewayService) workbuddyStreamErrorHandler(ctx context.Context, c *gin.Context, account *Account) func(code, message string) {
	if s == nil || account == nil {
		return nil
	}
	return func(code, message string) {
		code = strings.TrimSpace(code)
		class := classifyWorkbuddyChatError(code)
		if class == workbuddyErrorClassNone {
			return
		}
		if !workbuddyMarkBizErrorOnce(c, account.ID, code) {
			return
		}
		if class != workbuddyErrorClassUnknown {
			applyWorkbuddyErrorClass(ctx, s.accountRepo, account, code, class, message)
		}
		// 字典外码（含 6004）也留 Ops 痕迹：不落状态但必须可见，否则管理员既看不到
		// 上游到底返了什么，也无从回传样本补码表。
		// UpstreamStatusCode 填 200 而非留 0 —— 留 0 会让
		// checkSkipMonitoringForUpstreamEvent 直接早退，skip_monitoring 规则永久失效。
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
			Kind:               "stream_error",
			Stage:              "workbuddy_upstream_frame",
			Reason:             string(class),
			Message:            fmt.Sprintf("workbuddy upstream business error code=%s", code),
			Detail:             workbuddyErrorDetail(code, message),
		})
	}
}
