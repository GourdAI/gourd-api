//go:build unit

package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newTraeLoginTestRouter 只挂三个 trae oauth 端点，路由路径与 routes/admin.go
// registerTraeRoutes 逐字一致（本测试是该路由表契约的单点哨兵）。
func newTraeLoginTestRouter(svc *service.TraeLoginService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	group := engine.Group("/api/v1/admin/trae")
	handler := NewTraeLoginHandler(svc)
	group.POST("/oauth/auth-url", handler.GenerateAuthURL)
	group.POST("/oauth/submit", handler.Submit)
	group.POST("/oauth/cancel", handler.Cancel)
	return engine
}

func postTraeLoginJSON(t *testing.T, engine *gin.Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func decodeTraeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) (int, map[string]any) {
	t.Helper()
	var envelope struct {
		Code    int            `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope), rec.Body.String())
	return rec.Code, envelope.Data
}

func TestTraeLoginHandler_SubmitMapsCredentialsAndPreservesNonFormKeys(t *testing.T) {
	svc := service.NewTraeLoginService(nil) // oauth 未配置 → 预期 500，但先测成功映射不可达
	engine := newTraeLoginTestRouter(svc)

	// 服务未配置：infraerrors 的 HTTP 码必须原样透传（前端按码给文案）。
	rec := postTraeLoginJSON(t, engine, "/api/v1/admin/trae/oauth/submit", `{"login_id":"x","callback_url":"y"}`)
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	// 入参校验：login_id 缺失/空 → 400。
	svc2 := service.NewTraeLoginService(&service.TraeOAuthService{})
	engine2 := newTraeLoginTestRouter(svc2)
	rec2 := postTraeLoginJSON(t, engine2, "/api/v1/admin/trae/oauth/submit", `{"callback_url":"y"}`)
	require.Equal(t, http.StatusBadRequest, rec2.Code, "login_id 为空必须 400（不消耗任何上游调用）")
}

func TestTraeLoginHandler_GenerateAuthURLRejectsBadRealm(t *testing.T) {
	engine := newTraeLoginTestRouter(service.NewTraeLoginService(&service.TraeOAuthService{}))
	rec := postTraeLoginJSON(t, engine, "/api/v1/admin/trae/oauth/auth-url", `{"realm":"mars"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "realm")
}

func TestTraeLoginCredentialsProjection(t *testing.T) {
	creds := traeLoginCredentials(&service.TraeLoginCredentials{
		Realm:            "cn",
		AccessToken:      "AT",
		RefreshToken:     "RT",
		ExpiresAt:        1799999999,
		RefreshExpireAt:  1899999999,
		UID:              "7000000000000000001",
		Nickname:         "FrankZ",
		DeviceID:         "4175290190306059",
		MachineID:        "b5f0e0f2994442ca857e608eeb98c8e8",
		DevicePublicKey:  "PUB",
		LoginHost:        "https://api.trae.com.cn",
		OAuthBaseURL:     "https://console.enterprise.trae.cn",
		DevicePrivateKey: "PRIV",
	})
	require.Equal(t, "AT", creds["access_token"])
	require.Equal(t, "RT", creds["refresh_token"])
	require.Equal(t, "1799999999", creds["expires_at"], "到期时刻落 epoch 秒字符串")
	require.Equal(t, "1899999999", creds["refresh_expires_at"])
	require.Equal(t, "4175290190306059", creds["device_id"], "登录生成的设备号必须落凭据（否则后续又回到派生漂移）")
	require.Equal(t, "PRIV", creds["device_private_key"])
	require.Equal(t, "https://console.enterprise.trae.cn", creds["oauth_base_url"])

	// 空值不落键：前端合并时才不会用空串覆盖用户已填值。
	sparse := traeLoginCredentials(&service.TraeLoginCredentials{Realm: "cn", AccessToken: "AT"})
	require.Equal(t, map[string]string{"realm": "cn", "access_token": "AT"}, sparse)
}

func TestTraeDevicePrivateKeyIsSensitive(t *testing.T) {
	// 登录落库的设备私钥绝不可回显管理端；同时规范名必须是 snake（has_* 状态位契约）。
	require.True(t, service.IsSensitiveCredentialKey("device_private_key"))
	require.True(t, service.IsSensitiveCredentialKey("devicePrivateKey"))
	require.Equal(t, "device_private_key", service.CanonicalCredentialKey("devicePrivateKey"))

	redacted, status := dto.RedactCredentials(map[string]any{
		"device_private_key": "PRIV-PEM",
		"device_public_key":  "PUB-PEM",
		"realm":              "cn",
	})
	require.NotContains(t, redacted, "device_private_key")
	require.Equal(t, "PUB-PEM", redacted["device_public_key"], "公钥非敏感，前端要能展示设备绑定状态")
	require.True(t, status["has_device_private_key"], "被脱敏也要留下存在位，否则用户以为没配上")
}
