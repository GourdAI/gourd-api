package service

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// PlazaOfficialPricing 模型广场展示用的官方参考价（USD per token），与计费同源：
// LiteLLM → 内置兜底价卡 → 模型策略。字段为 nil 表示该项缺失（0 视为未配置）。
type PlazaOfficialPricing struct {
	InputPrice        *float64
	OutputPrice       *float64
	CacheWritePrice   *float64 // 5m 缓存写入（= LiteLLM cache_creation）
	CacheWrite1hPrice *float64 // 1h 缓存写入，仅计费会区分 5m/1h 时给出
	CacheReadPrice    *float64
	// Intervals 官方长上下文阶梯（多档时给出），不受分组开关影响。
	Intervals []PricingInterval
}

// PlazaModel 模型广场中单个模型条目：按实收口径合成的展示定价 + 官方参考价。
type PlazaModel struct {
	Name            string
	Platform        string
	Pricing         *ChannelModelPricing
	OfficialPricing *PlazaOfficialPricing
	// LongContextBasis 多档时的计价基准（整单 / 仅超出部分），单档为空。
	LongContextBasis ContextPricingBasis
	// TimePricing 计费会生效的分时倍率时段；无分时为 nil。
	TimePricing *TimePricingSchedule
}

// PlazaGroup 模型广场中以分组为顶层的条目。
//
// 与 AvailableGroupRef 相比多了 Description 与 Models；Models 来自该分组关联渠道的
// 支持模型（普通分组按分组平台隔离，Composite 分组展开关联渠道已配置的
// 具体平台），与「可用渠道」页口径一致。
type PlazaGroup struct {
	ID                 int64
	Name               string
	Description        string
	Platform           string
	SubscriptionType   string
	RateMultiplier     float64
	PeakRateEnabled    bool
	PeakStart          string
	PeakEnd            string
	PeakRateMultiplier float64
	IsExclusive        bool
	// 图片按次实付倍率：ImageRateIndependent 为 true 时，图片计费模型的实付
	// = 档位价 × ImageRateMultiplier，不乘分组/用户专属倍率（与计费口径一致）。
	ImageRateIndependent bool
	ImageRateMultiplier  float64
	// LongContextPricingEnabled 分组是否按上下文长度应用阶梯价；关闭时模型展示的是最低档。
	LongContextPricingEnabled bool
	Models                    []PlazaModel
}

// ModelPlazaService 聚合模型广场数据。
//
// 模型枚举有两个来源，取并集（详见 ListGroups）：渠道配置的模型，以及分组内
// 可调度账号能服务的模型（与网关 /v1/models 同一条枚举函数）。token 模型的展示
// 单价与阶梯由 BillingService 的阶梯表查询给出（与扣费走同一条解析链与计费
// 函数），图片/按次模型沿用渠道/分组档位价。
type ModelPlazaService struct {
	channelRepo    ChannelRepository
	groupRepo      GroupRepository
	pricingService *PricingService
	billingService *BillingService
	resolver       *ModelPricingResolver
	// gateway 提供账号侧「本分组能服务哪些模型」的枚举。刻意复用 GatewayService
	// 而不是自己查账号池：模型广场承诺「展示的即可调用」，只有调同一个函数才
	// 不会出现口径漂移（见 plazaAccountModels）。可为 nil（测试/未接线），
	// 此时退化为仅渠道枚举的历史行为。
	gateway *GatewayService
}

// NewModelPlazaService 创建模型广场服务。
func NewModelPlazaService(
	channelRepo ChannelRepository,
	groupRepo GroupRepository,
	pricingService *PricingService,
	billingService *BillingService,
	resolver *ModelPricingResolver,
	gateway *GatewayService,
) *ModelPlazaService {
	return &ModelPlazaService{
		channelRepo:    channelRepo,
		groupRepo:      groupRepo,
		pricingService: pricingService,
		billingService: billingService,
		resolver:       resolver,
		gateway:        gateway,
	}
}

// ListGroups 返回模型广场数据：每个活跃分组附带其可用模型与定价。
//
// 模型枚举取两个来源的并集：
//
//  1. 渠道配置（历史唯一来源）：Active 渠道的 SupportedModels ∪ 全局定价回落，
//     并按分组平台隔离；携带渠道档位价（图片/按次定价只能来路于此）。
//  2. 分组内可调度账号（新增，见 plazaAccountModels）：与网关 /v1/models 调同一
//     条枚举函数得到「本分组能服务哪些模型」。
//
// 为何需要第二个来源：网关判定「能不能调用」看的是账号（调度器用
// account.IsModelSupported 选账号），而广场以前只看渠道配了什么模型。管理员
// 「建分组 + 挂账号 + 在账号上写 model_mapping」而没建渠道（或渠道只配了部分
// 模型）时，这些模型实际能调且能算出价，却从广场上整批消失（分组一个模型都没
// 有时连分组本身都不返回）。用户拍板口径：「能调且算得出价 = 全部展示」。
//
// 其余口径：
//   - 渠道按 lower(name) 排序后遍历，保证同名模型去重结果确定；
//   - 同分组同名模型「先见者胜」（渠道条目优先），仅当已存条目无定价而新条目
//     有定价时升级替换；账号侧条目无渠道档位价，不会被它覆盖掉渠道价；
//   - token 模型的单价与阶梯按实收口径合成（见 ResolveContextPricingSchedule），
//     图片计费模型的档位价按实收口径合成（见 plazaImageDisplayPricing）；
//   - 未定价模型仍然不上架（定价闸门保留）：与网关入口拒绝对齐，避免展示
//     「点了必 404」的模型；算得出价的来源（分组卡 → 渠道卡 → 目录 → 兜底卡）
//     与计费同源，因此「能调」的模型几乎总能过闸；
//   - 分组启用模型白名单（ModelAllowlist）时同步过滤：白名单外的模型客户端请求
//     会 404，展示它们同样违背「展示的即可调用」，过滤复用 FilterForListing
//     （与 /v1/models 同一函数，口径不漂移）；
//   - 每个模型附带官方参考价（查不到为 nil）；
//   - 只返回 Models 非空的分组；分组按 RateMultiplier 升序（同倍率按名称），
//     组内模型按名称排序。
//
// 可见性过滤（专属分组）不在此层做，由 handler 按登录态裁剪。
func (s *ModelPlazaService) ListGroups(ctx context.Context) ([]PlazaGroup, error) {
	channels, err := s.channelRepo.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	groups, err := s.groupRepo.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active groups: %w", err)
	}

	sort.SliceStable(channels, func(i, j int) bool {
		return strings.ToLower(channels[i].Name) < strings.ToLower(channels[j].Name)
	})

	byGroup := make(map[int64]*PlazaGroup, len(groups))
	groupEnt := make(map[int64]*Group, len(groups))
	order := make([]int64, 0, len(groups))
	for i := range groups {
		g := &groups[i]
		byGroup[g.ID] = &PlazaGroup{
			ID:                        g.ID,
			Name:                      g.Name,
			Description:               g.Description,
			Platform:                  g.Platform,
			SubscriptionType:          g.SubscriptionType,
			RateMultiplier:            g.RateMultiplier,
			PeakRateEnabled:           g.PeakRateEnabled,
			PeakStart:                 g.PeakStart,
			PeakEnd:                   g.PeakEnd,
			PeakRateMultiplier:        g.PeakRateMultiplier,
			IsExclusive:               g.IsExclusive,
			ImageRateIndependent:      g.ImageRateIndependent,
			ImageRateMultiplier:       g.ImageRateMultiplier,
			LongContextPricingEnabled: g.LongContextPricingEnabled,
		}
		groupEnt[g.ID] = g
		order = append(order, g.ID)
	}

	type modelKey = plazaModelKey
	modelIdx := make(map[int64]map[modelKey]int, len(groups))
	for i := range channels {
		ch := &channels[i]
		if ch.Status != StatusActive {
			continue
		}
		ch.normalizeBillingModelSource()
		supported := ch.SupportedModels()
		fillGlobalPricingFallback(s.pricingService, supported)

		for _, gid := range ch.GroupIDs {
			pg, ok := byGroup[gid]
			if !ok {
				continue
			}
			idx := modelIdx[gid]
			if idx == nil {
				idx = make(map[modelKey]int, len(supported))
				modelIdx[gid] = idx
			}
			for j := range supported {
				m := supported[j]
				if !plazaModelMatchesGroupPlatform(pg.Platform, m.Platform) {
					continue
				}
				key := modelKey{platform: m.Platform, name: m.Name}
				if at, seen := idx[key]; seen {
					// 先见者胜；仅当已存条目无定价而新条目有定价时升级。
					if pg.Models[at].Pricing == nil && m.Pricing != nil {
						pg.Models[at].Pricing = m.Pricing
					}
					continue
				}
				idx[key] = len(pg.Models)
				pg.Models = append(pg.Models, PlazaModel{
					Name:     m.Name,
					Platform: m.Platform,
					Pricing:  m.Pricing,
				})
			}
		}
	}

	// 第二个枚举源：分组内可调度账号能服务的模型（与 /v1/models 同源）。
	// 放在渠道之后，因为渠道条目携带账号侧给不出的档位价（图片/按次）；
	// 同名模型由上面的「先见者胜 + 无价升级」规则保证不覆盖渠道价。
	for _, gid := range order {
		s.appendAccountModels(byGroup[gid], modelIdx[gid], s.plazaAccountModels(ctx, groupEnt[gid]))
	}

	officialMemo := make(map[string]*PlazaOfficialPricing)
	out := make([]PlazaGroup, 0, len(order))
	gate := NewModelPricingGate(s.resolver, nil)
	for _, gid := range order {
		pg := byGroup[gid]
		if len(pg.Models) == 0 {
			continue
		}
		// 未定价模型不上架模型广场：与网关入口拒绝对齐，避免展示根本不可用的模型。
		if gate.Enabled() {
			kept := make([]PlazaModel, 0, len(pg.Models))
			for _, model := range pg.Models {
				if gate.HasPricing(ctx, model.Name, groupEnt[gid]) {
					kept = append(kept, model)
				}
			}
			pg.Models = kept
			if len(pg.Models) == 0 {
				continue
			}
		}
		// 分组启用模型白名单时同步过滤：白名单外的模型网关会 404，
		// 展示它们等于「点了必 404」。判定复用 Allows——即网关入口的同一个准入函数。
		if g := groupEnt[gid]; g.ModelAllowlistEnabled() {
			kept := make([]PlazaModel, 0, len(pg.Models))
			for _, m := range pg.Models {
				if g.ModelAllowlist.Allows(m.Name) {
					kept = append(kept, m)
				}
			}
			pg.Models = kept
			if len(pg.Models) == 0 {
				continue
			}
		}
		// 排序先于档位价合成，保证同名不同平台条目（composite 分组可同时存在，
		// 各自拿着自己平台的渠道价）按 名字+平台 稳定排列。
		sort.SliceStable(pg.Models, func(i, j int) bool {
			if pg.Models[i].Name != pg.Models[j].Name {
				return pg.Models[i].Name < pg.Models[j].Name
			}
			return pg.Models[i].Platform < pg.Models[j].Platform
		})
		g := groupEnt[gid]
		for j := range pg.Models {
			s.fillDisplayPricing(ctx, &pg.Models[j], g)
			pg.Models[j].OfficialPricing = s.lookupOfficialPricing(ctx, pg.Models[j].Name, officialMemo)
		}
		out = append(out, *pg)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].RateMultiplier != out[j].RateMultiplier {
			return out[i].RateMultiplier < out[j].RateMultiplier
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// plazaModelKey 分组内模型去重键：平台隔离渠道条目，同时保证 composite 分组
// 下不同平台的同名模型在档位价合成时各用各的平台上下文。
type plazaModelKey struct {
	platform string
	name     string
}

// plazaModelMatchesGroupPlatform 渠道条目的平台能否归属到该分组：
// composite 分组要具体请求平台，普通分组要求平台完全一致。
func plazaModelMatchesGroupPlatform(groupPlatform, modelPlatform string) bool {
	if groupPlatform == PlatformComposite {
		return isConcreteRequestPlatform(modelPlatform)
	}
	return modelPlatform == groupPlatform
}

// plazaAccountModel 账号侧枚举出的一个可服务模型（无渠道档位价）。
type plazaAccountModel struct {
	platform string
	name     string
}

// appendAccountModels 把账号侧模型并入分组展示清单：(平台,名字) 已存在则沿用
// 既有条目（渠道档位价不被覆盖），名字已被其他平台占用则跳过（/v1/models 同名
// 只列一条，广场跟这个口径）。
func (s *ModelPlazaService) appendAccountModels(pg *PlazaGroup, idx map[plazaModelKey]int, models []plazaAccountModel) {
	if pg == nil || len(models) == 0 {
		return
	}
	if idx == nil {
		idx = make(map[plazaModelKey]int, len(models))
	}
	for _, m := range models {
		if m.name == "" {
			continue
		}
		key := plazaModelKey{platform: m.platform, name: m.name}
		if _, ok := idx[key]; ok {
			continue
		}
		idx[key] = len(pg.Models)
		pg.Models = append(pg.Models, PlazaModel{Name: m.name, Platform: m.platform})
	}
}

// plazaAccountModels 返回「该分组的可调度账号能服务哪些模型」，与网关
// /v1/models 同口径（复用其枚举函数 GetAvailableModels：账号 model_mapping 键，
// 全组无映射时回落平台静态目录）。语义上限：临时不可调度（如今日额度用尽）的
// 账号不贡献模型，它们只是暂时不可选，次日自动恢复。
//
// 不返回任何模型的两种保守情形：
//   - 分组内该平台的可调度账号数为 0：没有账号能服务，展示该平台默认目录就是空头
//     承诺（分组 platform 与账号 platform 不一致时尤其容易误展示）。composite 分组不在此列，
//     由 compositePlazaAccountModels 逐平台判定；
//   - OpenAI 分组启用了 Codex 模型清单钉住（pinnedOpenAIModels）：该入口的真实
//     模型集合由 manifest 定义且本层拿不到，不猜，只保留渠道枚举。
func (s *ModelPlazaService) plazaAccountModels(ctx context.Context, g *Group) []plazaAccountModel {
	if s == nil || s.gateway == nil || g == nil || g.ID <= 0 {
		return nil
	}
	groupID := g.ID
	if g.Platform == PlatformOpenAI && g.CodexModelsManifestConfig.Enabled {
		return nil
	}

	if g.Platform == PlatformComposite {
		// composite 分组无单一账号平台（g.Platform == "composite"），平台存在性由下游逐平台判定。
		return s.gateway.compositePlazaAccountModels(ctx, &groupID)
	}
	if !s.gateway.hasSchedulableAccountsOnPlatform(ctx, groupID, g.Platform) {
		return nil
	}
	models := s.gateway.GetAvailableModels(ctx, &groupID, g.Platform)
	if len(models) == 0 {
		models = s.gateway.fallbackPlazaModelIDs(g.Platform)
	}
	out := make([]plazaAccountModel, 0, len(models))
	for _, model := range models {
		if trimmed := strings.TrimSpace(model); trimmed != "" {
			out = append(out, plazaAccountModel{platform: g.Platform, name: trimmed})
		}
	}
	return out
}

// compositePlazaAccountModels 展开 composite 分组下各具体平台的可服务模型。
// 平台迭代序与 handler 侧 compositeAvailableModels 保持同一字面量序，
// 使同名模型的归属平台与 /v1/models 一致。
//
// 逐平台先确认「该平台在本分组确有可调度账号」再枚举：否则分组里只挂了一两个
// 平台时，其余平台的静态目录会被整体当作无 mapping 回落灌进广场（数百个模型实际
// 一个都调不通）。定价闸门默认开启时能遮住这个缺口，但闸门可关，不能依赖它兜底。
func (s *GatewayService) compositePlazaAccountModels(ctx context.Context, groupID *int64) []plazaAccountModel {
	if s == nil || groupID == nil {
		return nil
	}
	out := make([]plazaAccountModel, 0, 16)
	seen := make(map[plazaModelKey]struct{})
	for _, platform := range plazaConcretePlatforms {
		if !s.hasSchedulableAccountsOnPlatform(ctx, *groupID, platform) {
			continue
		}
		models := s.GetAvailableModels(ctx, groupID, platform)
		if len(models) == 0 {
			models = s.fallbackPlazaModelIDs(platform)
		}
		for _, model := range models {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			key := plazaModelKey{platform: platform, name: model}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, plazaAccountModel{platform: platform, name: model})
		}
	}
	return out
}

// plazaConcretePlatforms 具体请求平台的固定迭代序。与 handler
// （compositeAvailableModels / defaultModelIDsForPlatform）内的同名字面量序一致；
// 新增平台需同步两处。handler 不 import service 的反向依赖，故无法共用。
var plazaConcretePlatforms = []string{
	PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok,
	PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo,
	PlatformWorkbuddy, PlatformQoder,
}

// fallbackPlazaModelIDs 账号未配 model_mapping 时的平台静态目录（与网关 /v1/models
// 的回落同源）；CN 多协议供应商无静态目录（网关不回落，这里同样不回落，
// 否则 defaultModelsListCandidateIDs 的 default 分支会错返 Claude 列表）。
func (s *GatewayService) fallbackPlazaModelIDs(platform string) []string {
	if s == nil || IsMultiProtocolAPIKeyProvider(platform) {
		return nil
	}
	return defaultModelsListCandidateIDs(platform)
}

// hasSchedulableAccountsOnPlatform 分组内「指定平台」是否存在启用且可调度的账号
// （不缓存，广场低频）。口径与网关调度侧一致：调度按 (group, platform) 取账号，
// 广场展示模型也必须用同一维度判定，否则会把没有账号的平台凭静态目录上架。
func (s *GatewayService) hasSchedulableAccountsOnPlatform(ctx context.Context, groupID int64, platform string) bool {
	if s == nil || s.accountRepo == nil || groupID <= 0 {
		return false
	}
	accounts, err := s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, groupID, platform)
	if err != nil {
		slog.Warn("model_plaza_account_lookup_failed",
			"error", err, "group_id", groupID, "platform", platform)
		return false
	}
	return len(accounts) > 0
}

// fillDisplayPricing 把模型的展示定价换成实收口径：
// token 模型取计费阶梯表（单价与档位均由真实计费函数得出），
// 图片/按次模型（或阶梯表不可用时）沿用渠道定价与分组图片档位价。
func (s *ModelPlazaService) fillDisplayPricing(ctx context.Context, m *PlazaModel, g *Group) {
	if s.billingService != nil && s.resolver != nil {
		sched, err := s.billingService.ResolveContextPricingSchedule(ctx, s.resolver, ContextPricingScheduleInput{
			Model:    m.Name,
			Group:    g,
			Platform: m.Platform,
		})
		if err == nil && sched != nil && len(sched.Tiers) > 0 {
			m.Pricing = withDefaultMaxReasoningEffortMultiplier(plazaPricingFromSchedule(m.Pricing, sched), m.Name)
			if len(sched.Tiers) > 1 {
				m.LongContextBasis = sched.Basis
			}
			m.TimePricing = sched.TimePricing
			return
		}
	}
	m.Pricing = withDefaultMaxReasoningEffortMultiplier(plazaImageDisplayPricing(m.Pricing, g), m.Name)
}

func withDefaultMaxReasoningEffortMultiplier(pricing *ChannelModelPricing, model string) *ChannelModelPricing {
	if pricing == nil || pricing.MaxReasoningEffortMultiplier != nil {
		return pricing
	}
	multiplier := defaultMaxReasoningEffortMultiplier(model)
	if multiplier == nil {
		return pricing
	}
	cloned := pricing.Clone()
	cloned.MaxReasoningEffortMultiplier = multiplier
	return &cloned
}

// plazaPricingFromSchedule 把阶梯表压成展示用的 ChannelModelPricing：
// 平价取首档单价，多档时 Intervals 逐档给出绝对单价；图片/按次字段沿用原始定价。
func plazaPricingFromSchedule(raw *ChannelModelPricing, sched *ContextPricingSchedule) *ChannelModelPricing {
	out := &ChannelModelPricing{BillingMode: BillingModeToken}
	if raw != nil {
		out.ImageInputPrice = raw.ImageInputPrice
		out.ImageOutputPrice = raw.ImageOutputPrice
		out.PerRequestPrice = raw.PerRequestPrice
		out.MaxReasoningEffortMultiplier = raw.MaxReasoningEffortMultiplier
	}
	first := sched.Tiers[0]
	out.InputPrice = first.Input
	out.OutputPrice = first.Output
	out.CacheWritePrice = first.CacheWrite
	out.CacheWrite1hPrice = first.CacheWrite1h
	out.CacheReadPrice = first.CacheRead
	if len(sched.Tiers) > 1 {
		out.Intervals = plazaIntervalsFromTiers(sched.Tiers)
	}
	return out
}

func plazaIntervalsFromTiers(tiers []ContextPricingTier) []PricingInterval {
	intervals := make([]PricingInterval, 0, len(tiers))
	for i, t := range tiers {
		intervals = append(intervals, PricingInterval{
			MinTokens:         t.MinTokens,
			MaxTokens:         t.MaxTokens,
			TierLabel:         t.Label,
			InputPrice:        t.Input,
			OutputPrice:       t.Output,
			CacheWritePrice:   t.CacheWrite,
			CacheWrite1hPrice: t.CacheWrite1h,
			CacheReadPrice:    t.CacheRead,
			SortOrder:         i,
		})
	}
	return intervals
}

// plazaImageDisplayPricing 为图片计费模型合成展示定价，使档位价与实收口径一致：
// 每档（1K/2K/4K）单价 = 分组图片价 > 渠道同档位价 > 渠道默认按次价，无价的档不展示。
// 分组未配任何图片价、或定价非图片模式时原样返回。返回克隆，不修改入参
// （渠道定价指针指向缓存共享数据）。
func plazaImageDisplayPricing(p *ChannelModelPricing, g *Group) *ChannelModelPricing {
	if p == nil || g == nil || p.BillingMode != BillingModeImage {
		return p
	}
	if g.ImagePrice1K == nil && g.ImagePrice2K == nil && g.ImagePrice4K == nil {
		return p
	}
	channelTierPrice := func(label string) *float64 {
		for i := range p.Intervals {
			if p.Intervals[i].TierLabel == label && p.Intervals[i].PerRequestPrice != nil {
				return p.Intervals[i].PerRequestPrice
			}
		}
		return p.PerRequestPrice
	}
	tiers := []struct {
		label      string
		groupPrice *float64
	}{
		{"1K", g.ImagePrice1K},
		{"2K", g.ImagePrice2K},
		{"4K", g.ImagePrice4K},
	}
	clone := *p
	clone.Intervals = make([]PricingInterval, 0, len(tiers))
	for i, t := range tiers {
		price := t.groupPrice
		if price == nil {
			price = channelTierPrice(t.label)
		}
		if price == nil {
			continue
		}
		v := *price
		clone.Intervals = append(clone.Intervals, PricingInterval{
			TierLabel:       t.label,
			PerRequestPrice: &v,
			SortOrder:       i,
		})
	}
	return &clone
}

// lookupOfficialPricing 查询模型的官方参考价（与计费同源：LiteLLM → 内置兜底 → 模型策略），
// 带 memo 避免同名模型重复解析。官方阶梯按无分组、无渠道的口径查阶梯表。
// billingService 为 nil（测试场景）或查不到时返回 nil。
func (s *ModelPlazaService) lookupOfficialPricing(ctx context.Context, modelName string, memo map[string]*PlazaOfficialPricing) *PlazaOfficialPricing {
	if s.billingService == nil {
		return nil
	}
	if cached, ok := memo[modelName]; ok {
		return cached
	}
	var result *PlazaOfficialPricing
	if mp, err := s.billingService.GetModelPricing(modelName); err == nil && mp != nil {
		result = &PlazaOfficialPricing{
			InputPrice:      nonZeroPtr(mp.InputPricePerToken),
			OutputPrice:     nonZeroPtr(mp.OutputPricePerToken),
			CacheWritePrice: nonZeroPtr(mp.CacheCreationPricePerToken),
			CacheReadPrice:  nonZeroPtr(mp.CacheReadPricePerToken),
		}
		// 计费只在支持 5m/1h 分档时使用 1h 价，其余情况 1h 价对用户无意义。
		if mp.SupportsCacheBreakdown {
			result.CacheWrite1hPrice = nonZeroPtr(mp.CacheCreation1hPrice)
		}
		if s.resolver != nil {
			sched, schedErr := s.billingService.ResolveContextPricingSchedule(ctx, s.resolver, ContextPricingScheduleInput{Model: modelName})
			if schedErr == nil && sched != nil && len(sched.Tiers) > 1 {
				result.Intervals = plazaIntervalsFromTiers(sched.Tiers)
			}
		}
		if result.InputPrice == nil && result.OutputPrice == nil && result.CacheWritePrice == nil &&
			result.CacheWrite1hPrice == nil && result.CacheReadPrice == nil && len(result.Intervals) == 0 {
			result = nil
		}
	}
	memo[modelName] = result
	return result
}
