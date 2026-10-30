//go:build unit

package service

// qoder_thinking_effort_test.go 锁定 Qoder 思考强度的「真实下发 + 能力白名单」。
//
// 报障（2026-10-27）与定案：
//   - 后台用量显示 qwen3.8-max 请求 max、转发 xhigh。取证后确认这是**正确收敛**：
//     QwenCloud 与 Qoder CLI 文档均写 qwen3.8-max 仅支持 low/medium/xhigh（无 max，
//     也无 high），模型名里的 Max 是型号不是档位。
//   - 但核查暴露真缺陷：BuildQoderBody 从不写档位，上游永远收自己的默认档。
//     本文件锁定修复后的行为：档位按能力白名单收敛后真正写进 parameters，
//     不支持思考强度的模型一个字段都不加（出站体与改动前逐字节一致）。

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// qoderParamsOf 解出出站体的 parameters（不存在时返回 nil）。
func qoderParamsOf(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	require.NoError(t, json.Unmarshal(body, &obj))
	params, _ := obj["parameters"].(map[string]any)
	return params
}

// TestQoderForwardEffortHonorsModelCapability 覆盖「请求档位 → 实际下发档位」矩阵，
// 每个断言都对应一条已取证的官方档位清单。
func TestQoderForwardEffortHonorsModelCapability(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		effort  string
		want    string // 空串表示不下发
		comment string
	}{
		// Qwen3.8 系：官方 low/medium/xhigh。max 必须收敛为 xhigh（本次报障点）。
		{"qwen38max_max", "qmodel_38max", "max", "xhigh", "官方最高档就是 xhigh，max 收敛正确"},
		{"qwen38max_medium", "qmodel_38max", "medium", "medium", "命中档位原样下发"},
		{"qwen38max_high", "qmodel_38max", "high", "medium", "high 不在清单 → 就近降到 medium"},
		{"qwen38flash_max", "qfmodel", "max", "xhigh", "与 max 同理收敛"},

		// DeepSeek-V4-Flash：low/high/max → max 是真档位，必须保真。
		{"deepseek_flash_max", "dfmodel", "max", "max", "官方有 max，不得被折叠"},
		{"deepseek_flash_medium", "dfmodel", "medium", "low", "medium 不在清单 → 降到 low"},

		// DeepSeek-V4-Pro：high/max。
		{"deepseek_pro_low", "dmodel", "low", "high", "请求档低于清单最低档 → 取最低档"},

		// GLM 系：GLM-5.3 low/high/max；GLM-5.2 high/max。
		{"glm53_medium", "gmodel", "medium", "low", "medium 不在清单 → 降到 low"},
		{"glm52_low", "gm51model", "low", "high", "GLM-5.2 无 low → 取最低 high"},
		{"glm53flash_medium", "gfmodel", "medium", "low", "GLM-5.3-Flash 档位未公开，按同族外推"},

		// Kimi-K3：low/high/max。
		{"kimi_k3_xhigh", "kmodel_latest", "xhigh", "high", "xhigh 不在清单 → 降到 high"},
		{"kimi_k28_max", "kmodel", "max", "max", "K2.8 档位未公开，按 Kimi-K3 同族外推"},

		// 明确不支持思考强度的模型：一个字段都不下发。
		{"qwen37max_none", "qmodel_latest", "max", "", "Qwen3.7-Max 仅上下文窗口，不支持思考强度"},
		{"qwen37plus_none", "qmodel", "high", "", "Qwen3.7-Plus 同上"},
		{"qwen37flash_none", "q37fmodel", "max", "", "不在官方「支持思考强度」清单内 → 不下发"},
		{"minimax_none", "mmodel", "max", "", "MiniMax 用 thinking 体系，不认 reasoning_effort"},
		{"auto_none", "auto", "max", "", "路由入口档位不可知"},

		// 未知模型：不猜测、不下发。
		{"unknown_none", "zzmodel", "max", "", "未知 key 不得凭空下发档位"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := qoderResolveForwardEffort(tt.model, tt.effort)
			require.Equal(t, tt.want, got, "%s：%s", tt.model, tt.comment)
		})
	}
}

// TestApplyQoderReasoningEffortWritesParameters 锁定真实下发：档位与 enable_thinking
// 同时写进 parameters，且不破坏模板自带的 max_tokens。
func TestApplyQoderReasoningEffortWritesParameters(t *testing.T) {
	cc := []byte(`{"model":"qwen3.8-max","max_tokens":4096,"reasoning_effort":"max",
		"messages":[{"role":"user","content":"hi"}]}`)
	prepared, _, err := BuildQoderBody(cc, "qmodel_38max", "")
	require.NoError(t, err)

	// 改动前基线：模板与构造管线不含任何档位字段。
	base := qoderParamsOf(t, prepared)
	require.NotContains(t, base, "reasoning_effort", "改动前不该有档位（本用例的对照基线）")
	require.NotContains(t, base, "enable_thinking")

	withEffort, err := applyQoderReasoningEffortToJSON(prepared, qoderForwardedEffort("qmodel_38max", cc))
	require.NoError(t, err)
	params := qoderParamsOf(t, withEffort)
	require.Equal(t, "xhigh", params["reasoning_effort"], "Qwen3.8-Max 的 max 收敛为 xhigh 后真实下发")
	require.Equal(t, true, params["enable_thinking"], "下发档位必须同时开启思考")
	require.Contains(t, params, "max_tokens", "模板既有 max_tokens 不得被冲掉")
}

// TestQoderRecordedEffortMatchesForwarded 锁死「记录值 == 真实下发值」：两者必须
// 同源（qoderForwardedEffort），任何一侧单独把模型名后缀当档位都会造成背离。
//
// 背离形态（探针实测后修）：body 无任何 effort 字段、仅模型名带 max 时，
// extractOpenAIReasoningEffortFromBody 会从后缀推导出 xhigh，而 send 侧并未注入。
// 旧实现把该值写进 usage_log.reasoning_effort（计费倍率/上限策略的输入）→ 凭空计费。
func TestQoderRecordedEffortMatchesForwarded(t *testing.T) {
	tests := []struct {
		name     string
		modelKey string
		cc       string
		want     string // 期望的「记录值 == 下发值」，空串表示两边都不得有值
	}{
		{"suffix_max_not_a_level", "qmodel_38max", `{"model":"qwen3.8-max","messages":[]}`, ""},
		{"explicit_max_folded", "qmodel_38max", `{"model":"qwen3.8-max","reasoning_effort":"max","messages":[]}`, "xhigh"},
		{"reasoning_effort_nested", "qmodel_38max", `{"model":"qwen3.8-max","reasoning":{"effort":"medium"},"messages":[]}`, "medium"},
		{"output_config_effort", "gmodel", `{"model":"glm-5.3","output_config":{"effort":"max"},"messages":[]}`, "max"},
		{"unsupported_model", "qmodel_latest", `{"model":"qwen3.7-max","reasoning_effort":"max","messages":[]}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc := []byte(tt.cc)
			// 记录侧与注入侧必须调同一个函数，结果必然一致。
			forwarded := qoderForwardedEffort(tt.modelKey, cc)
			require.Equal(t, tt.want, forwarded, "下发档位")

			// 反向锁：旧实现（extract + 单独收敛）在「仅型号名带 max」上的结果与转发值不同。
			// 这保证本用例真能拦住「两侧不同源」的回归，而不是恒真的空断言。
			// 候选顺序必须与网关一致（upstreamModel, billingModel, originalModel）：
			// 封闭 key 的后缀是 "38max"、推不出档位，真正命中后缀推导的是展示名。
			if tt.name == "suffix_max_not_a_level" {
				legacy := extractOpenAIReasoningEffortFromBody(cc, tt.modelKey, tt.modelKey, "qwen3.8-max")
				require.NotNil(t, legacy, "旧实现确实会从型号名后缀推导出档位（背离来源）")
				require.Equal(t, "xhigh", *legacy, "旧实现凭空给出 xhigh")
				require.NotEqual(t, *legacy, forwarded, "两侧本就背离，本用例能拦住回归")
			}
		})
	}
}

// TestApplyQoderReasoningEffortLeavesUnsupportedModelsUntouched 锁定零副作用：
// 不支持思考强度的模型，出站体与改动前**逐字节一致**（不是「等价」）。
func TestApplyQoderReasoningEffortLeavesUnsupportedModelsUntouched(t *testing.T) {
	cc := []byte(`{"model":"qwen3.7-max","max_tokens":2048,"reasoning_effort":"max",
		"messages":[{"role":"user","content":"hi"}]}`)

	for _, model := range []string{"qmodel_latest", "qmodel", "q37fmodel", "mmodel", "auto", "zzmodel"} {
		t.Run(model, func(t *testing.T) {
			prepared, _, err := BuildQoderBody(cc, model, "")
			require.NoError(t, err)
			got, err := applyQoderReasoningEffortToJSON(prepared, qoderForwardedEffort(model, cc))
			require.NoError(t, err)
			require.Equal(t, string(prepared), string(got), "不支持思考强度的模型必须逐字节不变")

			params := qoderParamsOf(t, got)
			require.NotContains(t, params, "reasoning_effort")
			require.NotContains(t, params, "enable_thinking")
		})
	}
}

// TestApplyQoderReasoningEffortDeepSeekMaxNotFolded 反向护栏：DeepSeek-Flash 的 max
// 是官方档位，必须原样下发——防止把「Qwen 收敛」误做成「所有 Qoder 模型都折叠」。
func TestApplyQoderReasoningEffortDeepSeekMaxNotFolded(t *testing.T) {
	cc := []byte(`{"model":"deepseek-flash","reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`)
	prepared, _, err := BuildQoderBody(cc, "dfmodel", "")
	require.NoError(t, err)
	got, err := applyQoderReasoningEffortToJSON(prepared, qoderForwardedEffort("dfmodel", cc))
	require.NoError(t, err)
	require.Equal(t, "max", qoderParamsOf(t, got)["reasoning_effort"], "DeepSeek-Flash 官方支持 max，不得折叠")
}

// TestQoderForwardEffortFromRequestShapes 覆盖网关真实入参形态（三种档位字段），
// 确认 sendQoderUpstreamRequest 取到的就是客户端原始请求值。
func TestQoderForwardEffortFromRequestShapes(t *testing.T) {
	shapes := map[string][]byte{
		"reasoning_effort":     []byte(`{"model":"qwen3.8-max","reasoning_effort":"max","messages":[]}`),
		"reasoning.effort":     []byte(`{"model":"qwen3.8-max","reasoning":{"effort":"max"},"messages":[]}`),
		"output_config.effort": []byte(`{"model":"qwen3.8-max","output_config":{"effort":"max"},"messages":[]}`),
	}
	for name, cc := range shapes {
		t.Run(name, func(t *testing.T) {
			effort := explicitRequestedReasoningEffortFromBody(cc)
			require.Equal(t, "max", effort)
			require.Equal(t, "xhigh", qoderForwardedEffort("qmodel_38max", cc),
				"三种入参形态都必须能取到显式档位并收敛到真实下发的 xhigh")
		})
	}
}
