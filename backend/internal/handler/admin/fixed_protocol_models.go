package admin

import (
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// fixed_protocol_models.go 固定协议平台（WorkBuddy / Trae / Qoder）的可用模型列表。
//
// 背景（2026-10-13）：GetAvailableModels 按平台逐个分支处理，末尾落到
// 「Claude/Anthropic」兜底（IsOAuth → claude.DefaultModels；API Key 无映射 →
// claude.DefaultModels）。这三个固定协议平台既不是 OAuth（账号类型是 apikey）
// 就一路走到兜底分支：管理员在「测试连接」弹窗与批量改模型里看到的模型下拉
// 显示的是 Claude 模型名，而它们真实可调的是上游各自目录里的模型
// （glm-5.x / kimi-k3-2 / hy3-x / qmodel_38max / DeepSeek-V4-Pro 等）。
// 同名模型选错，测试请求会直接打到上游不认的模型上。
//
// 为什么不复用 accountTestService 的实时拉取：那个路径要真凭据、会打上游，
// 而本接口被弹窗打开即调用（无显式点击），把上游请求挂在页面打开路径上不合适；
// 管理员要取最新目录有专门的「同步上游模型」按钮（workbuddy/trae/qoder 均已接通
// 原生目录路径，见 service/workbuddy_models.go 等）。因此这里只读本地已有的
// 两份数据：平台默认目录常量 + 上一轮同步落盘的能力快照。

// fixedProtocolAccountModels 返回这三个平台的可用模型；非这些平台返回 nil
// （交由调用方继续走原有分支）。
//
// 取值优先级与 OpenAI / Grok 分支同语义：账号配了 model_mapping 就只列映射键
// （白名单/映射模式下账号确实只服务这些模型）；未配置则回平台默认目录，并把
// 上一轮同步拿到的快照条目并进来——快照里可能有默认目录还没跟上的新模型。
func fixedProtocolAccountModels(account *service.Account) []claude.Model {
	if account == nil {
		return nil
	}
	var defaults []string
	switch {
	case account.IsWorkbuddy():
		defaults = service.DefaultWorkbuddyModelIDs()
	case account.IsTrae():
		defaults = service.DefaultTraeModelIDs()
	case account.IsQoderPlatform():
		defaults = service.DefaultQoderModelIDs()
	default:
		return nil
	}

	displayNames := map[string]string{}
	if snapshot := account.GetUpstreamModelMetadataSnapshot(); snapshot != nil {
		for modelID, metadata := range snapshot.Models {
			modelID = strings.TrimSpace(modelID)
			if modelID == "" {
				continue
			}
			if name := strings.TrimSpace(metadata.DisplayName); name != "" {
				displayNames[modelID] = name
			}
			seen := false
			for _, existing := range defaults {
				if existing == modelID {
					seen = true
					break
				}
			}
			if !seen {
				defaults = append(defaults, modelID)
			}
		}
	}

	mapping := account.GetModelMapping()
	ids := defaults
	if len(mapping) > 0 {
		ids = make([]string, 0, len(mapping))
		for requested := range mapping {
			requested = strings.TrimSpace(requested)
			// 通配键不是具体模型，列出来没有可选意义。
			if requested == "" || strings.Contains(requested, "*") {
				continue
			}
			ids = append(ids, requested)
		}
	}

	sorted := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)

	if len(sorted) == 0 {
		return nil
	}
	models := make([]claude.Model, 0, len(sorted))
	for _, id := range sorted {
		displayName := id
		if name, ok := displayNames[id]; ok {
			displayName = name
		}
		models = append(models, claude.Model{
			ID:          id,
			Type:        "model",
			DisplayName: displayName,
		})
	}
	return models
}
