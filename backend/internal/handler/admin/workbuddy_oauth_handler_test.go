//go:build unit

package admin

// workbuddy_oauth_handler_test.go WorkBuddy 设备授权 handler 的单测：
// realm/state 入参校验、响应信封形状（response.Success 包装）、
// pending/completed 两种轮询结果透传，以及非法 realm 的 400 语义。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newWorkBuddyOAuthTestHandler 构造指向 httptest 假上游的 handler。
func newWorkBuddyOAuthTestHandler(serverURL string) *WorkBuddyOAuthHandler {
	return NewWorkBuddyOAuthHandler(service.NewWorkBuddyOAuthServiceWithBaseURL(nil, serverURL))
}

// postWorkBuddyOAuthJSON 向 handler 发起 POST JSON 请求并返回响应记录器。
func postWorkBuddyOAuthJSON(t *testing.T, h *WorkBuddyOAuthHandler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	switch path {
	case "/oauth/auth-url":
		h.GenerateAuthURL(c)
	case "/oauth/exchange-code":
		h.ExchangeCode(c)
	default:
		t.Fatalf("unsupported test path %q", path)
	}
	return rec
}

// decodeWorkBuddyOAuthEnvelope 解出 {code,message,data} 响应信封。
func decodeWorkBuddyOAuthEnvelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	return envelope
}

// TestWorkBuddyOAuthHandlerGenerateAuthURLReturnsEnvelope 覆盖成功路径：
// 200 + code=0 信封，data 含 auth_url/state/realm，且 realm 为归一后的值。
func TestWorkBuddyOAuthHandlerGenerateAuthURLReturnsEnvelope(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v2/plugin/auth/state", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"state": "s-1", "authUrl": "https://example.com/go"},
		})
	}))
	defer server.Close()

	h := newWorkBuddyOAuthTestHandler(server.URL)
	rec := postWorkBuddyOAuthJSON(t, h, "/oauth/auth-url", `{"realm":"global"}`)
	require.Equal(t, http.StatusOK, rec.Code)

	envelope := decodeWorkBuddyOAuthEnvelope(t, rec)
	require.EqualValues(t, 0, envelope["code"])
	data, ok := envelope["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "https://example.com/go", data["auth_url"])
	require.Equal(t, "s-1", data["state"])
	require.Equal(t, "global", data["realm"])
}

// TestWorkBuddyOAuthHandlerGenerateAuthURLRejectsInvalidRealm 覆盖 realm 校验：
// 非法值必须 400（静默回落 cn 会让用户误以为登录了 global）。
// 空值等价缺省（cn），走成功路径。
func TestWorkBuddyOAuthHandlerGenerateAuthURLRejectsInvalidRealm(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"state": "s-1", "authUrl": "https://example.com/go"},
		})
	}))
	defer server.Close()
	h := newWorkBuddyOAuthTestHandler(server.URL)

	rec := postWorkBuddyOAuthJSON(t, h, "/oauth/auth-url", `{"realm":"us"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "invalid realm")

	// 空 realm → 缺省 cn，成功且回显 cn。
	rec = postWorkBuddyOAuthJSON(t, h, "/oauth/auth-url", `{"realm":""}`)
	require.Equal(t, http.StatusOK, rec.Code)
	data := decodeWorkBuddyOAuthEnvelope(t, rec)["data"].(map[string]any)
	require.Equal(t, "cn", data["realm"])
}

// TestWorkBuddyOAuthHandlerGenerateAuthURLRejectsMalformedBody 覆盖请求体非法 JSON 的 400。
func TestWorkBuddyOAuthHandlerGenerateAuthURLRejectsMalformedBody(t *testing.T) {
	t.Parallel()
	h := newWorkBuddyOAuthTestHandler("http://127.0.0.1:1")
	rec := postWorkBuddyOAuthJSON(t, h, "/oauth/auth-url", `{"realm":`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestWorkBuddyOAuthHandlerExchangeCodeRequiresState 覆盖 state 必填校验（400）。
func TestWorkBuddyOAuthHandlerExchangeCodeRequiresState(t *testing.T) {
	t.Parallel()
	h := newWorkBuddyOAuthTestHandler("http://127.0.0.1:1")
	for _, body := range []string{`{}`, `{"state":""}`, `{"state":"   "}`} {
		rec := postWorkBuddyOAuthJSON(t, h, "/oauth/exchange-code", body)
		require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", body)
		require.Contains(t, rec.Body.String(), "state is required")
	}
}

// TestWorkBuddyOAuthHandlerExchangeCodeRejectsInvalidRealm 覆盖轮询路径的 realm 校验。
func TestWorkBuddyOAuthHandlerExchangeCodeRejectsInvalidRealm(t *testing.T) {
	t.Parallel()
	h := newWorkBuddyOAuthTestHandler("http://127.0.0.1:1")
	rec := postWorkBuddyOAuthJSON(t, h, "/oauth/exchange-code", `{"state":"s-1","realm":"globalx"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "invalid realm")
}

// TestWorkBuddyOAuthHandlerExchangeCodePendingThenCompleted 覆盖轮询两态：
// 未完成时 status=pending 且无 token 字段；完成后 status=completed 且 token 全字段可见。
func TestWorkBuddyOAuthHandlerExchangeCodePendingThenCompleted(t *testing.T) {
	t.Parallel()
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token":
			calls++
			if calls == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 10001, "msg": "login ing"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"accessToken": "at-1", "refreshToken": "rt-1", "expiresIn": 120, "domain": "www.workbuddy.ai"},
			})
		case "/v2/plugin/login/account":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"uid": "u-1", "enterpriseId": "e-1", "nickname": "n-1"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	h := newWorkBuddyOAuthTestHandler(server.URL)

	rec := postWorkBuddyOAuthJSON(t, h, "/oauth/exchange-code", `{"state":"s-1","realm":"global"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	pending := decodeWorkBuddyOAuthEnvelope(t, rec)["data"].(map[string]any)
	require.Equal(t, "pending", pending["status"])
	// omitempty：pending 不应出现 token 键（前端据此避免误回填空凭据）。
	require.NotContains(t, pending, "token")

	rec = postWorkBuddyOAuthJSON(t, h, "/oauth/exchange-code", `{"state":"s-1","realm":"global"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	completed := decodeWorkBuddyOAuthEnvelope(t, rec)["data"].(map[string]any)
	require.Equal(t, "completed", completed["status"])
	token, ok := completed["token"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "at-1", token["access_token"])
	require.Equal(t, "rt-1", token["refresh_token"])
	require.Equal(t, "www.workbuddy.ai", token["domain"])
	require.Equal(t, "global", token["realm"])
	require.Equal(t, "u-1", token["uid"])
	require.Equal(t, "e-1", token["enterprise_id"])
	require.Equal(t, "n-1", token["nickname"])
	require.NotZero(t, token["expires_at"])
}

// TestWorkBuddyOAuthHandlerExchangeCodePropagatesUpstreamError 覆盖上游硬错误透传：
// 5xx 映射为非 2xx 响应（response.ErrorFrom），body 携带 reason。
func TestWorkBuddyOAuthHandlerExchangeCodePropagatesUpstreamError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
	}))
	defer server.Close()
	h := newWorkBuddyOAuthTestHandler(server.URL)

	rec := postWorkBuddyOAuthJSON(t, h, "/oauth/exchange-code", `{"state":"s-1","realm":"cn"}`)
	require.GreaterOrEqual(t, rec.Code, 500)
	require.Contains(t, rec.Body.String(), "WORKBUDDY_OAUTH_TOKEN_HTTP_ERROR")
}

// TestNormalizeWorkbuddyRealmRequest 覆盖 handler 层 realm 校验分支。
func TestNormalizeWorkbuddyRealmRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input string
		want  string
		ok    bool
	}{
		{input: "", want: "cn", ok: true},
		{input: "cn", want: "cn", ok: true},
		{input: " CN ", want: "cn", ok: true},
		{input: "global", want: "global", ok: true},
		{input: "GLOBAL", want: "global", ok: true},
		{input: "us", ok: false},
		{input: "globalx", ok: false},
	} {
		got, err := normalizeWorkbuddyRealmRequest(tc.input)
		if !tc.ok {
			require.Error(t, err, "input=%q", tc.input)
			continue
		}
		require.NoError(t, err, "input=%q", tc.input)
		require.Equal(t, tc.want, got, "input=%q", tc.input)
	}
}
