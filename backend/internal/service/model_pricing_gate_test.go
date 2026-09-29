//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// newGateForTest 构造一个只依赖内置兜底价卡的定价闸门（不接渠道/远程目录），
// 用真实 NewBillingService 装全套内置价卡，避免手工假价卡与生产口径漂移。
func newGateForTest(t *testing.T, requirePriced bool) *ModelPricingGate {
	t.Helper()
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = requirePriced
	billing := NewBillingService(cfg, nil)
	resolver := NewModelPricingResolver(nil, billing)
	return NewModelPricingGate(resolver, cfg)
}

// TestModelPricingGate_BlocksUnpricedModel 主用例：
// 未配价的模型必须被判为「无定价」，这是准入拦截与列表过滤的共同前提。
func TestModelPricingGate_BlocksUnpricedModel(t *testing.T) {
	gate := newGateForTest(t, true)
	require.True(t, gate.Enabled())

	require.False(t, gate.HasPricing(context.Background(), "hy4-preview", nil),
		"未定价模型必须判定为无定价（否则仍会被免费放行）")
	require.False(t, gate.HasPricing(context.Background(), "totally-unpriced-model", nil))
}

// TestModelPricingGate_QoderDisplayNameAliasesArePriced 回归（2026-09-28 报障）：
// qwen3.8-flash 等 Qoder 展示名在出站链已归一为官方 key 计费（normalizeQoderModelKey），
// 定价链必须同口径归一——否则闸门将整个 qwen 系列判为未定价：模型广场整批剔除、
// 管理端候选消失、入口请求 404「no pricing is configured」。
func TestModelPricingGate_QoderDisplayNameAliasesArePriced(t *testing.T) {
	gate := newGateForTest(t, true)

	for _, model := range []string{
		"qwen3.8-max", "qwen3.8-flash", "qwen3.7-max", "qwen3.7-plus", "qwen3.7-flash",
		"Qwen3.8-Max", "Qwen3.8-Flash",
	} {
		require.Truef(t, gate.HasPricing(context.Background(), model, nil), "%s 必须判为已定价", model)
	}

	// 防误计价底线不变：闭集之外的 qwen 名称不得被兜底。
	require.False(t, gate.HasPricing(context.Background(), "qwen-max", nil))
	require.False(t, gate.HasPricing(context.Background(), "qwen3.9-flash", nil))
}

// TestModelPricingGate_AllowsPricedModel 正向不误伤：
// 命中内置兜底价卡的模型不得被拦，保证存量流量零误伤。
func TestModelPricingGate_AllowsPricedModel(t *testing.T) {
	gate := newGateForTest(t, true)

	require.True(t, gate.HasPricing(context.Background(), "deepseek-v4-flash", nil),
		"已定价模型不得被拦截")
	require.True(t, gate.HasPricing(context.Background(), "deepseek-v4.1-flash", nil),
		"deepseek- 前缀族按官方兜底卡计价，必须放行")
	// 大小写与空白归一化后同样命中。
	require.True(t, gate.HasPricing(context.Background(), "  DeepSeek-V4-Flash  ", nil))
}

// TestModelPricingGate_DisabledShortCircuits 开关关闭时必须完全回退旧行为（可回滚性）。
func TestModelPricingGate_DisabledShortCircuits(t *testing.T) {
	gate := newGateForTest(t, false)
	require.False(t, gate.Enabled())

	models := []string{"hy4-preview", "deepseek-v4-flash"}
	require.Equal(t, models, gate.FilterPriced(context.Background(), models, nil),
		"开关关闭时过滤必须原样返回")
}

// TestModelPricingGate_FilterPriced 列表过滤：保持顺序、去重、剔除未定价。
func TestModelPricingGate_FilterPriced(t *testing.T) {
	gate := newGateForTest(t, true)

	models := []string{"deepseek-v4-flash", "hy4-preview", "deepseek-v4-flash", "", "  "}
	got := gate.FilterPriced(context.Background(), models, nil)
	require.Equal(t, []string{"deepseek-v4-flash"}, got)
}

// TestModelPricingGate_GroupPricingWins 分组自定义价必须能让模型通过闸门
// （即使既不在渠道价也不在全局目录里）——口径① 的独立覆盖。
func TestModelPricingGate_GroupPricingWins(t *testing.T) {
	gate := newGateForTest(t, true)

	group := &Group{
		ID:       7,
		Platform: PlatformWorkbuddy,
		ModelPricing: []ChannelModelPricing{
			{
				Models:      []string{"hy4-preview"},
				InputPrice:  gatePtrFloat(1e-6),
				OutputPrice: gatePtrFloat(2e-6),
			},
		},
	}

	require.True(t, gate.HasPricing(context.Background(), "hy4-preview", group),
		"分组显式配价后必须放行（否则管理员配了价也不能用）")
	require.False(t, gate.HasPricing(context.Background(), "hy5-preview", group))
}

// TestModelPricingGate_EmptyGroupCardStillUnpriced 只填模型名不填价格的空卡
// 不能算「已定价」——那等于免费放行，与兜底告警场景性质相同。
func TestModelPricingGate_EmptyGroupCardStillUnpriced(t *testing.T) {
	gate := newGateForTest(t, true)

	group := &Group{
		ID:       8,
		Platform: PlatformWorkbuddy,
		ModelPricing: []ChannelModelPricing{
			{Models: []string{"hy4-preview"}},
		},
	}
	require.False(t, gate.HasPricing(context.Background(), "hy4-preview", group),
		"空价卡不得视为已定价")
}

// TestModelPricingGate_HasPricingForGroups 多渠道分组判定：任一分组命中即放行。
func TestModelPricingGate_HasPricingForGroups(t *testing.T) {
	gate := newGateForTest(t, true)

	groups := []*Group{
		{ID: 1, Platform: PlatformWorkbuddy},
		{
			ID:           2,
			Platform:     PlatformWorkbuddy,
			ModelPricing: []ChannelModelPricing{{Models: []string{"hy4-preview"}, InputPrice: gatePtrFloat(1e-6)}},
		},
	}

	require.True(t, gate.HasPricingForGroups(context.Background(), "hy4-preview", groups))
	require.False(t, gate.HasPricingForGroups(context.Background(), "unknown-xyz-model", groups))
}

// TestModelPricingGate_NilResolverFailsOpen 闸门未接线时必须放行（不误伤生产流量）。
func TestModelPricingGate_NilResolverFailsOpen(t *testing.T) {
	gate := NewModelPricingGate(nil, nil)
	require.False(t, gate.Enabled())
	require.True(t, gate.HasPricing(context.Background(), "anything", nil))
}

func gatePtrFloat(v float64) *float64 { return &v }
