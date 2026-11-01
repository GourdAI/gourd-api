package service

// workbuddy_headers.go WorkBuddy 出站请求头构造。
//
// 【2026-11-01 渠道对齐改造总纲】本次账号封禁排查把出站头按「官方客户端实际字节」
// 对齐。取证一手优先级：官方 app.asar 里 applyGatewayHeaders 的逐字构造（含默认值表
// JN）> magpie / 9router / dsh-workbuddy-panel 的抽包读数 > dsh-connect-workbuddy 源码。
//
// 只删一类东西：**官方不发、且本仓自己发明并且全池/全生命周期取同一个值**的头。
//   - X-Machine-ID / X-Session-ID：按 uid 派生的永久常量设备号。9router #4946 独立
//     做出同样判断并从 chat 头删除，原文：「实测对话请求不带这两个（只用于反馈上报），
//     而网关发的是每账号恒定派生值，本身就是一处「网关特征」」。
//
// 不删（本仓一度误删、已按官方逐字证据恢复）：x-codebuddy-request、X-Agent-Purpose、
// X-Agent-Intent、Accept-Language（值改为浏览器形状）、X-IDE-*。
// 口径：目标是「过闸门」的第三方可以删官方头，目标是「像官方」的我们不可以。
//
// 另一类真正的修正是**值的形式**而不是有无：
//   - X-Product：WorkBuddy → SaaS（多个一手来源收敛为官方实测值）；
//   - 版本段：5.5.4/2.137.1 → 5.9.3/2.165.0（陈旧两代且全池同值）；
//   - X-Conversation-ID 兜底：删除「按 uid+首句」的跨轮恒定派生，改为每请求新生成；
//   - X-IDE-* / X-Request-ID：收进账号级开关 usage_identity（默认关）。
//
// 【关于 11140 的关键事实（影响所有修复预期）】11140 +「账号存在异常行为」是
// 账号级限制：masknull 归档 README、magpie #1849（同一固定请求、多数号 403 少数 200、
// 200 的号下一分钟变 403）、以及 mitmproxy 复盘（官方 IDE 同样 403）三重印证。
// 因此**改本文件不会解除已发的封禁**；它只能降低其余号被同类识别的概率。
// 而 11128（渠道闸）与 11140 的 `msg` 逐字相同（都是 request illegal），全靠
// displayMsg 分流 —— 这既是上游的坑，也是本仓错误分类的根源（见
// workbuddy_upstream_error.go）。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	// workbuddyClientVersion UA 的 `WorkBuddy/<ver>` 段与 X-IDE-Version。
	// 2026-11-01 从 5.5.4 抬到 5.9.3：两个独立一手来源交叉印证国服稳态 UA 为
	// `WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0` —— magpie 插件 0.1.46 的
	// migrate_workbuddy.go，以及 dsh-workbuddy-panel server-v1.15.0 对 5.9.3/5.9.4
	// 的 app.asar 静态比对（20 个关键构造器 20/20 相同）。用户侧 11140 报错原文里的
	// `App Version: 5.9.2` 也是同代。
	// 【性质要说清楚】这是「追官方版本」的军备竞赛项，官方每次发版就重新陈旧，
	// 不得当成安全修复。旧值 5.5.4 已落后两代，且全池送同一个陈旧版本号是一枚
	// 静态、跨账号一致的识别键。
	workbuddyClientVersion = "5.9.3"
	// workbuddyCLIVersion UA 的 `CLI/<ver>` 段（同上来源，内置 CLI 2.165.0）。
	workbuddyCLIVersion = "2.165.0"

	// workbuddyOriginCN / workbuddyOriginGlobal Origin/Referer 基础域（按 realm 切换）。
	workbuddyOriginCN     = "https://www.codebuddy.cn"
	workbuddyOriginGlobal = "https://www.workbuddy.ai"

	// workbuddyAttributionName X-IDE-Name 取值（仅在账号显式开启 usage_identity 时发）。
	//
	// 【X-IDE-Type 保留 "WorkBuddy"，不改成 CLI】一手证据：官方 app.asar 里
	// applyGatewayHeaders 的默认值表 JN 逐字为 `X-IDE-Type: WorkBuddy` +
	// `X-IDE-Name: WorkBuddy` + `X-Product: SaaS`；magpie 同样送 WorkBuddy。
	// 只有 dsh-connect-workbuddy 的 usageIdentity 送 "CLI"，因为该项目自己走 CLI
	// 渠道（其注释：声称桌面应用的 type 是「比所需更大的身份声明」）。
	// 口径：本仓目标是「像官方桌面客户端」，所取官方逐字值，不取第三方的取舍。
	workbuddyAttributionName = "WorkBuddy"
	workbuddyIDETypeName     = "WorkBuddy"
	// workbuddyProductName X-Product 恒发值。多个一手来源收敛为 "SaaS"（实测值）：
	// 官方 bundle 默认表 JN、9router（「由 WorkBuddy → SaaS，实测值是 SaaS」）、
	// dsh-connect-workbuddy 4.13.0（「adapter 发 WorkBuddy，逆向抓包显示官方发 SaaS」）。
	workbuddyProductName = "SaaS"

	// workbuddyAgentPurpose / workbuddyAgentIntent：官方桌面端逐字值（同样出自
	// applyGatewayHeaders）。【保留】它们是官方发的头，不是网关自造；删掉只会让
	// 请求更不像官方（本次一度误删，已按一手证据恢复）。
	workbuddyAgentPurpose = "conversation"
	workbuddyAgentIntent  = "craft"

	// workbuddyCodeBuddyRequest：官方 bundle 逐字发 `x-codebuddy-request: 1`，【保留】。
	// 区分口径：9router 与 dsh-workbuddy-panel v1.14.0 主动删过它（它们的目标是「过
	// 闸门」，能删则删）；我们的目标是「像官方」，官方发的就不能删。
	workbuddyCodeBuddyRequest = "1"

	// workbuddyAcceptDefault common 层 Accept，对齐参考实现 commonHeaders 的浏览器形状
	//（此前是非流式 application/json + chat 覆盖，与官方不同源）。
	workbuddyAcceptDefault = "application/json, text/plain, */*"

	// workbuddyGlobalDomain global realm 显式声明的国际版域（与 Origin/Referer 同域）。
	workbuddyGlobalDomain = "www.workbuddy.ai"
)

// workbuddyUAFor 组装默认出站 UA（官方桌面端形状）：
// `WorkBuddy/<clientVersion> <platform>/<clientVersion> CLI/<cliVersion>`。
// 平台段（第二段）品牌按 realm 切换——CN 用 `WorkBuddy`，global 用官方国际版
// productName `WorkBuddy AI`（送错可能触发上游 403 code 11140）。
//
// 【为何不用参考实现的纯 CLI 形状 UA】它自称插件渠道（CLI/x CodeBuddy/x），而本仓
// 是「把 OpenAI 协议流量伪装成桌面端会话」；三段式形状修复前已被上游接受，不在
// 本次剔除清单里（剔除只针对「官方不发且值恒定」的头）。
func workbuddyUAFor(realm string) string {
	platform := "WorkBuddy"
	if realm == "global" {
		platform = "WorkBuddy AI"
	}
	return "WorkBuddy/" + workbuddyClientVersion + " " + platform + "/" + workbuddyClientVersion + " CLI/" + workbuddyCLIVersion
}

// workbuddyOriginFor 按 realm 返回 Origin/Referer 基础域。
func workbuddyOriginFor(realm string) string {
	if realm == "global" {
		return workbuddyOriginGlobal
	}
	return workbuddyOriginCN
}

// workbuddyAcceptLanguageFor 按 realm 返回 Accept-Language。
//
// 【保留（本次一度误删，已按一手证据改回）】官方 WorkBuddy 桌面端是
// Electron/Chromium，真实请求必带该头。真正的问题不是「发了」，而是「只发单一
// 语言标签」：Chromium 形状是主语言 + q 兜底串，而光秃秃的 `zh-CN` 是 curl/脚本的
// 默认形状。所以保留头、把值改成浏览器真实形状。
func workbuddyAcceptLanguageFor(realm string) string {
	if realm == "global" {
		return "en-US,en;q=0.9"
	}
	return "zh-CN,zh;q=0.9"
}

// workbuddyStableUID 返回账号级稳定标识：凭据 uid 优先；为空时回落
// `acct-<账号ID>`（跨重启稳定、账号间互异）。
//
// 【用途已收敛（2026-11-01）】它曾服务于 X-Machine-ID / X-Session-ID 与
// prompt_cache_key 的派生，三条均已剔除。现存**真实**消费点只剩一处：活跃上报
// body 的 user_id（workbuddy_tasks_service.go，该任务默认已关）。
// 不得再用它派生任何新的出站恒定身份值（按 uid 折叠出永久常量正是本次剔除的形态）。
func workbuddyStableUID(creds WorkbuddyCredentials, account *Account) string {
	if uid := strings.TrimSpace(creds.UID); uid != "" {
		return uid
	}
	if account != nil {
		return "acct-" + strconv.FormatInt(account.ID, 10)
	}
	return ""
}

// workbuddyDeriveAccountStableID 已删除（2026-11-01，账号封禁排查的直接结论）。
//
// 它按 sha256("wb2a:"+purpose+":"+uid) 派生恒定的 X-Machine-ID / X-Session-ID。两个
// 问题叠加使其成为净负资产：
//  1. 官方客户端与参考实现**都不发**这两个头（二分实测：NONE of those is required），
//     所以「多出来」本身就是网关标记；
//  2. 值是按 uid 派生的**永久常量**、跨进程重启永不轮换，而真实设备会话会变。
//     一枚永不变的设备号是上游侧最强的一致性键：一旦开始按该键做批量关联，
//     该账号全部历史流量会被聚到同一个「设备」上。
//
// 盐串 "wb2a:"（workbuddy2api 的项目名）也已从本仓出站数据中完全移除（另一处是
// prompt_cache_key，见 workbuddy_payload.go）。

// workbuddyNewMessageID 生成消息级 ID：32 位 hex（对齐官方 X-Request-ID /
// X-Conversation-Message-ID 的 UUID 去横线形态）。crypto/rand 失败（理论上不可能）
// 时回落 sha256 派生——恒 32 hex、恒合法，可安全用作 B3 TraceId。
func workbuddyNewMessageID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("workbuddy-msg:%d", time.Now().UnixNano())))
	return hex.EncodeToString(sum[:16])
}

// workbuddyValidTraceID 判断 B3 TraceId 是否合法：16 或 32 位 hex（大小写均可）。
// 入站透传值可能是任意形状（含横线/超长/非 hex），直接塞进 B3 头会破坏链路关联。
func workbuddyValidTraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// applyWorkbuddyCommonHeaders 设置所有 WorkBuddy API 共享的请求头。
//
// 【2026-11-01 剔除清单：只删「官方不发、且值全池恒定」的自造头】
//   - X-Machine-ID / X-Session-ID　—— 按 uid 派生的永久常量设备号。9router #4946
//     独立做出同样判断并从 chat 头删除，原文：「实测对话请求不带这两个（只用于反馈
//     上报），而网关发的是每账号恒定派生值，**本身就是一处「网关特征」**」。
//
// 【不删清单（本仓一度误删，已按官方 app.asar 逐字证据恢复）】
//   - x-codebuddy-request: 1　—— 官方 applyGatewayHeaders 逐字发；
//   - X-Agent-Purpose: conversation / X-Agent-Intent: craft　—— 同上；
//   - Accept-Language　—— Electron 必带，只把值改成浏览器真实形状（主语言+q 兜底）；
//   - X-IDE-* / X-Request-ID　—— 官方发，但收进账号级开关（见 chatHeaders）。
//
// 两个清单的区分口径：**以官方客户端实际字节为准，不以第三项目的为准**。目标是
// 「过闸门」的项目（9router、dsh 插件）可以删官方头，目标是「像官方」的我们不能。
func applyWorkbuddyCommonHeaders(req *http.Request, creds WorkbuddyCredentials, account *Account, realm string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", workbuddyAcceptDefault)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := workbuddyOriginFor(realm)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", workbuddyUAFor(realm))
	req.Header.Set("Accept-Language", workbuddyAcceptLanguageFor(realm))
	req.Header.Set("X-CodeBuddy-Request", workbuddyCodeBuddyRequest)
	// creds/account 不参与本函数头构造：按 uid 派生的 X-Machine-ID / X-Session-ID 已
	// 剔除。保留形参以免扰动六个调用点的签名与既有测试形状。
	_ = creds
	_ = account
}

// workbuddyChatMeta 一次 chat 出站的会话头族元数据（后台按 X-Conversation-Request-ID
// 聚合请求；同一次 user send 内的 tool call/重试/换号复用同一个 ID）。
type workbuddyChatMeta struct {
	ConversationID        string // X-Conversation-ID：入站透传或 body 提取值，两者都无则留空，由注入层每请求新生成随机值（恒发）
	ConversationRequestID string // X-Conversation-Request-ID / X-Root-Request-ID：聚合主键，必发
	TraceID               string // X-Trace-ID：入站透传值，空则回落 conversationRequestID
}

// resolveWorkbuddyChatMeta 组装会话头族元数据：入站头透传优先（客户端给什么用什么），
// 缺省按请求新生成（sub2api 无粘性会话键，每请求独立聚合主键；调用方可复用返回的
// meta 使同一次发送的多次出站共享聚合 ID）。
//
// 【2026-11-01 变更】uid 形参已移除：它此前只服务于「按 uid+首句派生会话 ID」，
// 而该派生正是本次封号排查判定要剔除的复用形态（见 workbuddyDeriveConversationID）。
func resolveWorkbuddyChatMeta(c *gin.Context, body []byte) workbuddyChatMeta {
	meta := workbuddyChatMeta{}
	if c != nil && c.Request != nil {
		meta.ConversationRequestID = strings.TrimSpace(c.Request.Header.Get("X-Conversation-Request-ID"))
		meta.TraceID = strings.TrimSpace(c.Request.Header.Get("X-Trace-ID"))
		meta.ConversationID = strings.TrimSpace(c.Request.Header.Get("X-Conversation-ID"))
	}
	if meta.ConversationRequestID == "" {
		meta.ConversationRequestID = workbuddyNewMessageID()
	}
	if meta.ConversationID == "" {
		meta.ConversationID = workbuddyResolveConversationID(body)
	}
	// 【恒发兜底】国际版渠道校验要求会话头三件套齐全，缺 X-Conversation-ID 即拦
	// （400/11128）。这里刻意保持为空，由注入层用**每请求随机**值补齐：不在派生层
	// 做任何按内容折叠，避免把「多轮复用同一个会话身份」重新引回来。
	return meta
}

// workbuddyDeriveConversationID 已删除（2026-11-01，账号封禁排查的直接结论）。
//
// 它把 X-Conversation-ID 派生成 sha256(uid + "|" + 首条 user 文本)，即「同一对话的
// 每一轮、每一次重试、每一次换号都发同一个会话 ID」。参考实现明确反对这种复用，
// 原文：
//
//	"reusing one value across requests would forge a conversation identity the plugin
//	 does not actually have, and the first thing that would break is the upstream's
//	 own correlation of a session it is being told about twice under one id"
//
// 而官方客户端是**每次调用现生成**会话身份（其注释：官方 CLI 也是每次现生成一组
// 会话 UUID）。
//
// 为什么这是风控信号而不是优化点：渠道闸只查三件套的**存在性**（不查值是否注册过），
// 所以稳定 ID 换不来任何通过收益；代价却是「一个会话身份被反复复用，且跨账号
// failover 时同一个 cid 换一个 Authorization 继续发」——后者是典型的池化/中转特征。
// 11128 的正确修法是「恒发」，不是「恒发同一个值」。

// workbuddyFirstUserText 提取 messages 中首条 role=user 的纯文本内容。
//
// 【用途变更（2026-11-01）】原先只服务于会话 ID 派生（已删除）。现在仅供诊断与
// 既有单测使用：**不得**再用它的返回值生成任何发往上游的身份值 —— 按内容折叠出
// 恒定 ID 就是本次排查剔除的那个形态。content 支持 string 与 OpenAI multipart
// 数组（只拼 type=text 段）两种载体；无 user 消息或解析失败时返回 ""。
func workbuddyFirstUserText(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	for _, msg := range obj.Messages {
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			continue
		}
		var text string
		if err := json.Unmarshal(msg.Content, &text); err == nil {
			return text
		}
		// multipart：[{"type":"text","text":"..."}, {"type":"image_url",...}]
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(msg.Content, &parts); err == nil {
			var sb strings.Builder
			for _, p := range parts {
				if p.Type == "text" {
					sb.WriteString(p.Text)
				}
			}
			return sb.String()
		}
		return ""
	}
	return ""
}

// workbuddyResolveConversationID 从请求体提取会话 ID（metadata 优先、snake 优先于
// camel；只认 conversationId，绝不回落 user_id）。缺失返回 ""（不伪造——透传
// 客户端原值优先，客户端没给就不发，避免误导后台建错会话）。
func workbuddyResolveConversationID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v, ok := meta["conversation_id"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		if v, ok := meta["conversationId"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if v, ok := obj["conversation_id"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := obj["conversationId"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

// applyWorkbuddyChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
func applyWorkbuddyChatHeaders(req *http.Request, creds WorkbuddyCredentials, account *Account, realm string, meta workbuddyChatMeta) {
	applyWorkbuddyCommonHeaders(req, creds, account, realm)
	// chat 流式 Accept：显式声明 text/event-stream（本仓出站恒被 PrepareWorkbuddyBody
	// 强制 stream:true，该声明与真实流式请求形状一致）。
	req.Header.Set("Accept", "application/json, text/event-stream")
	if at := strings.TrimSpace(creds.AccessToken); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if uid := strings.TrimSpace(creds.UID); uid != "" {
		req.Header.Set("X-User-Id", uid)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	// 企业与域头按 realm 分发：CN 走 EnterpriseID/Domain 原样透传（缺省 X-No-*）；
	// global 账号统一覆写为国际客户端形态（X-No-Enterprise-Id=1 声明无企业 +
	// X-Domain=www.workbuddy.ai 声明国际版域，不回退登录会话原值）。
	if realm == "global" {
		req.Header.Set("X-No-Enterprise-Id", "1")
		req.Header.Set("X-Domain", workbuddyGlobalDomain)
	} else {
		if ent := strings.TrimSpace(creds.EnterpriseID); ent != "" {
			req.Header.Set("X-Enterprise-Id", ent)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}
		if d := strings.TrimSpace(creds.Domain); d != "" {
			req.Header.Set("X-Domain", d)
		} else {
			req.Header.Set("X-No-Department-Info", "1")
		}
	}
	// X-Product：官方恒发，值 "SaaS"（此前送 "WorkBuddy" 与官方 bundle 不符）。
	req.Header.Set("X-Product", workbuddyProductName)
	// 官方渠道意图头（app.asar applyGatewayHeaders 逐字）：保留。
	req.Header.Set("X-Agent-Purpose", workbuddyAgentPurpose)
	req.Header.Set("X-Agent-Intent", workbuddyAgentIntent)
	//
	// 用量归属头 X-IDE-* / X-Request-ID：改为**账号级开关 usage_identity**（默认关）。
	// 参考实现在这里同样用开关控制而非默认开启，因为它实测到：这几枚头只改变上游
	// 用量页的显示读数，不参与任何闸门判定（"Nothing upstream REJECTS a v4 id ... so
	// this is a display-fidelity fix, not a gate"）。既然对通过无用、只是身份声明，
	// 就不该由网关静默地替每个账号宣称「我是 WorkBuddy 桌面端」。
	if creds.UsageIdentity {
		req.Header.Set("X-IDE-Name", workbuddyAttributionName)
		req.Header.Set("X-IDE-Type", workbuddyIDETypeName)
		req.Header.Set("X-IDE-Version", workbuddyClientVersion)
	}
	// 设备风控头：凭据 DeviceToken 非空才注入（空则不注入，优雅降级）。
	// 【已知硬缺口】官方每请求都带 465 字符的 x-device-token，它由腾讯 T-Sec
	// native SDK 在设备侧生成（每设备一个、与账号无关）；三个独立第三方项目均声明
	// 无法伪造，qimei36 同理。所以「完全像官方」做不到 —— 本次改动只能消除
	// 「自造且值恒定」这类可避免的网关特征，不能声称已补齐设备层。
	// 该值来自真实登录时，属「有就用」而非「伪造」。
	if dt := strings.TrimSpace(creds.DeviceToken); dt != "" {
		req.Header.Set("X-Device-Token", dt)
	}
	applyWorkbuddyConversationHeaders(req, meta, creds.UsageIdentity)
}

// applyWorkbuddyConversationHeaders 注入官方客户端会话头族。
// 头族分层（三件套的存在性是国际版渠道闸 11128 的实测判据，缺一不可）：
//   - X-Conversation-ID：会话级。入站/body 显式值优先透传；两者都没有时**每请求
//     新生成**随机 32 hex，恒发。不再按「uid+首条 user」派生跨轮恒定值——那种复用
//     正是 2026-11-01 封号排查判定要剔除的形态（见文件内 workbuddyDeriveConversationID
//     的删除说明）。
//   - X-Conversation-Request-ID：对话轮级聚合主键，必发（空值兜底新生成 32 hex）。
//   - X-Conversation-Message-ID：消息级，每条独立（32 hex）；X-Request-ID 与之同值，
//     但仅在 usage_identity 开启时发（它只是上游用量页的请求 id 读数，非闸门）。
//   - X-Root-Request-ID：= conversationRequestID（根请求追踪）。
//   - X-Trace-ID：入站透传或 = conversationRequestID。
//   - X-B3-TraceId / X-B3-SpanId / X-B3-Sampled：链路族。B3 规范只认 16/32 hex
//     TraceId 与 16 hex SpanId；入站值非法时 TraceId 回落 messageID（恒 32 hex），
//     SpanId 取 messageID[:16]（每消息新）。
//
// usageIdentity 只影响 X-IDE-* 与 X-Request-ID；三件套与 Trace/B3 族**不受开关影响**，
// 否则开关一关就把 11128 那道闸门重新打开了。
func applyWorkbuddyConversationHeaders(req *http.Request, meta workbuddyChatMeta, usageIdentity bool) {
	convReqID := strings.TrimSpace(meta.ConversationRequestID)
	if convReqID == "" {
		convReqID = workbuddyNewMessageID()
	}
	messageID := workbuddyNewMessageID()
	// 恒发：调用方（resolveWorkbuddyChatMeta）已保证显式值优先；这里仍做 trim 与兜底，
	// 防止未来新增绕过 resolve 的直接构造点又把头丢掉（11128 那次故障的确切形态）。
	//
	// 兜底一律**每请求随机**。禁止改回按内容/uid 派生：那会造出跨轮复用的会话身份
	//（本次 11140 排查判定为池化特征）；极端形态下「sha256("|") 这类编译期常量」
	// 更会让全部账号发出同一个 X-Conversation-ID —— 比「缺头被拦」更糟，因为串号
	// 不可观察、不可归因。宁可牺牲会话稳定性，也不能牺牲账号隔离。
	cid := strings.TrimSpace(meta.ConversationID)
	if cid == "" {
		cid = workbuddyNewMessageID()
	}
	req.Header.Set("X-Conversation-ID", cid)
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	if usageIdentity {
		req.Header.Set("X-Request-ID", messageID)
	}
	req.Header.Set("X-Root-Request-ID", convReqID)
	traceID := strings.TrimSpace(meta.TraceID)
	if traceID == "" {
		traceID = convReqID
	}
	req.Header.Set("X-Trace-ID", traceID)
	b3Trace := convReqID
	if !workbuddyValidTraceID(b3Trace) {
		b3Trace = messageID // 非法 B3 TraceId → 回落恒 32 hex 的消息级 ID
	}
	req.Header.Set("X-B3-TraceId", b3Trace)
	req.Header.Set("X-B3-SpanId", messageID[:16])
	req.Header.Set("X-B3-Sampled", "1")
}

// applyWorkbuddyRefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func applyWorkbuddyRefreshHeaders(req *http.Request, creds WorkbuddyCredentials, account *Account, realm string) {
	applyWorkbuddyCommonHeaders(req, creds, account, realm)
	req.Header.Set("X-Refresh-Token", strings.TrimSpace(creds.RefreshToken))
	if ent := strings.TrimSpace(creds.EnterpriseID); ent != "" {
		req.Header.Set("X-Enterprise-Id", ent)
	}
	// X-Auth-Refresh-Source 对齐参考实现 refreshHeaders 的取值 "workbuddy"。
	// 此前送 "plugin"：那是 workbuddy2api 时代的值，现在两家已不同源，且它字面
	// 自报「插件渠道」——正是渠道校验最想看的读数。
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
}

// applyWorkbuddyBillingHeaders billing 域（积分查询 / 每日签到）请求头。
// 与 chat 域头组的差异（对齐官方 billing 白名单头组，workbuddy2api BillingHeaders）：
//   - UA 覆写为单段 `WorkBuddy/<ver>`（官方 banner/check-in 显式覆写形态，不带 CLI 段）；
//   - 带 Authorization: Bearer <access_token> 与 X-User-Id；
//   - CN 带 X-Enterprise-Id + X-Tenant-Id；global 显式声明无企业 + 国际域；
//   - 绝不携带 X-Refresh-Token。
func applyWorkbuddyBillingHeaders(req *http.Request, creds WorkbuddyCredentials, account *Account, realm string) {
	applyWorkbuddyCommonHeaders(req, creds, account, realm)
	// billing 白名单接口的单段 UA 覆写（不带 CLI 段）。
	req.Header.Set("User-Agent", "WorkBuddy/"+workbuddyClientVersion)
	if at := strings.TrimSpace(creds.AccessToken); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if uid := strings.TrimSpace(creds.UID); uid != "" {
		req.Header.Set("X-User-Id", uid)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if realm == "global" {
		req.Header.Set("X-No-Enterprise-Id", "1")
		req.Header.Set("X-Domain", workbuddyGlobalDomain)
	} else {
		if ent := strings.TrimSpace(creds.EnterpriseID); ent != "" {
			req.Header.Set("X-Enterprise-Id", ent)
			req.Header.Set("X-Tenant-Id", ent)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}
		if d := strings.TrimSpace(creds.Domain); d != "" {
			req.Header.Set("X-Domain", d)
		} else {
			req.Header.Set("X-No-Department-Info", "1")
		}
	}
	// 设备风控头：凭据 DeviceToken 非空才注入（空则不注入，优雅降级）。
	if dt := strings.TrimSpace(creds.DeviceToken); dt != "" {
		req.Header.Set("X-Device-Token", dt)
	}
}
