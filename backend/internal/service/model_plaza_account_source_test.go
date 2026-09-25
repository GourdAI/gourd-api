//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// plazaAccountRepoStub 只提供广场账号枚举所需的两个查询。
type plazaAccountRepoStub struct {
	AccountRepository
	byGroup map[int64][]Account
}

func (s *plazaAccountRepoStub) ListSchedulableByGroupID(_ context.Context, groupID int64) ([]Account, error) {
	return s.byGroup[groupID], nil
}

func (s *plazaAccountRepoStub) ListSchedulableByGroupIDAndPlatform(_ context.Context, groupID int64, platform string) ([]Account, error) {
	out := make([]Account, 0)
	for _, acc := range s.byGroup[groupID] {
		if acc.Platform == platform {
			out = append(out, acc)
		}
	}
	return out, nil
}

// plazaAccount 构造一个带 model_mapping 公开名的账号。
func plazaAccount(id int64, platform string, publicModels ...string) Account {
	mapping := make(map[string]any, len(publicModels))
	for _, model := range publicModels {
		mapping[model] = model
	}
	return Account{
		ID:       id,
		Platform: platform,
		Credentials: map[string]any{
			"model_mapping": mapping,
		},
	}
}

func newPlazaServiceWithGateway(channels []Channel, groups []Group, gateway *GatewayService) *ModelPlazaService {
	repo := &mockChannelRepository{
		listAllFn: func(context.Context) ([]Channel, error) { return channels, nil },
	}
	return NewModelPlazaService(repo, &stubGroupRepoForAvailable{activeGroups: groups}, nil, nil, nil, gateway)
}

func plazaNames(models []PlazaModel) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.Name)
	}
	return out
}

// TestListPlazaGroups_IncludesAccountServableModelsWithoutChannel 复现并锁定用户报的缺陷：
// 分组只挂了账号、渠道里完全没配该模型时，模型实际可调用（/v1/models 会列出）却
// 不在广场展示。修复后必须出现，且展示价来自计费解析链（无渠道档位价）。
func TestListPlazaGroups_IncludesAccountServableModelsWithoutChannel(t *testing.T) {
	gateway := &GatewayService{accountRepo: &plazaAccountRepoStub{
		byGroup: map[int64][]Account{
			10: {plazaAccount(1, PlatformQoder, "dmodel", "gmodel")},
		},
	}}
	groups := []Group{{ID: 10, Name: "Qoder", Platform: PlatformQoder, RateMultiplier: 1}}
	svc := newPlazaServiceWithGateway(nil, groups, gateway)

	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 1, "无渠道、只有账号的分组也必须上架（此前整组消失）")
	require.Equal(t, []string{"dmodel", "gmodel"}, plazaNames(out[0].Models))
	for _, m := range out[0].Models {
		require.Equal(t, PlatformQoder, m.Platform)
	}
}

// TestListPlazaGroups_AccountSourceSkippedWithoutAccounts 分组内没有任何可调度账号时
// 不得展示模型：没有账号能服务，展示就是空头承诺（网关会 503/404）。
func TestListPlazaGroups_AccountSourceSkippedWithoutAccounts(t *testing.T) {
	gateway := &GatewayService{accountRepo: &plazaAccountRepoStub{byGroup: map[int64][]Account{}}}
	groups := []Group{{ID: 10, Name: "empty", Platform: PlatformQoder, RateMultiplier: 1}}
	svc := newPlazaServiceWithGateway(nil, groups, gateway)

	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Empty(t, out, "无账号分组不得凭静态目录上架模型")
}

// TestListPlazaGroups_ChannelPricingNotOverriddenByAccountSource 同名模型同时被渠道与账号
// 覆盖时，必须沿用渠道条目（其档位价账号侧给不出），且不能重复出现。
func TestListPlazaGroups_ChannelPricingNotOverriddenByAccountSource(t *testing.T) {
	channels := []Channel{plazaPricedChannel(1, "ch", []int64{10}, PlatformQoder, "dmodel")}
	gateway := &GatewayService{accountRepo: &plazaAccountRepoStub{
		byGroup: map[int64][]Account{
			10: {plazaAccount(1, PlatformQoder, "dmodel", "gmodel")},
		},
	}}
	groups := []Group{{ID: 10, Name: "Qoder", Platform: PlatformQoder, RateMultiplier: 1}}
	svc := newPlazaServiceWithGateway(channels, groups, gateway)

	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"dmodel", "gmodel"}, plazaNames(out[0].Models))

	dmodel := out[0].Models[0]
	require.NotNil(t, dmodel.Pricing, "渠道档位价必须保留")
	require.InDelta(t, 3e-6, *dmodel.Pricing.InputPrice, 1e-15)
	require.Nil(t, out[0].Models[1].Pricing, "仅账号侧枚举出的模型无渠道档位价")
}

// TestListPlazaGroups_PricingGateStillDropsUnpricedAccountModels 定价闸门与账号来源同时生效：
// 账号能服务但四处定价源（分组卡/渠道卡/目录/兜底卡）都算不出价的模型仍然不上架，
// 与网关入口 404 保持一致，避免出现「点了报错」的展示。
func TestListPlazaGroups_PricingGateStillDropsUnpricedAccountModels(t *testing.T) {
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = true
	billing := NewBillingService(cfg, nil)
	resolver := NewModelPricingResolver(nil, billing)

	gateway := &GatewayService{accountRepo: &plazaAccountRepoStub{
		byGroup: map[int64][]Account{
			// deepseek-v4-flash 有内置兜底价；totally-unpriced-model 无任何定价源。
			10: {plazaAccount(1, PlatformWorkbuddy, "deepseek-v4-flash", "totally-unpriced-model")},
		},
	}}
	repo := &mockChannelRepository{
		listAllFn: func(context.Context) ([]Channel, error) { return nil, nil },
	}
	groups := []Group{{ID: 10, Name: "wb", Platform: PlatformWorkbuddy, RateMultiplier: 1}}
	svc := NewModelPlazaService(repo, &stubGroupRepoForAvailable{activeGroups: groups}, nil, billing, resolver, gateway)

	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, []string{"deepseek-v4-flash"}, plazaNames(out[0].Models),
		"未定价模型即使账号能服务也不得上架")
}

// TestListPlazaGroups_PlatformGateDropsStaticCatalogWithoutAccounts 分组 platform 与账号
// platform 不一致时（如 qoder 分组只挂了 workbuddy 账号），不得凭分组平台的静态目录
// 上架模型：该平台一个账号都没有，展示出来全是调不通的空头承诺。
func TestListPlazaGroups_PlatformGateDropsStaticCatalogWithoutAccounts(t *testing.T) {
	gateway := &GatewayService{accountRepo: &plazaAccountRepoStub{
		byGroup: map[int64][]Account{
			10: {plazaAccount(1, PlatformWorkbuddy, "glm-5.3")},
		},
	}}
	groups := []Group{{ID: 10, Name: "qoder-group", Platform: PlatformQoder, RateMultiplier: 1}}
	svc := newPlazaServiceWithGateway(nil, groups, gateway)

	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Empty(t, out, "分组平台无账号时不得展示该平台静态目录")
}

// TestListPlazaGroups_CompositeAccountModelsExpandedPerPlatform composite 分组要按具体平台
// 展开各平台账号的模型；CN 多协议供应商无静态目录，无 mapping 时不贡献模型。
func TestListPlazaGroups_CompositeAccountModelsExpandedPerPlatform(t *testing.T) {
	gateway := &GatewayService{accountRepo: &plazaAccountRepoStub{
		byGroup: map[int64][]Account{
			10: {
				plazaAccount(1, PlatformQoder, "dmodel"),
				plazaAccount(2, PlatformWorkbuddy, "glm-5.3"),
				{ID: 3, Platform: PlatformDeepseek}, // 无 mapping 的 CN 供应商：不回落静态目录
			},
		},
	}}
	groups := []Group{{ID: 10, Name: "combo", Platform: PlatformComposite, RateMultiplier: 1}}
	svc := newPlazaServiceWithGateway(nil, groups, gateway)

	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, []string{"dmodel", "glm-5.3"}, plazaNames(out[0].Models))
}

// TestListPlazaGroups_ModelAllowlistFiltersListedModels 分组启用模型白名单时，白名单外的模型
// 网关会 404，广场同样不得展示（否则“展示的即可调用”承诺破裂）；白名单内的保留。
func TestListPlazaGroups_ModelAllowlistFiltersListedModels(t *testing.T) {
	gateway := &GatewayService{accountRepo: &plazaAccountRepoStub{
		byGroup: map[int64][]Account{
			10: {plazaAccount(1, PlatformQoder, "dfmodel", "dmodel", "gmodel")},
		},
	}}
	groups := []Group{{
		ID: 10, Name: "qoder-allow", Platform: PlatformQoder, RateMultiplier: 1,
		ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"dfmodel", "dmodel"}},
	}}
	svc := newPlazaServiceWithGateway(nil, groups, gateway)

	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, []string{"dfmodel", "dmodel"}, plazaNames(out[0].Models),
		"白名单外的 gmodel 必须与 /v1/models 一样被过滤掉")
}

// TestListPlazaGroups_ModelAllowlistAllFilteredHidesGroup 白名单与账号模型零交集时
// 整组不上架（与“无模型分组不展示”一致），避免出现空价目表。
func TestListPlazaGroups_ModelAllowlistAllFilteredHidesGroup(t *testing.T) {
	gateway := &GatewayService{accountRepo: &plazaAccountRepoStub{
		byGroup: map[int64][]Account{
			10: {plazaAccount(1, PlatformQoder, "dmodel")},
		},
	}}
	groups := []Group{{
		ID: 10, Name: "qoder-allow", Platform: PlatformQoder, RateMultiplier: 1,
		ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"absent-model"}},
	}}
	svc := newPlazaServiceWithGateway(nil, groups, gateway)

	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Empty(t, out, "白名单与可服务模型零交集时整组不得上架")
}

// TestListPlazaGroups_NoGatewayFallsBackToChannelOnly gateway 未接线（旧构造/测试替身）时
// 必须退回历史的纯渠道枚举，不因新参数把广场打空。
func TestListPlazaGroups_NoGatewayFallsBackToChannelOnly(t *testing.T) {
	channels := []Channel{plazaPricedChannel(1, "ch", []int64{10}, PlatformQoder, "dmodel")}
	groups := []Group{{ID: 10, Name: "Qoder", Platform: PlatformQoder, RateMultiplier: 1}}
	svc := newPlazaServiceWithGateway(channels, groups, nil)

	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"dmodel"}, plazaNames(out[0].Models))
}

// TestFallbackPlazaModelIDsSkipsMultiProtocolProviders 与网关 compositeAvailableModels 的
// 注释同口径：CN 供应商没有可用静态目录，回落会错返 Claude 列表。
func TestFallbackPlazaModelIDsSkipsMultiProtocolProviders(t *testing.T) {
	s := &GatewayService{}
	require.Nil(t, s.fallbackPlazaModelIDs(PlatformDeepseek))
	require.Nil(t, s.fallbackPlazaModelIDs(PlatformKimi))
	require.Nil(t, s.fallbackPlazaModelIDs(PlatformMiniMax))
	require.Nil(t, s.fallbackPlazaModelIDs(PlatformZhipu))
	require.NotEmpty(t, s.fallbackPlazaModelIDs(PlatformWorkbuddy))
	require.NotEmpty(t, s.fallbackPlazaModelIDs(PlatformQoder))
	require.NotEmpty(t, s.fallbackPlazaModelIDs(PlatformAnthropic))
}
