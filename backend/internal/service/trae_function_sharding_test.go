package service

// 2026-09-29 报障回归：Trae 上游按 function 分片下发可调模型表，
// 聊天侧与目录侧必须同一 function；同时补齐 solo_agent 表提供的元数据解析
// （reasoning / modalities / 窗口上限）与内部工具条目过滤。

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTraeCatalogFunctionMatchesChatFunction 锁死「目录与聊天同源」这条契约。
//
// 两侧分头改动是本类故障的根因形态：拉到 solo_agent 的表、却用 solo_work_lite 调用，
// 下游拿到的是必 4001 的模型（qwen3.8-flash 就是这个形态）。常量同源只挡得住字面量
// 漂移，挡不住有人「觉得另一个值更好」而只改一处，所以这里同时断言值与同源。
func TestTraeCatalogFunctionMatchesChatFunction(t *testing.T) {
	t.Parallel()

	require.Equal(t, "solo_agent", traeCatalogFunction,
		"聊天通道判据：2026-09-29 同一张有效账号票实测 solo_agent 能调通 qwen3.8-flash，solo_work_lite 必流内 4001")
	require.Equal(t, traeCatalogFunction, traeChatFunction, "聊天侧必须与目录侧同一 function")

	var sent map[string]any
	require.NoError(t, json.Unmarshal([]byte(traeModelListBody), &sent))
	require.Equal(t, traeCatalogFunction, sent["function"], "目录侧必须与聊天侧同一 function")
	// 其余字段是上游契约，改动会让目录返回形态变化（null 占位键必须存在）。
	require.Contains(t, sent, "config_names")
	require.Contains(t, sent, "current_config_info")
	require.Contains(t, sent, "mode_type")
	require.Contains(t, sent, "agent_type")
	require.Equal(t, false, sent["need_prompt"])
	require.Equal(t, true, sent["poly_prompt"])
}

// TestParseTraeModelCatalogReadsSoloAgentMetadata 用 solo_agent 表里 qwen3.8-flash 的
// 一手条目形态（2026-09-29 dump）验证元数据解析。此前文件头写着「该端点不提供
// reasoning / modalities」并据此每轮回一条 metadata_partial 告警，实际字段一直都在，
// 只是没读。
func TestParseTraeModelCatalogReadsSoloAgentMetadata(t *testing.T) {
	t.Parallel()

	const body = `{
	  "code": 0,
	  "config_info_list": [
	    {"config_name":"qwen3.8-flash","config_switch":true,"usage":"chat_completion",
	     "context_window_tokens":{"dev":200000,"max":1000000},
	     "display_config":{"display_name":"Qwen3.8-Flash","multimodal":true},
	     "reasoning_effort_config":{"default_level":"high","options":["light","high","extra_high"],"support_thinking":true},
	     "model_detail_list":[
	       {"model_name":"qwen3.8-flash__dev","max_tokens":32000,"prompt_max_tokens":168000},
	       {"model_name":"qwen3.8-flash__max","max_tokens":64000,"prompt_max_tokens":936000}
	     ]},
	    {"config_name":"glm-5.2","usage":"chat_completion",
	     "context_window_tokens":{"dev":200000},
	     "display_config":{"display_name":"GLM-5.2","multimodal":false},
	     "reasoning_effort_config":{"default_level":"extra_high","options":["high","extra_high"],"support_thinking":true},
	     "model_detail_list":[{"model_name":"glm-5.2__dev","max_tokens":32000}]},
	    {"config_name":"summary-tool","usage":"summary",
	     "context_window_tokens":{"dev":256000},
	     "display_config":{"display_name":"Summary Tool"}}
	  ]
	}`

	models, metadata, err := parseTraeModelCatalog([]byte(body))
	require.NoError(t, err)
	// usage 非 chat_completion 的条目必须被剔除：上游给内部条目补上 display_name 时，
	// 原有「空 display_name」那条防线会失效，这一条是兜底。
	require.Equal(t, []string{"qwen3.8-flash", "glm-5.2"}, models, "usage=summary 的内部条目应被过滤")

	flash := metadata["qwen3.8-flash"]
	require.Equal(t, "Qwen3.8-Flash", flash.DisplayName)
	require.EqualValues(t, 200000, flash.ContextWindow)
	require.EqualValues(t, 1000000, flash.MaxContextWindow, "context_window_tokens.max 是 Max 模式上限")
	require.EqualValues(t, 32000, flash.MaxOutputTokens, "取 __dev 档，不能用 __max 档的 64000 冒充默认输出上限")
	require.Equal(t, []string{"text", "image"}, flash.InputModalities)
	require.NotNil(t, flash.Reasoning)
	require.True(t, *flash.Reasoning)
	// Trae 的 "light" 不在 normalizeReasoningLevel 白名单里，归一后丢弃，与 qoder 同口径。
	require.Equal(t, []string{"high", "xhigh"}, flash.SupportedReasoningLevels)
	require.Equal(t, "high", flash.DefaultReasoningLevel)

	// default_level 也要先归一再比：上游下发 "extra_high"，levels 里已是 "xhigh"，
	// 不归一就落空并错退到 levels[0]（=high）。
	require.Equal(t, "xhigh", metadata["glm-5.2"].DefaultReasoningLevel,
		"default_level 必须经 normalizeReasoningLevel 后与 levels 比对")
	require.Equal(t, []string{"text"}, metadata["glm-5.2"].InputModalities)

	// 元数据必须能过完整性判定（这条断言就是「不再谎报 metadata_partial」的根据）。
	for _, id := range models {
		require.Truef(t, upstreamModelMetadataIsComplete(metadata[id]),
			"%s 的元数据应判定为完整（solo_agent 表提供了 reasoning/modalities/窗口）", id)
	}
}

// TestParseTraeModelCatalogKeepsPartialWhenFieldsAbsent 上游字段缺失时不得编造：
// Reasoning 保持 nil（不完整，继续告警），不能假装 reasoning=false 把告警压掉。
func TestParseTraeModelCatalogKeepsPartialWhenFieldsAbsent(t *testing.T) {
	t.Parallel()

	const body = `{"config_info_list":[{"config_name":"bare-model","usage":"chat_completion",
	  "display_config":{"display_name":"Bare"}}]}`
	models, metadata, err := parseTraeModelCatalog([]byte(body))
	require.NoError(t, err)
	require.Equal(t, []string{"bare-model"}, models)
	require.Nil(t, metadata["bare-model"].Reasoning, "support_thinking 缺失不得编造成 true/false")
	require.Nil(t, metadata["bare-model"].InputModalities)
	require.False(t, upstreamModelMetadataIsComplete(metadata["bare-model"]),
		"缺字段时应如实判为不完整，而不是谎报同步成功")
}

// TestTraeModelUnavailableHintPointsAtFunctionNotVersionCode 锁死 4001 提示的方向。
// 旧文案让运营「提升 credentials.ide_version_code」，实测是反向操作：本仓默认画像
// 0.1.61/20260820 能在 solo_agent 表里拉到 qwen3.8-flash，改用更新的 IDE 真实头
// 1.107.1/20260212 拉到的表反而没有它。
func TestTraeModelUnavailableHintPointsAtFunctionNotVersionCode(t *testing.T) {
	t.Parallel()

	require.Contains(t, traeModelUnavailableHint, "function")
	require.Contains(t, traeModelUnavailableHint, "not an expired x-ide-version-code")
	require.Contains(t, traeModelUnavailableHint, "do NOT raise credentials.ide_version_code")
	require.Equal(t, traeModelUnavailableHint, traeUpstreamHint("4001"))
	require.Empty(t, traeUpstreamHint("1005"), "只有 4001 给该提示")
}
