//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 分组级「免费模型名单」的行为规格（group_free_models.go + resolveGroupModelPricing）。
//
// 必须守住的口径：
//  1. 名单命中的模型在定价层等价于「全 0 价卡」，因此能通过「未定价即拒绝」闸门；
//  2. 分组已配了带实际价格的价卡时，价卡优先——名单不得把真实价格洗成 0；
//  3. 空价卡（只填模型名）不足以推翻名单；
//  4. 展示价必须是「指向 0 的非 nil 指针」，前端才能渲染 $0.00 而不是 "-"。

func freePrice(v float64) *float64 { return &v }

func TestNormalizeGroupFreeModels(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		want    []string
		wantErr string
	}{
		{name: "nil 入参", in: nil, want: nil},
		{name: "空数组", in: []string{}, want: nil},
		{name: "全空白条目视作空", in: []string{"  ", ""}, want: nil},
		{
			name: "trim 后保序",
			in:   []string{" glm-4.5-flash ", "trae-solo"},
			want: []string{"glm-4.5-flash", "trae-solo"},
		},
		{
			name: "大小写不敏感去重，保留首次出现的原形",
			in:   []string{"GLM-4.5-Flash", "glm-4.5-flash", "kimi-*"},
			want: []string{"GLM-4.5-Flash", "kimi-*"},
		},
		{
			name:    "非尾部通配符拒绝",
			in:      []string{"gpt-*-5.4"},
			wantErr: "INVALID_GROUP_FREE_MODELS",
		},
		{
			name:    "中间含通配符拒绝（即使末尾也有）",
			in:      []string{"kimi-*-linear-*"},
			wantErr: "INVALID_GROUP_FREE_MODELS",
		},
		{name: "尾部通配符合法", in: []string{"kimi-*"}, want: []string{"kimi-*"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeGroupFreeModels(tc.in)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestGroupFreeModelMatches(t *testing.T) {
	cases := []struct {
		name    string
		entries []string
		model   string
		want    bool
	}{
		{name: "空名单永不命中", entries: nil, model: "glm-4.5-flash", want: false},
		{name: "精确命中", entries: []string{"glm-4.5-flash"}, model: "glm-4.5-flash", want: true},
		{name: "大小写不敏感", entries: []string{"GLM-4.5-Flash"}, model: "glm-4.5-flash", want: true},
		{name: "条目与请求名都带空白", entries: []string{" glm-4.5-flash "}, model: " glm-4.5-flash ", want: true},
		{name: "尾部通配前缀命中", entries: []string{"kimi-*"}, model: "kimi-k2.5-turbo", want: true},
		{name: "尾部通配不命中其他前缀", entries: []string{"kimi-*"}, model: "glm-4.5-flash", want: false},
		{name: "单独 * 命中一切非空名", entries: []string{"*"}, model: "anything", want: true},
		{name: "空模型名不命中", entries: []string{"glm-*"}, model: "   ", want: false},
		{name: "空条目跳过", entries: []string{"", "glm-4.5-flash"}, model: "glm-4.5-flash", want: true},
		{name: "精确条目要求全等而非前缀", entries: []string{"glm-4.5"}, model: "glm-4.5-flash", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, groupFreeModelMatches(tc.entries, tc.model))
		})
	}
}

// 定价收口的优先级：带真实价格的分组价卡 > 免费名单 > 渠道/目录/兜底。
func TestResolveGroupModelPricingPriority(t *testing.T) {
	const model = "zzfree-probe-9x"

	t.Run("名单命中合成全 0 卡", func(t *testing.T) {
		group := &Group{ID: 1, Platform: PlatformZhipu, FreeModels: []string{model}}
		pricing, fromFreeList := resolveGroupModelPricing(group, model)
		require.NotNil(t, pricing, "名单命中必须给出价卡，否则闸门判未定价")
		require.True(t, fromFreeList, "名单合成的卡必须标记来自免费名单，否则 Resolve 不会整张清零")
		// 关键：必须是「非 nil 指针 + 值为 0」，nil 会被 pricingNeedsFallback 认成没价。
		require.NotNil(t, pricing.InputPrice)
		require.Zero(t, *pricing.InputPrice)
		require.NotNil(t, pricing.OutputPrice)
		require.Zero(t, *pricing.OutputPrice)
		require.NotNil(t, pricing.PerRequestPrice)
		require.Zero(t, *pricing.PerRequestPrice)
		require.False(t, pricingNeedsFallback(pricing), "全 0 卡必须被认作已定价")
	})

	t.Run("真实价卡优先于名单", func(t *testing.T) {
		group := &Group{
			ID:         2,
			Platform:   PlatformZhipu,
			FreeModels: []string{model},
			ModelPricing: []ChannelModelPricing{{
				Platform:    PlatformZhipu,
				Models:      []string{model},
				BillingMode: BillingModeToken,
				InputPrice:  freePrice(2e-6),
			}},
		}
		pricing, fromFreeList := resolveGroupModelPricing(group, model)
		require.NotNil(t, pricing)
		require.False(t, fromFreeList, "真实价卡优先，不得被标记为名单卡")
		require.NotNil(t, pricing.InputPrice)
		require.Equal(t, 2e-6, *pricing.InputPrice, "已配真实价时不得被名单洗成 0")
		require.False(t, groupModelFreeDeclared(group, model), "真实价卡存在时不得报告为免费")
	})

	t.Run("空价卡不推翻名单", func(t *testing.T) {
		group := &Group{
			ID:         3,
			Platform:   PlatformZhipu,
			FreeModels: []string{model},
			ModelPricing: []ChannelModelPricing{{
				Platform: PlatformZhipu, Models: []string{model}, BillingMode: BillingModeToken,
			}},
		}
		pricing, fromFreeList := resolveGroupModelPricing(group, model)
		require.NotNil(t, pricing)
		require.True(t, fromFreeList)
		require.NotNil(t, pricing.InputPrice)
		require.Zero(t, *pricing.InputPrice, "只绑模型名的空卡应让位给名单")
		require.True(t, groupModelFreeDeclared(group, model))
	})

	t.Run("名单未命中返回 nil 交给下游", func(t *testing.T) {
		group := &Group{ID: 4, Platform: PlatformZhipu, FreeModels: []string{"other-model"}}
		pricing, fromFreeList := resolveGroupModelPricing(group, model)
		require.Nil(t, pricing)
		require.False(t, fromFreeList)
		require.False(t, groupModelFreeDeclared(group, model))
	})

	t.Run("nil 分组安全", func(t *testing.T) {
		pricing, fromFreeList := resolveGroupModelPricing(nil, model)
		require.Nil(t, pricing)
		require.False(t, fromFreeList)
		require.False(t, groupModelFreeDeclared(nil, model))
		require.False(t, (*Group)(nil).GroupModelIsFree(model))
	})
}

// 闸门与计费必须同源：名单让「原本查无此价」的模型可调用。
func TestModelPricingGateFreeListAdmission(t *testing.T) {
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = true
	billing := NewBillingService(cfg, nil)
	gate := NewModelPricingGate(NewModelPricingResolver(nil, billing), cfg)
	ctx := context.Background()

	const model = "zzfree-probe-9x"
	// 前置自证：该模型全局无任何定价来源（否则下面的对照没有意义）。
	_, err := billing.GetModelPricing(model)
	require.ErrorIs(t, err, ErrModelPricingUnavailable)

	require.False(t, gate.HasPricing(ctx, model, &Group{ID: 5, Platform: PlatformZhipu}),
		"无名单、无价卡时必须拦死（这是用户看到的 404 根因）")
	require.True(t, gate.HasPricing(ctx, model, &Group{ID: 6, Platform: PlatformZhipu, FreeModels: []string{model}}),
		"名单命中必须放行")
	require.True(t, gate.HasPricing(ctx, model, &Group{ID: 7, Platform: PlatformZhipu, FreeModels: []string{"zzfree-*"}}),
		"尾部通配同样放行")

	// 并集口径（多分组 Key 走这条）也必须放行。
	groups := []*Group{
		{ID: 8, Platform: PlatformAnthropic},
		{ID: 9, Platform: PlatformZhipu, FreeModels: []string{model}},
	}
	require.True(t, gate.HasPricingForGroups(ctx, model, groups))
	require.Equal(t, []string{model}, gate.FilterPricedForGroups(ctx, []string{model}, groups))
}

// 免费模型的实收解析必须是 0 元（不能因为查无目录价而 fail-closed）。
func TestResolveFreeModelBillsZero(t *testing.T) {
	cfg := &config.Config{}
	billing := NewBillingService(cfg, nil)
	resolver := NewModelPricingResolver(nil, billing)
	ctx := context.Background()

	const model = "zzfree-probe-9x"
	group := &Group{ID: 10, Platform: PlatformZhipu, RateMultiplier: 2, FreeModels: []string{model}}
	gid := group.ID

	resolved := resolver.Resolve(ctx, PricingInput{Model: model, Group: group, GroupID: &gid})
	require.NotNil(t, resolved)
	require.Equal(t, PricingSourceGroup, resolved.Source, "应命中分组定价分支")
	require.NotNil(t, resolved.BasePricing)

	cost, err := billing.CalculateTokenCostForRequest(TokenCostRequest{
		Ctx: ctx, Model: model, Group: group, RateMultiplier: group.RateMultiplier,
		Resolver: resolver, Resolved: resolved,
		Tokens: UsageTokens{InputTokens: 1000, OutputTokens: 2000},
	})
	require.NoError(t, err)
	require.Zero(t, cost.TotalCost, "分组倍率 2x 叠加 0 元单价仍必须是 0")
	require.Zero(t, cost.ActualCost)
}

// forceFreeDisplayPricing：展示价必须重建为与计费同源的全 0 卡。
func TestForceFreeDisplayPricing(t *testing.T) {
	t.Run("nil 定价补齐为全 0 token 卡", func(t *testing.T) {
		m := &PlazaModel{Name: "m1", Platform: PlatformZhipu}
		forceFreeDisplayPricing(m)
		require.NotNil(t, m.Pricing)
		require.Equal(t, BillingModeToken, m.Pricing.BillingMode)
		require.Equal(t, PlatformZhipu, m.Pricing.Platform)
		require.NotNil(t, m.Pricing.InputPrice)
		require.Zero(t, *m.Pricing.InputPrice)
		require.NotNil(t, m.Pricing.CacheReadPrice)
		require.Zero(t, *m.Pricing.CacheReadPrice)
		require.NotNil(t, m.Pricing.ImageInputPrice, "图片输入价也要补齐，否则前端退化成 -")
		require.Zero(t, *m.Pricing.ImageInputPrice)
	})

	t.Run("渠道真实价不得与免费徽章并存", func(t *testing.T) {
		// 回归：旧实现「只补 nil 价项」会把渠道价原样保留，得到「免费徽章 + $0.000001」
		// 这种自相矛盾的卡片。计费走分组全 0 卡（分组层优先于渠道层），所以展示必须是 0。
		m := &PlazaModel{Name: "m2", Platform: PlatformZhipu, Pricing: &ChannelModelPricing{
			BillingMode:     BillingModeToken,
			InputPrice:      freePrice(1e-6),
			PerRequestPrice: freePrice(0.04),
			Intervals: []PricingInterval{
				{MinTokens: 0, MaxTokens: ptrInt(32000), InputPrice: freePrice(1e-6)},
			},
		}}
		forceFreeDisplayPricing(m)
		require.Zero(t, *m.Pricing.InputPrice, "报免费就必须展示 0，不得沿用渠道价")
		require.Zero(t, *m.Pricing.PerRequestPrice)
		require.Empty(t, m.Pricing.Intervals, "阶梯与全 0 卡矛盾，不得保留")
	})

	t.Run("不修改入参共享对象", func(t *testing.T) {
		shared := &ChannelModelPricing{
			BillingMode: BillingModeToken,
			InputPrice:  freePrice(1e-6),
			Intervals:   []PricingInterval{{MinTokens: 0, MaxTokens: ptrInt(32000)}},
		}
		m := &PlazaModel{Name: "m3", Platform: PlatformZhipu, Pricing: shared}
		forceFreeDisplayPricing(m)
		require.NotSame(t, shared, m.Pricing, "渠道缓存价卡是跨请求共享数据，必须丢弃重建")
		require.Equal(t, 1e-6, *shared.InputPrice, "原对象不得被写入")
		require.Len(t, shared.Intervals, 1, "原档位不得被改写")
	})

	t.Run("分时倍率与长上下文基准被清零", func(t *testing.T) {
		m := &PlazaModel{Name: "m4", Platform: PlatformZhipu,
			TimePricing:      &TimePricingSchedule{Timezone: "Asia/Shanghai", Periods: []TimePricingPeriod{{StartTime: "09:00", EndTime: "18:00", Multiplier: 2}}},
			LongContextBasis: ContextPricingBasisWholeRequest}
		forceFreeDisplayPricing(m)
		require.Nil(t, m.TimePricing, "0 × 倍率 = 0，分时徒条与「免费」相抵，不得下发")
		require.Empty(t, m.LongContextBasis)
	})
}

// 回归（P1-A）：分组价卡只能表达 9 个价项，而 ModelPricing 还带 ImageCacheReadPricePerToken
// 等目录字段。修复前 applyChannelTokenPriceOverrides 只归零自己能表达的项，其余沿用目录价，
// 导致「目录里恰好有同名条目」的免费模型带着免费徽章按图片缓存价真实扣费。
func TestResolveFreeModelZeroesCatalogOnlyFields(t *testing.T) {
	const model = "zzfree-image-cache-7x"
	catalog := newStubPricingServiceFromJSON(t, `{
		"`+model+`": {"mode": "chat", "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06,
			"cache_read_input_token_cost": 1e-07, "cache_read_input_image_token_cost": 5e-06}
	}`)
	bs, resolver := newTokenCostTestEnv(t, PlatformOpenAI, nil, catalog)
	ctx := context.Background()

	// 前置自证：目录确实带了非 0 的图片缓存价（否则本用例无法证伪）。
	direct, err := bs.GetModelPricing(model)
	require.NoError(t, err)
	require.Greater(t, direct.ImageCacheReadPricePerToken, 0.0, "前置：目录必须带图片缓存价")

	group := &Group{ID: 501, Platform: PlatformOpenAI, FreeModels: []string{model}}
	gid := group.ID
	resolved := resolver.Resolve(ctx, PricingInput{Model: model, Group: group, GroupID: &gid})
	require.Equal(t, PricingSourceGroup, resolved.Source)
	require.Zero(t, resolved.BasePricing.ImageCacheReadPricePerToken, "名单命中必须把目录独有价项也清零")

	cost, err := bs.CalculateTokenCostForRequest(TokenCostRequest{
		Ctx: ctx, Model: model, Group: group, Resolver: resolver, Resolved: resolved,
		Tokens: UsageTokens{InputTokens: 1000, OutputTokens: 1000,
			CacheReadTokens: 2000, ImageCacheReadTokens: 2000},
	})
	require.NoError(t, err)
	require.Zero(t, cost.CacheReadCost, "图片缓存读取不得产生任何费用")
	require.Zero(t, cost.TotalCost, "报免费却扣费是资损方向相反的缺陷")
	require.Zero(t, cost.ActualCost)
}

// 回归（P1-B）：分组价卡只要填了任一项就不算「空卡」。旧实现复用 pricingNeedsFallback
// （只认 7 个字段，不含 image_input_price 与区间倍率），于是「只配了图片输入价」或
// 「只配了区间倍率打折」的卡会被误判为空，进而被免费名单盖成全 0，静默丢失真实价格。
func TestResolveGroupPriceCardValueDetection(t *testing.T) {
	const model = "zzfree-partial-3x"

	t.Run("仅图片输入价也算有价", func(t *testing.T) {
		group := &Group{
			ID:         1,
			FreeModels: []string{model},
			ModelPricing: []ChannelModelPricing{{
				Models: []string{model}, ImageInputPrice: freePrice(8e-6),
			}},
		}
		pricing, fromFreeList := resolveGroupModelPricing(group, model)
		require.NotNil(t, pricing)
		require.False(t, fromFreeList, "已填图片输入价，不得被名单接管")
		require.NotNil(t, pricing.ImageInputPrice)
		require.Equal(t, 8e-6, *pricing.ImageInputPrice)
		require.False(t, groupModelFreeDeclared(group, model), "徽章不得报免费")
		require.True(t, hasGroupModelPricing(group, model), "闸门与计费必须同结论")
	})

	t.Run("仅区间倍率也算有价", func(t *testing.T) {
		group := &Group{
			ID:         2,
			FreeModels: []string{model},
			ModelPricing: []ChannelModelPricing{{
				Models: []string{model},
				Intervals: []PricingInterval{{
					MinTokens: 0, MaxTokens: ptrInt(32000), InputMultiplier: freePrice(0.5),
				}},
			}},
		}
		_, fromFreeList := resolveGroupModelPricing(group, model)
		require.False(t, fromFreeList, "倍率打折是管理员的显式声明，不得被全 0 卡覆盖")
		require.False(t, groupModelFreeDeclared(group, model))
		require.True(t, hasGroupModelPricing(group, model))
	})

	t.Run("真正空卡仍由名单兜底", func(t *testing.T) {
		group := &Group{
			ID:           3,
			FreeModels:   []string{model},
			ModelPricing: []ChannelModelPricing{{Models: []string{model}, BillingMode: BillingModeToken}},
		}
		_, fromFreeList := resolveGroupModelPricing(group, model)
		require.True(t, fromFreeList)
		require.True(t, groupModelFreeDeclared(group, model))
		require.True(t, hasGroupModelPricing(group, model))
	})
}

// 注：ptrInt 已在包内其他 unit 测试声明，直接复用。

// 广场端到端：分组把「目录里查无此价」的上游免费模型列入名单后，该模型必须
// 出现在广场返回里（原先同时表现为 API 404 + 广场隐身），带 Free 标记，
// 且展示价是真实的 0（非 nil），前端才能渲染 $0.00 而不是「-」。
func TestPlazaListGroupsFreeModelListed(t *testing.T) {
	ctx := context.Background()
	const model = freeProbeModelNoGlobalPrice

	// 渠道只声明模型（mapping），不配任何价卡：定价完全来自分组免费名单。
	channels := []Channel{{
		ID: 20, Name: "zhipu-catalog", Status: StatusActive, GroupIDs: []int64{20},
		ModelMapping: map[string]map[string]string{PlatformZhipu: {model: model}},
	}}
	groups := []Group{{
		ID: 20, Name: "free-group", Platform: PlatformZhipu, RateMultiplier: 1,
		FreeModels: []string{model},
	}}
	svc := newFreePlazaService(channels, groups, map[int64]string{20: PlatformZhipu})

	// 前置自证：把名单从分组上拿掉后该模型上不了广场（证明本用例的因果在于名单）。
	// 渠道价卡仍空，因此没有任何定价来源。
	baseline := []Group{{ID: 20, Name: "free-group", Platform: PlatformZhipu, RateMultiplier: 1}}
	out, err := newFreePlazaService(channels, baseline, map[int64]string{20: PlatformZhipu}).ListGroups(ctx)
	require.NoError(t, err)
	require.Empty(t, out, "无名单且无价卡时，模型必须被定价闸门排除")

	out, err = svc.ListGroups(ctx)
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, []string{model}, plazaNames(out[0].Models))

	m := out[0].Models[0]
	require.True(t, m.Free, "名单命中必须透出 Free 标记")
	require.NotNil(t, m.Pricing)
	require.NotNil(t, m.Pricing.InputPrice, "免费模型展示价必须是 0 而非 nil")
	require.Zero(t, *m.Pricing.InputPrice)
	require.NotNil(t, m.Pricing.OutputPrice)
	require.Zero(t, *m.Pricing.OutputPrice)
	require.NotNil(t, m.Pricing.CacheReadPrice, "未配置价项也要补齐为 0")
	require.Zero(t, *m.Pricing.CacheReadPrice)
}

// 同一模型在「配了真实价的分组」与「免费名单分组」共存时，徽章与价格必须一致：
// 真实价分组不报 Free，免费分组报 Free——否则会出现徽章与价格自相矛盾。
func TestPlazaFreeFlagMatchesDisplayedPrice(t *testing.T) {
	ctx := context.Background()
	const model = freeProbeModelNoGlobalPrice

	channels := []Channel{{
		ID: 21, Name: "zhipu-pricing", Status: StatusActive, GroupIDs: []int64{21, 22},
		ModelPricing: []ChannelModelPricing{{
			Platform: PlatformZhipu, Models: []string{model},
			BillingMode: BillingModeToken, InputPrice: freePrice(2e-6), OutputPrice: freePrice(8e-6),
		}},
	}}
	groups := []Group{
		{ID: 21, Name: "paid", Platform: PlatformZhipu, RateMultiplier: 1},
		{ID: 22, Name: "free", Platform: PlatformZhipu, RateMultiplier: 1, FreeModels: []string{model}},
	}
	svc := newFreePlazaService(channels, groups, map[int64]string{21: PlatformZhipu, 22: PlatformZhipu})

	out, err := svc.ListGroups(ctx)
	require.NoError(t, err)
	require.Len(t, out, 2)

	byName := make(map[string]PlazaGroup, len(out))
	for _, g := range out {
		byName[g.Name] = g
	}
	paid := byName["paid"].Models[0]
	require.False(t, paid.Free, "有真实价卡的分组不得报免费")
	require.NotNil(t, paid.Pricing.InputPrice)
	require.Greater(t, *paid.Pricing.InputPrice, 0.0)

	free := byName["free"].Models[0]
	require.True(t, free.Free, "名单分组必须报免费")
	require.NotNil(t, free.Pricing.InputPrice)
	require.Zero(t, *free.Pricing.InputPrice, "徽章与价格必须同源：报免费就一定是 0 元")
}
