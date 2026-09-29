//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// traeUnpricedBeforeFix 是 2026-09-29 报障时在全局定价链（分组卡→渠道卡→LiteLLM
// 目录→内置兜底卡）四处全部查无价格的 Trae 目录型号。定价闸门把它们的输入名
// 逐字拿去查价（Trae 出站原样透传 config_name，不存在 Qoder 那套官方 key 归一），
// 查无即判未定价 → /v1/models 与模型广场整批剔除、入口中间件同时 404。
var traeUnpricedBeforeFix = []string{
	"qwen-3.7-plus",
	"qwen-3.6-plus",
	"qwen-3.5",
	"qwen3-coder",
	"Doubao-Seed-2.1-Pro",
	"Doubao-Seed-2.0-Code",
	"seed-code-pro-0430",
}

// TestTraeFallbackPricingCoversCatalog 守卫用例（本轮核心）：Trae 目录此前没有
// 任何「每个型号必须有价」的守卫（对照 qoder_test.go 的
// TestQoderFallbackPricingCoversCatalog），缺口因此在 2026-09-28 那轮修复中被
// 整体漏过。此后 DefaultTraeModelIDs 新增型号若忘了配价卡，本用例当场失败。
func TestTraeFallbackPricingCoversCatalog(t *testing.T) {
	t.Parallel()
	svc := newTestBillingService()

	for _, id := range DefaultTraeModelIDs() {
		pricing := svc.getFallbackPricing(id)
		require.NotNilf(t, pricing, "Trae 目录型号 %s 必须有 fallback 价卡（否则被定价闸门整批剔除）", id)
		require.Greaterf(t, pricing.InputPricePerToken, 0.0, "%s input 价必须 > 0", id)
		require.Greaterf(t, pricing.OutputPricePerToken, 0.0, "%s output 价必须 > 0", id)
	}
}

// TestTraePricingGatePassesWholeCatalog 闸门口径：25 项目录全部放行。断言用
// gate.HasPricing（与准入中间件/广场/列表同一判定源），而不只是 getFallbackPricing。
func TestTraePricingGatePassesWholeCatalog(t *testing.T) {
	t.Parallel()
	gate := newGateForTest(t, true)
	require.True(t, gate.Enabled())

	group := &Group{ID: 77, Platform: PlatformTrae}
	for _, id := range DefaultTraeModelIDs() {
		require.Truef(t, gate.HasPricing(context.Background(), id, group),
			"Trae 目录型号 %s 必须通过定价闸门（2026-09-29 报障回归）", id)
	}
}

// TestTraePreviouslyUnpricedModelsResolveToOfficialCards 逐个钉死新价卡的数字，
// 防止后续被改错档。汇率口径 ¥1≈$0.14（÷7.14），与既有 doubao-embedding-vision
// 一致；均取官方「中国内地」基础档。
func TestTraePreviouslyUnpricedModelsResolveToOfficialCards(t *testing.T) {
	t.Parallel()
	svc := newTestBillingService()

	cases := []struct {
		model        string
		input        float64
		output       float64
		cacheRead    float64
		evidenceNote string
	}{
		{"Doubao-Seed-2.1-Pro", 0.84e-6, 4.20e-6, 0.168e-6, "方舟 ¥6/¥30/缓存¥1.2"},
		{"Doubao-Seed-2.0-Code", 0.448e-6, 2.24e-6, 0.09e-6, "方舟 ¥3.2/¥16/缓存¥0.64"},
		{"seed-code-pro-0430", 0.448e-6, 2.24e-6, 0.09e-6, "无独立价目，按豆包编程模型现役档"},
		{"qwen-3.6-plus", 0.28e-6, 1.68e-6, 0.028e-6, "百炼 ¥2/¥12（≤256K）"},
		{"qwen-3.5", 0.112e-6, 0.672e-6, 0.011e-6, "百炼 ¥0.8/¥4.8（≤128K）"},
		{"qwen3-coder", 0.56e-6, 2.24e-6, 0.112e-6, "百炼 coder-plus ¥4/¥16（≤32K）"},
	}
	for _, tc := range cases {
		pricing := svc.getFallbackPricing(tc.model)
		require.NotNilf(t, pricing, "%s 必须有价卡（%s）", tc.model, tc.evidenceNote)
		require.InDeltaf(t, tc.input, pricing.InputPricePerToken, 1e-12, "%s input", tc.model)
		require.InDeltaf(t, tc.output, pricing.OutputPricePerToken, 1e-12, "%s output", tc.model)
		require.InDeltaf(t, tc.cacheRead, pricing.CacheReadPricePerToken, 1e-12, "%s cacheRead", tc.model)
	}

	// 同款模型对齐（2026-09-22 口径）：Trae 的 qwen-3.7-plus 与 Qoder 的
	// Qwen3.7-Plus 是同一个模型，必须复用 qmodel 那张卡，不得出现两份价目。
	trae := svc.getFallbackPricing("qwen-3.7-plus")
	require.NotNil(t, trae)
	require.InDelta(t, svc.getFallbackPricing("qmodel").InputPricePerToken, trae.InputPricePerToken, 1e-12)
	require.InDelta(t, svc.getFallbackPricing("qmodel").OutputPricePerToken, trae.OutputPricePerToken, 1e-12)

	// 大小写宽容：闸门入口已 ToLower，这里确认大写/小写两种写法同卡。
	require.InDelta(t, trae.InputPricePerToken,
		svc.getFallbackPricing("QWEN-3.7-PLUS").InputPricePerToken, 1e-12)
}

// TestTraePricingRulesDoNotOverBillCheaperVariants 反向保护（比"能不能展示"更要命）：
// 新增的 doubao/qwen 规则不得把更便宜的兄弟型号按旗舰档多收，也不得把按张/按秒
// 计费的媒体模型按 token 计价。此前这些名字一律处于「无价」状态，维持原样。
func TestTraePricingRulesDoNotOverBillCheaperVariants(t *testing.T) {
	t.Parallel()
	svc := newTestBillingService()

	// 更便宜/更旧的变体：必须继续无价，不得被家族默认抢走。
	for _, model := range []string{
		"doubao-pro",                   // 既有反向断言，必须保持无价
		"doubao-1.5-pro-250115",        // 旧一代，价格低一个量级
		"doubao-1.5-lite-32k",          // 轻量档
		"doubao-seed-1.6-flash",        // 上一代 flash
		"doubao-embedding-text-240515", // 纯文本 embedding，既有断言
		"qwen-max",                     // 既有反向断言，必须保持无价
		"qwen3-coder-flash",            // 更便宜的 coder 子档
		"qwen3-coder-next",
		"qwen3-coder-30b-a3b-instruct",
		"qwen3.5-flash",
		"qwen-turbo",
	} {
		require.Nilf(t, svc.getFallbackPricing(model),
			"%s 必须维持无价（新增规则不得把它按旗舰/plus 档多收）", model)
	}

	// 媒体族：按张/按秒计费，绝不可命中 token 价卡。
	for _, model := range []string{
		"doubao-seedance-2-0",
		"doubao-seedream-5-0-pro",
		"doubao-seed-2.1-asr",
	} {
		require.Nilf(t, svc.getFallbackPricing(model), "%s 是媒体型号，不得按 token 计价", model)
	}

	// turbo 有独立卡，且必须严格便宜于 Pro（防止家族默认抢走后翻倍收费）。
	turbo := svc.getFallbackPricing("Doubao-Seed-2.1-Turbo")
	require.NotNil(t, turbo, "2.1 Turbo 官方价 = 2.1 Pro 一半，必须有独立卡")
	pro := svc.getFallbackPricing("Doubao-Seed-2.1-Pro")
	require.Greater(t, pro.InputPricePerToken, turbo.InputPricePerToken)
	require.InDelta(t, 0.42e-6, turbo.InputPricePerToken, 1e-12)
	require.InDelta(t, 2.10e-6, turbo.OutputPricePerToken, 1e-12)
}

// TestListPlazaGroups_TraeCatalogModelsAllVisible 端到端复现用户报障：账号
// model_mapping 用 Trae 目录原始写法（含 7 个修复前查无价的名字）。修复前广场
// 只剩 4 个（探针实测），修复后 7 个全部上架且带实付价。
func TestListPlazaGroups_TraeCatalogModelsAllVisible(t *testing.T) {
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = true
	billing := NewBillingService(cfg, nil)
	resolver := NewModelPricingResolver(nil, billing)

	acct := plazaAccount(1, PlatformTrae, traeUnpricedBeforeFix...)
	gateway := &GatewayService{accountRepo: &plazaAccountRepoStub{
		byGroup: map[int64][]Account{12: {acct}},
	}}
	repo := &mockChannelRepository{
		listAllFn: func(context.Context) ([]Channel, error) { return nil, nil },
	}
	groups := []Group{{ID: 12, Name: "Trae 直连", Platform: PlatformTrae, RateMultiplier: 1}}
	svc := NewModelPlazaService(repo, &stubGroupRepoForAvailable{activeGroups: groups}, nil, billing, resolver, gateway)

	// 枚举源本来就全（说明症状不是「没枚举出来」），修复点在定价闸门。
	models := gateway.GetAvailableModels(context.Background(), ptrInt64(12), PlatformTrae)
	require.Len(t, models, len(traeUnpricedBeforeFix), "账号映射的 7 个名字必须全部被枚举")
	filtered := NewModelPricingGate(resolver, cfg).FilterPriced(context.Background(), models, &groups[0])
	require.ElementsMatch(t, traeUnpricedBeforeFix, filtered,
		"闸门不得再剔除任何 Trae 目录型号（修复前 7 个全被剪掉）")

	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.ElementsMatch(t, traeUnpricedBeforeFix, plazaNames(out[0].Models))

	byName := make(map[string]PlazaModel, len(out[0].Models))
	for _, m := range out[0].Models {
		byName[m.Name] = m
	}
	for _, name := range traeUnpricedBeforeFix {
		m := byName[name]
		require.NotNilf(t, m.Pricing, "%s 上架后必须带价", name)
		require.NotNilf(t, m.Pricing.InputPrice, "%s 输入价必须非 nil（nil 会让广场显示「-」）", name)
		require.Greaterf(t, *m.Pricing.InputPrice, 0.0, "%s 输入价必须 > 0", name)
	}

	// 抽查一张具体卡的实付价，证明展示走的是新价卡而非零头。
	doubao := byName["Doubao-Seed-2.1-Pro"]
	require.InDelta(t, 0.84e-6, *doubao.Pricing.InputPrice, 1e-12)
	require.InDelta(t, 4.20e-6, *doubao.Pricing.OutputPrice, 1e-12)
}
