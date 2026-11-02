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
//   - X-Conversation-ID / X-Conversation-Request-ID：同日两次修正，终态为「会话级/
//     轮级稳定派生 + 带横线 UUID 形态」（第一次改成「每请求随机」已被官方一手源码
//     推翻，翻证记录见本文件 workbuddyStableConversationID 上方）；
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
	ConversationID        string // X-Conversation-ID：入站头 → body 显式值 → 会话级稳定派生（带横线 UUID）；无锚点时由注入层每请求新生成
	ConversationRequestID string // X-Conversation-Request-ID / X-Root-Request-ID：聚合主键，必发
	TraceID               string // X-Trace-ID：入站透传值，空则回落 conversationRequestID
}

// resolveWorkbuddyChatMeta 组装会话头族元数据：入站头透传优先（客户端给什么用什么），
// 其次 body 显式值，两者都没有时按「账号 + 客户端区分因子 + 内容锚点」派生稳定值。
//
// 【2026-11-01 二次修正：撤销「每请求随机」口径】同日第一次修正把派生值改成每请求
// 随机，依据是第三方网关的自述；本轮拿到官方一手源码后该依据被推翻（详见下方
// workbuddyStableConversationID 的翻证记录）。现在按官方三层生命周期分别取锚点：
//   - ConversationID：会话级，锚点=**首条**用户发言（一个聊天窗口一个值）
//   - ConversationRequestID：轮级，锚点=**末条**用户发言（一次 user send 一个值）
//
// account 参与派生是**隔离**用途而非恒定用途：换号 failover 时 cid/reqID 必须随之改变
// （magpie 一手注释：one conversation or turn never shows up under two X-User-Ids），
// 与同仓 OpenAI 路径 isolateOpenAIUpstreamSessionID 把 session 钉在账号命名空间上同口径。
func resolveWorkbuddyChatMeta(c *gin.Context, account *Account, body []byte) workbuddyChatMeta {
	meta := workbuddyChatMeta{}
	if c != nil && c.Request != nil {
		meta.ConversationRequestID = strings.TrimSpace(c.Request.Header.Get("X-Conversation-Request-ID"))
		meta.TraceID = strings.TrimSpace(c.Request.Header.Get("X-Trace-ID"))
		meta.ConversationID = strings.TrimSpace(c.Request.Header.Get("X-Conversation-ID"))
	}
	if meta.ConversationID == "" {
		meta.ConversationID = workbuddyResolveConversationID(body)
	}
	var accountID int64
	if account != nil {
		accountID = account.ID
	}
	disc := workbuddyClientDiscriminator(c)
	if meta.ConversationID == "" {
		meta.ConversationID = workbuddyStableConversationID(accountID, disc, workbuddyFirstUserText(body))
	}
	if meta.ConversationRequestID == "" {
		meta.ConversationRequestID = workbuddyStableRequestID(accountID, disc, workbuddyLastUserText(body))
	}
	// 【恒发兜底】国际版渠道校验要求会话头三件套齐全，缺 X-Conversation-ID 即拦
	// （400/11128）。上面两个派生函数在无锚点时各自回落随机值，因此这里保证非空，
	// 但不保证跨请求恒定 —— 无锚点就没有会话可标识，此时宁可每请求新随机，
	// 也不能退化成「账号级恒定值」把整号流量聚成一个会话。
	return meta
}

// workbuddyClientDiscriminator 客户端区分因子：IP + 归一化 UA + API Key ID。
// 与 Claude 伪装路径 sessionContextDiscriminator 同口径，作用是避免不同终端用户的
// 相同首条消息派生出同一个会话 ID（跨用户串会话）。
func workbuddyClientDiscriminator(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	ua := ""
	if c.Request.Header != nil {
		ua = c.Request.Header.Get("User-Agent")
	}
	return c.ClientIP() + ":" + NormalizeSessionUserAgent(ua) + ":" + strconv.FormatInt(getAPIKeyIDFromContext(c), 10)
}

// workbuddyStableConversationID 会话级 X-Conversation-ID：**带横线 UUIDv4**。
//
// 形态依据：官方 session.id 就是带横线 UUID；9router #4946 拓包原文「main 对话的会话
// id 实际是 **v4**、辅助请求是 v7，两者都被接受」⇒ 主对话用 v4 形态与官方一致。
//
// 不复用仓内 generateSessionUUID：它用 `%x-…` 格式化，每段会丢前导零（bytes[0]=0x0a
// 输出 "a" 而非 "0a"），导致长度不是稳定 36 字符 —— 那本身就是一处非官方形态。
// 也不伪造成 v7：v7 高 48 位是毫秒时间戳，拿 sha256 摘要置上 version nibble 会得到
// 「时间前缀落在无意义时刻」的值，比 v4 更像伪造（上游不校版本位，无收益）。
//
// 无锚点时每请求新随机，不得退化成账号级恒定值。
func workbuddyStableConversationID(accountID int64, discriminator, sessionAnchor string) string {
	anchor := strings.TrimSpace(sessionAnchor)
	if anchor == "" {
		return workbuddyNewConversationID()
	}
	sum := sha256.Sum256([]byte("workbuddy-conv:" + strconv.FormatInt(accountID, 10) + "::" + discriminator + "::" + anchor))
	return workbuddyFormatUUID(sum[:16])
}

// workbuddyStableRequestID 轮级 X-Conversation-Request-ID：32 位小写无横线 hex，对齐
// 官方文档强制形态（「小写、无连字符的 32 位 UUIDv7」；实测版本位不校验，sha256 摘要即可）。
func workbuddyStableRequestID(accountID int64, discriminator, turnAnchor string) string {
	anchor := strings.TrimSpace(turnAnchor)
	if anchor == "" {
		return workbuddyNewMessageID()
	}
	sum := sha256.Sum256([]byte("workbuddy-turn:" + strconv.FormatInt(accountID, 10) + "::" + discriminator + "::" + anchor))
	return hex.EncodeToString(sum[:16])
}

// workbuddyNewConversationID 生成全新的带横线 UUIDv4（无会话锚点时的会话级兜底）。
// crypto/rand 失败（理论上不可能）时回落时间摘要，保证恒为合法 36 字符 UUID。
func workbuddyNewConversationID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("workbuddy-conv:%d", time.Now().UnixNano())))
		copy(b, sum[:16])
	}
	return workbuddyFormatUUID(b)
}

// workbuddyFormatUUID 把字节格式化为标准 36 字符小写 UUID，并置 UUIDv4 版本位与
// RFC 4122 变体位。与 generateSessionUUID 同构，但用 %02x 零填充修复其丢前导零
// （generateSessionUUID 服务于 Claude 伪装路径、上游不校验其形态，本轮不动它以免
// 扰动既有断言）。不足 16 字节时零填充，不 panic。
func workbuddyFormatUUID(src []byte) string {
	var b [16]byte
	copy(b[:], src)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7], b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15])
}

// 【翻证记录 2026-11-01：为什么同日第一次修正的「每请求随机」是错的】
//
// 第一次修正在此处删除了「按 uid+首句派生会话 ID」，其两条依据本轮均被官方一手
// 源码推翻，登记如下以免日后又被改回去：
//
//  1. 原声明「官方客户端每次调用现生成会话身份」——错。官方 npm 包
//     @tencent-ai/codebuddy-code@2.179.1 的 dist-server/codebuddy.js（未混淆）里
//     buildConversationHeaders 逐字为：
//         let n = (0,I.nH)().replace(/-/g,"");            // I.nH === uuidv7
//         [CONVERSATION_ID_HEADER]:         i.id,        // ★ session.id 原值，不是随机
//         [CONVERSATION_REQUEST_ID_HEADER]: getEffectiveConversationRequestId(i)||"",
//         [CONVERSATION_MESSAGE_ID_HEADER]: n,
//         [REQUEST_ID_HEADER]:              n
//     官方对 messageId 显式去横线、对 session.id 不去 ⇒ 「会话 ID 带横线、请求/消息 ID
//     不带」是有意区分的两种形态。session 创建点 `let n=(0,eL.nH)();
//     sessionManager.create({id:n, meta:{sessionId:n}})` 全程未去横线，且该 id 可
//     resume/持久化（官方 SDK 文档）。
//  2. 原引用的两处「反对复用」——dsh-connect-workbuddy 与 9router wb_proxy——都是
//     第三方网关**自身架构受限下的取舍**（9router 自述理由是按账号派生会在多池/多
//     设备场景撞 id），不是官方形态观测。口径应当是：目标是「过闸门」的项目可以
//     牺牲保真度，目标是「像官方」的我们不行。
//
// 【独立交叉印证（方向一致）】
//   - sub2api PR #4684（读同一个官方 npm 包）：「the official client sends
//     X-Conversation-ID = currentSession.id on every model request, while
//     X-Conversation-Request-ID / -Message-ID / X-Request-ID rotate」；
//   - 9router #4946（5.8.4 静态解包 + mitmproxy ~350 条流量）：「会话 id 是带连字符的
//     UUID，请求/消息 id 是 32 位无连字符 hex」，且实测 v4/v7 都被接受 ⇒ 版本位不校验；
//   - 官方 ACP 文档对 request-id 强制「小写、无连字符的 32 位 UUIDv7」，并说明同一值
//     会写入本轮 PromptResponse._meta / JSONL providerData / 模型请求头 ⇒ 一次 user
//     send 一个值；
//   - magpie 插件一手注释：上游**按 X-Conversation-Request-ID 计一次用户发送**；
//   - wild-work R116：「旧实现四个头绑成同一个值且每请求现生成 —— 等于把一次多轮
//     对话拆成上千个一次性会话，在上游按 Request-ID 计 send 的口径下直接放大限流命中面」。
//
// 【每请求随机真实代价】一个账号每天发出海量「只含 1 次请求、零历史深度」的全新
// 会话，与「单会话 ID 复用上万次」是同一枚硬币的两面，两者都是机器特征；同时
// 用量明细被拆碎、限流计数被放大。稳定化换不来通过率（四处独立实测：值不校验，
// 非 UUID 字符串也放行），收益是计数/归并口径正确 + 更像官方。
//
// 【必须保留的正确结论】派生值不能跨账号复用 —— 故种子含账号 ID，换号 failover
// 自然得到不同 cid/reqID（magpie：one conversation or turn never shows up under two
// X-User-Ids）。11128 的正解仍是「恒发」，只是现在发的值与官方语义一致。

// workbuddyFirstUserText 提取 messages 中首条「用户自己说的」纯文本，作为**会话级**锚点
// （一个聊天窗口的第一句话，跨轮不变）。
//
// 【用途恢复（2026-11-01 二次修正）】同日第一次修正曾禁止它参与身份派生，理由是
// 把「按内容折叠出恒定 ID」当成池化特征；官方一手源码证明会话 ID 本就跨轮复用，
// 故恢复派生用途（见 workbuddyStableConversationID 一节的翻证记录）。
// 与 workbuddyLastUserText 共用 workbuddyUserTextAt，两条口径必须一致。
func workbuddyFirstUserText(body []byte) string { return workbuddyUserTextAt(body, true) }

// workbuddyLastUserText 提取 messages 中**末条**「用户自己说的」纯文本，作为**轮级**锚点。
//
// 官方语义：一次 user send 对应一个 X-Conversation-Request-ID；一轮内的 tool 调用回传
// 追加的是 assistant/tool 消息，末条用户发言不变 ⇒ 同一轮多次出站复用同一值；用户说
// 下一句时末条改变 ⇒ 自然换新值（wild-work R116 与 magpie turnKey 同口径）。
func workbuddyLastUserText(body []byte) string { return workbuddyUserTextAt(body, false) }

// workbuddyUserTextAt 按方向提取用户发言文本（fromFirst=true 取首条，false 取末条）。
//
// 「用户自己说的」判定：role 必须是 user（排除 role=tool 的工具结果回传），且当
// content 为 part 数组时不得是纯 tool_result（Anthropic 形态的工具结果也走 user 角色，
// 它不是用户发言；若同时带文字则取文字，视为用户的附加说明）。content 支持 string
// 与 OpenAI multipart 数组两种载体；无匹配消息或解析失败返回 ""。
func workbuddyUserTextAt(body []byte, fromFirst bool) string {
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
	for i := range obj.Messages {
		idx := i
		if !fromFirst {
			idx = len(obj.Messages) - 1 - i
		}
		msg := obj.Messages[idx]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			continue
		}
		if text, ok := workbuddyUserContentText(msg.Content); ok {
			return text
		}
	}
	return ""
}

// workbuddyUserContentText 把一条 user 消息的 content 归一为用户发言文本。
// ok=false 表示该消息不是用户自己的发言（纯工具结果回传）或无法解析。
func workbuddyUserContentText(content json.RawMessage) (string, bool) {
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return text, true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err == nil {
		var sb strings.Builder
		sawToolResult := false
		for _, p := range parts {
			switch p.Type {
			case "text":
				sb.WriteString(p.Text)
			case "tool_result":
				sawToolResult = true
			}
		}
		if sb.Len() == 0 && sawToolResult {
			return "", false
		}
		return sb.String(), true
	}
	return "", false
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
//   - X-Conversation-ID：会话级（= 官方 session.id）。优先级：入站头 → body 显式值 →
//     按「账号 ID + 客户端区分因子 + 首条用户发言」派生的带横线 UUID；无锚点时由
//     注入层每请求新生成带横线 UUID。同一聊天窗口跨轮复用同一个值（对齐官方），
//     换号 failover 因账号 ID 参与派生而自然得到不同值。
//   - X-Conversation-Request-ID：轮级（= 一次 user send），必发。锚点=**末条**用户
//     发言（tool 回传不算），32 位小写无横线 hex；一轮内多次出站复用同一值。
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
	// 恒发：调用方（resolveWorkbuddyChatMeta）已保证显式值优先与锚点派生；这里仍做
	// trim 与兜底，防止未来新增绕过 resolve 的直接构造点又把头丢掉（11128 那次故障的
	// 确切形态）。
	//
	// 兜底形态：**带横线 UUID**（与官方 session.id 同形）。禁止改回 32 hex 无横线 ——
	// 那与官方两种形态有意区分的结论相反（见上方翻证记录）。禁止改成「按账号派生的
	// 恒定值」：那会把一个账号的全部历史流量聚成一个会话（masknull 实测形态：单会话
	// ID 复用 18615 次被判定为网关池化）。无锚点 ⇒ 无真实会话可标识 ⇒ 宁可每请求新随机。
	cid := strings.TrimSpace(meta.ConversationID)
	if cid == "" {
		cid = workbuddyNewConversationID()
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
