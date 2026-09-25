//go:build unit

package service

// workbuddy_oauth_service_test.go WorkBuddy 设备授权服务的单测（httptest 假上游）：
// auth/state 端点路径与请求头断言、双域 Origin/UA、pending/completed 轮询语义、
// expires_at 计算、login/account best-effort 补全、上游 5xx 硬错误与 realm 归一。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// workbuddyOAuthUpstream 记录设备流假上游收到的请求，供路径/查询参数/请求头断言。
type workbuddyOAuthUpstream struct {
	mu       sync.Mutex
	requests []*http.Request
	queries  []string
}

func (u *workbuddyOAuthUpstream) record(r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.requests = append(u.requests, r.Clone(context.Background()))
	u.queries = append(u.queries, r.URL.RawQuery)
}

func (u *workbuddyOAuthUpstream) snapshot() ([]*http.Request, []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]*http.Request(nil), u.requests...), append([]string(nil), u.queries...)
}

// newWorkBuddyOAuthTestService 构造指向 httptest 假上游的服务实例
// （baseURLOverride 仅供测试注入，生产恒为官方域）。
func newWorkBuddyOAuthTestService(serverURL string) *WorkBuddyOAuthService {
	return NewWorkBuddyOAuthServiceWithBaseURL(nil, serverURL)
}

// TestWorkBuddyOAuthGenerateAuthURLUsesCLIPlatformAndRealmHeaders 覆盖生成授权 URL 的
// 请求形状（路径、platform=CLI 查询参数、空 JSON body）与 CN/global 双域请求头，
// 并断言 state/authUrl 解析结果与 realm 回显。
func TestWorkBuddyOAuthGenerateAuthURLUsesCLIPlatformAndRealmHeaders(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		realm        string
		wantOrigin   string
		wantBasePath string
	}{
		{name: "cn realm", realm: "cn", wantOrigin: "https://www.codebuddy.cn"},
		{name: "global realm", realm: "global", wantOrigin: "https://www.workbuddy.ai"},
		// 空/非法 realm 回落 cn（对齐 GetWorkbuddyRealm 缺省语义）。
		{name: "empty realm falls back to cn", realm: "", wantOrigin: "https://www.codebuddy.cn"},
		{name: "invalid realm falls back to cn", realm: "GLOBALX", wantOrigin: "https://www.codebuddy.cn"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			upstream := &workbuddyOAuthUpstream{}
			var bodies []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstream.record(r)
				raw, _ := io.ReadAll(r.Body)
				bodies = append(bodies, string(raw))
				if r.URL.Path != "/v2/plugin/auth/state" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code": 0,
					"msg":  "ok",
					"data": map[string]any{
						"state":   "state-" + tc.name,
						"authUrl": "https://example.com/login?state=" + tc.name,
					},
				})
			}))
			defer server.Close()

			svc := newWorkBuddyOAuthTestService(server.URL)
			result, err := svc.GenerateAuthURL(context.Background(), tc.realm, nil)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, "state-"+tc.name, result.State)
			require.Equal(t, "https://example.com/login?state="+tc.name, result.AuthURL)
			// realm 回显是归一后的值：空/非法必须回落 cn，前端据此显示当前登录域。
			wantRealm := "cn"
			if tc.realm == "global" {
				wantRealm = "global"
			}
			require.Equal(t, wantRealm, result.Realm)

			requests, queries := upstream.snapshot()
			require.Len(t, requests, 1)
			req := requests[0]
			require.Equal(t, http.MethodPost, req.Method)
			require.Equal(t, "/v2/plugin/auth/state", req.URL.Path)
			// platform=CLI 是设备流的固定契约（上游据此走 CLI 登录分支）。
			require.Equal(t, "platform=CLI", queries[0])
			require.Equal(t, []string{"{}"}, bodies)
			require.Equal(t, tc.wantOrigin, req.Header.Get("Origin"))
			require.Equal(t, tc.wantOrigin+"/", req.Header.Get("Referer"))
			require.Equal(t, "CLI/2.63.2 CodeBuddy/2.63.2", req.Header.Get("User-Agent"))
			require.Equal(t, "application/json", req.Header.Get("Content-Type"))
			require.Equal(t, "application/json, text/plain, */*", req.Header.Get("Accept"))
			require.Equal(t, "XMLHttpRequest", req.Header.Get("X-Requested-With"))
		})
	}
}

// TestWorkBuddyOAuthGenerateAuthURLRejectsInvalidUpstreamState 覆盖 state 端点的失败语义：
// 业务 code != 0、缺 state/authUrl、HTTP 5xx 都是硬错误（没有 state 就无法继续轮询）。
func TestWorkBuddyOAuthGenerateAuthURLRejectsInvalidUpstreamState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "business code non-zero", handler: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 10001, "msg": "denied"})
		}},
		{name: "missing state and authUrl", handler: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
		}},
		{name: "http 500", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			svc := newWorkBuddyOAuthTestService(server.URL)
			result, err := svc.GenerateAuthURL(context.Background(), "cn", nil)
			require.Error(t, err)
			require.Nil(t, result)
		})
	}
}

// TestWorkBuddyOAuthExchangeStatePendingOnBusinessCode 覆盖「登录未完成」的 pending 语义：
// 上游用非 0 code（实测 msg 形如 "login ing"）或空 accessToken 表达未完成，
// 服务必须返回 status=pending 且 err=nil（前端据此继续轮询，报错会中断轮询体验）。
func TestWorkBuddyOAuthExchangeStatePendingOnBusinessCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "code non-zero with message", handler: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 10001, "msg": "login ing"})
		}},
		{name: "code zero with empty accessToken", handler: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"accessToken": ""}})
		}},
		{name: "http 400 with business code", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 10001, "msg": "login ing"})
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			svc := newWorkBuddyOAuthTestService(server.URL)
			result, err := svc.ExchangeState(context.Background(), "state-1", "cn", nil)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, WorkBuddyOAuthStatusPending, result.Status)
			require.Nil(t, result.Token)
		})
	}
}

// TestWorkBuddyOAuthExchangeStateCompletedParsesTokenAndAccount 覆盖完成态：
// token 端点返回全字段，login/account 补全 uid/enterpriseId/nickname，
// expires_at = now + expiresIn（± 容差），并断言 account 请求带 Bearer 头。
func TestWorkBuddyOAuthExchangeStateCompletedParsesTokenAndAccount(t *testing.T) {
	t.Parallel()
	upstream := &workbuddyOAuthUpstream{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.record(r)
		switch r.URL.Path {
		case "/v2/plugin/auth/token":
			require.Equal(t, "state-abc", r.URL.Query().Get("state"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"accessToken":  "at-123",
					"refreshToken": "rt-456",
					"expiresIn":    3600,
					"domain":       "www.workbuddy.ai",
				},
			})
		case "/v2/plugin/login/account":
			require.Equal(t, "state-abc", r.URL.Query().Get("state"))
			require.Equal(t, "Bearer at-123", r.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"uid":          "uid-9",
					"enterpriseId": "ent-7",
					"nickname":     "昵称",
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	svc := newWorkBuddyOAuthTestService(server.URL)
	before := time.Now().Unix()
	result, err := svc.ExchangeState(context.Background(), "state-abc", "global", nil)
	after := time.Now().Unix()
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, WorkBuddyOAuthStatusCompleted, result.Status)
	require.NotNil(t, result.Token)
	token := result.Token
	require.Equal(t, "at-123", token.AccessToken)
	require.Equal(t, "rt-456", token.RefreshToken)
	require.Equal(t, "www.workbuddy.ai", token.Domain)
	require.Equal(t, "global", token.Realm)
	require.Equal(t, "uid-9", token.UID)
	require.Equal(t, "ent-7", token.EnterpriseID)
	require.Equal(t, "昵称", token.Nickname)
	require.GreaterOrEqual(t, token.ExpiresAt, before+3600)
	require.LessOrEqual(t, token.ExpiresAt, after+3600)

	requests, _ := upstream.snapshot()
	require.Len(t, requests, 2)
	require.Equal(t, "/v2/plugin/auth/token", requests[0].URL.Path)
	require.Equal(t, "/v2/plugin/login/account", requests[1].URL.Path)
	// global realm 的 Origin 与 base 同域。
	require.Equal(t, "https://www.workbuddy.ai", requests[0].Header.Get("Origin"))
}

// TestWorkBuddyOAuthExchangeStateToleratesAccountLookupFailure 覆盖 login/account 的
// best-effort 语义：展示信息拉取失败（5xx / 业务错误）不得阻断凭据回填。
func TestWorkBuddyOAuthExchangeStateToleratesAccountLookupFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"accessToken": "at-only", "refreshToken": "rt-only", "expiresIn": 60},
			})
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("account endpoint down"))
		}
	}))
	defer server.Close()

	svc := newWorkBuddyOAuthTestService(server.URL)
	result, err := svc.ExchangeState(context.Background(), "state-1", "cn", nil)
	require.NoError(t, err)
	require.Equal(t, WorkBuddyOAuthStatusCompleted, result.Status)
	require.Equal(t, "at-only", result.Token.AccessToken)
	require.Empty(t, result.Token.UID)
	require.Empty(t, result.Token.EnterpriseID)
	require.Empty(t, result.Token.Nickname)
}

// TestWorkBuddyOAuthExchangeStateOmitsExpiresAtOnInvalidExpiresIn 覆盖 expiresIn 缺失/
// 异常时 expires_at 保留 0（表示无过期信息），避免脏数据让 token 被判成过期。
func TestWorkBuddyOAuthExchangeStateOmitsExpiresAtOnInvalidExpiresIn(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		expiresIn any
	}{
		{name: "missing expiresIn", expiresIn: nil},
		{name: "zero expiresIn", expiresIn: 0},
		{name: "negative expiresIn", expiresIn: -100},
		{name: "absurd expiresIn", expiresIn: int64(20 * 365 * 24 * 3600)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := map[string]any{"accessToken": "at-x", "refreshToken": "rt-x"}
			if tc.expiresIn != nil {
				data["expiresIn"] = tc.expiresIn
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/plugin/auth/token" {
					_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
			}))
			defer server.Close()

			svc := newWorkBuddyOAuthTestService(server.URL)
			result, err := svc.ExchangeState(context.Background(), "state-1", "cn", nil)
			require.NoError(t, err)
			require.Equal(t, WorkBuddyOAuthStatusCompleted, result.Status)
			require.Zero(t, result.Token.ExpiresAt)
		})
	}
}

// TestWorkBuddyOAuthExchangeStateErrorsOnUpstreamFailure 覆盖硬错误：上游 5xx、
// 响应体非 JSON、传输层不可达都必须报错（继续轮询无意义，由前端提示失败）。
func TestWorkBuddyOAuthExchangeStateErrorsOnUpstreamFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "http 500", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
		}},
		{name: "non-json body", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>not json</html>"))
		}},
		{name: "http 400 without business code", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"msg":"bad request"}`))
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			svc := newWorkBuddyOAuthTestService(server.URL)
			result, err := svc.ExchangeState(context.Background(), "state-1", "cn", nil)
			require.Error(t, err)
			require.Nil(t, result)
		})
	}
}

// TestWorkBuddyOAuthExchangeStateRequiresState 覆盖空 state 的 BadRequest 校验。
func TestWorkBuddyOAuthExchangeStateRequiresState(t *testing.T) {
	t.Parallel()
	svc := newWorkBuddyOAuthTestService("http://127.0.0.1:1")
	for _, state := range []string{"", "   "} {
		result, err := svc.ExchangeState(context.Background(), state, "cn", nil)
		require.Error(t, err)
		require.Nil(t, result)
		require.Equal(t, http.StatusBadRequest, infraerrorsCodeOf(err))
	}
}

// TestWorkBuddyOAuthProxyLookup 覆盖代理解析：仓库缺失/未找到 → 400，查询失败 → 503，
// nil proxyID → 直连（空 URL 不报错）。
func TestWorkBuddyOAuthProxyLookup(t *testing.T) {
	t.Parallel()
	t.Run("nil proxy id returns direct connection", func(t *testing.T) {
		t.Parallel()
		svc := &WorkBuddyOAuthService{}
		proxyURL, err := svc.workbuddyOAuthProxyURL(context.Background(), nil)
		require.NoError(t, err)
		require.Empty(t, proxyURL)
	})

	t.Run("missing proxy repository", func(t *testing.T) {
		t.Parallel()
		svc := &WorkBuddyOAuthService{}
		id := int64(1)
		_, err := svc.workbuddyOAuthProxyURL(context.Background(), &id)
		require.Error(t, err)
		require.Equal(t, http.StatusBadRequest, infraerrorsCodeOf(err))
	})

	t.Run("proxy not found", func(t *testing.T) {
		t.Parallel()
		svc := &WorkBuddyOAuthService{proxyRepo: &workbuddyOAuthProxyRepoStub{err: ErrProxyNotFound}}
		id := int64(2)
		_, err := svc.workbuddyOAuthProxyURL(context.Background(), &id)
		require.Error(t, err)
		require.Equal(t, http.StatusBadRequest, infraerrorsCodeOf(err))
	})

	t.Run("proxy lookup failure is service unavailable", func(t *testing.T) {
		t.Parallel()
		svc := &WorkBuddyOAuthService{proxyRepo: &workbuddyOAuthProxyRepoStub{err: errWorkbuddyOAuthTestDBDown}}
		id := int64(3)
		_, err := svc.workbuddyOAuthProxyURL(context.Background(), &id)
		require.Error(t, err)
		require.Equal(t, http.StatusServiceUnavailable, infraerrorsCodeOf(err))
	})

	t.Run("resolved proxy url is used", func(t *testing.T) {
		t.Parallel()
		svc := &WorkBuddyOAuthService{proxyRepo: &workbuddyOAuthProxyRepoStub{proxy: &Proxy{
			Protocol: "http", Host: "127.0.0.1", Port: 8888,
		}}}
		id := int64(4)
		proxyURL, err := svc.workbuddyOAuthProxyURL(context.Background(), &id)
		require.NoError(t, err)
		require.Equal(t, "http://127.0.0.1:8888", proxyURL)
	})
}

// TestWorkBuddyOAuthNormalizeRealm 覆盖 realm 归一（大小写/空白容忍，其余回落 cn）。
func TestWorkBuddyOAuthNormalizeRealm(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":          "cn",
		"cn":        "cn",
		"CN":        "cn",
		" cn ":      "cn",
		"global":    "global",
		"Global":    "global",
		" global ":  "global",
		"GLOBAL":    "global",
		"us":        "cn",
		"globalx":   "cn",
		"cn-global": "cn",
	}
	for input, want := range cases {
		require.Equal(t, want, normalizeWorkbuddyOAuthRealm(input), "realm=%q", input)
	}
}

// TestWorkBuddyOAuthUpstreamBaseAndOriginPerRealm 断言官方双域取值（不依赖 httptest），
// 防止后续改动把 CN 的 Origin 误改成 workbuddy.ai 等回归。
func TestWorkBuddyOAuthUpstreamBaseAndOriginPerRealm(t *testing.T) {
	t.Parallel()
	base, origin := workbuddyOAuthEndpoints("cn")
	require.Equal(t, DefaultWorkbuddyBaseURL, base)
	require.Equal(t, "https://www.codebuddy.cn", origin)
	base, origin = workbuddyOAuthEndpoints("global")
	require.Equal(t, DefaultWorkbuddyGlobalBaseURL, base)
	require.Equal(t, "https://www.workbuddy.ai", origin)
}

// TestWorkBuddyOAuthHeaderShape 直接断言请求头构造函数的完整形状（防字段丢失）。
func TestWorkBuddyOAuthHeaderShape(t *testing.T) {
	t.Parallel()
	req, err := http.NewRequest(http.MethodPost, "https://example.com", strings.NewReader("{}"))
	require.NoError(t, err)
	applyWorkbuddyOAuthHeaders(req, "https://www.codebuddy.cn")
	require.Equal(t, "https://www.codebuddy.cn", req.Header.Get("Origin"))
	require.Equal(t, "https://www.codebuddy.cn/", req.Header.Get("Referer"))
	require.Equal(t, "CLI/2.63.2 CodeBuddy/2.63.2", req.Header.Get("User-Agent"))
	require.Equal(t, "application/json", req.Header.Get("Content-Type"))
	require.Equal(t, "application/json, text/plain, */*", req.Header.Get("Accept"))
	require.Equal(t, "XMLHttpRequest", req.Header.Get("X-Requested-With"))
}

// workbuddyOAuthProxyRepoStub 是 ProxyRepository 的测试替身：只覆盖 GetByID，
// 其余方法经内嵌接口置 nil（调用即 panic，确保测试不意外依赖未打桩的方法）。
type workbuddyOAuthProxyRepoStub struct {
	ProxyRepository
	proxy *Proxy
	err   error
}

func (r *workbuddyOAuthProxyRepoStub) GetByID(_ context.Context, _ int64) (*Proxy, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.proxy, nil
}

// errWorkbuddyOAuthTestDBDown 模拟非 ErrProxyNotFound 的仓库故障。
var errWorkbuddyOAuthTestDBDown = errors.New("database temporarily unavailable")

// infraerrorsCodeOf 读取 infraerrors 的 HTTP 状态码。
func infraerrorsCodeOf(err error) int {
	return infraerrors.Code(err)
}
