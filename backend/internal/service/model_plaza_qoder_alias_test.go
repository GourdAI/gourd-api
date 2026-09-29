//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestListPlazaGroups_QoderNonIdentityAliasMappingShowsAll（2026-09-29 报障回归）：
// 账号 model_mapping 为「别名 → 官方 key」的非恒等映射（用户线上原始配置）：
//
//	qwen3.8-flash → qfmodel
//	qwen3.8-max   → qmodel_38max
//	qfmodel       → qfmodel
//	qmodel_38max  → qmodel_38max
//	glm-5.3       → glm-5.3
//	glm-5.3-flash → glm-5.3-flash
//
// 清单枚举取映射的「键」（请求名）；定价链对键做 Qoder 闭集归一后按官方 key 取价。
// 修复前两个别名在闸门处被判「未定价」→ /v1/models 与模型广场整批剔除（用户实测
// 只显示 4 个直接命中价卡的模型）；修复后 6 个全展示且别名带价（同官方 key 卡）。
func TestListPlazaGroups_QoderNonIdentityAliasMappingShowsAll(t *testing.T) {
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = true
	billing := NewBillingService(cfg, nil)
	resolver := NewModelPricingResolver(nil, billing)

	acct := Account{
		ID:       1,
		Platform: PlatformQoder,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"qwen3.8-flash": "qfmodel",
				"qwen3.8-max":   "qmodel_38max",
				"qfmodel":       "qfmodel",
				"qmodel_38max":  "qmodel_38max",
				"glm-5.3":       "glm-5.3",
				"glm-5.3-flash": "glm-5.3-flash",
			},
		},
	}
	gateway := &GatewayService{accountRepo: &plazaAccountRepoStub{
		byGroup: map[int64][]Account{10: {acct}},
	}}
	repo := &mockChannelRepository{
		listAllFn: func(context.Context) ([]Channel, error) { return nil, nil },
	}
	groups := []Group{{ID: 10, Name: "国模半价1", Platform: PlatformQoder, RateMultiplier: 1}}
	svc := NewModelPlazaService(repo, &stubGroupRepoForAvailable{activeGroups: groups}, nil, billing, resolver, gateway)

	want := []string{"glm-5.3", "glm-5.3-flash", "qfmodel", "qmodel_38max", "qwen3.8-flash", "qwen3.8-max"}

	// /v1/models 同源枚举 → 定价闸门必须全部放行（修复前别名被整批剔除）。
	models := gateway.GetAvailableModels(context.Background(), ptrInt64(10), PlatformQoder)
	require.Equal(t, want, models)
	filtered := NewModelPricingGate(resolver, cfg).FilterPriced(context.Background(), models, &groups[0])
	require.Equal(t, want, filtered, "别名不得因闸门误判未定价而被剔除")

	// 模型广场：同一账号配置下 6 个全部上架，别名展示价为官方 key 同卡。
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, want, plazaNames(out[0].Models))

	byName := make(map[string]PlazaModel, len(out[0].Models))
	for _, m := range out[0].Models {
		byName[m.Name] = m
	}
	flash := byName["qwen3.8-flash"]
	require.NotNil(t, flash.Pricing)
	require.NotNil(t, flash.Pricing.InputPrice)
	require.InDelta(t, 0.15e-6, *flash.Pricing.InputPrice, 1e-12, "qwen3.8-flash 实付输入价 = qfmodel 卡")
	require.NotNil(t, flash.Pricing.OutputPrice)
	require.InDelta(t, 0.47e-6, *flash.Pricing.OutputPrice, 1e-12, "qwen3.8-flash 实付输出价 = qfmodel 卡")

	maxModel := byName["qwen3.8-max"]
	require.NotNil(t, maxModel.Pricing)
	require.NotNil(t, maxModel.Pricing.InputPrice)
	require.InDelta(t, 2e-6, *maxModel.Pricing.InputPrice, 1e-12, "qwen3.8-max 实付输入价 = qmodel_38max 卡")
	require.NotNil(t, maxModel.Pricing.OutputPrice)
	require.InDelta(t, 6e-6, *maxModel.Pricing.OutputPrice, 1e-12, "qwen3.8-max 实付输出价 = qmodel_38max 卡")
}
