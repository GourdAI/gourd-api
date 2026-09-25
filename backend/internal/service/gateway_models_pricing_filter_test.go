//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// gateway_models_pricing_filter_test.go 覆盖「模型列表不返回未定价模型」的实现面：
// GatewayService.GetAvailableModels 的聚合结果经 PricingGate.FilterPriced 后，
// 未定价模型必须消失。闸门本体已在 model_pricing_gate_test.go 覆盖。

// TestGetAvailableModels_PricingFilterRemovesUnpriced 列表过滤核心断言：
// 账号 model_mapping 里同时给出已定价与未定价模型，过滤后只剩已定价的。
func TestGetAvailableModels_PricingFilterRemovesUnpriced(t *testing.T) {
	groupID := int64(11)
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = true
	billing := NewBillingService(cfg, nil)
	gate := NewModelPricingGate(NewModelPricingResolver(nil, billing), cfg)

	raw := []string{"deepseek-v4-flash", "hy4-preview", "qwen3.8-flash", "deepseek-v4-pro"}
	got := gate.FilterPriced(context.Background(), raw, &Group{ID: groupID, Platform: PlatformWorkbuddy})

	require.Equal(t, []string{"deepseek-v4-flash", "deepseek-v4-pro"}, got,
		"未定价模型必须从可用模型列表剔除，且保持已定价模型的原有顺序")
}

// TestGetAvailableModels_PricingFilterDisabledKeepsAll 开关关闭时列表必须原样（可回滚）。
func TestGetAvailableModels_PricingFilterDisabledKeepsAll(t *testing.T) {
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = false
	billing := NewBillingService(cfg, nil)
	gate := NewModelPricingGate(NewModelPricingResolver(nil, billing), cfg)

	raw := []string{"hy4-preview", "qwen3.8-flash"}
	got := gate.FilterPriced(context.Background(), raw, &Group{ID: 11, Platform: PlatformWorkbuddy})
	require.Equal(t, raw, got)
}

// TestGetAvailableModels_PricingFilterWithGroupPrice 管理员给未定价模型配上分组价后，
// 它必须重新出现在列表里（否则「配了价仍不可见」是死锁）。
func TestGetAvailableModels_PricingFilterWithGroupPrice(t *testing.T) {
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = true
	billing := NewBillingService(cfg, nil)
	gate := NewModelPricingGate(NewModelPricingResolver(nil, billing), cfg)

	group := &Group{
		ID:       11,
		Platform: PlatformWorkbuddy,
		ModelPricing: []ChannelModelPricing{
			{Models: []string{"hy4-preview"}, InputPrice: gatePtrFloat(1e-6), OutputPrice: gatePtrFloat(2e-6)},
		},
	}
	got := gate.FilterPriced(context.Background(), []string{"hy4-preview", "qwen3.8-flash"}, group)
	require.Equal(t, []string{"hy4-preview"}, got,
		"配过分组价的模型必须可见；未配价的仍需剔除")
}

// 防止筛选逻辑意外依赖时间（保证相同输入稳定输出，便于生产排查对账）。
var _ = time.Second
