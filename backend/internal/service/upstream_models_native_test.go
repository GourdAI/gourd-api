//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// upstream_models_native.go 的固定协议平台接入层契约。
//
// 这一层的价值在于「把上游直出元数据整理成与通用路径同形态的 catalog」，并且：
//   - 分派（handled 判定）必须与平台严格绑定，未登记的平台一律回落通用路径；
//   - 快照只写能力完整的条目，不完整时保留既有快照并回 metadata_incomplete /
//     metadata_partial 警告（绝不能把「ID 同步成功」包装成「完全成功」）；
//   - 上游目录不下发 codex_tool_capabilities，合并时必须保留上一轮同步结果。
//
// 拉取实现通过临时替换包级变量 nativeModelCatalogFetchers 注入桩（t.Cleanup 恢复），
// 因此本文件中的用例**不使用** t.Parallel()，避免跨用例读写同一全局表。
//
// 共享基建（modelCatalogTestRepo / requireUpstreamModelSyncError /
// newModelCatalogTestServer / traeCatalogHappyPath / qoderCatalogHappyPath 等）
// 复用 trae_models_test.go 与 qoder_models_test.go。

// swapNativeModelCatalogFetcher 用桩替换指定平台的拉取实现，并在用例结束时恢复。
func swapNativeModelCatalogFetcher(t *testing.T, platform string, build func(s *AccountTestService) nativeModelCatalogFetcher) {
	t.Helper()
	original := nativeModelCatalogFetchers
	replacement := make(map[string]func(s *AccountTestService) nativeModelCatalogFetcher, len(original)+1)
	for key, value := range original {
		replacement[key] = value
	}
	if build == nil {
		delete(replacement, platform)
	} else {
		replacement[platform] = build
	}
	nativeModelCatalogFetchers = replacement
	t.Cleanup(func() { nativeModelCatalogFetchers = original })
}

func nativeCatalogStubResult(models []string, metadata map[string]UpstreamModelMetadata, err error) nativeModelCatalogFetcher {
	return func(context.Context, *Account) ([]string, map[string]UpstreamModelMetadata, error) {
		return models, metadata, err
	}
}

func nativeTestAccount(id int64, platform string) *Account {
	return &Account{
		ID:          id,
		Platform:    platform,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "test-token"},
	}
}

// nativeBoolPtr 避免与无关测试文件的同名 helper 耦合。
func nativeBoolPtr(value bool) *bool { return &value }

// nativeCompleteMetadata 造一条「能力完整」（reasoning + modalities + context_window）
// 的元数据，用于驱动快照持久化分支。
func nativeCompleteMetadata(id string, window int64) UpstreamModelMetadata {
	return UpstreamModelMetadata{
		ID:              id,
		DisplayName:     "Display " + id,
		Reasoning:       nativeBoolPtr(false),
		InputModalities: []string{"text"},
		ContextWindow:   window,
	}
}

// decodePersistedSnapshot 把仓库记账里的 extra 值还原成快照结构。
func decodePersistedSnapshot(t *testing.T, raw any) UpstreamModelMetadataSnapshot {
	t.Helper()
	require.NotNil(t, raw, "未观测到 upstream_model_metadata 落库")
	encoded, err := json.Marshal(raw)
	require.NoError(t, err)
	var snapshot UpstreamModelMetadataSnapshot
	require.NoError(t, json.Unmarshal(encoded, &snapshot))
	return snapshot
}

// ---------------------------------------------------------------------------
// C. fetchNativeModelCatalog 分派
// ---------------------------------------------------------------------------

func TestFetchNativeModelCatalogDispatchesTraeFetcher(t *testing.T) {
	var serviceSeen *AccountTestService
	var accountSeen *Account
	swapNativeModelCatalogFetcher(t, PlatformTrae, func(s *AccountTestService) nativeModelCatalogFetcher {
		serviceSeen = s
		return func(_ context.Context, account *Account) ([]string, map[string]UpstreamModelMetadata, error) {
			accountSeen = account
			return []string{"trae-model"}, map[string]UpstreamModelMetadata{
				"trae-model": {ID: "trae-model", DisplayName: "Trae Model"},
			}, nil
		}
	})

	svc := &AccountTestService{cfg: modelSyncInsecureTestConfig()}
	account := nativeTestAccount(5001, PlatformTrae)
	models, metadata, handled, err := svc.fetchNativeModelCatalog(context.Background(), account)
	require.NoError(t, err)
	require.True(t, handled, "trae 必须走原生目录路径")
	require.Equal(t, []string{"trae-model"}, models)
	require.Equal(t, "Trae Model", metadata["trae-model"].DisplayName)
	require.Same(t, svc, serviceSeen, "构造器必须收到服务实例（真实实现依赖 httpUpstream/cfg）")
	require.Same(t, account, accountSeen)
}

func TestFetchNativeModelCatalogDispatchesQoderFetcher(t *testing.T) {
	var called bool
	swapNativeModelCatalogFetcher(t, PlatformQoder, func(*AccountTestService) nativeModelCatalogFetcher {
		return func(ctx context.Context, account *Account) ([]string, map[string]UpstreamModelMetadata, error) {
			called = true
			require.Equal(t, PlatformQoder, account.Platform)
			return []string{"dmodel"}, map[string]UpstreamModelMetadata{
				"dmodel": {ID: "dmodel", DisplayName: "DeepSeek-V4-Pro", Reasoning: nativeBoolPtr(true)},
			}, nil
		}
	})

	svc := &AccountTestService{cfg: modelSyncInsecureTestConfig()}
	models, metadata, handled, err := svc.fetchNativeModelCatalog(context.Background(), nativeTestAccount(5002, PlatformQoder))
	require.NoError(t, err)
	require.True(t, handled)
	require.True(t, called, "qoder 账号必须调用到注入的 qoder 实现")
	require.Equal(t, []string{"dmodel"}, models)
	require.NotNil(t, metadata["dmodel"].Reasoning)
}

func TestFetchNativeModelCatalogLeavesOtherPlatformsToGenericPath(t *testing.T) {
	for _, platform := range []string{PlatformWorkbuddy, PlatformOpenAI, PlatformAnthropic, PlatformGemini, ""} {
		account := nativeTestAccount(5003, platform)
		svc := &AccountTestService{cfg: modelSyncInsecureTestConfig()}
		models, metadata, handled, err := svc.fetchNativeModelCatalog(context.Background(), account)
		require.NoError(t, err, platform)
		require.False(t, handled, "%s 应回落通用 /v1/models 路径", platform)
		require.Nil(t, models, platform)
		require.Nil(t, metadata, platform)
	}

	// nil 账号 / nil 服务都不该 panic，也不该宣称 handled。
	nilSvcModels, _, nilSvcHandled, nilSvcErr := (*AccountTestService)(nil).fetchNativeModelCatalog(context.Background(), nativeTestAccount(1, PlatformTrae))
	require.NoError(t, nilSvcErr)
	require.False(t, nilSvcHandled)
	require.Nil(t, nilSvcModels)

	svc := &AccountTestService{cfg: modelSyncInsecureTestConfig()}
	_, _, handled, err := svc.fetchNativeModelCatalog(context.Background(), nil)
	require.NoError(t, err)
	require.False(t, handled)
}

func TestFetchNativeModelCatalogPassesThroughErrorsWithHandledTrue(t *testing.T) {
	upstreamFailure := errors.New("trae model list returned HTTP 401")
	swapNativeModelCatalogFetcher(t, PlatformTrae, func(*AccountTestService) nativeModelCatalogFetcher {
		return nativeCatalogStubResult([]string{"partial"}, nil, upstreamFailure)
	})

	svc := &AccountTestService{cfg: modelSyncInsecureTestConfig()}
	models, metadata, handled, err := svc.fetchNativeModelCatalog(context.Background(), nativeTestAccount(5004, PlatformTrae))
	require.True(t, handled, "失败也必须标记 handled，否则上层会退回通用路径再报 unsupported platform")
	require.ErrorIs(t, err, upstreamFailure, "错误必须原样透传（含 UpstreamModelSyncError 语义）")
	require.Empty(t, models)
	require.Nil(t, metadata)
}

func TestFetchNativeModelCatalogWrapsSyncErrorType(t *testing.T) {
	syncErr := &UpstreamModelSyncError{Kind: UpstreamModelSyncErrorUpstream, Message: "Trae returned no schedulable models"}
	swapNativeModelCatalogFetcher(t, PlatformTrae, func(*AccountTestService) nativeModelCatalogFetcher {
		return nativeCatalogStubResult(nil, nil, syncErr)
	})
	svc := &AccountTestService{cfg: modelSyncInsecureTestConfig()}
	_, _, handled, err := svc.fetchNativeModelCatalog(context.Background(), nativeTestAccount(5005, PlatformTrae))
	require.True(t, handled)
	require.Equal(t, syncErr, requireUpstreamModelSyncError(t, err))
}

// ---------------------------------------------------------------------------
// 真实实现接线（不替换 fetchers）：确认包级表确实绑定到两个专用方法
// ---------------------------------------------------------------------------

func TestNativeModelCatalogFetchersAreWiredToRealImplementations(t *testing.T) {
	server, capture, upstream := newModelCatalogTestServer(t, http.StatusOK, traeCatalogHappyPath)
	svc := &AccountTestService{httpUpstream: upstream, cfg: modelSyncInsecureTestConfig()}

	models, metadata, handled, err := svc.fetchNativeModelCatalog(context.Background(), traeModelSyncTestAccount(server.URL))
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{"doubao-seed-2.1-pro", "gpt-5.6-sol", "minimax-m3"}, models)
	require.NotNil(t, metadata["gpt-5.6-sol"])
	require.Len(t, capture.all(), 1, "真实 trae 实现应被分派到")
	require.Equal(t, traeModelListPath, capture.last(t).path)

	qserver, qcapture, qupstream := newModelCatalogTestServer(t, http.StatusOK, qoderCatalogHappyPath)
	qsvc := &AccountTestService{httpUpstream: qupstream, cfg: modelSyncInsecureTestConfig()}
	qmodels, _, qhandled, err := qsvc.fetchNativeModelCatalog(context.Background(), qoderModelSyncTestAccount(qserver.URL))
	require.NoError(t, err)
	require.True(t, qhandled)
	require.Equal(t, []string{"dmodel", "gmodel", "kmodel_latest"}, qmodels)
	require.Equal(t, "/algo/api/v2/model/list", qcapture.last(t).path)
}

// ---------------------------------------------------------------------------
// D. buildCatalogFromNativeModels
// ---------------------------------------------------------------------------

func TestBuildCatalogFromNativeModelsSortsDedupesAndPersistsCompleteSnapshot(t *testing.T) {
	repo := &modelCatalogTestRepo{}
	svc := &AccountTestService{accountRepo: repo, cfg: modelSyncInsecureTestConfig()}
	account := nativeTestAccount(6001, PlatformQoder)

	complete := map[string]UpstreamModelMetadata{
		"zeta-model":  nativeCompleteMetadata("zeta-model", 131072),
		"alpha-model": nativeCompleteMetadata("alpha-model", 262144),
	}
	// 输入故意乱序且含重复 / 空白条目。
	catalog, err := svc.buildCatalogFromNativeModels(context.Background(), account,
		[]string{"zeta-model", "alpha-model", " zeta-model ", "zeta-model", ""}, complete)
	require.NoError(t, err)
	require.Equal(t, []string{"alpha-model", "zeta-model"}, catalog.Models, "去重 + 字典序")
	require.Equal(t, complete, catalog.Metadata)
	require.Empty(t, catalog.Warnings, "能力齐备时不该回警告")

	// 落库快照
	require.Len(t, repo.calls(), 1)
	require.Equal(t, int64(6001), repo.calls()[0].accountID)
	snapshot := decodePersistedSnapshot(t, repo.lastExtraSnapshot(UpstreamModelMetadataExtraKey))
	require.Equal(t, "upstream", snapshot.Source)
	require.NotEmpty(t, snapshot.SyncedAt)
	require.Equal(t, []string{"alpha-model", "zeta-model"}, dedupeAndSortModelIDs(keysOfMetadata(snapshot.Models)))
	require.Equal(t, int64(262144), snapshot.Models["alpha-model"].ContextWindow)

	// 内存账号快照同步生效（后续展示/别名归一靠它）
	live := account.GetUpstreamModelMetadataSnapshot()
	require.NotNil(t, live)
	require.Equal(t, "Display alpha-model", live.Models["alpha-model"].DisplayName)
	fromAccount, ok := account.GetUpstreamModelMetadata("zeta-model")
	require.True(t, ok)
	require.Equal(t, int64(131072), fromAccount.ContextWindow)
}

func TestBuildCatalogFromNativeModelsKeepsSnapshotUnsetForNilMetadata(t *testing.T) {
	repo := &modelCatalogTestRepo{}
	svc := &AccountTestService{accountRepo: repo, cfg: modelSyncInsecureTestConfig()}
	account := nativeTestAccount(6002, PlatformTrae)

	catalog, err := svc.buildCatalogFromNativeModels(context.Background(), account, []string{"solo-lite"}, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"solo-lite"}, catalog.Models)
	require.NotNil(t, catalog.Metadata, "nil 元数据要归一成空表，避免下游 map 写入 panic")
	require.Empty(t, catalog.Metadata)
	require.Empty(t, repo.calls())
}

func TestBuildCatalogFromNativeModelsReportsIncompleteAndSkipsSnapshot(t *testing.T) {
	repo := &modelCatalogTestRepo{}
	svc := &AccountTestService{accountRepo: repo, cfg: modelSyncInsecureTestConfig()}
	account := nativeTestAccount(6003, PlatformTrae)
	// 预置一份旧快照：不完整时绝不能被清空。
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{
		Source: "upstream", SyncedAt: "2026-01-01T00:00:00Z",
		Models: map[string]UpstreamModelMetadata{"old-model": nativeCompleteMetadata("old-model", 64000)},
	})

	// Trae 目录的典型形态：有 display_name / context window，但没有 reasoning。
	traeStyle := map[string]UpstreamModelMetadata{
		"doubao-seed-2.1-pro": {ID: "doubao-seed-2.1-pro", DisplayName: "Doubao Seed 2.1 Pro", ContextWindow: 256000, MaxContextWindow: 256000},
		"gpt-5.6-sol":         {ID: "gpt-5.6-sol", DisplayName: "GPT-5.6 Sol", ContextWindow: 400000},
	}
	catalog, err := svc.buildCatalogFromNativeModels(context.Background(), account,
		[]string{"doubao-seed-2.1-pro", "gpt-5.6-sol"}, traeStyle)
	require.NoError(t, err)
	require.Len(t, catalog.Warnings, 1)
	require.Equal(t, UpstreamModelMetadataIncompleteCode, catalog.Warnings[0].Code)
	require.Contains(t, catalog.Warnings[0].Message, "incomplete")

	require.Empty(t, repo.calls(), "元数据不完整时不得写快照")
	require.Contains(t, account.GetUpstreamModelMetadataSnapshot().Models, "old-model", "既有快照必须保留")
}

func TestBuildCatalogFromNativeModelsPersistsCompleteSubsetWithPartialWarning(t *testing.T) {
	repo := &modelCatalogTestRepo{}
	svc := &AccountTestService{accountRepo: repo, cfg: modelSyncInsecureTestConfig()}
	account := nativeTestAccount(6004, PlatformQoder)

	complete := nativeCompleteMetadata("alpha-model", 262144)
	incomplete := UpstreamModelMetadata{ID: "zeta-model", DisplayName: "Zeta"} // 无 reasoning
	catalog, err := svc.buildCatalogFromNativeModels(context.Background(), account,
		[]string{"alpha-model", "zeta-model"}, map[string]UpstreamModelMetadata{
			"alpha-model": complete,
			"zeta-model":  incomplete,
		})
	require.NoError(t, err)
	require.Len(t, catalog.Warnings, 1)
	require.Equal(t, UpstreamModelMetadataPartialCode, catalog.Warnings[0].Code)
	require.Contains(t, catalog.Warnings[0].Message, "Some model capabilities were saved")

	snapshot := decodePersistedSnapshot(t, repo.lastExtraSnapshot(UpstreamModelMetadataExtraKey))
	require.Equal(t, []string{"alpha-model"}, dedupeAndSortModelIDs(keysOfMetadata(snapshot.Models)))
	require.NotContains(t, snapshot.Models, "zeta-model", "不完整条目不得进快照")
	require.Equal(t, complete, snapshot.Models["alpha-model"])
}

func TestBuildCatalogFromNativeModelsPreservesExistingCodexToolCapabilities(t *testing.T) {
	repo := &modelCatalogTestRepo{}
	svc := &AccountTestService{accountRepo: repo, cfg: modelSyncInsecureTestConfig()}
	account := nativeTestAccount(6005, PlatformQoder)

	caps := map[string]json.RawMessage{"web_search": json.RawMessage(`{"type":"function"}`)}
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{
		Source: "upstream", SyncedAt: "2026-01-01T00:00:00Z",
		Models: map[string]UpstreamModelMetadata{
			"alpha-model": func() UpstreamModelMetadata {
				entry := nativeCompleteMetadata("alpha-model", 262144)
				entry.CodexToolCapabilities = caps
				return entry
			}(),
			// 已从上游目录消失的旧条目：不应被带回本轮快照
			"gone-model": func() UpstreamModelMetadata {
				entry := nativeCompleteMetadata("gone-model", 32000)
				entry.CodexToolCapabilities = map[string]json.RawMessage{"shell": json.RawMessage(`{}`)}
				return entry
			}(),
		},
	})

	// 本轮上游直出的条目不带工具能力（目录接口不下发该字段）。
	fresh := nativeCompleteMetadata("alpha-model", 524288)
	catalog, err := svc.buildCatalogFromNativeModels(context.Background(), account, []string{"alpha-model"},
		map[string]UpstreamModelMetadata{"alpha-model": fresh})
	require.NoError(t, err)
	require.Empty(t, catalog.Warnings)

	snapshot := decodePersistedSnapshot(t, repo.lastExtraSnapshot(UpstreamModelMetadataExtraKey))
	require.Equal(t, caps, snapshot.Models["alpha-model"].CodexToolCapabilities,
		"上游目录不下发 codex_tool_capabilities，必须继承上一轮结果")
	require.Equal(t, int64(524288), snapshot.Models["alpha-model"].ContextWindow, "本轮元数据优先")
	require.NotContains(t, snapshot.Models, "gone-model", "上游已不再列出的条目不保留")
}

func TestBuildCatalogFromNativeModelsIgnoresMediaModelsForCompleteness(t *testing.T) {
	repo := &modelCatalogTestRepo{}
	svc := &AccountTestService{accountRepo: repo, cfg: modelSyncInsecureTestConfig()}
	account := nativeTestAccount(6006, PlatformQoder)

	catalog, err := svc.buildCatalogFromNativeModels(context.Background(), account,
		[]string{"gpt-image-2"}, map[string]UpstreamModelMetadata{"gpt-image-2": {ID: "gpt-image-2", DisplayName: "GPT Image 2"}})
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-image-2"}, catalog.Models)
	require.Empty(t, catalog.Warnings, "专用图像模型不参与能力完整性判定")
	require.Empty(t, repo.calls())
}

func TestBuildCatalogFromNativeModelsClassifiesPersistenceFailureAsInternal(t *testing.T) {
	repo := &modelCatalogTestRepo{extraErr: errors.New("db down")}
	svc := &AccountTestService{accountRepo: repo, cfg: modelSyncInsecureTestConfig()}
	account := nativeTestAccount(6007, PlatformQoder)

	catalog, err := svc.buildCatalogFromNativeModels(context.Background(), account,
		[]string{"alpha-model"}, map[string]UpstreamModelMetadata{"alpha-model": nativeCompleteMetadata("alpha-model", 1000)})
	require.Nil(t, catalog)
	syncErr := requireUpstreamModelSyncError(t, err)
	require.Equal(t, UpstreamModelSyncErrorInternal, syncErr.Kind)
	require.ErrorContains(t, err, "Failed to save upstream model metadata")
	require.ErrorContains(t, err, "db down")
	require.Nil(t, account.GetUpstreamModelMetadataSnapshot(), "落库失败不得更新内存快照")
}

func TestBuildCatalogFromNativeModelsSkipsPersistWithoutAccountID(t *testing.T) {
	repo := &modelCatalogTestRepo{}
	svc := &AccountTestService{accountRepo: repo, cfg: modelSyncInsecureTestConfig()}

	for _, account := range []*Account{
		{Platform: PlatformQoder},           // ID == 0
		nativeTestAccount(0, PlatformQoder), // ID == 0
	} {
		catalog, err := svc.buildCatalogFromNativeModels(context.Background(), account,
			[]string{"alpha-model"}, map[string]UpstreamModelMetadata{"alpha-model": nativeCompleteMetadata("alpha-model", 1000)})
		require.NoError(t, err)
		require.Equal(t, []string{"alpha-model"}, catalog.Models)
		require.Empty(t, repo.calls(), "无账号 ID 时不得落库")
	}
	// accountRepo 为 nil 时同样只跳过持久化，不报错。
	bare := &AccountTestService{cfg: modelSyncInsecureTestConfig()}
	catalog, err := bare.buildCatalogFromNativeModels(context.Background(), nativeTestAccount(6008, PlatformQoder),
		[]string{"alpha-model"}, map[string]UpstreamModelMetadata{"alpha-model": nativeCompleteMetadata("alpha-model", 1000)})
	require.NoError(t, err)
	require.Empty(t, catalog.Warnings)
}

// ---------------------------------------------------------------------------
// SyncUpstreamModelCatalog 走原生路径的整体行为
// ---------------------------------------------------------------------------

func TestSyncUpstreamModelCatalogUsesNativePathWithoutRegistryFallback(t *testing.T) {
	// 分派与快照走原生路径，且**不得**再打 models.dev（那两个 host 不对应任何 provider）。
	_, capture, upstream := newModelCatalogTestServer(t, http.StatusInternalServerError, `should not be called`)
	repo := &modelCatalogTestRepo{}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: modelSyncInsecureTestConfig()}

	models := []string{"alpha-model"}
	metadata := map[string]UpstreamModelMetadata{"alpha-model": nativeCompleteMetadata("alpha-model", 131072)}
	for _, platform := range []string{PlatformTrae, PlatformQoder} {
		var called bool
		swapNativeModelCatalogFetcher(t, platform, func(*AccountTestService) nativeModelCatalogFetcher {
			return func(ctx context.Context, account *Account) ([]string, map[string]UpstreamModelMetadata, error) {
				called = true
				return append([]string(nil), models...), metadata, nil
			}
		})
		catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), nativeTestAccount(7001, platform))
		require.NoError(t, err, platform)
		require.True(t, called, platform)
		require.Equal(t, []string{"alpha-model"}, catalog.Models)
		require.Equal(t, metadata, catalog.Metadata)
		require.Empty(t, catalog.Warnings, platform)
	}
	require.Empty(t, capture.all(), "原生路径不应触发任何额外上游请求（含 models.dev）")
	snapshot := decodePersistedSnapshot(t, repo.lastExtraSnapshot(UpstreamModelMetadataExtraKey))
	require.Equal(t, "upstream", snapshot.Source)
}

func TestSyncUpstreamModelCatalogReturnsNativeErrorUntouched(t *testing.T) {
	_, capture, upstream := newModelCatalogTestServer(t, http.StatusOK, traeCatalogHappyPath)
	repo := &modelCatalogTestRepo{}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: modelSyncInsecureTestConfig()}
	nativeErr := newUpstreamModelSyncUpstreamError("Trae model list rejected (401)", errors.New("refresh failed"))
	swapNativeModelCatalogFetcher(t, PlatformTrae, func(*AccountTestService) nativeModelCatalogFetcher {
		return nativeCatalogStubResult(nil, nil, nativeErr)
	})

	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), nativeTestAccount(7002, PlatformTrae))
	require.Nil(t, catalog)
	require.Equal(t, nativeErr, requireUpstreamModelSyncError(t, err))
	require.Equal(t, UpstreamModelSyncErrorUpstream, requireUpstreamModelSyncError(t, err).Kind)
	require.Empty(t, repo.calls())
	require.Empty(t, capture.all())
}

func TestFetchUpstreamSupportedModelsUsesNativePathWhenHandled(t *testing.T) {
	_, capture, upstream := newModelCatalogTestServer(t, http.StatusOK, traeCatalogHappyPath)
	svc := &AccountTestService{httpUpstream: upstream, cfg: modelSyncInsecureTestConfig()}
	swapNativeModelCatalogFetcher(t, PlatformTrae, func(*AccountTestService) nativeModelCatalogFetcher {
		return nativeCatalogStubResult([]string{"zeta", "alpha", "alpha", " "}, nil, nil)
	})

	models, err := svc.FetchUpstreamSupportedModels(context.Background(), nativeTestAccount(7003, PlatformTrae))
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "zeta"}, models, "原生路径同样去重排序")
	require.Empty(t, capture.all())
}

// ---------------------------------------------------------------------------
// E. WorkBuddy 显式不支持
// ---------------------------------------------------------------------------

func TestBuildUpstreamModelsRequestRejectsWorkbuddyWithActionableMessage(t *testing.T) {
	// httpUpstream 非 nil，确保通用路径不会在「客户端未配置」就提前退出，
	// 从而真正走到 buildUpstreamModelsRequest 的 workbuddy 分支。
	svc := &AccountTestService{cfg: modelSyncInsecureTestConfig(), httpUpstream: &qoderFakeUpstream{}}
	req, err := svc.buildUpstreamModelsRequest(context.Background(), &Account{
		ID:          8001,
		Platform:    PlatformWorkbuddy,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "wb-token"},
	})
	require.Nil(t, req)
	syncErr := requireUpstreamModelSyncError(t, err)
	require.Equal(t, UpstreamModelSyncErrorUnsupported, syncErr.Kind)
	require.Contains(t, syncErr.SafeMessage(), "no model list API")
	require.Contains(t, syncErr.SafeMessage(), "WorkBuddy")
	require.Contains(t, syncErr.SafeMessage(), "static model catalog")

	// 整体走同步入口时也必须原样回该错误（不能落进 default 的「unsupported platform」）。
	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), &Account{
		ID:          8001,
		Platform:    PlatformWorkbuddy,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "wb-token"},
	})
	require.Nil(t, catalog)
	require.Equal(t, UpstreamModelSyncErrorUnsupported, requireUpstreamModelSyncError(t, err).Kind)
	require.Contains(t, err.Error(), "no model list API")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func keysOfMetadata(metadata map[string]UpstreamModelMetadata) []string {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	return keys
}
