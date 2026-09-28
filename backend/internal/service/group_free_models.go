package service

import (
	"net/http"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// group_free_models.go 承载「分组级免费模型名单」。
//
// 存在的原因：网关有一条「未定价即拒绝」准入闸门（pricing.require_priced_models，默认开），
// 它与模型广场 / 可用渠道 / /v1/models 的列表过滤共用同一个判定源（ModelPricingGate）。
// 上游自带的免费模型（LiteLLM 目录未收录、或平台自有型号）在分组卡、渠道卡、目录、内置
// 兜底卡四处都解析不出定价，于是同时表现为「客户端 404 no pricing is configured」+
// 「广场上完全看不到」。本字段给管理员一个显式的定价豁免入口：名单内的模型在本分组
// 按「全 0 价卡」参与定价解析，因此调得通、算得出 0 元、也上得了广场。
//
// 语义边界（刻意收紧，避免变成第二个白名单）：
//   - 它只影响「定价」，不影响模型枚举与准入白名单。模型能否被服务仍由分组内账号 /
//     渠道配置决定；把名单里没有账号服务的模型塞进广场等于空头承诺，违背
//     「展示的即可调用」契约。
//   - 分组已为该模型配了**带实际价格**的价卡时，以价卡为准，名单不会把价格洗成 0；
//     空价卡（只绑模型名不填价）不足以定案，仍由名单兜底（见 resolveGroupModelPricing）。

// normalizeGroupFreeModels 归一化免费模型名单：TrimSpace、按小写去重保序、
// 拒绝非法通配位置。与 normalizeGroupModelAllowlist 同口径（通配符只允许在末尾），
// 但允许空名单——关闭态就是空数组，不存在「开启但为空」的配置错误。
func normalizeGroupFreeModels(models []string) ([]string, error) {
	if len(models) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if strings.Contains(strings.TrimSuffix(model, "*"), "*") {
			return nil, infraerrors.New(http.StatusBadRequest, "INVALID_GROUP_FREE_MODELS", `wildcard "*" is only allowed at the end of a free model entry`)
		}
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, model)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// groupFreeModelMatches 判断模型名是否命中免费名单条目。
// 大小写不敏感；条目末尾 * 视为前缀通配。与定价链其余模型名匹配一致，
// 先过 normalizeChannelPricingModelName（去空白 + 小写），保证
// "models/gemini-x" 之类的形态与价卡查找同源。
func groupFreeModelMatches(models []string, model string) bool {
	if len(models) == 0 {
		return false
	}
	target := normalizeChannelPricingModelName(model)
	if target == "" {
		return false
	}
	for _, entry := range models {
		pattern := normalizeChannelPricingModelName(entry)
		if pattern == "" {
			continue
		}
		if strings.HasSuffix(pattern, "*") {
			if strings.HasPrefix(target, strings.TrimSuffix(pattern, "*")) {
				return true
			}
			continue
		}
		if pattern == target {
			return true
		}
	}
	return false
}

// GroupModelIsFree 报告该分组是否把此模型声明为免费。
func (g *Group) GroupModelIsFree(model string) bool {
	return g != nil && groupFreeModelMatches(g.FreeModels, model)
}

// groupFreeModelPricing 合成免费名单的全 0 价卡。
//
// 关键：所有价格字段必须是**指向 0 的非 nil 指针**，而不是 nil。
//   - 定价闸门用 pricingNeedsFallback 判「有没有价」，它只认 nil；返回 nil 字段
//     等于没配价，闸门照样拦死。
//   - 展示链用 explicitContextPricingFields（判 `!= nil`）决定 0 元是否保留为
//     "$0.00"；指针为 nil 会退化成 "-"，用户看不到「免费」。
//
// BillingMode 留空即 token（resolveConfiguredPricing 的空 mode 兜底），
// 图片/按次价同样置 0，使免费声明覆盖全部计费形态。
func groupFreeModelPricing(model string) *ChannelModelPricing {
	zero := 0.0
	price := &zero
	return &ChannelModelPricing{
		Models:            []string{model},
		InputPrice:        price,
		OutputPrice:       price,
		CacheWritePrice:   price,
		CacheWrite1hPrice: price,
		CacheReadPrice:    price,
		ImageInputPrice:   price,
		ImageOutputPrice:  price,
		PerRequestPrice:   price,
	}
}
