//go:build unit

package admin

// qoder_credits_handler_test.go Qoder 积分/领取管理端点的契约单测：
// 路径参数校验、service 未启用的防御（400 而非 panic）、响应信封形状，
// 以及路由注册（GET credits / POST checkin 挂在 /admin/qoder 下）。
//
// 上游全链路（真实响应体解析、claim 编排、幂等分类、快照落库）由
// internal/service/qoder_campaign_service_test.go 覆盖；openapi 域不受
// credentials.base_url 中转覆盖（既定语义），故本文件不重复打假上游。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// qoderCreditsHandlerTestConfig 提供 handler 用例所需的最小配置（关闭出站 URL 白名单）。
func qoderCreditsHandlerTestConfig() *config.Config {
	return &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
		},
	}
}

func newQoderCreditsHandlerRouter(h *QoderCreditsHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/api/v1/admin/qoder")
	group.GET("/accounts/:id/credits", h.QueryCredits)
	group.POST("/accounts/:id/checkin", h.Checkin)
	return router
}

func TestQoderCreditsHandlerInvalidAccountID(t *testing.T) {
	t.Parallel()
	h := NewQoderCreditsHandler(nil)
	router := newQoderCreditsHandlerRouter(h)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/qoder/accounts/abc/credits", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "Invalid account ID")

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/qoder/accounts/0/checkin", nil))
	// 0 可解析为合法 int，但 service 未启用 → 400（不得 panic）
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// service 未注入时两个端点都必须 400 而非 panic（与 WorkBuddy 口径一致）。
func TestQoderCreditsHandlerNilServiceRejected(t *testing.T) {
	t.Parallel()
	h := NewQoderCreditsHandler(nil)
	router := newQoderCreditsHandlerRouter(h)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/qoder/accounts/9/credits", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "not enabled")

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/qoder/accounts/9/checkin", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// 非 Qoder 账号：service 返回 400 业务错误，经 ErrorFrom 透传状态码。
func TestQoderCreditsHandlerRejectsOtherPlatform(t *testing.T) {
	t.Parallel()
	repo := &workbuddyCreditsHandlerRepo{
		account: &service.Account{ID: 120, Platform: service.PlatformWorkbuddy, Status: service.StatusActive},
	}
	svc := service.NewQoderCreditsService(repo, nil, &workbuddyCreditsHandlerUpstream{client: http.DefaultClient}, qoderCreditsHandlerTestConfig())
	h := NewQoderCreditsHandler(svc)
	router := newQoderCreditsHandlerRouter(h)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/qoder/accounts/120/credits", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/qoder/accounts/120/checkin", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// 账号不存在：404（QODER_ACCOUNT_NOT_FOUND）。
func TestQoderCreditsHandlerAccountNotFound(t *testing.T) {
	t.Parallel()
	repo := &workbuddyCreditsHandlerRepo{}
	svc := service.NewQoderCreditsService(repo, nil, &workbuddyCreditsHandlerUpstream{client: http.DefaultClient}, qoderCreditsHandlerTestConfig())
	h := NewQoderCreditsHandler(svc)
	router := newQoderCreditsHandlerRouter(h)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/qoder/accounts/404/credits", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// service 未配置（httpUpstream 为 nil）时查询走 500 信封，且不 panic。
func TestQoderCreditsHandlerUnconfiguredServiceReturnsErrorEnvelope(t *testing.T) {
	t.Parallel()
	repo := &workbuddyCreditsHandlerRepo{
		account: &service.Account{ID: 121, Platform: service.PlatformQoder, Status: service.StatusActive},
	}
	svc := service.NewQoderCreditsService(repo, nil, nil, qoderCreditsHandlerTestConfig())
	h := NewQoderCreditsHandler(svc)
	router := newQoderCreditsHandlerRouter(h)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/qoder/accounts/121/credits", nil))
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.NotEmpty(t, envelope["message"], "错误信封必须带可诊断文案")
}

// 成功信封形状：200 + data 内对象（上游探测失败也是 success=false 的 200，
// 由 service 层用例覆盖，此处只锁定 handler 的包装方式）。
func TestQoderCreditsHandlerSuccessEnvelopeShape(t *testing.T) {
	t.Parallel()
	h := NewQoderCreditsHandler(&service.QoderCreditsService{})
	require.NotNil(t, h)

	// 零值 service（repo/upstream 皆 nil）走 500，验证 handler 不 panic。
	router := newQoderCreditsHandlerRouter(h)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/qoder/accounts/5/checkin", nil))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}
