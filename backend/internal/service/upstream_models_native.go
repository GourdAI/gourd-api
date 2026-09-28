package service

// upstream_models_native.go 固定协议平台的上游模型目录接入。
//
// 背景：Trae / Qoder 的上游目录**不是** OpenAI /v1/models 形态（端点、鉴权头族、
// 响应结构各自一套），无法复用 buildUpstreamModelsRequest 的「构造 GET 请求 →
// extractUpstreamModelIDs」通用路径。此前这两个平台在通用 switch 里落 default，
// 管理页「同步上游模型」按钮必然 400，导致上游上新模型后我们完全无法感知。
//
// 本文件提供两件事：
//  1. fetchNativeModelCatalog：按平台分派到各自的专用拉取实现，返回 ID 列表 +
//     上游直出的能力元数据；
//  2. buildCatalogFromNativeModels：把上游直出元数据整理成与通用路径同形态的
//     UpstreamModelCatalog（含快照持久化与不完整告警）。
//
// 为什么原生路径不调 models.dev 兜底：那两个 host（trae-api-cn.mchost.guru /
// api2.qoder.sh）不对应 models.dev 的任何 provider，matchModelsDevProvider 必然
// 失配，白跑一次网络请求并在日志里留一条无意义的 warn。上游自己给的
// display_name / context window 才是这里的权威来源。

import (
	"context"
	"time"
)

// nativeModelCatalogFetcher 是平台原生目录拉取签名（单测替换以避开真实网络与签名链路）。
type nativeModelCatalogFetcher func(ctx context.Context, account *Account) ([]string, map[string]UpstreamModelMetadata, error)

// nativeModelCatalogFetchers 声明「平台 → 专用拉取实现」。用变量而非直接调用，
// 是为了让分派与组装逻辑（handled 判定、警告口径、快照持久化）可在不联网的前提下被测。
var nativeModelCatalogFetchers = map[string]func(s *AccountTestService) nativeModelCatalogFetcher{
	PlatformTrae: func(s *AccountTestService) nativeModelCatalogFetcher {
		return s.fetchTraeUpstreamModels
	},
	PlatformQoder: func(s *AccountTestService) nativeModelCatalogFetcher {
		return s.fetchQoderUpstreamModels
	},
}

// fetchNativeModelCatalog 分派原生目录拉取。handled=false 表示该平台走通用路径。
func (s *AccountTestService) fetchNativeModelCatalog(ctx context.Context, account *Account) ([]string, map[string]UpstreamModelMetadata, bool, error) {
	if account == nil || s == nil {
		return nil, nil, false, nil
	}
	build, ok := nativeModelCatalogFetchers[account.Platform]
	if !ok {
		return nil, nil, false, nil
	}
	models, metadata, err := build(s)(ctx, account)
	if err != nil {
		return nil, nil, true, err
	}
	return models, metadata, true, nil
}

// buildCatalogFromNativeModels 组装原生路径的 catalog 并尽力持久化能力快照。
//
// 快照只写「能力完整」的条目（口径与通用路径一致：reasoning + modalities +
// context_window 齐备），不完整时保留既有快照并回 metadata_partial / metadata_incomplete
// 警告 —— Trae 目录不含 reasoning，正是「ID 同步成功但能力元数据不完整」的典型场景。
func (s *AccountTestService) buildCatalogFromNativeModels(
	ctx context.Context,
	account *Account,
	models []string,
	metadata map[string]UpstreamModelMetadata,
) (*UpstreamModelCatalog, error) {
	catalog := &UpstreamModelCatalog{Models: dedupeAndSortModelIDs(models), Metadata: metadata}
	if catalog.Metadata == nil {
		catalog.Metadata = map[string]UpstreamModelMetadata{}
	}

	capabilityIDs := capabilitySyncModelIDs(catalog.Models)
	if len(capabilityIDs) > 0 && account != nil && account.ID > 0 && s.accountRepo != nil {
		complete := completeUpstreamModelMetadataSubset(capabilityIDs, catalog.Metadata)
		if len(complete) > 0 {
			// 保留既有条目的工具能力（上游目录不下发 codex_tool_capabilities，
			// 丢掉会把上一次同步到的能力信息清零）。
			if previous := account.GetUpstreamModelMetadataSnapshot(); previous != nil {
				for modelID, entry := range complete {
					old, exists := previous.Models[modelID]
					if !exists {
						continue
					}
					if entry.CodexToolCapabilities == nil {
						entry.CodexToolCapabilities = old.CodexToolCapabilities
					}
					complete[modelID] = entry
				}
			}
			snapshot := UpstreamModelMetadataSnapshot{
				Source:   "upstream",
				SyncedAt: time.Now().UTC().Format(time.RFC3339),
				Models:   complete,
			}
			if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{UpstreamModelMetadataExtraKey: snapshot}); err != nil {
				return nil, newUpstreamModelSyncInternalError("Failed to save upstream model metadata", err)
			}
			account.SetUpstreamModelMetadataSnapshot(snapshot)
		}
	}

	if upstreamCatalogNeedsRegistry(capabilityIDs, catalog.Metadata) {
		code := UpstreamModelMetadataIncompleteCode
		message := "Model IDs were synced, but capability metadata is incomplete."
		if len(completeUpstreamModelMetadataSubset(capabilityIDs, catalog.Metadata)) > 0 {
			code = UpstreamModelMetadataPartialCode
			message = "Some model capabilities were saved; remaining models are still incomplete."
		}
		catalog.Warnings = append(catalog.Warnings, UpstreamModelSyncWarning{Code: code, Message: message})
	}
	return catalog, nil
}
