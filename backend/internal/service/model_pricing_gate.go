package service

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// ModelPricingGate 回答「这个模型有没有定价」，并据此决定请求准入与模型列表可见性。
//
// 存在的原因：定价此前只在「转发成功之后落账」时被读取，算不出价就零成本入账并打
// openai_usage.pricing_missing_record_zero_cost 告警（见 openai_gateway_usage.go
// 的 recordOpenAIUsageCost 分支）。也就是说未定价模型可以被随意调用却不产生任何
// 费用，而模型列表接口又不查定价，把这类模型继续暴露给下游选择——两头都漏。
//
// 判定口径与计费链 ModelPricingResolver.Resolve 同源（Group → Channel → LiteLLM →
// Fallback，任一命中即视为已定价），避免出现「列表放行了但计费 fail-closed」或
// 「拦住了一个其实算得出价的模型」这类口径倒挂：
//  1. 分组自定义定价 groups.model_pricing
//  2. 渠道定价 channel_model_pricing（含 OpenAI/Codex 变体名归一化二次查）
//  3. LiteLLM 官方价格目录（含系列匹配）
//  4. 进程内置兜底价卡 billingService.fallbackPrices
//
// 1/2 命中但整张卡所有价格字段为空时按「未定价」处理：管理员只填了模型名没填价格
// 等于免费放行，性质与兜底告警相同。
type ModelPricingGate struct {
	resolver *ModelPricingResolver
	cfg      *config.Config
}

// NewModelPricingGate 创建定价准入闸门。cfg 可为 nil——此时回落到 resolver 持有的
// BillingService 配置；仍取不到开关时闸门按「关闭」处理（不拦截），宁可放过也不要
// 因为接线不全把线上流量打死。
func NewModelPricingGate(resolver *ModelPricingResolver, cfg *config.Config) *ModelPricingGate {
	return &ModelPricingGate{resolver: resolver, cfg: cfg}
}

// Enabled 返回「未定价即拒绝」开关是否生效（pricing.require_priced_models）。
func (g *ModelPricingGate) Enabled() bool {
	if g == nil || g.resolver == nil {
		return false
	}
	cfg := g.cfg
	if cfg == nil && g.resolver.billingService != nil {
		cfg = g.resolver.billingService.cfg
	}
	return cfg != nil && cfg.Pricing.RequirePricedModels
}

// HasPricing 判断模型能否解析出定价。group 为 nil 时退化为只查全局价（LiteLLM 目录 +
// 内置兜底卡），即无分组上下文（渠道视图、模型广场）下仍拦得住真正查无价格的模型。
//
// 返回 true 的非定价情形刻意保守：闸门未接线、价格服务缺失。
func (g *ModelPricingGate) HasPricing(ctx context.Context, model string, group *Group) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	if g == nil || g.resolver == nil {
		return true
	}
	if group != nil {
		if hasGroupModelPricing(group, model) {
			return true
		}
		if group.ID > 0 && g.resolver.hasChannelPricingNormalized(ctx, group.ID, model) {
			return true
		}
	}
	if g.resolver.billingService == nil {
		return true
	}
	_, err := g.resolver.billingService.GetModelPricing(model)
	return err == nil
}

// HasPricingForGroups 判定模型在给定任一分组下是否可解析出定价：逐一按分组上下文
// 尝试（分组自定义价 → 渠道价 → 全局价），任一命中即 true。
// 无分组上下文时退化为只查全局价。
func (g *ModelPricingGate) HasPricingForGroups(ctx context.Context, model string, groups []*Group) bool {
	if g == nil || g.resolver == nil {
		return true
	}
	if len(groups) == 0 {
		return g.HasPricing(ctx, model, nil)
	}
	for _, group := range groups {
		if group == nil {
			continue
		}
		if g.HasPricing(ctx, model, group) {
			return true
		}
	}
	return false
}

// FilterPriced 按 HasPricing 过滤模型列表，保持入参顺序并去重。
// 开关关闭或闸门未接线时原样返回，保证可回滚。
func (g *ModelPricingGate) FilterPriced(ctx context.Context, models []string, group *Group) []string {
	if len(models) == 0 || !g.Enabled() {
		return models
	}
	filtered := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		trimmed := strings.TrimSpace(model)
		if trimmed == "" {
			continue
		}
		key := trimmed
		if _, ok := seen[key]; ok {
			continue
		}
		if !g.HasPricing(ctx, trimmed, group) {
			continue
		}
		seen[key] = struct{}{}
		filtered = append(filtered, trimmed)
	}
	return filtered
}

// FilterPricedForGroups 按并集口径过滤模型列表：任一候选分组能解析出定价即保留。
// 与 FilterPriced 的区别仅在判定集，去重/保序/开关行为完全一致；开关关闭或未接线
// 时原样返回。多分组 Key 的模型列表必须走本方法，否则会把「只在另一个分组配了价」
// 的模型整列删掉（与准入闸门口径也必须一致，否则会出「列表可见但一点就 404」）。
func (g *ModelPricingGate) FilterPricedForGroups(ctx context.Context, models []string, groups []*Group) []string {
	if len(models) == 0 || !g.Enabled() {
		return models
	}
	filtered := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		trimmed := strings.TrimSpace(model)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		if !g.HasPricingForGroups(ctx, trimmed, groups) {
			continue
		}
		seen[trimmed] = struct{}{}
		filtered = append(filtered, trimmed)
	}
	return filtered
}

// NewModelPricingGateForPlatformService 供不持有 resolver 的服务（渠道视图）按需构造。
func NewModelPricingGateForPlatformService(resolver *ModelPricingResolver) *ModelPricingGate {
	return NewModelPricingGate(resolver, nil)
}

// PricingGate 暴露网关自身的定价闸门，供入口中间件与模型列表复用同一判定源。
func (s *GatewayService) PricingGate() *ModelPricingGate {
	if s == nil {
		return NewModelPricingGate(nil, nil)
	}
	return NewModelPricingGate(s.resolver, s.cfg)
}

// PricingGate 暴露 OpenAI 网关自身的定价闸门（Responses WebSocket 逐帧校验用）。
func (s *OpenAIGatewayService) PricingGate() *ModelPricingGate {
	if s == nil {
		return NewModelPricingGate(nil, nil)
	}
	return NewModelPricingGate(s.resolver, s.cfg)
}
