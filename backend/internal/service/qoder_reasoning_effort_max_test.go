package service

// qoder_reasoning_effort_max_test.go 锁定 Qoder 封闭模型 key 的 max 档位判定。
//
// 报障场景：客户端对 Qoder 的 DeepSeek-Flash（key=dfmodel）请求 reasoning_effort
// ="max"，用量记录与回显却显示 xhigh —— 因为 supportsOpenAIReasoningEffortMax 的
// 白名单按品牌前缀（deepseek-v4/deepseek-flash/glm-/kimi-…）判定，而 Qoder 对外
// 只暴露封闭 key（dfmodel/dmodel/gmodel/kmodel_latest），前缀一律不命中，于是
// max 被静默折叠成 xhigh（usage_logs.reasoning_effort、max 计费倍率、上限策略同
// 口径跟着错）。修复：按静态目录把 key 还原为底层家族名后再走同一套前缀判定。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestQoderModelKeysKeepDistinctMaxTier 断言有独立 max 档的 Qoder key 不再被折叠。
func TestQoderModelKeysKeepDistinctMaxTier(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		// DeepSeek 系（上游 low/high/max）——用户报障的那个。
		{key: "dfmodel", want: "max"},
		{key: "dmodel", want: "max"},
		// GLM 系（glm-* 白名单）。
		{key: "gmodel", want: "max"},
		{key: "gfmodel", want: "max"},
		{key: "gm51model", want: "max"},
		// Kimi 系（kimi- 白名单）。
		{key: "kmodel_latest", want: "max"},
		{key: "kmodel", want: "max"},
		// Qwen 系与 MiniMax：无一手证据存在独立 max 档，必须保持旧折叠行为。
		{key: "qmodel_38max", want: "xhigh"},
		{key: "qfmodel", want: "xhigh"},
		{key: "qmodel_latest", want: "xhigh"},
		{key: "qmodel", want: "xhigh"},
		{key: "q37fmodel", want: "xhigh"},
		{key: "mmodel", want: "xhigh"},
		// auto 是智能路由入口，档位未知，同样不得宣告 max。
		{key: "auto", want: "xhigh"},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			require.Equal(t, tt.want, normalizeOpenAIReasoningEffortForModel("max", tt.key))
		})
	}
}

// TestQoderReasoningEffortModelScale 校验还原函数的边界：只认 Qoder 封闭 key，
// 其余模型名一律返回空串（保持调用方原判定），绝不给没取证过的模型族开 max。
func TestQoderReasoningEffortModelScale(t *testing.T) {
	require.Equal(t, "deepseek-flash", qoderReasoningEffortModelScale("dfmodel"))
	// 大小写与供应商前缀路径都要能还原。
	require.Equal(t, "deepseek-v4-pro", qoderReasoningEffortModelScale("DMODEL"))
	require.Equal(t, "kimi-k3", qoderReasoningEffortModelScale("qoder/kmodel_latest"))
	// 未知 key / 非 Qoder 模型名 / 空值 → 空串。
	require.Equal(t, "", qoderReasoningEffortModelScale("zzmodel"))
	require.Equal(t, "", qoderReasoningEffortModelScale("gpt-5.6-sol"))
	require.Equal(t, "", qoderReasoningEffortModelScale("deepseek-flash"))
	require.Equal(t, "", qoderReasoningEffortModelScale("  "))
	// 未知 key 走 max 判定时行为与修复前一致（折叠）。
	require.Equal(t, "xhigh", normalizeOpenAIReasoningEffortForModel("max", "zzmodel"))
}

// TestExtractQoderReasoningEffortKeepsMaxFromGatewayCandidates 复现 Qoder 网关的
// 真实调用形态：extractOpenAIReasoningEffortFromBody(body, upstreamModel,
// billingModel, originalModel)，第一个非空候选是映射后 key（dfmodel）。
// 修复前该形态返回 xhigh（首候选是封闭 key），而只有展示名候选时才返回 max，
// 说明折叠点就在 key 判定上。
func TestExtractQoderReasoningEffortKeepsMaxFromGatewayCandidates(t *testing.T) {
	nested := []byte(`{"model":"deepseek-flash","reasoning":{"effort":"max"},"messages":[]}`)
	flat := []byte(`{"model":"dfmodel","reasoning_effort":"max","messages":[]}`)

	for _, body := range [][]byte{nested, flat} {
		got := extractOpenAIReasoningEffortFromBody(body, "dfmodel", "dfmodel", "deepseek-flash")
		require.NotNil(t, got)
		require.Equal(t, "max", *got, "body=%s", string(body))
	}

	// GLM key 同理；Qwen key 仍折叠，防止「所有 Qoder key 都算 max」的过宽修复。
	glm := []byte(`{"model":"gmodel","reasoning_effort":"max","messages":[]}`)
	require.Equal(t, "max", *extractOpenAIReasoningEffortFromBody(glm, "gmodel", "gmodel", "glm-5.3"))
	qwen := []byte(`{"model":"qfmodel","reasoning_effort":"max","messages":[]}`)
	require.Equal(t, "xhigh", *extractOpenAIReasoningEffortFromBody(qwen, "qfmodel", "qfmodel", "qwen3.8-flash"))

	// 非 max 档位不受影响。
	high := []byte(`{"model":"dfmodel","reasoning_effort":"high","messages":[]}`)
	require.Equal(t, "high", *extractOpenAIReasoningEffortFromBody(high, "dfmodel"))
}
