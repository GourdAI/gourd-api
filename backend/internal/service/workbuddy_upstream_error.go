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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
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

// workbuddyForbiddenClass 403 错误体的分流结果（只看上游回的字面值，不做猜测）。
type workbuddyForbiddenClass string

const (
	// workbuddyForbiddenUnknown 字典外：既不重放也不处罚（宁可不处置，不可误杀）。
	workbuddyForbiddenUnknown workbuddyForbiddenClass = "unknown"
	// workbuddyForbiddenAccountRestricted 账号级限制：displayMsg 写着「异常行为/已被限制
	// 使用/restricted」，masknull、magpie #1849、mitmproxy 复盘三重印证这是账号层结论，
	// SetError 是**事实记录**（该号确实不能再服务）。
	workbuddyForbiddenAccountRestricted workbuddyForbiddenClass = "account_restricted"
	// workbuddyForbiddenContentModeration 内容审核拒绝：同样的 code 11140，但
	// displayMsg 是「内容未通过安全审核」。这是**请求内容**问题，账号没有做错任何事。
	// 旧实现把它当账号失效：首次即 SetError（误杀好号），而 403 又在 failover 状态集
	// 里 ⇒ 同一个坏 body 在池内逐个账号重放，每个号各烧一次 —— 一次内容拒绝足以
	// 把整组账号打成 error（且 SetError 需人工重登才能救）。
	workbuddyForbiddenContentModeration workbuddyForbiddenClass = "content_moderation"
)

// 上游 11140 响应体实测形状（用户提供的一手样本，2026-11-01）：
//
//	{"code":-32603,"message":"Internal error","data":{"statusCode":403,
//	  "displayMsg":{"en":"Your account has been restricted due to unusual activity",
//	              "zh":"账号存在异常行为，已被限制使用",
//	              "zh-hant":"帳號存在異常行為，已被限制使用"},
//	  "retry_policy":{"retry":false},"actions":["EDIT_INPUT","SUBMIT_FEEDBACK"],
//	  "code":11140,"category":"internal"}}
//
// 【为什么必须读 data.* 嵌套】extractUpstreamErrorMessage（gateway_upstream_response.go:294）
// 只认 error.message / detail / 顶层 message，**不读 data.displayMsg**；而 11128（渠道闸）
// 与 11140（账号限制）的 msg 逐字都是 request illegal —— 只有 displayMsg 能分流。
// 于是旧实现拿到空 upstreamMsg，直接落 handle403 兜底分支：首次即永久 SetError。
const (
	workbuddyCodeAccountRestricted = 11140 // 超载码：账号限制 与 内容审核 共用，靠 displayMsg 分流
	workbuddyCodeChannelGate       = 11128 // 渠道校验（实测为 400，不在本 403 路径）
)

// workbuddyAccountRestrictedPhrases 账号级限制文案特征（小写比对，中英繁三路都读）。
//
// 【为何用 \u 转义而不是直接写汉字】本仓工具链（read/edit/write/bash）在多次写同一
// 文件时会产生**肉眼不可见**的 CJK 码点漂移（实测：繁中「查 U+67E5」与「核 U+6838」
// 在不同写入通道里不一致，导致包含关系判定静默失效）。上游文案不受我们控制，
// 因此匹配特征一律用 \u 转义写定，不依赖任何编辑器行为。改这些短语时请用码点，
// 不要手输汉字。
var workbuddyAccountRestrictedPhrases = []string{
	"\u5f02\u5e38", // 异常
	"\u7570\u5e38", // 異常（繁）
	"\u9650\u5236", // 限制（简繁同形）
	"restricted due to unusual activity",
	"unusual activity",
	"account has been restricted",
}

// workbuddyContentModerationPhrases 内容审核文案特征。
// 审核类词根覆盖 审/審 × 核/查 四种组合（简繁混排在上游文案里真实存在），
// 组合全覆盖后不再依赖“写对繁体字”这件事。
var workbuddyContentModerationPhrases = []string{
	"\u5ba1\u6838",       // 审核
	"\u5be9\u6838",       // 審核
	"\u5be9\u67e5",       // 審查
	"\u5ba1\u67e5",       // 审查
	"\u8fdd\u89c4",       // 违规
	"\u9055\u89c4",       // 違規（繁）
	"\u654f\u611f",       // 敏感
	"\u672a\u901a\u8fc7", // 未通过
	"\u672a\u901a\u904e", // 未通過（繁）
	"safety review",
	"security review",
	"content review",
	"violat",
	"prohibited content",
}

// classifyWorkbuddyForbiddenBody 从 403 错误体分流「账号限制 / 内容审核 / 未知」。
//
// 纪律：只在上游回了可读文案时才分流；读不到 code 或 displayMsg 一律 unknown。
// 绝不按状态码猜（11140 一个码对应两种语义是实测事实，不是推测）。
func classifyWorkbuddyForbiddenBody(body []byte) workbuddyForbiddenClass {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return workbuddyForbiddenUnknown
	}
	// code 优先取 data.code（实测信封），其次取顶层 code。
	code := gjson.GetBytes(body, "data.code").Int()
	if code == 0 {
		code = gjson.GetBytes(body, "code").Int()
	}
	if code != workbuddyCodeAccountRestricted {
		// 不是 11140：不强行分流（11128 是 400、额度类是 200+code，均不走这里）。
		return workbuddyForbiddenUnknown
	}
	// 三种语言的 displayMsg 都看（上游按客户端语言回不同键）。
	text := strings.ToLower(strings.Join([]string{
		gjson.GetBytes(body, "data.displayMsg.zh").String(),
		gjson.GetBytes(body, "data.displayMsg.zh-hant").String(),
		gjson.GetBytes(body, "data.displayMsg.en").String(),
		gjson.GetBytes(body, "data.displayTips.zh").String(),
	}, "|"))
	if strings.TrimSpace(text) == "" {
		return workbuddyForbiddenUnknown
	}
	// 【判定顺序是刻意的】先内容、后账号：文案同时命中两类特征时归入内容审核。
	// 理由是不对称危害 —— 把内容审核误判成账号限制会永久 SetError 一枚好号（只能
	// 人工重登救回）；反向误判只会让已封号继续报错，且既不重放也不静默丢弃，
	// 管理端有 warn 日志可查。**不要把这个顺序当成偶然写法改掉。**
	for _, p := range workbuddyContentModerationPhrases {
		if strings.Contains(text, strings.ToLower(p)) {
			return workbuddyForbiddenContentModeration
		}
	}
	for _, p := range workbuddyAccountRestrictedPhrases {
		if strings.Contains(text, strings.ToLower(p)) {
			return workbuddyForbiddenAccountRestricted
		}
	}
	return workbuddyForbiddenUnknown
}

// workbuddyForbiddenDisplayMessage 取上游回的人类可读文案（管理端展示用）。
// 没有则返回 ""（不编造）。
func workbuddyForbiddenDisplayMessage(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	for _, path := range []string{"data.displayMsg.zh", "data.displayMsg.zh-hant", "data.displayMsg.en"} {
		if v := strings.TrimSpace(gjson.GetBytes(body, path).String()); v != "" {
			return v
		}
	}
	return ""
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

// workbuddySSEDeliveryReader 包装规范化读取器，在**被消费到末尾**时补记一次「流内错误
// 已交付客户端」。
//
// 【为何需要这一层】WorkBuddy 的主要拒绝形态是 HTTP 200 + 流内 error 帧，而：
//   - streamRawChatCompletions 只认 [DONE]/usage/finish_reason，不认识「带内 error 帧」；
//   - workbuddyStreamErrorHandler 只调 appendOpsUpstreamError，而它把事件挂到
//     **已存在的**错误行 JSON 上，本身不生成行（ops_error_logger 只在 wire>=400 或
//     有人调 MarkOpsStreamFailure 时才落行）；
//   - chat 域码表恒 unknown（classifyWorkbuddyChatError），applyWorkbuddyErrorClass
//     对 unknown 直接 return，也不落账号状态。
//
// 三条叠加 ⇒ 账号被上游拒了，运维面板却显示「成功」、调度器继续选同一枚号。
// 这与 Qoder 10-20 那次「报错却记成功」完全同构（Qoder 已有同名观察器，WorkBuddy 一直没有）。
//
// 【为何在交付点、而不在 onErrFrame 回调里】只有流被读到末尾（客户端没中途断开）才能
// 确定错误帧真的会被用户看到。
type workbuddySSEDeliveryReader struct {
	*workbuddySSEReader
	onDelivered func()
	marked      bool
}

func (r *workbuddySSEDeliveryReader) Read(p []byte) (int, error) {
	n, err := r.workbuddySSEReader.Read(p)
	if err == io.EOF && !r.marked {
		r.marked = true
		// 只有**上游**错误帧才算失败：网关自己合成的空流兜底帧（errFrameCode 被置为
		// workbuddyEmptyStreamErrFrameCode = "upstream_parse"）属本地解析缺陷，不该记成
		// 上游失败——留它走恢复行口径。
		if r.onDelivered != nil && r.upstreamErrFrameDelivered() {
			r.onDelivered()
		}
	}
	return n, err
}

// upstreamErrFrameDelivered 报告是否交付过**上游**错误帧（排除本网关合成的空流帧）。
func (r *workbuddySSEReader) upstreamErrFrameDelivered() bool {
	if r.errFrameCode == "" {
		return false
	}
	if r.errFrameCode == workbuddyEmptyStreamErrFrameCode {
		return false
	}
	return true
}

// newWorkbuddySSEDeliveryReader 用交付观察包装器包住规范化读取器。
func newWorkbuddySSEDeliveryReader(r *workbuddySSEReader, onDelivered func()) *workbuddySSEDeliveryReader {
	return &workbuddySSEDeliveryReader{workbuddySSEReader: r, onDelivered: onDelivered}
}

// workbuddyMarkDeliveredInBandError 在「上游错误帧已交付客户端」这一刻补记 SLA 失败。
//
// 不区分分类：字典外 unknown 码也记失败（与 Qoder 同口径）——不认识不等于成功。
// 账号状态仍按既有分级处理（unknown 不落）——本函数只负责可观测性，不改变调度行为。
//
// 【两个消费路径共用】流式由 workbuddySSEDeliveryReader 在 EOF 交付点调用；非流式在
// 聚合完成后调用（那里没有交付点，聚合在内存里就结束了）。参数取**值**而非 reader：
// 非流式路径拿不到 workbuddySSEReader 实例。
//
// 【客户端已断开不算本次失败】raw 路径把 clientDisconnected / context.Canceled /
// DeadlineExceeded 归为 clientAborted 并且不计上游故障（openai_gateway_chat_completions_raw.go）,
// 此处同口径：入站 ctx 已取消说明错误帧并未真正被用户读到，记了会虚抬错误率。
func (s *OpenAIGatewayService) workbuddyMarkDeliveredInBandError(c *gin.Context, account *Account, code, upstreamMessage string) {
	if c == nil {
		return
	}
	code = strings.TrimSpace(code)
	if code == "" || code == workbuddyEmptyStreamErrFrameCode {
		// 空/未交付是「无事可记」；workbuddyEmptyStreamErrFrameCode 是网关本地合成的
		// 空流兜底帧（解析缺陷），不得记成上游失败——否则会把好号误报成账号问题。
		return
	}
	if c.Request != nil {
		if err := c.Request.Context().Err(); err != nil {
			return
		}
	}
	message := workbuddyErrorDetail(code, upstreamMessage)
	if strings.TrimSpace(message) == "" {
		message = "WorkBuddy upstream returned an in-stream error"
	}
	MarkOpsStreamFailure(c, "upstream_error", code, message, http.StatusBadGateway)
	if s != nil && account != nil {
		logger.L().Warn("workbuddy in-band stream error delivered to client",
			zap.Int64("account_id", account.ID),
			zap.String("code", code),
			zap.Int("logic_status", http.StatusBadGateway))
	}
}

// workbuddyInBandFailureFromAggregate 从**原始上游流**（而非聚合后的 JSON）中提取首个
// 上游 error 帧的 code/message，用于非流式路径判定「本次请求是否应计入 SLA 失败」。
//
// 为何不看聚合结果：聚合函数按既有协议保持「error 帧计入 validEvents 但不改响应形态」，
// 聚合出的 body 是一份空 content 的正常形状 chat.completion，从它身上分辨不出错误。
// 因此直接扫原始流；只取首帧，与流式路径 errFrameCode 的「首个错误帧归一」口径一致。
func workbuddyInBandFailureFromAggregate(raw []byte) (code, message string, ok bool) {
	br := bufio.NewReaderSize(bytes.NewReader(raw), 64*1024)
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if payload, isData := extractOpenAISSEDataLine(trimmed); isData {
			payload = strings.TrimSpace(payload)
			if payload != "" && payload != "[DONE]" {
				var obj map[string]any
				if json.Unmarshal([]byte(payload), &obj) == nil {
					if c, m, hasErr := workbuddyErrorFrameFields(obj); hasErr {
						if strings.TrimSpace(c) != "" {
							return c, m, true
						}
						// error 为字符串形态：无码但有消息，仍算上游失败（与流式路径的
						// upstreamInBandErrFallbackCode 占位同口径）。
						return upstreamInBandErrFallbackCode, m, true
					}
				}
			}
		}
		if err != nil {
			return "", "", false
		}
	}
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
