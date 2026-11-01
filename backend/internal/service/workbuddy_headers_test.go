//go:build unit

package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// workbuddy_headers_test.go WorkBuddy 出站请求头构造的单测。
//
// 【2026-11-01 规格说明】本次账号封禁排查只剔除「官方不发且值恒定」的自造头，
// 并修正若干头值形式。下面几条用例的**期望值与修复前相反**：
//   - X-Machine-ID / X-Session-ID：断言从「必须等于某派生值」翻转为「必须为空」；
//   - X-Product：WorkBuddy → SaaS；X-Auth-Refresh-Source：plugin → workbuddy；
//   - 版本段：5.5.4/2.137.1 → 5.9.3/2.165.0；
//   - Accept-Language：裸单标签 → 浏览器形状（主语言 + q 兜底）；
//   - X-Conversation-ID 兜底：按内容派生的恒定值 → 每请求随机；
//   - X-IDE-* / X-Request-ID：默认关，受 usage_identity 控制。
//
// 【刻意保留并有断言】x-codebuddy-request、X-Agent-Purpose、X-Agent-Intent：
// 它们出自官方 app.asar 的 applyGatewayHeaders，不是网关自造；本文件早期曾误删，
// 下面的用例同时钉住「不得再被删」与「不得再被当成剔除项」。
//
// 旧断言把缺陷固化成了契约（同 2026-10 那次「X-Conversation-ID 可缺省」），每条
// 翻转都在注释里写明修复前形态，以便反向护栏能真的失败。

func TestWorkbuddyUAForRealm(t *testing.T) {
	t.Parallel()
	// 版本从 5.5.4/2.137.1 抬到 5.9.3/2.165.0：陈旧且全池同一个版本号是静态识别键。
	require.Equal(t, "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0", workbuddyUAFor("cn"))
	require.Equal(t, "WorkBuddy/5.9.3 WorkBuddy AI/5.9.3 CLI/2.165.0", workbuddyUAFor("global"))
	require.Equal(t, workbuddyUAFor("cn"), workbuddyUAFor(""), "空 realm 按 CN 处理")
	require.NotContains(t, workbuddyUAFor("cn"), "5.5.4", "不得再送陈旧版本段")
}

func TestWorkbuddyCommonHeadersNoFabricatedIdentity(t *testing.T) {
	t.Parallel()
	account := &Account{ID: 42, Platform: PlatformWorkbuddy}
	creds := WorkbuddyCredentials{UID: "user-abc"}
	req := httptest.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	applyWorkbuddyCommonHeaders(req, creds, account, "cn")

	require.Equal(t, "application/json", req.Header.Get("Content-Type"))
	require.Equal(t, "application/json, text/plain, */*", req.Header.Get("Accept"))
	require.Equal(t, "XMLHttpRequest", req.Header.Get("X-Requested-With"))
	require.Equal(t, "https://www.codebuddy.cn", req.Header.Get("Origin"))
	require.Equal(t, "https://www.codebuddy.cn/", req.Header.Get("Referer"))
	require.Equal(t, workbuddyUAFor("cn"), req.Header.Get("User-Agent"))

	// 【保留项】官方 applyGatewayHeaders 逐字发；不得被当成「剔除自造头」而误删。
	require.Equal(t, "1", req.Header.Get("X-CodeBuddy-Request"),
		"x-codebuddy-request 是官方头，必须保留（本次一度误删）")
	require.Equal(t, "zh-CN,zh;q=0.9", req.Header.Get("Accept-Language"),
		"Accept-Language 保留但取浏览器形状（修复前是裸单标签 zh-CN）")

	// 【剔除项】官方不发、且本仓按 uid 派生出永久常量值。
	require.Empty(t, req.Header.Get("X-Machine-ID"),
		"X-Machine-ID 已剔除（修复前是按 uid 派生的永久常量设备号）")
	require.Empty(t, req.Header.Get("X-Session-ID"),
		"X-Session-ID 已剔除（同上，跨重启永不轮换）")
	// 盐串 wb2a: 不得残留在任何出站头值里（wb2a = workbuddy2api 项目名）。
	require.NotContains(t, strings.Join(headerValues(req), "|"), "wb2a",
		"wb2a 盐串不得出现在出站头里")
}

func TestWorkbuddyCommonHeadersAcceptLanguageGlobalShape(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	applyWorkbuddyCommonHeaders(req, WorkbuddyCredentials{UID: "u"}, &Account{ID: 1, Platform: PlatformWorkbuddy}, "global")
	require.Equal(t, "en-US,en;q=0.9", req.Header.Get("Accept-Language"))
}

// TestWorkbuddyCommonHeadersNotConstantAcrossAccounts 剔除设备头后，跨账号可区分性
// 只剩 X-User-Id（真实 uid）与 Authorization，因此必须确认这两者仍按账号不同。
func TestWorkbuddyCommonHeadersNotConstantAcrossAccounts(t *testing.T) {
	t.Parallel()
	chat := func(uid string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
		applyWorkbuddyChatHeaders(req, WorkbuddyCredentials{AccessToken: "tok-" + uid, UID: uid},
			&Account{ID: 1, Platform: PlatformWorkbuddy}, "cn", workbuddyChatMeta{})
		return req
	}
	a, b := chat("u-a"), chat("u-b")
	require.Equal(t, "u-a", a.Header.Get("X-User-Id"))
	require.Equal(t, "u-b", b.Header.Get("X-User-Id"))
	require.NotEqual(t, a.Header.Get("Authorization"), b.Header.Get("Authorization"))
	require.NotEqual(t, a.Header.Get("X-Conversation-ID"), b.Header.Get("X-Conversation-ID"),
		"两个账号的兜底会话 ID 不得相同")
}

func TestWorkbuddyChatHeadersCN(t *testing.T) {
	t.Parallel()
	account := &Account{ID: 1, Platform: PlatformWorkbuddy}
	creds := WorkbuddyCredentials{
		AccessToken:  "tok-1",
		UID:          "u-1",
		EnterpriseID: "ent-1",
		Domain:       "copilot.tencent.com",
		DeviceToken:  "dev-1",
	}
	req := httptest.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	applyWorkbuddyChatHeaders(req, creds, account, "cn", workbuddyChatMeta{ConversationRequestID: "0123456789abcdef0123456789abcdef"})

	require.Equal(t, "application/json, text/event-stream", req.Header.Get("Accept"))
	require.Equal(t, "Bearer tok-1", req.Header.Get("Authorization"))
	require.Equal(t, "u-1", req.Header.Get("X-User-Id"))
	require.Equal(t, "ent-1", req.Header.Get("X-Enterprise-Id"))
	require.Equal(t, "copilot.tencent.com", req.Header.Get("X-Domain"))
	require.Empty(t, req.Header.Get("X-No-Enterprise-Id"))
	require.Empty(t, req.Header.Get("X-No-Department-Info"))

	// X-Product 对齐官方 bundle 值 SaaS（修复前是自造值 "WorkBuddy"）。
	require.Equal(t, "SaaS", req.Header.Get("X-Product"))
	// 官方渠道意图头：保留（本次一度误删）。
	require.Equal(t, "conversation", req.Header.Get("X-Agent-Purpose"),
		"X-Agent-Purpose 是官方头，必须保留")
	require.Equal(t, "craft", req.Header.Get("X-Agent-Intent"),
		"X-Agent-Intent 是官方头，必须保留")
	// 默认（usage_identity=false）不发 X-IDE-* 与 X-Request-ID。
	require.Empty(t, req.Header.Get("X-IDE-Name"), "默认关闭身份声明")
	require.Empty(t, req.Header.Get("X-IDE-Type"), "默认关闭身份声明")
	require.Empty(t, req.Header.Get("X-IDE-Version"), "默认关闭身份声明")
	// 设备风控头来自真实登录抓包，仍「有就用」。
	require.Equal(t, "dev-1", req.Header.Get("X-Device-Token"))
	// 安全红线：chat 请求绝不带 refresh token。
	require.Empty(t, req.Header.Get("X-Refresh-Token"))
	// 三件套与链路族不受开关影响（渠道闸判据）。
	require.NotEmpty(t, req.Header.Get("X-Conversation-ID"))
	require.NotEmpty(t, req.Header.Get("X-Conversation-Request-ID"))
	require.NotEmpty(t, req.Header.Get("X-Conversation-Message-ID"))
	require.Empty(t, req.Header.Get("X-Request-ID"), "X-Request-ID 属身份声明，默认不发")
}

func TestWorkbuddyChatHeadersUsageIdentityOn(t *testing.T) {
	t.Parallel()
	account := &Account{ID: 21, Platform: PlatformWorkbuddy}
	creds := WorkbuddyCredentials{AccessToken: "tok", UID: "u-21", UsageIdentity: true}
	req := httptest.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	applyWorkbuddyChatHeaders(req, creds, account, "cn", workbuddyChatMeta{})

	require.Equal(t, "WorkBuddy", req.Header.Get("X-IDE-Name"))
	// X-IDE-Type 取官方 bundle 逐字值 WorkBuddy（不取第三方 dsh 插件的 CLI 取舍）。
	require.Equal(t, "WorkBuddy", req.Header.Get("X-IDE-Type"))
	require.Equal(t, workbuddyClientVersion, req.Header.Get("X-IDE-Version"))
	require.NotEmpty(t, req.Header.Get("X-Request-ID"), "开启后用量页才有请求 id 读数")
	require.Equal(t, req.Header.Get("X-Conversation-Message-ID"), req.Header.Get("X-Request-ID"))
	require.Equal(t, "SaaS", req.Header.Get("X-Product"), "X-Product 与开关无关")
}

func TestWorkbuddyChatHeadersCNFallbacks(t *testing.T) {
	t.Parallel()
	account := &Account{ID: 9, Platform: PlatformWorkbuddy}
	req := httptest.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	applyWorkbuddyChatHeaders(req, WorkbuddyCredentials{}, account, "cn", workbuddyChatMeta{})

	require.Equal(t, "1", req.Header.Get("X-No-Authorization"))
	require.Equal(t, "1", req.Header.Get("X-No-User-Id"))
	require.Equal(t, "1", req.Header.Get("X-No-Enterprise-Id"))
	require.Equal(t, "1", req.Header.Get("X-No-Department-Info"))
	require.Empty(t, req.Header.Get("Authorization"))
	require.Empty(t, req.Header.Get("X-User-Id"))
	require.Empty(t, req.Header.Get("X-Device-Token"), "无 device token 时不注入该头")
}

func TestWorkbuddyChatHeadersGlobal(t *testing.T) {
	t.Parallel()
	account := &Account{ID: 2, Platform: PlatformWorkbuddy}
	creds := WorkbuddyCredentials{AccessToken: "tok-g", UID: "u-g", EnterpriseID: "ent-should-ignore"}
	req := httptest.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	applyWorkbuddyChatHeaders(req, creds, account, "global", workbuddyChatMeta{})

	require.Equal(t, workbuddyUAFor("global"), req.Header.Get("User-Agent"))
	require.Equal(t, "https://www.workbuddy.ai", req.Header.Get("Origin"))
	require.Equal(t, "https://www.workbuddy.ai/", req.Header.Get("Referer"))
	require.Equal(t, "en-US,en;q=0.9", req.Header.Get("Accept-Language"), "global 取浏览器形状而非裸 en-US")
	require.Equal(t, "1", req.Header.Get("X-No-Enterprise-Id"), "global 显式声明无企业")
	require.Equal(t, "www.workbuddy.ai", req.Header.Get("X-Domain"))
	require.Empty(t, req.Header.Get("X-Enterprise-Id"), "global 不携带企业 ID")
}

func TestWorkbuddyConversationHeaderFamily(t *testing.T) {
	t.Parallel()
	account := &Account{ID: 3, Platform: PlatformWorkbuddy}
	creds := WorkbuddyCredentials{AccessToken: "tok", UID: "u-3"}
	convReqID := strings.Repeat("ab", 16) // 32 hex
	req := httptest.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	applyWorkbuddyChatHeaders(req, creds, account, "cn", workbuddyChatMeta{
		ConversationID:        "conv-1",
		ConversationRequestID: convReqID,
		TraceID:               "trace-inbound",
	})

	require.Equal(t, "conv-1", req.Header.Get("X-Conversation-ID"))
	require.Equal(t, convReqID, req.Header.Get("X-Conversation-Request-ID"))
	require.Equal(t, convReqID, req.Header.Get("X-Root-Request-ID"))
	require.Equal(t, "trace-inbound", req.Header.Get("X-Trace-ID"))

	messageID := req.Header.Get("X-Conversation-Message-ID")
	require.True(t, workbuddyValidTraceID(messageID), "message ID 必须是 32 hex")
	require.Empty(t, req.Header.Get("X-Request-ID"), "默认关闭身份声明时不发 X-Request-ID")
	require.Equal(t, convReqID, req.Header.Get("X-B3-TraceId"))
	require.Equal(t, messageID[:16], req.Header.Get("X-B3-SpanId"))
	require.Equal(t, "1", req.Header.Get("X-B3-Sampled"))
}

func TestWorkbuddyConversationHeaderFamilyInvalidTraceFallback(t *testing.T) {
	t.Parallel()
	account := &Account{ID: 4, Platform: PlatformWorkbuddy}
	creds := WorkbuddyCredentials{AccessToken: "tok", UID: "u-4"}
	req := httptest.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	// 非法 B3 TraceId（含横线/超长）→ 回落 messageID（恒 32 hex）。
	applyWorkbuddyChatHeaders(req, creds, account, "cn", workbuddyChatMeta{
		ConversationRequestID: "not-a-hex-trace-id",
	})
	messageID := req.Header.Get("X-Conversation-Message-ID")
	require.Equal(t, messageID, req.Header.Get("X-B3-TraceId"))
	// X-Conversation-ID 无来源 → 仍恒发（国际版渠道校验按存在性拦截，缺头即 400/11128）。
	convID := req.Header.Get("X-Conversation-ID")
	require.NotEmpty(t, convID, "X-Conversation-ID 必须恒发")
	require.True(t, workbuddyValidTraceID(convID), "兜底会话 ID 必须是 32 hex")
	require.Equal(t, "not-a-hex-trace-id", req.Header.Get("X-Trace-ID"))
}

func TestResolveWorkbuddyChatMeta(t *testing.T) {
	t.Parallel()
	// 入站头透传优先。
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request.Header.Set("X-Conversation-Request-ID", "inbound-conv-req")
	c.Request.Header.Set("X-Trace-ID", "inbound-trace")
	c.Request.Header.Set("X-Conversation-ID", "inbound-conv")
	meta := resolveWorkbuddyChatMeta(c, []byte(`{"conversationId":"body-conv"}`))
	require.Equal(t, "inbound-conv-req", meta.ConversationRequestID)
	require.Equal(t, "inbound-trace", meta.TraceID)
	require.Equal(t, "inbound-conv", meta.ConversationID)

	// 无入站头 → 生成 32 hex 聚合主键；conversationID 从 body 提取。
	rec2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(rec2)
	c2.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	meta2 := resolveWorkbuddyChatMeta(c2, []byte(`{"conversationId":"body-conv"}`))
	require.True(t, workbuddyValidTraceID(meta2.ConversationRequestID), "生成的聚合主键必须是 32 hex")
	require.Equal(t, "body-conv", meta2.ConversationID)
	require.Empty(t, meta2.TraceID)

	// 无 gin 上下文也不 panic；无 body 会话字段时留空由注入层随机兜底。
	meta3 := resolveWorkbuddyChatMeta(nil, nil)
	require.True(t, workbuddyValidTraceID(meta3.ConversationRequestID))
	require.Empty(t, meta3.ConversationID,
		"派生层不再按内容折叠恒定 ID；兜底交给注入层的每请求随机值")
}

// TestResolveWorkbuddyChatMetaConversationIDFromBodyOnly body 带显式会话标识时透传原值。
func TestResolveWorkbuddyChatMetaConversationIDFromBodyOnly(t *testing.T) {
	t.Parallel()
	rec, _ := gin.CreateTestContext(httptest.NewRecorder())
	rec.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte(`{"model":"gpt-6-astra","conversation_id":"client-conv","messages":[{"role":"user","content":"hello there"}]}`)
	require.Equal(t, "client-conv", resolveWorkbuddyChatMeta(rec, body).ConversationID)
}

// TestWorkbuddyConversationIDNotDerivedFromContent 2026-11-01 规格翻转的核心用例：
// 同一会话的后续轮次**不得**再复用同一个 X-Conversation-ID。
//
// 修复前的形态（本用例反向护栏注入的就是它）：X-Conversation-ID =
// sha256(uid + "|" + 首条 user 文本)，于是「第 2 轮」与「第 1 轮」同值。
// 参考实现明确反对复用（"reusing one value across requests would forge a conversation
// identity the plugin does not actually have"），而渠道闸只查存在性，稳定值换不来
// 任何通过收益，却换来「一个会话身份被反复复用、跨号 failover 时同 cid 换票继续发」
// 这一池化特征。
func TestWorkbuddyConversationIDNotDerivedFromContent(t *testing.T) {
	t.Parallel()
	rec, _ := gin.CreateTestContext(httptest.NewRecorder())
	rec.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	first := []byte(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hello there"}]}`)
	followUp := []byte(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hello there"},{"role":"assistant","content":"hi"},{"role":"user","content":"and then?"}]}`)

	// 两者都没有显式标识 → resolve 返回空，注入层每请求随机。
	require.Empty(t, resolveWorkbuddyChatMeta(rec, first).ConversationID)
	require.Empty(t, resolveWorkbuddyChatMeta(rec, followUp).ConversationID)

	// 同一对话的两轮，注入后的头值必须不同（旧实现这里必然相等）。
	idOf := func(body []byte) string {
		req := httptest.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
		applyWorkbuddyChatHeaders(req, WorkbuddyCredentials{AccessToken: "t", UID: "u-intl"},
			&Account{ID: 31, Platform: PlatformWorkbuddy}, "global", resolveWorkbuddyChatMeta(rec, body))
		return req.Header.Get("X-Conversation-ID")
	}
	id1, id2 := idOf(first), idOf(followUp)
	require.True(t, workbuddyValidTraceID(id1))
	require.NotEqual(t, id1, id2, "同一会话的后续轮次不得复用同一会话 ID（旧形态正是本次剔除项）")
	// 不同首句当然也不同。
	other := []byte(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"completely different"}]}`)
	require.NotEqual(t, id1, idOf(other))
}

// TestWorkbuddyFirstUserText 内容提取边界（现仅供诊断，不得再用作身份值）。
func TestWorkbuddyFirstUserText(t *testing.T) {
	t.Parallel()
	require.Equal(t, "hi",
		workbuddyFirstUserText([]byte(`{"messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`)))
	require.Equal(t, "ab",
		workbuddyFirstUserText([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"b"}]}]}`)))
	require.Equal(t, "", workbuddyFirstUserText([]byte(`{"messages":[{"role":"system","content":"s"}]}`)))
	require.Equal(t, "", workbuddyFirstUserText(nil))
	require.Equal(t, "", workbuddyFirstUserText([]byte(`not json`)))
}

// TestWorkbuddyConversationHeadersNeverDropsID 直接构造 meta（绕过 resolve）时
// 头族注入层仍兜底，防止未来新增调用点又把头丢掉——11128 那次故障的确切形态。
func TestWorkbuddyConversationHeadersNeverDropsID(t *testing.T) {
	t.Parallel()
	account := &Account{ID: 11, Platform: PlatformWorkbuddy}
	req := httptest.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	applyWorkbuddyChatHeaders(req, WorkbuddyCredentials{AccessToken: "t", UID: "u-11"}, account, "global", workbuddyChatMeta{})

	convID := req.Header.Get("X-Conversation-ID")
	require.NotEmpty(t, convID, "即使 meta 绕过 resolve 构造，三件套也不得缺项")
	require.NotEmpty(t, req.Header.Get("X-Conversation-Request-ID"))
	require.NotEmpty(t, req.Header.Get("X-Conversation-Message-ID"))
	require.True(t, workbuddyValidTraceID(convID))
}

// TestWorkbuddyConversationIDFallbackNotConstant 兜底不得串起整个账号池。
//
// 为什么单独一条：上一版兜底是 sha256("|") 编译期常量——一旦触发，所有账号发出同一个
// X-Conversation-ID。只断言 NotEmpty 的用例对这种缺陷全绿（假绿），故必须直接断言
// 「多次独立兜底彼此不同」。
func TestWorkbuddyConversationIDFallbackNotConstant(t *testing.T) {
	t.Parallel()
	account := &Account{ID: 12, Platform: PlatformWorkbuddy}
	seen := make(map[string]struct{}, 8)
	for i := 0; i < 8; i++ {
		req := httptest.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
		applyWorkbuddyChatHeaders(req, WorkbuddyCredentials{AccessToken: "t"}, account, "global", workbuddyChatMeta{})
		cid := req.Header.Get("X-Conversation-ID")
		require.NotEmpty(t, cid)
		require.True(t, workbuddyValidTraceID(cid), "兜底值必须是 32 hex")
		seen[cid] = struct{}{}
	}
	require.Len(t, seen, 8, "8 次独立兜底必须得到 8 个不同 ID（跨请求/跨账号隔离）")
}

func TestWorkbuddyResolveConversationID(t *testing.T) {
	t.Parallel()
	require.Equal(t, "c1", workbuddyResolveConversationID([]byte(`{"conversation_id":"c1"}`)))
	require.Equal(t, "c2", workbuddyResolveConversationID([]byte(`{"conversationId":"c2"}`)))
	require.Equal(t, "c3", workbuddyResolveConversationID([]byte(`{"metadata":{"conversation_id":"c3"},"conversationId":"c4"}`)))
	require.Equal(t, "c5", workbuddyResolveConversationID([]byte(`{"metadata":{"conversationId":"c5"}}`)))
	require.Equal(t, "", workbuddyResolveConversationID([]byte(`{"model":"glm"}`)))
	require.Equal(t, "", workbuddyResolveConversationID(nil))
	require.Equal(t, "", workbuddyResolveConversationID([]byte(`not json`)))
}

func TestWorkbuddyRefreshHeaders(t *testing.T) {
	t.Parallel()
	account := &Account{ID: 5, Platform: PlatformWorkbuddy}
	creds := WorkbuddyCredentials{RefreshToken: "rt-1", UID: "u-5", EnterpriseID: "ent-5"}
	req := httptest.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/plugin/auth/token/refresh", nil)
	applyWorkbuddyRefreshHeaders(req, creds, account, "cn")

	require.Equal(t, "rt-1", req.Header.Get("X-Refresh-Token"))
	// 渠道标识改为参考实现 refreshHeaders 的 "workbuddy"（此前是 "plugin"，字面自报
	// 插件渠道，正是渠道校验最想看的读数）。
	require.Equal(t, "workbuddy", req.Header.Get("X-Auth-Refresh-Source"))
	require.Equal(t, "ent-5", req.Header.Get("X-Enterprise-Id"))
	require.Equal(t, workbuddyUAFor("cn"), req.Header.Get("User-Agent"))
	require.Equal(t, "1", req.Header.Get("X-CodeBuddy-Request"), "官方头，保留")
}

func TestWorkbuddyNewMessageIDShape(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		id := workbuddyNewMessageID()
		require.True(t, workbuddyValidTraceID(id), "32 hex 形态")
		require.False(t, seen[id], "不重复")
		seen[id] = true
	}
	require.False(t, workbuddyValidTraceID("short"))
	require.False(t, workbuddyValidTraceID(strings.Repeat("z", 32)))
}

// TestWorkbuddyCredentialsIdentityFlags 两个账号级开关的读取宽容性。
// 关键边界：JSON 原生 bool 必须能读到（GetCredential 会把 bool 归到 default 返回空串），
// 否则前端写 true 也等于没开。
func TestWorkbuddyCredentialsIdentityFlags(t *testing.T) {
	t.Parallel()
	build := func(creds map[string]any) *Account {
		return &Account{ID: 1, Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey, Credentials: creds}
	}
	require.False(t, build(map[string]any{}).GetWorkbuddyCredentials().UsageIdentity, "未设置即默认关")
	require.False(t, build(map[string]any{}).GetWorkbuddyCredentials().SanitizeFingerprints, "未设置即默认关")

	require.True(t, build(map[string]any{"usage_identity": true}).GetWorkbuddyCredentials().UsageIdentity,
		"JSON 原生 bool 必须生效")
	require.True(t, build(map[string]any{"usageIdentity": true}).GetWorkbuddyCredentials().UsageIdentity,
		"camelCase 别名（粘贴上游配置）必须生效")
	require.True(t, build(map[string]any{"usage_identity": "true"}).GetWorkbuddyCredentials().UsageIdentity,
		"字符串形态必须生效")
	require.False(t, build(map[string]any{"usage_identity": "false"}).GetWorkbuddyCredentials().UsageIdentity)

	require.True(t, build(map[string]any{"sanitize_fingerprints": true}).GetWorkbuddyCredentials().SanitizeFingerprints)
	require.True(t, build(map[string]any{"sanitizeFingerprints": "1"}).GetWorkbuddyCredentials().SanitizeFingerprints)
	// 两键互不干扰。
	only := build(map[string]any{"sanitize_fingerprints": true}).GetWorkbuddyCredentials()
	require.True(t, only.SanitizeFingerprints)
	require.False(t, only.UsageIdentity)
}

// headerValues 收集请求头全部值（用于跨头扫描盐串，不含 Authorization 之外的筛选）。
func headerValues(req *http.Request) []string {
	out := make([]string, 0, len(req.Header))
	for _, vals := range req.Header {
		out = append(out, vals...)
	}
	return out
}
