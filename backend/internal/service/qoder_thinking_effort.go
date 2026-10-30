package service

// qoder_thinking_effort.go Qoder 思考强度（reasoning_effort）的能力白名单与出站注入。
//
// 背景（2026-10-27 线上报障）：
//   - 后台用量页「推理强度」列显示 qwen3.8-max 请求 max、转发 xhigh。经取证这是
//     **正确**的：QwenCloud 官方文档 qwen3.8-max 的 options 只有 low/medium/xhigh
//     （default xhigh），Qoder CLI 文档同样写「low / medium / xhigh」。模型名里的
//     Max 是型号而不是档位，所以 max 必须收敛到该模型最深档 xhigh。
//   - 但核查同时暴露了更严重的缺陷：BuildQoderBody 从未把档位写进出站体（只注入
//     max_tokens），上游永远收它自己的默认档 —— 记录/计费/分组上限策略全部建立在
//     一个没真正生效的值上。
//
// 本文件的职责：
//   1. qoderThinkingEffortsForModel 给出每个模型 key **已取证**支持的档位集合；
//   2. qoderResolveForwardEffort 把客户端档位收敛到该集合（不支持则就近降级，
//      绝不下发模型不认的档位——上游对非法档位的行为未取证，盲发有签名体被拒风险）；
//   3. qoderForwardedEffort 是「本次真正下发的档位」唯一真源（注入方与记录方共用）；
//      ApplyQoderReasoningEffort 在签名前把该值写进 parameters（与 max_tokens 平级）。
//
// 档位取证来源（逐条可查，未知模型**不猜测**）：
//   - QwenCloud 文档（docs.qwencloud.com）：qwen3.8-max/qwen3.8-flash = low/medium/xhigh；
//   - Qoder CLI 文档（docs.qoder.com/zh/cli/model）：
//       Qwen3.8-Max      → low / medium / xhigh          （无 max）
//       Kimi-K3          → low / high / max
//       GLM-5.3          → low / high / max
//       GLM-5.2          → high / max
//       DeepSeek-V4-Pro  → high / max
//       DeepSeek-V4-Flash→ low / high / max
//       Qwen3.7-Max / Qwen3.7-Plus → 仅上下文窗口，**不支持思考强度**
//   - Qoder IDE 文档：Ultimate / Performance / DeepSeek 系 / Qwen3.8 系 / GLM 系 /
//     Kimi-K3 / Kimi-K2.8-Preview 支持思考强度；Qwen3.7-Max、Qwen3.7-Plus、
//     MiniMax-M3.1-Flash 仅上下文窗口。

import (
	"encoding/json"
	"strings"
)

// qoderLevels 以固定顺序声明常见档位集合，避免每个模型重复字面量。
var (
	qoderLevelsLowMediumXHigh = []string{"low", "medium", "xhigh"}
	qoderLevelsLowHighMax     = []string{"low", "high", "max"}
	qoderLevelsHighMax        = []string{"high", "max"}
	qoderLevelsLowMediumHigh  = []string{"low", "medium", "high"}
)

// qoderThinkingEffortCatalog 是「模型 key → 已取证支持的档位（从低到高）」。
//
// 刻意**不登记**的 key 与原因：
//   - qmodel_latest（Qwen3.7-Max）/ qmodel（Qwen3.7-Plus）：Qoder 文档明确「仅支持
//     上下文窗口」，不支持思考强度 → 登记为空集，拒绝下发。
//   - gfmodel / kmodel：官方只写「支持思考强度」但**未公开档位清单**；二者官方
//     确实列在「同时支持上下文窗口和思考强度」的模型清单内，只是具体档位未披露，
//     按同族（GLM-5.3 / Kimi-K3 = low/high/max）外推 —— 这是**家族内档位外推**，
//     不是凭空猜测，已在测试里单独标注。
//   - q37fmodel / mmodel / auto：不在「支持思考强度」清单内或体系不同/档位
//     不可知 → 登记为空集，不下发。
var qoderThinkingEffortCatalog = map[string][]string{
	// Qwen 3.8 系：官方 low/medium/xhigh（无 max，无 high）。
	"qmodel_38max": qoderLevelsLowMediumXHigh,
	"qfmodel":      qoderLevelsLowMediumXHigh,

	// Qwen 3.7 系：Qoder 文档明确「仅支持上下文窗口」，不支持思考强度。
	"qmodel_latest": {},
	"qmodel":        {},
	// Qwen3.7-Flash：官方支持思考强度的模型清单里**没有**它（清单只列了
	// Qwen3.8-Max/Flash、DeepSeek 系、GLM 系、Kimi-K3/K2.8、Ultimate/Fast…），
	// 档位与是否支持双重未知 → 不下发（宁可不发，也不拿真实请求去试）。
	"q37fmodel": {},

	// DeepSeek 系。
	"dfmodel": qoderLevelsLowHighMax, // DeepSeek-V4-Flash：low/high/max
	"dmodel":  qoderLevelsHighMax,    // DeepSeek-V4-Pro：high/max

	// GLM 系。
	"gmodel":    qoderLevelsLowHighMax, // GLM-5.3：low/high/max
	"gfmodel":   qoderLevelsLowHighMax, // GLM-5.3-Flash：未公开清单，按 GLM-5.3 外推
	"gm51model": qoderLevelsHighMax,    // GLM-5.2：high/max

	// Kimi 系。
	"kmodel_latest": qoderLevelsLowHighMax, // Kimi-K3：low/high/max
	"kmodel":        qoderLevelsLowHighMax, // Kimi-K2.8-Preview：未公开清单，按 Kimi-K3 外推

	// MiniMax 与 auto：体系不同/档位不可知 → 不下发。
	"mmodel": {},
	"auto":   {},
}

// qoderThinkingEffortsForModel 返回模型 key 已取证支持的档位（从低到高）。
// 第二个返回值为 false 表示「该 key 不在能力表内」（未知模型，调用方不得下发）。
func qoderThinkingEffortsForModel(modelKey string) ([]string, bool) {
	key := strings.ToLower(strings.TrimSpace(lastOpenAIModelSegment(modelKey)))
	if key == "" {
		return nil, false
	}
	levels, ok := qoderThinkingEffortCatalog[key]
	if !ok {
		return nil, false
	}
	return levels, true
}

// qoderEffortRankForCatalog 返回档位在给定序列中的位置（-1 表示不在序列内）。
func qoderEffortRankForCatalog(levels []string, effort string) int {
	for i, level := range levels {
		if level == effort {
			return i
		}
	}
	return -1
}

// qoderResolveForwardEffort 把客户端请求的档位收敛到模型真实支持的集合。
//
// 语义（与 supportsOpenAIReasoningEffortMax 的 max 判定同一套口径，但更严格——
// 这里是**实际下发**，必须逐档命中）：
//   - 模型不支持思考强度（空集）→ 返回空串，调用方不下发任何档位；
//   - 档位精确命中 → 原样返回；
//   - 未命中（如对只支持 low/medium/xhigh 的 Qwen3.8 请求 max）→ 就近降级到
//     不超过它的最接近档位；低于全部档位时取最低档。
//
// 绝不返回模型不认的档位：上游对非法 reasoning_effort 的处置未取证，
// 盲发可能整请求被拒。
func qoderResolveForwardEffort(modelKey, effort string) string {
	normalized := NormalizeMaxReasoningEffort(effort)
	if normalized == "" {
		return ""
	}
	levels, ok := qoderThinkingEffortsForModel(modelKey)
	if !ok || len(levels) == 0 {
		return ""
	}
	if qoderEffortRankForCatalog(levels, normalized) >= 0 {
		return normalized
	}
	// 就近降级：向下取第一个不超过请求值的档位；没有更低档时取最低档。
	want, ok := reasoningEffortRank(normalized)
	if !ok {
		return ""
	}
	best := ""
	bestRank := -1
	for _, level := range levels {
		rank, ok := reasoningEffortRank(level)
		if !ok || rank > want {
			continue
		}
		if rank > bestRank {
			best, bestRank = level, rank
		}
	}
	if best != "" {
		return best
	}
	// 请求档位低于该模型全部档位 → 取最低档。
	lowest := levels[0]
	lowestRank, _ := reasoningEffortRank(lowest)
	for _, level := range levels[1:] {
		if rank, ok := reasoningEffortRank(level); ok && rank < lowestRank {
			lowest, lowestRank = level, rank
		}
	}
	return lowest
}

// qoderForwardedEffort 是「本次请求真正下发给上游的档位」的**唯一真源**
// （空串表示不下发）。
//
// 注入方（sendQoderUpstreamRequest）与记录方（forwardQoderChatCompletions 写
// usage_log.reasoning_effort）必须都只调本函数，否则两列口径会背离。实测背离形态
// （2026-10-29 探针）：客户端未带任何 effort 字段、仅模型名含 max（qwen3.8-max）时，
// extractOpenAIReasoningEffortFromBody 的**模型名后缀推导**会给出 xhigh，而真正
// 出站什么都没注入 —— 记录侧因此凭空多出一个档位（同时污染计费倍率与分组上限
// 策略的输入）。故这里只认**显式**档位字段（reasoning.effort / reasoning_effort /
// output_config.effort），不接受模型名后缀推导：Qoder 的 model 是型号名
// （Qwen3.8-Max 的 Max 是型号不是档位），后缀推导在封闭 key 上必然误判。
func qoderForwardedEffort(modelKey string, ccBody []byte) string {
	return qoderResolveForwardEffort(modelKey, explicitRequestedReasoningEffortFromBody(ccBody))
}

// ApplyQoderReasoningEffort 把**已收敛**的档位写进出站体 parameters
// （与模板自带的 max_tokens 平级；enable_thinking 与档位同时下发以真正开启思考）。
//
// forwarded 必须是 qoderForwardedEffort / qoderResolveForwardEffort 的产物：本函数
// 不再做能力判定，避免把「已收敛值」二次收敛成别的值。空串时**不动** body
// （交上游默认），绝不清除模板既有字段。
//
// 必须在 COSY 签名**之前**调用：签名是对最终 body 计算的，注入放在签名后会让
// 签名与实际请求体不一致（上游判 Signature invalid）。
func ApplyQoderReasoningEffort(body map[string]any, forwarded string) {
	if body == nil || forwarded == "" {
		return
	}
	params, ok := body["parameters"].(map[string]any)
	if !ok || params == nil {
		params = map[string]any{}
		body["parameters"] = params
	}
	params["enable_thinking"] = true
	params["reasoning_effort"] = forwarded
}

// applyQoderReasoningEffortToJSON 是 ApplyQoderReasoningEffort 的字节形态包装：
// 解析 → 注入 → 重新序列化。用于在 BuildQoderBody 之后、签名之前补注入，
// 保持 BuildQoderBody 既有签名（21 处测试调用点）不变。
//
// 无档位可下发时原样返回入参（零拷贝），保证「不支持思考强度的模型出站体
// 与改动前逐字节一致」。
func applyQoderReasoningEffortToJSON(prepared []byte, forwarded string) ([]byte, error) {
	if forwarded == "" {
		return prepared, nil
	}
	var body map[string]any
	if err := json.Unmarshal(prepared, &body); err != nil {
		return nil, err
	}
	ApplyQoderReasoningEffort(body, forwarded)
	return json.Marshal(body)
}
