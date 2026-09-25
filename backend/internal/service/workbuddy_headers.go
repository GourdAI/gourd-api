package service

// workbuddy_headers.go WorkBuddy 出站请求头构造（移植自 workbuddy2api/internal/upstream/headers.go，
// 配置来源由全局 config 改为账号凭据）。
//
// WorkBuddy 上游对客户端指纹敏感：UA 三段式、X-CodeBuddy-Request 风控闸门头、
// 按 uid 稳定派生的机器/会话标识、CN/global 双域的 Origin/语言/企业头形态，
// 以及官方客户端的会话头族（X-Conversation-* / X-B3-*，后台按
// X-Conversation-Request-ID 聚合请求）。任一缺项可能触发上游风控误判
// （如 global 账号 UA 平台段送错触发 403 code 11140）。

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
	// workbuddyClientVersion UA 的 `WorkBuddy/<ver>` 段与 X-IDE-Version
	// （对齐官方 WorkBuddy Desktop 5.5.4 分发包版本）。
	workbuddyClientVersion = "5.5.4"
	// workbuddyCLIVersion UA 的 `CLI/<ver>` 段（对齐官方内置 CLI 2.137.1）。
	workbuddyCLIVersion = "2.137.1"

	// workbuddyOriginCN / workbuddyOriginGlobal Origin/Referer 基础域（按 realm 切换）。
	workbuddyOriginCN     = "https://www.codebuddy.cn"
	workbuddyOriginGlobal = "https://www.workbuddy.ai"

	// workbuddyAttributionName 用量归属头取值（伪造官方桌面端指纹，避免上游用量
	// 统计里出现 client/agentPurpose 为空的「网关特征」）。
	workbuddyAttributionName = "WorkBuddy"

	// workbuddyGlobalDomain global realm 显式声明的国际版域（与 Origin/Referer 同域）。
	workbuddyGlobalDomain = "www.workbuddy.ai"
)

// workbuddyUAFor 组装默认出站 UA（官方桌面端 RestOperations 层形状）：
// `WorkBuddy/<clientVersion> <platform>/<clientVersion> CLI/<cliVersion>`。
// 平台段（第二段）品牌按 realm 切换——CN 用 `WorkBuddy`，global 用官方国际版
// productName `WorkBuddy AI`（送错可能触发上游 403 code 11140）。
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

// workbuddyAcceptLanguageFor 按 realm 返回 Accept-Language：global → en-US，cn → zh-CN。
func workbuddyAcceptLanguageFor(realm string) string {
	if realm == "global" {
		return "en-US"
	}
	return "zh-CN"
}

// workbuddyStableUID 返回账号级稳定标识：凭据 uid 优先；为空时回落
// `acct-<账号ID>`（跨重启稳定、账号间互异，保证机器/会话派生与缓存键隔离
// 在无 uid 凭据下依然成立）。
func workbuddyStableUID(creds WorkbuddyCredentials, account *Account) string {
	if uid := strings.TrimSpace(creds.UID); uid != "" {
		return uid
	}
	if account != nil {
		return "acct-" + strconv.FormatInt(account.ID, 10)
	}
	return ""
}

// workbuddyDeriveAccountStableID 按 uid + 用途盐稳定派生 36 hex 设备/会话标识：
// sha256("wb2a:"+purpose+":"+uid) 前 18 字节的 hex。
// 固定盐（不随进程换）、账号间互异（uid 不同则不同）、同 uid 同用途恒同值（幂等），
// 对齐官方桌面端「每账号一台固定虚拟设备」语义，防多号被上游按设备指纹漂移关联风控。
//   - purpose="machine" → X-Machine-ID（设备级，跨会话稳定）
//   - purpose="session" → X-Session-ID（账号固定会话，跨重启稳定）
func workbuddyDeriveAccountStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

// workbuddyNewMessageID 生成消息级 ID：32 位 hex（对齐官方 X-Request-ID /
// X-Conversation-Message-ID 的 UUID 去横线形态）。crypto/rand 失败（理论上不可能）
// 时回落 sha256 派生——恒 32 hex、恒合法，可安全用作 B3 TraceId。
func workbuddyNewMessageID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("wb2a-msg:%d", time.Now().UnixNano())))
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

// applyWorkbuddyCommonHeaders 设置所有 WorkBuddy API 共享的请求头
// （对齐官方客户端风控闸门：UA/Origin/Accept-Language/X-CodeBuddy-Request
// 与账号级机器/会话标识）。
func applyWorkbuddyCommonHeaders(req *http.Request, creds WorkbuddyCredentials, account *Account, realm string) {
	req.Header.Set("Content-Type", "application/json")
	// Accept 非流式默认 application/json；chat 路径在 applyWorkbuddyChatHeaders 覆盖为流式。
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := workbuddyOriginFor(realm)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", workbuddyUAFor(realm))
	// X-CodeBuddy-Request: 1（官方客户端风控闸门头，所有 API 请求必带）。
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("Accept-Language", workbuddyAcceptLanguageFor(realm))
	// X-Machine-ID / X-Session-ID：按 uid 稳定派生的账号级设备头。
	// uid 为空时用 acct-<id> 兜底（sub2api 账号恒有 ID，等价「每账号一台固定虚拟设备」）。
	if uid := workbuddyStableUID(creds, account); uid != "" {
		req.Header.Set("X-Machine-ID", workbuddyDeriveAccountStableID(uid, "machine"))
		req.Header.Set("X-Session-ID", workbuddyDeriveAccountStableID(uid, "session"))
	}
}

// workbuddyChatMeta 一次 chat 出站的会话头族元数据（后台按 X-Conversation-Request-ID
// 聚合请求；同一次 user send 内的 tool call/重试/换号复用同一个 ID）。
type workbuddyChatMeta struct {
	ConversationID        string // X-Conversation-ID：入站透传或 body 提取值，空则不发
	ConversationRequestID string // X-Conversation-Request-ID / X-Root-Request-ID：聚合主键，必发
	TraceID               string // X-Trace-ID：入站透传值，空则回落 conversationRequestID
}

// resolveWorkbuddyChatMeta 组装会话头族元数据：入站头透传优先（客户端给什么用什么），
// 缺省按请求新生成（sub2api 无粘性会话键，每请求独立聚合主键；调用方可复用返回的
// meta 使同一次发送的多次出站共享聚合 ID）。
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
	return meta
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
	// chat 流式 Accept 覆盖 common 的非流式默认。
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
	// 用量归属头：伪造真实桌面端头组（X-Agent-Purpose + X-IDE-* + X-Product）。
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", workbuddyAttributionName)
	req.Header.Set("X-IDE-Type", workbuddyAttributionName)
	req.Header.Set("X-IDE-Version", workbuddyClientVersion)
	req.Header.Set("X-Product", workbuddyAttributionName)
	// 设备风控头：凭据 DeviceToken 非空才注入（空则不注入，优雅降级）。
	if dt := strings.TrimSpace(creds.DeviceToken); dt != "" {
		req.Header.Set("X-Device-Token", dt)
	}
	applyWorkbuddyConversationHeaders(req, meta)
}

// applyWorkbuddyConversationHeaders 注入官方客户端会话头族。
// 头族分四层：
//   - X-Conversation-ID：会话级，多轮稳定（入站/body 值）。空则不发——透传优先，
//     客户端没给就不伪造。
//   - X-Conversation-Request-ID：对话轮级聚合主键，必发（空值兜底新生成 32 hex）。
//   - X-Conversation-Message-ID = X-Request-ID：消息级，每条独立（32 hex）。
//   - X-Root-Request-ID：= conversationRequestID（根请求追踪）。
//   - X-Trace-ID：入站透传或 = conversationRequestID。
//   - X-B3-TraceId / X-B3-SpanId / X-B3-Sampled：链路族。B3 规范只认 16/32 hex
//     TraceId 与 16 hex SpanId；入站值非法时 TraceId 回落 messageID（恒 32 hex），
//     SpanId 取 messageID[:16]（每消息新）。
func applyWorkbuddyConversationHeaders(req *http.Request, meta workbuddyChatMeta) {
	convReqID := strings.TrimSpace(meta.ConversationRequestID)
	if convReqID == "" {
		convReqID = workbuddyNewMessageID()
	}
	messageID := workbuddyNewMessageID()
	if cid := strings.TrimSpace(meta.ConversationID); cid != "" {
		req.Header.Set("X-Conversation-ID", cid)
	}
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
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
	// X-Auth-Refresh-Source 对齐官方客户端 refresh 渠道标识 "plugin"。
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
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
