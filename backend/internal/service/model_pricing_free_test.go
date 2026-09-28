//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 探针目的
//
// 验证「显式 0 价（免费）」价卡在两条判定链上的实际行为：
//  1. 定价准入闸门 ModelPricingGate.HasPricing（model_pricing_gate.go）
//  2. 模型广场 ModelPlazaService.ListGroups（model_plaza_service.go:230 过滤）
//
// 三条定价源逐级隔离，避免「内置兜底卡刚好有这个模型」混淆结论：
//   - 分组自定义价卡（Group.ModelPricing）
//   - 渠道价卡（Channel.ModelPricing → ChannelService.HasChannelModelPricing）
//   - 两者皆走一遍「有全局兜底价」与「无任何全局价」的模型名
//
// 期望值（先写下判断，再用实际运行结果验证/推翻）：
//   显式 0 价 == 已定价（因为 pricingNeedsFallback 只认 nil，不认 0）。
// ---------------------------------------------------------------------------

func freePtr(v float64) *float64 { return &v }

const (
	// freeProbeModelWithGlobalPrice 在内置兜底价卡里存在（billing_service.go:730
	// s.fallbackPrices["glm-4.5-flash"]），因此闸门最后一步 GetModelPricing 一定成功。
	freeProbeModelWithGlobalPrice = "glm-4.5-flash"
	// freeProbeModelNoGlobalPrice 命中不了 LiteLLM 目录、也命中不了内置兜底卡的
	// 任何系列子串规则 —— 它是「仅靠 0 价卡定价」的真正隔离探针。
	freeProbeModelNoGlobalPrice = "zzfree-probe-9x"
)

// newFreeGate 构造闸门：不接渠道服务（channelService=nil），因此
// resolver.hasChannelPricingNormalized 恒为 false，只剩「分组卡」与「全局价」两个变量。
func newFreeGate(t *testing.T, requirePriced bool) (*ModelPricingGate, *BillingService) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = requirePriced
	billing := NewBillingService(cfg, nil)
	resolver := NewModelPricingResolver(nil, billing)
	return NewModelPricingGate(resolver, cfg), billing
}

// ---------------------------------------------------------------------------
// 探针 1：分组自定义价卡 0 价
// ---------------------------------------------------------------------------

func TestFreePricing_GroupCardZeroPricePassesGate(t *testing.T) {
	gate, _ := newFreeGate(t, true)
	require.True(t, gate.Enabled(), "前置：闸门必须处于开启态")

	ctx := context.Background()

	// 1a. 用户指定的原始形态：glm-4.5-flash + 分组 0 价卡。
	group := &Group{
		ID:       7,
		Platform: PlatformZhipu,
		ModelPricing: []ChannelModelPricing{{
			Platform:    PlatformZhipu,
			Models:      []string{freeProbeModelWithGlobalPrice},
			BillingMode: BillingModeToken,
			InputPrice:  freePtr(0),
			OutputPrice: freePtr(0),
		}},
	}
	require.True(t, hasGroupModelPricing(group, freeProbeModelWithGlobalPrice),
		"分组 0 价卡应被 hasGroupModelPricing 视为已定价（pricingNeedsFallback 只认 nil）")
	require.True(t, gate.HasPricing(ctx, freeProbeModelWithGlobalPrice, group),
		"探针1a 预期：显式 0 价的分组卡能让模型过闸")

	// 1b. 对照（隔离内置兜底卡的干扰）：同名模型换成兜底卡里查不到的名字，
	// 只有分组 0 价卡这一个定价源。若此断言为 false，说明闸门其实不认 0 价卡，
	// 1a 的通过是兜底卡的功劳。
	groupNoGlobal := &Group{
		ID:       8,
		Platform: PlatformZhipu,
		ModelPricing: []ChannelModelPricing{{
			Platform:    PlatformZhipu,
			Models:      []string{freeProbeModelNoGlobalPrice},
			BillingMode: BillingModeToken,
			InputPrice:  freePtr(0),
			OutputPrice: freePtr(0),
		}},
	}
	// 前置自证：该模型在全局价链上确实查不到（否则 1b 无隔离意义）。
	billing := NewBillingService(&config.Config{}, nil)
	_, globalErr := billing.GetModelPricing(freeProbeModelNoGlobalPrice)
	require.Error(t, globalErr, "前置自证失败：探针模型本应无任何全局/兜底价")

	require.True(t, hasGroupModelPricing(groupNoGlobal, freeProbeModelNoGlobalPrice),
		"探针1b 预期：分组 0 价卡单独即可判定为已定价")
	require.True(t, gate.HasPricing(ctx, freeProbeModelNoGlobalPrice, groupNoGlobal),
		"探针1b 预期：仅靠分组 0 价卡（全局无价）也能过闸")

	// 1c. 反向对照：完全空价卡（只填模型名）必须被判为未定价。
	emptyCardGroup := &Group{
		ID:           9,
		Platform:     PlatformZhipu,
		ModelPricing: []ChannelModelPricing{{Models: []string{freeProbeModelNoGlobalPrice}, BillingMode: BillingModeToken}},
	}
	require.False(t, hasGroupModelPricing(emptyCardGroup, freeProbeModelNoGlobalPrice),
		"前置：空价卡必须判为未定价（与 0 价卡的区别就是 nil vs 指向 0 的指针）")
	require.False(t, gate.HasPricing(ctx, freeProbeModelNoGlobalPrice, emptyCardGroup),
		"前置：空价卡 + 全局无价 → 闸门拦截")
}

// ---------------------------------------------------------------------------
// 探针 2：渠道价卡 0 价（ChannelService.HasChannelModelPricing 全链路）
// ---------------------------------------------------------------------------

// newFreeChannelGate 用现成的 mockChannelRepository 构造接渠道服务的闸门，
// 与 model_pricing_resolver_test.go 的 newResolverWithChannel 同一套路。
func newFreeChannelGate(t *testing.T, groupID int64, groupPlatform string, pricing []ChannelModelPricing) *ModelPricingGate {
	t.Helper()
	repo := &mockChannelRepository{
		listAllFn: func(_ context.Context) ([]Channel, error) {
			return []Channel{{
				ID:           1,
				Name:         "free-probe-channel",
				Status:       StatusActive,
				GroupIDs:     []int64{groupID},
				ModelPricing: pricing,
			}}, nil
		},
		getGroupPlatformsFn: func(_ context.Context, _ []int64) (map[int64]string, error) {
			return map[int64]string{groupID: groupPlatform}, nil
		},
	}
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = true
	cs := NewChannelService(repo, nil, nil, nil, nil)
	billing := NewBillingService(cfg, nil)
	return NewModelPricingGate(NewModelPricingResolver(cs, billing), cfg)
}

func TestFreePricing_ChannelCardZeroPricePassesGate(t *testing.T) {
	const groupID = int64(100)
	ctx := context.Background()

	zeroCard := func(model string) []ChannelModelPricing {
		return []ChannelModelPricing{{
			Platform:    PlatformZhipu,
			Models:      []string{model},
			BillingMode: BillingModeToken,
			InputPrice:  freePtr(0),
			OutputPrice: freePtr(0),
		}}
	}

	// 2a. 最小面直测 pricingNeedsFallback：指向 0 的指针不得被判为「需要回落」。
	require.False(t, pricingNeedsFallback(&ChannelModelPricing{
		BillingMode: BillingModeToken, InputPrice: freePtr(0), OutputPrice: freePtr(0),
	}), "探针2a 预期：全 0 但非 nil 的价格指针 = 已配置，不需要回落")
	require.True(t, pricingNeedsFallback(&ChannelModelPricing{BillingMode: BillingModeToken}),
		"前置：字段全 nil 的空卡才需要回落（判为未定价）")

	// 2b. 渠道查找层：HasChannelModelPricing 对 0 价卡是否放行。
	gate := newFreeChannelGate(t, groupID, PlatformZhipu, zeroCard(freeProbeModelNoGlobalPrice))
	cs := gate.resolver.channelService
	require.True(t, cs.HasChannelModelPricing(ctx, groupID, freeProbeModelNoGlobalPrice),
		"探针2b 预期：渠道 0 价卡被 HasChannelModelPricing 判为有价")

	// 2c. 闸门端到端（渠道卡是唯一定价源，全局价查不到）。
	group := &Group{ID: groupID, Platform: PlatformZhipu}
	require.True(t, gate.resolver.hasChannelPricingNormalized(ctx, groupID, freeProbeModelNoGlobalPrice),
		"探针2c 预期：resolver 的渠道归一化查找命中 0 价卡")
	require.True(t, gate.HasPricing(ctx, freeProbeModelNoGlobalPrice, group),
		"探针2c 预期：仅靠渠道 0 价卡，模型即可过闸门")

	// 2d. 反向对照：渠道空价卡必须拦。
	gateEmpty := newFreeChannelGate(t, groupID, PlatformZhipu,
		[]ChannelModelPricing{{Platform: PlatformZhipu, Models: []string{freeProbeModelNoGlobalPrice}, BillingMode: BillingModeToken}})
	require.False(t, gateEmpty.resolver.channelService.HasChannelModelPricing(ctx, groupID, freeProbeModelNoGlobalPrice),
		"前置：渠道空价卡判为无价")
	require.False(t, gateEmpty.HasPricing(ctx, freeProbeModelNoGlobalPrice, group),
		"前置：渠道空价卡 + 全局无价 → 闸门拦截")
}

// ---------------------------------------------------------------------------
// 探针 3：模型广场 ListGroups
// ---------------------------------------------------------------------------

// newFreePlazaService 复用 model_plaza_service_test.go 的 stub 组合。
// 注意：ListGroups 内部是 NewModelPricingGate(s.resolver, nil)（cfg 传 nil），
// 因此闸门开关只能从 billingService.cfg 回落取得 —— billingService 必须带
// RequirePricedModels=true 的 cfg，否则广场过滤分支根本不会执行。
func newFreePlazaService(channels []Channel, groups []Group, groupPlatforms map[int64]string) *ModelPlazaService {
	repo := &mockChannelRepository{
		listAllFn: func(_ context.Context) ([]Channel, error) { return channels, nil },
		getGroupPlatformsFn: func(_ context.Context, _ []int64) (map[int64]string, error) {
			return groupPlatforms, nil
		},
	}
	cs := NewChannelService(repo, nil, nil, nil, nil)
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = true
	bs := NewBillingService(cfg, nil)
	return NewModelPlazaService(repo, &stubGroupRepoForAvailable{activeGroups: groups}, nil, bs, NewModelPricingResolver(cs, bs), nil)
}

func TestFreePricing_PlazaListGroupsShowsZeroPricedModel(t *testing.T) {
	ctx := context.Background()

	// 3a. 用户指定形态：分组只挂一个「靠 0 价卡定价」的模型（该模型有内置兜底价）。
	channels := []Channel{{
		ID: 1, Name: "zhipu-free", Status: StatusActive, GroupIDs: []int64{10},
		ModelPricing: []ChannelModelPricing{{
			Platform: PlatformZhipu, Models: []string{freeProbeModelWithGlobalPrice},
			BillingMode: BillingModeToken, InputPrice: freePtr(0), OutputPrice: freePtr(0),
		}},
	}}
	groups := []Group{{ID: 10, Name: "free-glm", Platform: PlatformZhipu, RateMultiplier: 1}}

	out, err := newFreePlazaService(channels, groups, map[int64]string{10: PlatformZhipu}).ListGroups(ctx)
	require.NoError(t, err)
	require.Len(t, out, 1, "探针3a 预期：仅 0 价卡的模型所在分组仍被广场返回")
	require.Equal(t, []string{freeProbeModelWithGlobalPrice}, plazaNames(out[0].Models))

	m := out[0].Models[0]
	require.NotNil(t, m.Pricing, "探针3a 预期：PlazaModel.Pricing 非 nil")
	require.NotNil(t, m.Pricing.InputPrice, "探针3a 预期：InputPrice 是指向 0 的非 nil 指针（而非 nil）")
	require.NotNil(t, m.Pricing.OutputPrice, "探针3a 预期：OutputPrice 是指向 0 的非 nil 指针（而非 nil）")
	require.Zero(t, *m.Pricing.InputPrice, "探针3a 预期：展示单价为 0")
	require.Zero(t, *m.Pricing.OutputPrice, "探针3a 预期：展示单价为 0")
	// 实测观察（首轮我误判为「应非 nil」，实际为 nil）：价卡里没写的价项，即使在
	// 实收合成链里探针算出 0，也会被 contextPricePtr 归一为 nil —— 即「未显式配置 =
	// 不展示」，与「显式配 0 = 展示 0」是两套语义。这条不是 0 价缺陷，是刻意设计。
	require.Nil(t, m.Pricing.CacheReadPrice,
		"实测：价卡未配置的 CacheReadPrice 被归一为 nil（显式 0 与未配置在展示层可区分）")

	// 3a-补. 显式把缓存价也配成 0：此时应保留为指向 0 的非 nil 指针。
	cacheZero := []Channel{{
		ID: 4, Name: "zhipu-free-cache", Status: StatusActive, GroupIDs: []int64{13},
		ModelPricing: []ChannelModelPricing{{
			Platform: PlatformZhipu, Models: []string{freeProbeModelWithGlobalPrice},
			BillingMode: BillingModeToken, InputPrice: freePtr(0), OutputPrice: freePtr(0),
			CacheReadPrice: freePtr(0), CacheWritePrice: freePtr(0),
		}},
	}}
	cacheZeroGroups := []Group{{ID: 13, Name: "free-glm-cache", Platform: PlatformZhipu, RateMultiplier: 1}}
	outCache, err := newFreePlazaService(cacheZero, cacheZeroGroups, map[int64]string{13: PlatformZhipu}).ListGroups(ctx)
	require.NoError(t, err)
	require.Len(t, outCache, 1)
	mc := outCache[0].Models[0]
	require.NotNil(t, mc.Pricing.CacheReadPrice, "探针3a-补 预期：显式配 0 的缓存读价保留为非 nil 指针")
	require.NotNil(t, mc.Pricing.CacheWritePrice, "探针3a-补 预期：显式配 0 的缓存写价保留为非 nil 指针")
	require.Zero(t, *mc.Pricing.CacheReadPrice)
	require.Zero(t, *mc.Pricing.CacheWritePrice)

	// 3b. 隔离对照：换成全局/兜底都查不到的模型名，0 价卡是唯一定价源。
	// 若 3b 的模型从广场消失或价格指针变 nil，则说明「仅靠 0 价卡上架」不成立。
	channelsNoGlobal := []Channel{{
		ID: 2, Name: "zhipu-free-iso", Status: StatusActive, GroupIDs: []int64{11},
		ModelPricing: []ChannelModelPricing{{
			Platform: PlatformZhipu, Models: []string{freeProbeModelNoGlobalPrice},
			BillingMode: BillingModeToken, InputPrice: freePtr(0), OutputPrice: freePtr(0),
		}},
	}}
	groupsNoGlobal := []Group{{ID: 11, Name: "free-iso", Platform: PlatformZhipu, RateMultiplier: 1}}

	out2, err := newFreePlazaService(channelsNoGlobal, groupsNoGlobal, map[int64]string{11: PlatformZhipu}).ListGroups(ctx)
	require.NoError(t, err)
	require.Len(t, out2, 1, "探针3b 预期：仅靠渠道 0 价卡（无任何全局价）的分组仍上架")
	require.Equal(t, []string{freeProbeModelNoGlobalPrice}, plazaNames(out2[0].Models))

	m2 := out2[0].Models[0]
	require.NotNil(t, m2.Pricing)
	require.NotNil(t, m2.Pricing.InputPrice, "探针3b 预期：InputPrice 保留指向 0 的指针")
	require.NotNil(t, m2.Pricing.OutputPrice, "探针3b 预期：OutputPrice 保留指向 0 的指针")

	// 3c. 反向对照：空价卡模型必须被广场过滤掉（证明过滤分支确实生效，
	// 3a/3b 不是因为「闸门没跑」而侥幸通过）。
	channelsEmpty := []Channel{{
		ID: 3, Name: "zhipu-empty", Status: StatusActive, GroupIDs: []int64{12},
		ModelPricing: []ChannelModelPricing{{
			Platform: PlatformZhipu, Models: []string{freeProbeModelNoGlobalPrice}, BillingMode: BillingModeToken,
		}},
	}}
	groupsEmpty := []Group{{ID: 12, Name: "empty-card", Platform: PlatformZhipu, RateMultiplier: 1}}
	out3, err := newFreePlazaService(channelsEmpty, groupsEmpty, map[int64]string{12: PlatformZhipu}).ListGroups(ctx)
	require.NoError(t, err)
	require.Empty(t, out3, "前置：空价卡 + 全局无价 → 分组整体不上架（说明闸门过滤分支已生效）")
}
