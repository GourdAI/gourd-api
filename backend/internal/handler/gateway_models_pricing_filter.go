package handler

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// gateway_models_pricing_filter.go 收口模型列表的「未定价模型不可见」过滤。
//
// 背景：网关入口已拒绝未定价模型（middleware.GroupModelAllowlist 的定价闸门），
// 模型列表若继续列出它们，会造成「列表可见 → 选中即 404」的语义倒挂，也会把根本
// 用不了的模型推给下游客户端。因此所有对外模型列表出口共用本文件的过滤器，判定
// 与计费链同源（service.ModelPricingGate）。
//
// 开关：pricing.require_priced_models 关闭时所有过滤器原样返回（零拷贝）。

// pricingGate 取网关定价闸门；服务未接线时返回一个未接线闸门（Enabled()==false，不拦截）。
func (h *GatewayHandler) pricingGate() *service.ModelPricingGate {
	if h == nil || h.gatewayService == nil {
		return service.NewModelPricingGate(nil, nil)
	}
	return h.gatewayService.PricingGate()
}

// PricingGate 暴露网关定价闸门，供路由层中间件复用于「未定价模型禁止调用」准入。
func (h *GatewayHandler) PricingGate() *service.ModelPricingGate {
	return h.pricingGate()
}

// filterPricedModels 过滤字符串模型 ID 列表（保持顺序、去重、剔除空串）。
func (h *GatewayHandler) filterPricedModels(ctx context.Context, models []string, group *service.Group) []string {
	if h == nil || len(models) == 0 {
		return models
	}
	return h.pricingGate().FilterPriced(ctx, models, group)
}

// filterPricedModelsForKey 以「候选分组并集」口径过滤字符串模型列表。
// 多分组 Key 下生效分组可能回退主分组，单分组判定会把「只在候选 B 配了价」的
// 模型误删；必须与准入闸门（middleware.ModelPricingAdmission）同源用并集，否则
// 会出「列表里看不见但能调用」或「看得见但一点就 404」的口径倒挂。
// 单分组 Key 时 PricingCandidateGroups 退化为 [apiKey.Group]，行为与旧实现逐字一致。
func (h *GatewayHandler) filterPricedModelsForKey(ctx context.Context, models []string, apiKey *service.APIKey) []string {
	if h == nil || len(models) == 0 {
		return models
	}
	return h.pricingGate().FilterPricedForGroups(ctx, models, service.PricingCandidateGroups(apiKey))
}

// filterPricedOpenAIModels 过滤 OpenAI 形态模型列表（并集口径）。
func (h *GatewayHandler) filterPricedOpenAIModels(c *gin.Context, models []openai.Model) []openai.Model {
	if h == nil || len(models) == 0 {
		return models
	}
	gate := h.pricingGate()
	if !gate.Enabled() {
		return models
	}
	groups := pricingGroupsFromContext(c)
	filtered := make([]openai.Model, 0, len(models))
	for _, model := range models {
		if gate.HasPricingForGroups(c.Request.Context(), trimModelPrefix(model.ID), groups) {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// filterPricedGeminiModels 过滤 Gemini 形态模型列表（ID 为 models/xxx 形式时按最后一段判定）。
func (h *GatewayHandler) filterPricedGeminiModels(c *gin.Context, models []geminicli.Model) []geminicli.Model {
	if h == nil || len(models) == 0 {
		return models
	}
	gate := h.pricingGate()
	if !gate.Enabled() {
		return models
	}
	groups := pricingGroupsFromContext(c)
	filtered := make([]geminicli.Model, 0, len(models))
	for _, model := range models {
		if gate.HasPricingForGroups(c.Request.Context(), trimModelPrefix(model.ID), groups) {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// filterPricedClaudeModels 过滤 Claude 形态模型列表（并集口径）。
func (h *GatewayHandler) filterPricedClaudeModels(c *gin.Context, models []claude.Model) []claude.Model {
	if h == nil || len(models) == 0 {
		return models
	}
	gate := h.pricingGate()
	if !gate.Enabled() {
		return models
	}
	groups := pricingGroupsFromContext(c)
	filtered := make([]claude.Model, 0, len(models))
	for _, model := range models {
		if gate.HasPricingForGroups(c.Request.Context(), trimModelPrefix(model.ID), groups) {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// groupFromContext 取认证中间件写入的 API Key 分组；缺失时返回 nil（闸门退化为只查全局价）。
// 仅适用于确实只有单分组语义的调用点；新代码请用 pricingGroupsFromContext。
func groupFromContext(c *gin.Context) *service.Group {
	apiKey := apiKeyFromPricingContext(c)
	if apiKey == nil {
		return nil
	}
	return apiKey.Group
}

// pricingGroupsFromContext 取当前 Key 的定价判定分组集（候选并集，生效分组优先）。
func pricingGroupsFromContext(c *gin.Context) []*service.Group {
	return service.PricingCandidateGroups(apiKeyFromPricingContext(c))
}

func apiKeyFromPricingContext(c *gin.Context) *service.APIKey {
	if c == nil {
		return nil
	}
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil {
		return nil
	}
	return apiKey
}

// trimModelPrefix 去掉 Gemini 原生形态的 "models/" 前缀，让定价查找对齐目录里的模型 ID。
func trimModelPrefix(id string) string {
	return strings.TrimPrefix(strings.TrimSpace(id), "models/")
}
