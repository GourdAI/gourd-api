//go:build unit

package admin

// trae_credits_handler_test.go Trae 积分/签到/换票管理端点的契约单测：
// 路径参数校验、service 未注入的防御（400 而非 panic）、平台不符与账号不存在
// 的错误码透传，以及成功信封形状（200 + data）。
//
// 上游全链路（响应解析、签到状态机、快照落库）由
// internal/service/trae_credits_service_test.go 覆盖；本文件锁定 handler 层的
// 包装方式：上游探测失败也是 200 + success=false（由 service 层保证），
// 只有"账号不存在 / 平台不符 / 服务未启用"才用 HTTP 错误码。
//
// 复用 workbuddy_credits_handler_test.go 的 repo/upstream 替身（同包）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// traeHandlerTestConfig 提供 handler 用例所需的最小配置（关闭出站 URL 白名单，
// 允许打到 httptest 的 http://127.0.0.1）。
func traeHandlerTestConfig() *config.Config {
	return &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
		},
	}
}

// newTraeCreditsHandlerRouter 挂载与 routes/admin.go registerTraeRoutes 同形的路由。
func newTraeCreditsHandlerRouter(h *TraeCreditsHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/api/v1/admin/trae")
	group.GET("/accounts/:id/credits", h.QueryCredits)
	group.POST("/accounts/:id/checkin", h.Checkin)
	group.POST("/oauth/exchange-token", h.ExchangeToken)
	return router
}

// 非法账号 ID → 400；可解析但 service 未启用 → 400（不得 panic）。
func TestTraeCreditsHandlerInvalidAccountID(t *testing.T) {
	t.Parallel()
	h := NewTraeCreditsHandler(nil, nil)
	router := newTraeCreditsHandlerRouter(h)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/trae/accounts/abc/credits", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "Invalid account ID")

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/trae/accounts/0/checkin", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// service 未注入时三个端点都必须 400 而非 panic（与 WorkBuddy/Qoder 口径一致）。
func TestTraeCreditsHandlerNilServiceRejected(t *testing.T) {
	t.Parallel()
	h := NewTraeCreditsHandler(nil, nil)
	router := newTraeCreditsHandlerRouter(h)

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/trae/accounts/9/credits", nil),
		httptest.NewRequest(http.MethodPost, "/api/v1/admin/trae/accounts/9/checkin", nil),
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusBadRequest, rec.Code, req.URL.Path)
		require.Contains(t, rec.Body.String(), "not enabled")
	}
}

// 非 Trae 账号：service 返回 400 业务错误（TRAE_INVALID_PLATFORM），经 ErrorFrom 透传。
func TestTraeCreditsHandlerRejectsOtherPlatform(t *testing.T) {
	t.Parallel()
	repo := &workbuddyCreditsHandlerRepo{
		account: &service.Account{ID: 130, Platform: service.PlatformWorkbuddy, Status: service.StatusActive},
	}
	svc := service.NewTraeCreditsService(repo, nil, &workbuddyCreditsHandlerUpstream{client: http.DefaultClient}, traeHandlerTestConfig())
	router := newTraeCreditsHandlerRouter(NewTraeCreditsHandler(svc, nil))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/trae/accounts/130/credits", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/trae/accounts/130/checkin", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// 账号不存在 → 404（TRAE_ACCOUNT_NOT_FOUND）。
func TestTraeCreditsHandlerAccountNotFound(t *testing.T) {
	t.Parallel()
	repo := &workbuddyCreditsHandlerRepo{}
	svc := service.NewTraeCreditsService(repo, nil, &workbuddyCreditsHandlerUpstream{client: http.DefaultClient}, traeHandlerTestConfig())
	router := newTraeCreditsHandlerRouter(NewTraeCreditsHandler(svc, nil))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/trae/accounts/404/credits", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// 零值 service（repo/upstream 皆 nil）→ 500 信封且不 panic。
func TestTraeCreditsHandlerUnconfiguredServiceReturnsErrorEnvelope(t *testing.T) {
	t.Parallel()
	repo := &workbuddyCreditsHandlerRepo{
		account: &service.Account{ID: 131, Platform: service.PlatformTrae, Status: service.StatusActive},
	}
	svc := service.NewTraeCreditsService(repo, nil, nil, traeHandlerTestConfig())
	router := newTraeCreditsHandlerRouter(NewTraeCreditsHandler(svc, nil))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/trae/accounts/131/credits", nil))
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.NotEmpty(t, envelope["message"], "错误信封必须带可诊断文案")
}

// 成功链路：200 + data.success=true，且积分快照落进 extra（锁定 handler→service→上游
// 全接线可用，而不只是各自的单元测试通过）。
func TestTraeCreditsHandlerQueryCreditsSuccessEnvelope(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/trae/api/v2/ug/checkin_credits/status":
			_, _ = w.Write([]byte(`{"code":0,"message":"success","checked_in":false,"enable":true,"credits":2203.5264}`))
		case "/trae/api/v2/pay/ide_user_ent_usage":
			future := strconv.FormatInt(time.Now().AddDate(0, 0, 20).Unix(), 10)
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"user_entitlement_pack_list":[` +
				`{"entitlement_id":"checkin_20260920_7001","entitlement_base_info":{"quota":{"credits_limit":2000},"end_time":` +
				future + `},"usage":{"credits_amount":200.8772}}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"message":"no route"}`))
		}
	}))
	defer server.Close()

	account := &service.Account{
		ID: 132, Platform: service.PlatformTrae, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Concurrency: 1,
		Credentials: map[string]any{
			"realm": "cn", "access_token": "at-handler-test",
			"billing_base_url": server.URL,
			"expires_at":       time.Now().Add(48 * time.Hour).Unix(),
			"uid":              "7000000000000000002",
		},
	}
	repo := &workbuddyCreditsHandlerRepo{account: account}
	svc := service.NewTraeCreditsService(repo, nil, &workbuddyCreditsHandlerUpstream{client: server.Client()}, traeHandlerTestConfig())
	router := newTraeCreditsHandlerRouter(NewTraeCreditsHandler(svc, nil))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/trae/accounts/132/credits", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var envelope struct {
		Data struct {
			Success   bool    `json:"success"`
			Remain    float64 `json:"remain"`
			Credits   float64 `json:"credits"`
			Checkable bool    `json:"checkable"`
			Packs     int     `json:"packs"`
			Error     string  `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.True(t, envelope.Data.Success, "error=%s body=%s", envelope.Data.Error, rec.Body.String())
	require.InDelta(t, 2203.5264, envelope.Data.Credits, 1e-6)
	require.InDelta(t, 1799.1228, envelope.Data.Remain, 1e-6)
	require.Equal(t, 1, envelope.Data.Packs)
	require.True(t, envelope.Data.Checkable)

	mu.Lock()
	got := strings.Join(paths, ",")
	mu.Unlock()
	require.Contains(t, got, "/trae/api/v2/ug/checkin_credits/status")

	// 积分快照必须落 extra（列表页免探测渲染依赖它）。
	snapshot, ok := repo.snapshot(132)
	require.True(t, ok, "查询成功必须写入 extra 快照")
	require.Contains(t, snapshot, "trae_credits")
}

// 换票端点入参校验：缺 refresh_token → 400；service 未启用 → 400。
func TestTraeCreditsHandlerExchangeTokenValidation(t *testing.T) {
	t.Parallel()
	h := NewTraeCreditsHandler(nil, nil)
	router := newTraeCreditsHandlerRouter(h)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/trae/oauth/exchange-token",
		strings.NewReader(`{"realm":"cn"}`)))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "refresh_token")

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/trae/oauth/exchange-token",
		strings.NewReader(`{"realm":"cn","refresh_token":"rt-1"}`)))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "not enabled")
}
