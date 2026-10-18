package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// preview 同步入口（未落库凭据）的凭据契约。
//
// 这组用例守住 2026-10 的改造：WorkBuddy / Trae / Qoder 的目录拉取读的是
// credentials.access_token / personal_token 一族字段，而 preview 原先只递 api_key
// （且 api_key 还是 binding:"required"），导致新建这三平台时根本没法拉上游目录 ——
// 管理员只能「先建号 → 再进编辑页点一次同步」。现在 credentials 段按白名单透传。
//
// 载体用 openai/workbuddy 的最小上游形态即可：本文件断言的是 handler 的入参装配
// 与安全收口，三家真实目录的解析契约由 service 层 trae/qoder/workbuddy_models_test.go 覆盖。

func postPreviewSync(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	router := setupSyncUpstreamModelsRouter(newStubAdminService(), &syncUpstreamHTTPUpstream{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"glm-5.3"}]}`)),
		},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/models/sync-upstream-preview", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	return rec
}

func TestSyncUpstreamModelsPreview_AcceptsTokenCredentialsWithoutAPIKey(t *testing.T) {
	// 改造的直接目标：没有 api_key 也能进同步链路（旧实现被 binding:"required" 挡在门外）。
	rec := postPreviewSync(t, `{
		"platform":"workbuddy",
		"type":"apikey",
		"credentials":{"access_token":"wb-at","refresh_token":"wb-rt","realm":"cn","uid":"u-1"}
	}`)
	// workbuddy 走原生 /v3/config 路径，而本文件的桩回的是 OpenAI 形态目录，因此最终
	// 表现为 502（上游应答不可用）。关键断言是**不是 400**：旧实现里缺 api_key 会直接被
	// binding:"required" 拦下，凭据根本进不到 service 层。
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "Missing credentials")
}

func TestSyncUpstreamModelsPreview_QoderPATPassesValidation(t *testing.T) {
	rec := postPreviewSync(t, `{
		"platform":"qoder",
		"type":"apikey",
		"credentials":{"personal_token":"pt-abc","realm":"cn","uid":"u-9"}
	}`)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
}

func TestSyncUpstreamModelsPreview_RejectsWithoutAnyCredential(t *testing.T) {
	rec := postPreviewSync(t, `{"platform":"workbuddy","type":"apikey","credentials":{"realm":"cn"}}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "Missing credentials")
}

func TestSyncUpstreamModelsPreview_KeepsLegacyAPIKeyBehaviour(t *testing.T) {
	// 旧调用方（OpenAI / Anthropic 等）不传 credentials，必须照常工作。
	rec := postPreviewSync(t, `{
		"platform":"openai",
		"type":"apikey",
		"base_url":"https://provider.example/v1",
		"api_key":"key",
		"model_mapping":{"public-glm":"glm-5.3"}
	}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Data service.UpstreamModelCatalog `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, []string{"glm-5.3"}, resp.Data.Models)
}

func TestSyncUpstreamModelsPreview_DropsControlCredentialKeys(t *testing.T) {
	// 安全收口：能改写出站头族 / 转发协议分支 / 代理选择的控制键必须在白名单之外，
	// 否则「拉一次目录」就变成用管理员权限探测下游行为分支的通道。
	for _, key := range []string{
		"header_overrides",
		"system_credentials",
		"account_mode",
		"api_protocol",
		"proxy_id",
		"group_ids",
		"concurrency",
		"priority",
	} {
		_, allowed := previewSyncCredentialKeys[key]
		require.False(t, allowed, "credential key %q must not be accepted by the preview endpoint", key)
	}

	// 身份票据与地域字段必须在表内，否则本次改造白做。
	for _, key := range []string{"access_token", "refresh_token", "personal_token", "realm", "uid", "device_id", "ide_version_code"} {
		_, allowed := previewSyncCredentialKeys[key]
		require.True(t, allowed, "credential key %q must be forwarded so native catalog fetch works", key)
	}
}

func TestSyncUpstreamModelsPreview_NormalizesCamelCaseKeys(t *testing.T) {
	// service 层 GetXxxCredentials() 两种命名都读，但 preview 统一归一到 snake_case，
	// 避免同一含义出现两个键时取到意外的那个。
	require.Equal(t, "access_token", previewSyncCredentialKeys["accessToken"])
	require.Equal(t, "personal_token", previewSyncCredentialKeys["personalToken"])
	require.Equal(t, "device_id", previewSyncCredentialKeys["deviceId"])
	require.Equal(t, "uid", previewSyncCredentialKeys["userId"])
}

func TestPreviewSyncCredentialString(t *testing.T) {
	require.Equal(t, "", previewSyncCredentialString(nil))
	require.Equal(t, "tok", previewSyncCredentialString("  tok  "))
	// expires_at 经 encoding/json 会变成 float64；直接 FormatFloat 给出 1.78e+09，上游不认。
	require.Equal(t, "1780000000", previewSyncCredentialString(float64(1780000000)))
	require.Equal(t, "1.5", previewSyncCredentialString(1.5))
	require.Equal(t, "42", previewSyncCredentialString(json.Number("42")))
	require.Equal(t, "true", previewSyncCredentialString(true))
	require.Equal(t, "", previewSyncCredentialString(map[string]any{"a": 1}), "对象值一律拒绝")
}
