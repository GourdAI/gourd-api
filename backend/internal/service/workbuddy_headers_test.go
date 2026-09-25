//go:build unit

package service

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// workbuddy_headers_test.go WorkBuddy 出站请求头构造的单测：
// UA 三段式、关键风控头、X-No-* 兜底、机器/会话标识稳定性、会话头族。

func workbuddyTestMachineID(uid string) string {
	sum := sha256.Sum256([]byte("wb2a:machine:" + uid))
	return hex.EncodeToString(sum[:18])
}

func workbuddyTestSessionID(uid string) string {
	sum := sha256.Sum256([]byte("wb2a:session:" + uid))
	return hex.EncodeToString(sum[:18])
}

func TestWorkbuddyUAForRealm(t *testing.T) {
	t.Parallel()
	require.Equal(t, "WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1", workbuddyUAFor("cn"))
	require.Equal(t, "WorkBuddy/5.5.4 WorkBuddy AI/5.5.4 CLI/2.137.1", workbuddyUAFor("global"))
	require.Equal(t, workbuddyUAFor("cn"), workbuddyUAFor(""), "空 realm 按 CN 处理")
}

func TestWorkbuddyCommonHeadersStableIDs(t *testing.T) {
	t.Parallel()
	account := &Account{ID: 42, Platform: PlatformWorkbuddy}
	creds := WorkbuddyCredentials{UID: "user-abc"}
	req := httptest.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	applyWorkbuddyCommonHeaders(req, creds, account, "cn")

	require.Equal(t, "application/json", req.Header.Get("Content-Type"))
	require.Equal(t, "application/json", req.Header.Get("Accept"))
	require.Equal(t, "XMLHttpRequest", req.Header.Get("X-Requested-With"))
	require.Equal(t, "https://www.codebuddy.cn", req.Header.Get("Origin"))
	require.Equal(t, "https://www.codebuddy.cn/", req.Header.Get("Referer"))
	require.Equal(t, workbuddyUAFor("cn"), req.Header.Get("User-Agent"))
	require.Equal(t, "1", req.Header.Get("X-CodeBuddy-Request"))
	require.Equal(t, "zh-CN", req.Header.Get("Accept-Language"))
	require.Equal(t, workbuddyTestMachineID("user-abc"), req.Header.Get("X-Machine-ID"))
	require.Equal(t, workbuddyTestSessionID("user-abc"), req.Header.Get("X-Session-ID"))

	// 稳定性：同 uid 两次构造恒同值（幂等）。
	req2 := httptest.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	applyWorkbuddyCommonHeaders(req2, creds, account, "cn")
	require.Equal(t, req.Header.Get("X-Machine-ID"), req2.Header.Get("X-Machine-ID"))
	require.Equal(t, req.Header.Get("X-Session-ID"), req2.Header.Get("X-Session-ID"))
}

func TestWorkbuddyCommonHeadersUIDFallbackStable(t *testing.T) {
	t.Parallel()
	account := &Account{ID: 7, Platform: PlatformWorkbuddy}
	req := httptest.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	applyWorkbuddyCommonHeaders(req, WorkbuddyCredentials{}, account, "cn")
	// uid 为空 → 回落 "acct-<id>"，机器/会话标识保持稳定。
	require.Equal(t, workbuddyTestMachineID("acct-7"), req.Header.Get("X-Machine-ID"))
	require.Equal(t, workbuddyTestSessionID("acct-7"), req.Header.Get("X-Session-ID"))
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
	// 用量归属头（官方桌面端指纹）。
	require.Equal(t, "conversation", req.Header.Get("X-Agent-Purpose"))
	require.Equal(t, "WorkBuddy", req.Header.Get("X-IDE-Name"))
	require.Equal(t, "WorkBuddy", req.Header.Get("X-IDE-Type"))
	require.Equal(t, workbuddyClientVersion, req.Header.Get("X-IDE-Version"))
	require.Equal(t, "WorkBuddy", req.Header.Get("X-Product"))
	// 设备风控头。
	require.Equal(t, "dev-1", req.Header.Get("X-Device-Token"))
	// 安全红线：chat 请求绝不带 refresh token。
	require.Empty(t, req.Header.Get("X-Refresh-Token"))
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
	require.Equal(t, "en-US", req.Header.Get("Accept-Language"))
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
	require.Equal(t, messageID, req.Header.Get("X-Request-ID"))
	require.True(t, workbuddyValidTraceID(messageID), "message ID 必须是 32 hex")
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
	// X-Conversation-ID 无来源 → 不发（透传优先，不伪造）。
	require.Empty(t, req.Header.Get("X-Conversation-ID"))
	// X-Trace-ID 回落 conversationRequestID。
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

	// 无 gin 上下文也不 panic。
	meta3 := resolveWorkbuddyChatMeta(nil, nil)
	require.True(t, workbuddyValidTraceID(meta3.ConversationRequestID))
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
	require.Equal(t, "plugin", req.Header.Get("X-Auth-Refresh-Source"))
	require.Equal(t, "ent-5", req.Header.Get("X-Enterprise-Id"))
	require.Equal(t, workbuddyUAFor("cn"), req.Header.Get("User-Agent"))
	require.Equal(t, "1", req.Header.Get("X-CodeBuddy-Request"))
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
