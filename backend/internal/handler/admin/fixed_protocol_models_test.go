package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// fixed_protocol_models_test.go 锁住一件事：WorkBuddy / Trae / Qoder 账号在管理端
// 「可用模型」接口上必须拿到**自己平台**的模型表，而不是落到方法末尾的 Claude 兜底。
//
// 这条路径此前零覆盖（该文件既有测试只测 grok/openai/gemini），所以 Claude 兜底
// 一直默默生效：管理员在「测试连接」弹窗里看到的模型下拉全是 claude-* 名字，
// 选中后必然打到上游不认的模型上。

func decodeAvailableModels(t *testing.T, body string) []struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
} {
	t.Helper()
	var resp struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	return resp.Data
}

func availableModelsFor(t *testing.T, account service.Account) []struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
} {
	t.Helper()
	svc := &availableModelsAdminService{stubAdminService: newStubAdminService(), account: account}
	router := setupAvailableModelsRouter(svc)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/900/models", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return decodeAvailableModels(t, rec.Body.String())
}

func modelIDs(models []struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}) []string {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestAccountHandlerGetAvailableModels_WorkbuddyReturnsWorkbuddyCatalog(t *testing.T) {
	models := availableModelsFor(t, service.Account{
		ID:       900,
		Platform: service.PlatformWorkbuddy,
		Type:     service.AccountTypeAPIKey,
		Status:   service.StatusActive,
		Credentials: map[string]any{
			"access_token": "wb-token",
		},
	})
	ids := modelIDs(models)
	require.NotEmpty(t, ids)
	require.Contains(t, ids, "glm-5.3")
	require.Contains(t, ids, "kimi-k3-2", "2026-10-13 补齐的上游真值 ID 必须出现在候选里")

	// 核心回归断言：一条 Claude 名字都不该出现（旧行为是整表回 claude.DefaultModels）。
	for _, id := range ids {
		require.NotContains(t, id, "claude", "workbuddy 账号不得返回 Claude 模型：%s", id)
	}
	for _, m := range models {
		require.NotEqual(t, "Claude Fable 5.1", m.DisplayName)
	}
}

func TestAccountHandlerGetAvailableModels_TraeAndQoderUseOwnCatalogs(t *testing.T) {
	trae := modelIDs(availableModelsFor(t, service.Account{
		ID:          900,
		Platform:    service.PlatformTrae,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Credentials: map[string]any{"access_token": "trae-token"},
	}))
	require.Contains(t, trae, "glm-5.3")
	for _, id := range trae {
		require.NotContains(t, id, "claude", "trae 账号不得返回 Claude 模型：%s", id)
	}

	qoder := modelIDs(availableModelsFor(t, service.Account{
		ID:          900,
		Platform:    service.PlatformQoder,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Credentials: map[string]any{"personal_access_token": "pt"},
	}))
	require.Contains(t, qoder, "qmodel_38max")
	for _, id := range qoder {
		require.NotContains(t, id, "claude", "qoder 账号不得返回 Claude 模型：%s", id)
	}
}

// 配了 model_mapping 时只列映射键（与 OpenAI / Grok 分支同语义）：白名单模式下
// 账号确实只服务这些模型，把整张默认目录列出来会诱导管理员选一个真调不通的。
func TestAccountHandlerGetAvailableModels_WorkbuddyHonoursModelMapping(t *testing.T) {
	models := availableModelsFor(t, service.Account{
		ID:       900,
		Platform: service.PlatformWorkbuddy,
		Type:     service.AccountTypeAPIKey,
		Status:   service.StatusActive,
		Credentials: map[string]any{
			"access_token": "wb-token",
			"model_mapping": map[string]any{
				"glm-5.2":   "glm-5.2",
				"kimi-k3-2": "kimi-k3-2",
				"wild-*":    "wild-*",
				"":          "",
			},
		},
	})
	ids := modelIDs(models)
	require.Equal(t, []string{"glm-5.2", "kimi-k3-2"}, ids, "映射模式只列具体模型键，通配与空键不进列表")
}

// 上一轮同步落盘的能力快照里有新模型时，即使默认目录常量还没跟上也要列出来，
// 否则「同步成功了但下拉里看不到」会与本次报障同样形态复发。
func TestFixedProtocolAccountModelsMergesSyncedSnapshot(t *testing.T) {
	account := &service.Account{
		Platform: service.PlatformWorkbuddy,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"access_token": "wb-token",
		},
		Extra: map[string]any{
			service.UpstreamModelMetadataExtraKey: map[string]any{
				"source":    "upstream",
				"synced_at": "2026-10-13T00:00:00Z",
				"models": map[string]any{
					"brand-new-model": map[string]any{
						"id":             "brand-new-model",
						"display_name":   "Brand New Model",
						"context_window": 300000,
					},
					"glm-5.3": map[string]any{
						"id":           "glm-5.3",
						"display_name": "GLM-5.3",
					},
				},
			},
		},
	}

	models := fixedProtocolAccountModels(account)
	require.NotNil(t, models)
	byID := make(map[string]string, len(models))
	for _, m := range models {
		byID[m.ID] = m.DisplayName
	}
	require.Contains(t, byID, "brand-new-model")
	require.Equal(t, "Brand New Model", byID["brand-new-model"], "快照里的上游展示名要透出去")
	require.Equal(t, "GLM-5.3", byID["glm-5.3"], "快照优先于「ID 当名字」的兜底")

	// 反向护栏：非这三个平台必须返回 nil，否则会把 Anthropic/OpenAI 的既有分支劫持掉。
	require.Nil(t, fixedProtocolAccountModels(&service.Account{Platform: service.PlatformAnthropic}))
	require.Nil(t, fixedProtocolAccountModels(&service.Account{Platform: service.PlatformOpenAI}))
	require.Nil(t, fixedProtocolAccountModels(nil))
}

// Anthropic 账号必须继续拿到 Claude 默认表（本函数不得劫持既有语义）。
func TestAccountHandlerGetAvailableModels_AnthropicStillReturnsClaude(t *testing.T) {
	models := modelIDs(availableModelsFor(t, service.Account{
		ID:          900,
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Credentials: map[string]any{"api_key": "sk-ant-x"},
	}))
	require.NotEmpty(t, models)
	claudeCount := 0
	for _, id := range models {
		if len(id) >= 6 && id[:6] == "claude" {
			claudeCount++
		}
	}
	require.Positive(t, claudeCount, "Anthropic 账号的 Claude 列表不能被这次改动打断")
}
