package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

// Group management implementations
func (s *adminServiceImpl) ValidateSimpleModeGroupOperation(operation AdminGroupOperation) error {
	return ValidateSimpleModeGroupOperation(s.cfg, operation)
}

func (s *adminServiceImpl) ListGroups(ctx context.Context, page, pageSize int, platform, status, search string, isExclusive *bool, sortBy, sortOrder string) ([]Group, int64, error) {
	params := pagination.PaginationParams{Page: page, PageSize: pageSize, SortBy: sortBy, SortOrder: sortOrder}
	var groups []Group
	var result *pagination.PaginationResult
	var err error
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		repo, ok := s.groupRepo.(interface {
			ListBindableWithFilters(context.Context, pagination.PaginationParams, string, string, string, *bool) ([]Group, *pagination.PaginationResult, error)
		})
		if !ok {
			return nil, 0, errors.New("group repository does not support simple-mode filtering")
		}
		groups, result, err = repo.ListBindableWithFilters(ctx, params, platform, status, search, isExclusive)
	} else {
		groups, result, err = s.groupRepo.ListWithFilters(ctx, params, platform, status, search, isExclusive)
	}
	if err != nil {
		return nil, 0, err
	}
	return groups, result.Total, nil
}

func (s *adminServiceImpl) GetAllGroups(ctx context.Context) ([]Group, error) {
	return s.groupRepo.ListActive(ctx)
}

func (s *adminServiceImpl) GetAllGroupsByPlatform(ctx context.Context, platform string) ([]Group, error) {
	return s.groupRepo.ListActiveByPlatform(ctx, platform)
}

func (s *adminServiceImpl) GetAllGroupsIncludingInactive(ctx context.Context) ([]Group, error) {
	// ListWithFilters with empty status = no status filter, so active + disabled groups are returned.
	// PageSize 10000 is intentionally large; group count is O(dozens) in practice.
	groups, _, err := s.groupRepo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 10000}, "", "", "", nil)
	return groups, err
}

func (s *adminServiceImpl) GetGroup(ctx context.Context, id int64) (*Group, error) {
	group, err := s.groupRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.validateSimpleModeGroupAccess(group); err != nil {
		return nil, err
	}
	return group, nil
}

func (s *adminServiceImpl) validateSimpleModeGroupAccess(group *Group) error {
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple && !IsGroupBindableInSimpleMode(group) {
		return infraerrors.BadRequest("SIMPLE_MODE_GROUP_NOT_BINDABLE", "composite groups are not supported in simple mode")
	}
	return nil
}

func (s *adminServiceImpl) GetGroupModelsListCandidates(ctx context.Context, id int64, platform string) ([]string, error) {
	platform = strings.TrimSpace(platform)
	var group *Group
	if id > 0 {
		loaded, err := s.groupRepo.GetByIDLite(ctx, id)
		if err != nil {
			return nil, err
		}
		group = loaded
		if platform == "" {
			platform = group.Platform
		}
	}
	if platform == "" {
		platform = PlatformAnthropic
	}

	candidates := defaultModelsListCandidateIDs(platform)
	if id <= 0 || s.accountRepo == nil {
		// 无分组上下文（新建表单）时同样过滤，否则创建表单与编辑表单的候选
		// 集会不一致：group 为 nil 时闸门退化为只查全局价（LiteLLM 目录 +
		// 内置兜底卡），仍能剔除真正查无价格的模型。
		return s.filterPricedCandidates(ctx, candidates, group), nil
	}

	accounts, err := s.accountRepo.ListSchedulableByGroupID(ctx, id)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(candidates))
	for _, model := range candidates {
		seen[model] = struct{}{}
	}
	for _, acc := range accounts {
		if platform == PlatformComposite {
			if !isConcreteRequestPlatform(acc.Platform) {
				continue
			}
		} else if acc.Platform != platform {
			continue
		}
		for model := range acc.GetModelMapping() {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			if _, ok := seen[model]; ok {
				continue
			}
			seen[model] = struct{}{}
			candidates = append(candidates, model)
		}
	}

	return s.filterPricedCandidates(ctx, candidates, group), nil
}

// filterPricedCandidates 剔除未定价模型，保持与客户端可见模型列表（/v1/models）
// 同一口径，避免管理端在候选里选中一个客户端根本拉不到的模型。
//
// 只在这里过滤一次就够：候选列表的完整集合在本函数返回前才集齐（默认候选 +
// 各账号 model_mapping 键），提前过滤只能覆盖默认候选那一段，属于冗余重复。
//
// 注意这不是防「白嫖」的手段：未定价模型即使进了白名单也无法被调用，网关入口
// 的定价准入（middleware.ModelPricingAdmission）才是硬闸。定价配置的模型名走
// 自由输入（前端 ModelTagInput），不依赖本候选列表，因此过滤不会造成
// 「看不到未定价模型→无法给它配价→永远无法上架」的死锁。
// 闸门未接线或开关关闭时原样返回。
func (s *adminServiceImpl) filterPricedCandidates(ctx context.Context, candidates []string, group *Group) []string {
	if s.modelPricingGate == nil {
		return candidates
	}
	return s.modelPricingGate.FilterPriced(ctx, candidates, group)
}

func (s *adminServiceImpl) ListCompositeRoutes(ctx context.Context, groupID int64) ([]CompositeModelRoute, error) {
	if err := s.ValidateSimpleModeGroupOperation(AdminGroupOperationCompositeRoute); err != nil {
		return nil, err
	}
	if err := s.requireCompositeGroup(ctx, groupID); err != nil {
		return nil, err
	}
	if s.compositeRouteRepo == nil {
		return nil, fmt.Errorf("composite route repository is not configured")
	}
	return s.compositeRouteRepo.ListByGroup(ctx, groupID, true)
}

func (s *adminServiceImpl) CreateCompositeRoute(ctx context.Context, groupID int64, input CompositeRouteInput) (*CompositeModelRoute, error) {
	if err := s.ValidateSimpleModeGroupOperation(AdminGroupOperationCompositeRoute); err != nil {
		return nil, err
	}
	if err := s.requireCompositeGroup(ctx, groupID); err != nil {
		return nil, err
	}
	if s.compositeRouteRepo == nil {
		return nil, fmt.Errorf("composite route repository is not configured")
	}
	route, err := compositeRouteFromInput(groupID, input)
	if err != nil {
		return nil, err
	}
	if err := s.compositeRouteRepo.Create(ctx, route); err != nil {
		return nil, err
	}
	return route, nil
}

func (s *adminServiceImpl) UpdateCompositeRoute(ctx context.Context, groupID, routeID int64, input CompositeRouteInput) (*CompositeModelRoute, error) {
	if err := s.ValidateSimpleModeGroupOperation(AdminGroupOperationCompositeRoute); err != nil {
		return nil, err
	}
	if err := s.requireCompositeGroup(ctx, groupID); err != nil {
		return nil, err
	}
	if s.compositeRouteRepo == nil {
		return nil, fmt.Errorf("composite route repository is not configured")
	}
	if ok, err := s.compositeRouteBelongsToGroup(ctx, groupID, routeID); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrCompositeRouteNotFound
	}
	route, err := compositeRouteFromInput(groupID, input)
	if err != nil {
		return nil, err
	}
	route.ID = routeID
	if err := s.compositeRouteRepo.Update(ctx, route); err != nil {
		return nil, err
	}
	return route, nil
}

func (s *adminServiceImpl) DeleteCompositeRoute(ctx context.Context, groupID, routeID int64) error {
	if err := s.ValidateSimpleModeGroupOperation(AdminGroupOperationCompositeRoute); err != nil {
		return err
	}
	if err := s.requireCompositeGroup(ctx, groupID); err != nil {
		return err
	}
	if s.compositeRouteRepo == nil {
		return fmt.Errorf("composite route repository is not configured")
	}
	if ok, err := s.compositeRouteBelongsToGroup(ctx, groupID, routeID); err != nil {
		return err
	} else if !ok {
		return ErrCompositeRouteNotFound
	}
	return s.compositeRouteRepo.Delete(ctx, routeID)
}

func (s *adminServiceImpl) PreviewCompositeRoute(ctx context.Context, groupID int64, input CompositeRoutePreviewRequest) (*CompositeRouteDecision, error) {
	if err := s.ValidateSimpleModeGroupOperation(AdminGroupOperationCompositeRoute); err != nil {
		return nil, err
	}
	if err := s.requireCompositeGroup(ctx, groupID); err != nil {
		return nil, err
	}
	resolver := s.compositeResolver
	if resolver == nil {
		resolver = NewCompositeRouteResolver(s.compositeRouteRepo)
	}
	decision, err := resolver.Resolve(ctx, groupID, input.Model, input.Endpoint)
	if err != nil {
		return nil, err
	}
	return &decision, nil
}

func (s *adminServiceImpl) requireCompositeGroup(ctx context.Context, groupID int64) error {
	group, err := s.groupRepo.GetByIDLite(ctx, groupID)
	if err != nil {
		return err
	}
	if group.Platform != PlatformComposite {
		return fmt.Errorf("group %d is not a composite group", groupID)
	}
	return nil
}

func (s *adminServiceImpl) compositeRouteBelongsToGroup(ctx context.Context, groupID, routeID int64) (bool, error) {
	routes, err := s.compositeRouteRepo.ListByGroup(ctx, groupID, true)
	if err != nil {
		return false, err
	}
	for i := range routes {
		if routes[i].ID == routeID {
			return true, nil
		}
	}
	return false, nil
}

func compositeRouteFromInput(groupID int64, input CompositeRouteInput) (*CompositeModelRoute, error) {
	input = normalizeCompositeRouteInput(input)
	if input.PublicModel == "" {
		return nil, fmt.Errorf("public_model is required")
	}
	if !isConcreteRequestPlatform(input.TargetPlatform) {
		return nil, fmt.Errorf("target_platform must be a concrete provider")
	}
	if input.Priority == 0 {
		input.Priority = 100
	}
	return &CompositeModelRoute{
		GroupID:        groupID,
		PublicModel:    input.PublicModel,
		MatchType:      input.MatchType,
		TargetPlatform: input.TargetPlatform,
		UpstreamModel:  input.UpstreamModel,
		Endpoint:       input.Endpoint,
		Priority:       input.Priority,
		Enabled:        input.Enabled,
		Notes:          input.Notes,
	}, nil
}

func defaultModelsListCandidateIDs(platform string) []string {
	switch platform {
	case PlatformOpenAI:
		return openai.DefaultModelIDs()
	case PlatformGemini:
		ids := make([]string, 0, len(geminicli.DefaultModels))
		for _, model := range geminicli.DefaultModels {
			ids = append(ids, model.ID)
		}
		return ids
	case PlatformAntigravity:
		models := antigravity.DefaultModels()
		ids := make([]string, 0, len(models))
		for _, model := range models {
			ids = append(ids, model.ID)
		}
		return ids
	case PlatformGrok:
		return xai.DefaultModelIDs()
	case PlatformOpenCodeGo:
		return DefaultOpenCodeGoModelIDs()
	case PlatformWorkbuddy:
		return DefaultWorkbuddyModelIDs()
	case PlatformQoder:
		return DefaultQoderModelIDs()
	case PlatformComposite:
		return compositeDefaultModelsListCandidateIDs()
	default:
		ids := make([]string, 0, len(claude.DefaultModels))
		for _, model := range claude.DefaultModels {
			ids = append(ids, model.ID)
		}
		return ids
	}
}

func defaultAllowImageGenerationForPlatform(platform string) bool {
	// Grok image and video generation routes share the legacy image-generation gate.
	// Older clients send the false zero value, so Grok groups must default enabled.
	return platform == PlatformGrok
}

func compositeDefaultModelsListCandidateIDs() []string {
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	for _, platform := range []string{PlatformAnthropic, PlatformGemini, PlatformOpenAI, PlatformAntigravity, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformWorkbuddy, PlatformQoder} {
		for _, id := range defaultModelsListCandidateIDs(platform) {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	return ids
}

func canCopyAccountsFromGroupPlatform(targetPlatform, sourcePlatform string) bool {
	if targetPlatform == PlatformComposite {
		return sourcePlatform == PlatformComposite || isConcreteRequestPlatform(sourcePlatform)
	}
	return sourcePlatform == targetPlatform
}

func groupSupportsOAuthOnlyFilter(platform string) bool {
	return platform == PlatformOpenAI ||
		platform == PlatformAntigravity ||
		platform == PlatformAnthropic ||
		platform == PlatformGemini ||
		platform == PlatformGrok ||
		platform == PlatformComposite
}

func groupSupportsOpenAIFast(platform string) bool {
	return platform == PlatformOpenAI || platform == PlatformComposite
}

func sanitizeGroupOpenAIFast(group *Group) {
	if group == nil || !groupSupportsOpenAIFast(group.Platform) {
		if group != nil {
			group.ForceOpenAIFast = false
			group.FreeOpenAIFast = false
		}
	}
}

func normalizeCreateGroupInputForSimpleMode(input *CreateGroupInput) {
	if input == nil {
		return
	}
	*input = CreateGroupInput{
		Name: input.Name, Description: input.Description, Platform: input.Platform,
		RateMultiplier: 1, SubscriptionType: SubscriptionTypeStandard,
	}
}

func normalizeUpdateGroupInputForSimpleMode(input *UpdateGroupInput) {
	if input == nil {
		return
	}
	*input = UpdateGroupInput{Name: input.Name, Description: input.Description}
}

func (s *adminServiceImpl) CreateGroup(ctx context.Context, input *CreateGroupInput) (*Group, error) {
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple && NormalizeGroupPlatform(input.Platform) == PlatformComposite {
		return nil, infraerrors.BadRequest("SIMPLE_MODE_GROUP_NOT_BINDABLE", "composite groups are not supported in simple mode")
	}
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		normalizeCreateGroupInputForSimpleMode(input)
	}
	if input.RateMultiplier <= 0 {
		return nil, errors.New("rate_multiplier must be > 0")
	}

	platform := NormalizeGroupPlatform(input.Platform)
	// 固定账号 manifest 配置：账号绑定发生在创建之后，创建时无法校验成员关系，
	// 拒绝开启并在创建后的编辑里配置。
	if normalizeCodexModelsManifestConfig(platform, input.CodexModelsManifestConfig).Enabled {
		return nil, infraerrors.New(http.StatusBadRequest, "INVALID_CODEX_MODELS_MANIFEST_CONFIG", "codex models manifest config cannot be enabled at group creation; configure it after creation in the group editor")
	}
	modelPricing, err := normalizeGroupModelPricing(platform, input.ModelPricing)
	if err != nil {
		return nil, err
	}
	maxReasoningEffort, err := normalizeMaxReasoningEffortForPlatform(platform, input.MaxReasoningEffort)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadRequest, "INVALID_MAX_REASONING_EFFORT", "%v", err)
	}
	maxReasoningEffortOverLimit, err := normalizeMaxReasoningEffortOverLimitForPlatform(platform, input.MaxReasoningEffortOverLimit)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadRequest, "INVALID_MAX_REASONING_EFFORT_OVER_LIMIT", "%v", err)
	}
	reasoningEffortMappings, err := NormalizeReasoningEffortMappings(platform, input.ReasoningEffortMappings)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadRequest, "INVALID_REASONING_EFFORT_MAPPING", "%v", err)
	}

	subscriptionType := input.SubscriptionType
	if subscriptionType == "" {
		subscriptionType = SubscriptionTypeStandard
	}

	// 限额字段：nil/负数 表示"无限制"，0 表示"不允许用量"，正数表示具体限额
	dailyLimit := normalizeLimit(input.DailyLimitUSD)
	weeklyLimit := normalizeLimit(input.WeeklyLimitUSD)
	monthlyLimit := normalizeLimit(input.MonthlyLimitUSD)

	// 图片价格：负数表示清除（使用默认价格），0 保留（表示免费）
	imagePrice1K := normalizePrice(input.ImagePrice1K)
	imagePrice2K := normalizePrice(input.ImagePrice2K)
	imagePrice4K := normalizePrice(input.ImagePrice4K)
	videoPrice480P := normalizePrice(input.VideoPrice480P)
	videoPrice720P := normalizePrice(input.VideoPrice720P)
	videoPrice1080P := normalizePrice(input.VideoPrice1080P)
	webSearchPricePerCall := normalizePrice(input.WebSearchPricePerCall)
	searchPricePer1k := normalizePrice(input.SearchPricePer1k)
	audioRealtimePricePerMin := normalizePrice(input.AudioRealtimePricePerMin)
	audioTTSPricePerMillionChars := normalizePrice(input.AudioTTSPricePerMillionChars)
	audioSTTPricePerHour := normalizePrice(input.AudioSTTPricePerHour)
	imageRateMultiplier := 1.0
	if input.ImageRateMultiplier != nil {
		if *input.ImageRateMultiplier < 0 {
			return nil, errors.New("image_rate_multiplier must be >= 0")
		}
		imageRateMultiplier = *input.ImageRateMultiplier
	}
	batchImageDiscountMultiplier := defaultBatchImageDiscountMultiplier
	if input.BatchImageDiscountMultiplier != nil {
		if *input.BatchImageDiscountMultiplier < 0 {
			return nil, errors.New("batch_image_discount_multiplier must be >= 0")
		}
		batchImageDiscountMultiplier = *input.BatchImageDiscountMultiplier
	}
	batchImageHoldMultiplier := defaultBatchImageHoldMultiplier
	if input.BatchImageHoldMultiplier != nil {
		if *input.BatchImageHoldMultiplier < 0 {
			return nil, errors.New("batch_image_hold_multiplier must be >= 0")
		}
		batchImageHoldMultiplier = *input.BatchImageHoldMultiplier
	}
	// 不变式：hold 比例 >= discount 比例。否则批量任务成功率足够高时
	// 实际成本会超过冻结额，结算永远失败、用户冻结余额无法解冻。
	if batchImageHoldMultiplier < batchImageDiscountMultiplier {
		return nil, errors.New("batch_image_hold_multiplier must be >= batch_image_discount_multiplier")
	}
	videoRateMultiplier := 1.0
	if input.VideoRateMultiplier != nil {
		if *input.VideoRateMultiplier < 0 {
			return nil, errors.New("video_rate_multiplier must be >= 0")
		}
		videoRateMultiplier = *input.VideoRateMultiplier
	}

	peakRateMultiplier := 1.0
	if input.PeakRateMultiplier != nil {
		peakRateMultiplier = *input.PeakRateMultiplier
	}
	// 先归一化（非订阅分组清空高峰配置、清洗停用状态下的脏字段）再校验，与 UpdateGroup 同一收口。
	peakRateEnabled, peakStart, peakEnd, peakRateMultiplier := NormalizePeakRateConfig(subscriptionType, input.PeakRateEnabled, input.PeakStart, input.PeakEnd, peakRateMultiplier)
	if err := ValidatePeakRateConfig(subscriptionType, peakRateEnabled, peakStart, peakEnd, peakRateMultiplier); err != nil {
		return nil, infraerrors.BadRequest("INVALID_PEAK_RATE_CONFIG", err.Error())
	}

	profitMinMargin := 0.0
	if input.ProfitMinMargin != nil {
		profitMinMargin = *input.ProfitMinMargin
	}
	profitSafetyBuffer := 0.0
	if input.ProfitSafetyBuffer != nil {
		profitSafetyBuffer = *input.ProfitSafetyBuffer
	}
	// 利润控制与高峰倍率同一收口顺序：先按平台归一化（不支持的平台重置），再校验。
	profitControlEnabled, profitMinMargin, profitSafetyBuffer := NormalizeProfitControlConfig(platform, input.ProfitControlEnabled, profitMinMargin, profitSafetyBuffer)
	if err := ValidateProfitControlConfig(platform, profitControlEnabled, profitMinMargin, profitSafetyBuffer); err != nil {
		return nil, err
	}

	// 校验降级分组
	if input.FallbackGroupID != nil {
		if err := s.validateFallbackGroup(ctx, 0, *input.FallbackGroupID); err != nil {
			return nil, err
		}
	}
	fallbackOnInvalidRequest := input.FallbackGroupIDOnInvalidRequest
	if fallbackOnInvalidRequest != nil && *fallbackOnInvalidRequest <= 0 {
		fallbackOnInvalidRequest = nil
	}
	// 校验无效请求兜底分组
	if fallbackOnInvalidRequest != nil {
		if err := s.validateFallbackGroupOnInvalidRequest(ctx, 0, platform, subscriptionType, *fallbackOnInvalidRequest); err != nil {
			return nil, err
		}
	}

	// MCPXMLInject：默认为 true，仅当显式传入 false 时关闭
	mcpXMLInject := true
	if input.MCPXMLInject != nil {
		mcpXMLInject = *input.MCPXMLInject
	}

	allowImageGeneration := input.AllowImageGeneration || defaultAllowImageGenerationForPlatform(platform)
	allowBatchImageGeneration := input.AllowBatchImageGeneration && allowImageGeneration && platform == PlatformGemini

	// 如果指定了复制账号的源分组，先获取账号 ID 列表
	var accountIDsToCopy []int64
	if len(input.CopyAccountsFromGroupIDs) > 0 {
		// 去重源分组 IDs
		seen := make(map[int64]struct{})
		uniqueSourceGroupIDs := make([]int64, 0, len(input.CopyAccountsFromGroupIDs))
		for _, srcGroupID := range input.CopyAccountsFromGroupIDs {
			if _, exists := seen[srcGroupID]; !exists {
				seen[srcGroupID] = struct{}{}
				uniqueSourceGroupIDs = append(uniqueSourceGroupIDs, srcGroupID)
			}
		}

		// 校验源分组的平台是否与新分组一致
		for _, srcGroupID := range uniqueSourceGroupIDs {
			srcGroup, err := s.groupRepo.GetByIDLite(ctx, srcGroupID)
			if err != nil {
				return nil, fmt.Errorf("source group %d not found: %w", srcGroupID, err)
			}
			if !canCopyAccountsFromGroupPlatform(platform, srcGroup.Platform) {
				return nil, fmt.Errorf("source group %d platform mismatch: expected %s, got %s", srcGroupID, platform, srcGroup.Platform)
			}
		}

		// 获取所有源分组的账号（去重）
		var err error
		accountIDsToCopy, err = s.groupRepo.GetAccountIDsByGroupIDs(ctx, uniqueSourceGroupIDs)
		if err != nil {
			return nil, fmt.Errorf("failed to get accounts from source groups: %w", err)
		}
	}

	// 白名单在创建路径同样收口：开启但为空、通配位置非法都会 400。
	modelAllowlist, err := normalizeGroupModelAllowlist(input.ModelAllowlist)
	if err != nil {
		return nil, err
	}

	group := &Group{
		Name:                            input.Name,
		Description:                     input.Description,
		Platform:                        platform,
		RateMultiplier:                  input.RateMultiplier,
		IsExclusive:                     input.IsExclusive,
		Status:                          StatusActive,
		SubscriptionType:                subscriptionType,
		DailyLimitUSD:                   dailyLimit,
		WeeklyLimitUSD:                  weeklyLimit,
		MonthlyLimitUSD:                 monthlyLimit,
		LongContextPricingEnabled:       input.LongContextPricingEnabled,
		ModelPricing:                    modelPricing,
		AllowImageGeneration:            allowImageGeneration,
		AllowBatchImageGeneration:       allowBatchImageGeneration,
		ImageRateIndependent:            input.ImageRateIndependent,
		ImageRateMultiplier:             imageRateMultiplier,
		BatchImageDiscountMultiplier:    batchImageDiscountMultiplier,
		BatchImageHoldMultiplier:        batchImageHoldMultiplier,
		VideoRateIndependent:            input.VideoRateIndependent,
		VideoRateMultiplier:             videoRateMultiplier,
		PeakRateEnabled:                 peakRateEnabled,
		PeakStart:                       peakStart,
		PeakEnd:                         peakEnd,
		PeakRateMultiplier:              peakRateMultiplier,
		ProfitControlEnabled:            profitControlEnabled,
		ProfitMinMargin:                 profitMinMargin,
		ProfitSafetyBuffer:              profitSafetyBuffer,
		ImagePrice1K:                    imagePrice1K,
		ImagePrice2K:                    imagePrice2K,
		ImagePrice4K:                    imagePrice4K,
		VideoPrice480P:                  videoPrice480P,
		VideoPrice720P:                  videoPrice720P,
		VideoPrice1080P:                 videoPrice1080P,
		VideoModelPrices:                NormalizeVideoModelPrices(input.VideoModelPrices),
		WebSearchPricePerCall:           webSearchPricePerCall,
		SearchPricePer1k:                searchPricePer1k,
		AudioRealtimePricePerMin:        audioRealtimePricePerMin,
		AudioTTSPricePerMillionChars:    audioTTSPricePerMillionChars,
		AudioSTTPricePerHour:            audioSTTPricePerHour,
		ClaudeCodeOnly:                  input.ClaudeCodeOnly,
		FallbackGroupID:                 input.FallbackGroupID,
		FallbackGroupIDOnInvalidRequest: fallbackOnInvalidRequest,
		ModelRouting:                    input.ModelRouting,
		MCPXMLInject:                    mcpXMLInject,
		SupportedModelScopes:            input.SupportedModelScopes,
		AllowMessagesDispatch:           input.AllowMessagesDispatch,
		AllowLive:                       input.AllowLive,
		ForceOpenAIFast:                 input.ForceOpenAIFast,
		FreeOpenAIFast:                  input.FreeOpenAIFast,
		RequireOAuthOnly:                input.RequireOAuthOnly,
		RequirePrivacySet:               input.RequirePrivacySet,
		DefaultMappedModel:              input.DefaultMappedModel,
		MessagesDispatchModelConfig:     normalizeOpenAIMessagesDispatchModelConfig(input.MessagesDispatchModelConfig),
		ModelAllowlist:                  modelAllowlist,
		// 固定账号 manifest 配置：账号绑定发生在分组创建之后，创建路径禁止开启，
		// 成员关系无从校验（前端创建对话框也不展示）。
		CodexModelsManifestConfig:   normalizeCodexModelsManifestConfig(platform, input.CodexModelsManifestConfig),
		RPMLimit:                    input.RPMLimit,
		MaxReasoningEffort:          maxReasoningEffort,
		MaxReasoningEffortOverLimit: maxReasoningEffortOverLimit,
		ReasoningEffortMappings:     reasoningEffortMappings,
	}
	sanitizeGroupMessagesDispatchFields(group)
	sanitizeGroupOpenAIFast(group)
	if group.Platform != PlatformOpenAI && group.Platform != PlatformComposite {
		group.AllowLive = false
	}
	sanitizeGroupReasoningEffortPolicy(group)
	if err := s.groupRepo.Create(ctx, group); err != nil {
		return nil, err
	}

	// require_oauth_only: 过滤掉 apikey 类型账号
	if group.RequireOAuthOnly && groupSupportsOAuthOnlyFilter(group.Platform) && len(accountIDsToCopy) > 0 {
		accounts, err := s.accountRepo.GetByIDs(ctx, accountIDsToCopy)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch accounts for oauth filter: %w", err)
		}
		oauthIDs := make(map[int64]struct{}, len(accounts))
		for _, acc := range accounts {
			if acc.Type != AccountTypeAPIKey {
				oauthIDs[acc.ID] = struct{}{}
			}
		}
		var filtered []int64
		for _, aid := range accountIDsToCopy {
			if _, ok := oauthIDs[aid]; ok {
				filtered = append(filtered, aid)
			}
		}
		accountIDsToCopy = filtered
	}

	// 如果有需要复制的账号，绑定到新分组
	if len(accountIDsToCopy) > 0 {
		if err := s.groupRepo.BindAccountsToGroup(ctx, group.ID, accountIDsToCopy); err != nil {
			return nil, fmt.Errorf("failed to bind accounts to new group: %w", err)
		}
		group.AccountCount = int64(len(accountIDsToCopy))
	}

	return group, nil
}

// normalizeLimit 将负数转换为 nil（表示无限制），0 保留（表示限额为零）
func normalizeLimit(limit *float64) *float64 {
	if limit == nil || *limit < 0 {
		return nil
	}
	return limit
}

// normalizePrice 将负数转换为 nil（表示使用默认价格），0 保留（表示免费）
func normalizePrice(price *float64) *float64 {
	if price == nil || *price < 0 {
		return nil
	}
	return price
}

// validateFallbackGroup 校验降级分组的有效性
// currentGroupID: 当前分组 ID（新建时为 0）
// fallbackGroupID: 降级分组 ID
func (s *adminServiceImpl) validateFallbackGroup(ctx context.Context, currentGroupID, fallbackGroupID int64) error {
	// 不能将自己设置为降级分组
	if currentGroupID > 0 && currentGroupID == fallbackGroupID {
		return fmt.Errorf("cannot set self as fallback group")
	}

	visited := map[int64]struct{}{}
	nextID := fallbackGroupID
	for {
		if _, seen := visited[nextID]; seen {
			return fmt.Errorf("fallback group cycle detected")
		}
		visited[nextID] = struct{}{}
		if currentGroupID > 0 && nextID == currentGroupID {
			return fmt.Errorf("fallback group cycle detected")
		}

		// 检查降级分组是否存在
		fallbackGroup, err := s.groupRepo.GetByIDLite(ctx, nextID)
		if err != nil {
			return fmt.Errorf("fallback group not found: %w", err)
		}

		// 降级分组不能启用 claude_code_only，否则会造成死循环
		if nextID == fallbackGroupID && fallbackGroup.ClaudeCodeOnly {
			return fmt.Errorf("fallback group cannot have claude_code_only enabled")
		}

		if fallbackGroup.FallbackGroupID == nil {
			return nil
		}
		nextID = *fallbackGroup.FallbackGroupID
	}
}

// validateFallbackGroupOnInvalidRequest 校验无效请求兜底分组的有效性
// currentGroupID: 当前分组 ID（新建时为 0）
// platform/subscriptionType: 当前分组的有效平台/订阅类型
// fallbackGroupID: 兜底分组 ID
func (s *adminServiceImpl) validateFallbackGroupOnInvalidRequest(ctx context.Context, currentGroupID int64, platform, subscriptionType string, fallbackGroupID int64) error {
	if platform != PlatformAnthropic && platform != PlatformAntigravity {
		return fmt.Errorf("invalid request fallback only supported for anthropic or antigravity groups")
	}
	if subscriptionType == SubscriptionTypeSubscription {
		return fmt.Errorf("subscription groups cannot set invalid request fallback")
	}
	if currentGroupID > 0 && currentGroupID == fallbackGroupID {
		return fmt.Errorf("cannot set self as invalid request fallback group")
	}

	fallbackGroup, err := s.groupRepo.GetByIDLite(ctx, fallbackGroupID)
	if err != nil {
		return fmt.Errorf("fallback group not found: %w", err)
	}
	if fallbackGroup.Platform != PlatformAnthropic {
		return fmt.Errorf("fallback group must be anthropic platform")
	}
	if fallbackGroup.SubscriptionType == SubscriptionTypeSubscription {
		return fmt.Errorf("fallback group cannot be subscription type")
	}
	if fallbackGroup.FallbackGroupIDOnInvalidRequest != nil {
		return fmt.Errorf("fallback group cannot have invalid request fallback configured")
	}
	return nil
}

func (s *adminServiceImpl) UpdateGroup(ctx context.Context, id int64, input *UpdateGroupInput) (*Group, error) {
	group, err := s.groupRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.validateSimpleModeGroupAccess(group); err != nil {
		return nil, err
	}
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple && input.Platform == PlatformComposite {
		return nil, infraerrors.BadRequest("SIMPLE_MODE_GROUP_NOT_BINDABLE", "composite groups are not supported in simple mode")
	}
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		normalizeUpdateGroupInputForSimpleMode(input)
	}

	// 渠道缓存里存了 groupID → platform 的映射，改了平台要让它失效（见函数末尾）
	previousPlatform := group.Platform

	if input.Name != "" {
		group.Name = input.Name
	}
	if input.Description != nil {
		group.Description = *input.Description
	}
	if input.Platform != "" {
		group.Platform = input.Platform
	}
	if input.RateMultiplier != nil {
		if *input.RateMultiplier <= 0 {
			return nil, errors.New("rate_multiplier must be > 0")
		}
		group.RateMultiplier = *input.RateMultiplier
	}
	if input.IsExclusive != nil {
		group.IsExclusive = *input.IsExclusive
	}
	if input.Status != "" {
		group.Status = input.Status
	}
	if input.LongContextPricingEnabled != nil {
		group.LongContextPricingEnabled = *input.LongContextPricingEnabled
	}
	if input.ModelPricing != nil {
		modelPricing, normalizeErr := normalizeGroupModelPricing(group.Platform, *input.ModelPricing)
		if normalizeErr != nil {
			return nil, normalizeErr
		}
		group.ModelPricing = modelPricing
	}

	// 订阅相关字段
	if input.SubscriptionType != "" {
		group.SubscriptionType = input.SubscriptionType
	}
	// 限额字段：nil 表示不修改，负数表示"无限制"，0 表示"不允许用量"，正数表示具体限额。
	if input.DailyLimitUSD != nil {
		group.DailyLimitUSD = normalizeLimit(input.DailyLimitUSD)
	}
	if input.WeeklyLimitUSD != nil {
		group.WeeklyLimitUSD = normalizeLimit(input.WeeklyLimitUSD)
	}
	if input.MonthlyLimitUSD != nil {
		group.MonthlyLimitUSD = normalizeLimit(input.MonthlyLimitUSD)
	}
	// 图片生成计费配置：负数表示清除（使用默认价格）
	if input.AllowImageGeneration != nil {
		group.AllowImageGeneration = *input.AllowImageGeneration
	}
	if input.AllowBatchImageGeneration != nil {
		group.AllowBatchImageGeneration = *input.AllowBatchImageGeneration
	}
	if !group.AllowImageGeneration || group.Platform != PlatformGemini {
		group.AllowBatchImageGeneration = false
	}
	if input.ImageRateIndependent != nil {
		group.ImageRateIndependent = *input.ImageRateIndependent
	}
	if input.ImageRateMultiplier != nil {
		if *input.ImageRateMultiplier < 0 {
			return nil, errors.New("image_rate_multiplier must be >= 0")
		}
		group.ImageRateMultiplier = *input.ImageRateMultiplier
	}
	if input.BatchImageDiscountMultiplier != nil {
		if *input.BatchImageDiscountMultiplier < 0 {
			return nil, errors.New("batch_image_discount_multiplier must be >= 0")
		}
		group.BatchImageDiscountMultiplier = *input.BatchImageDiscountMultiplier
	}
	if input.BatchImageHoldMultiplier != nil {
		if *input.BatchImageHoldMultiplier < 0 {
			return nil, errors.New("batch_image_hold_multiplier must be >= 0")
		}
		group.BatchImageHoldMultiplier = *input.BatchImageHoldMultiplier
	}
	// 仅在本次更新显式触碰任一比例时校验合并后的不变式（hold >= discount），
	// 避免存量脏数据阻塞其他字段的正常更新（提交侧另有钳制兜底）。
	if (input.BatchImageDiscountMultiplier != nil || input.BatchImageHoldMultiplier != nil) &&
		group.BatchImageHoldMultiplier < group.BatchImageDiscountMultiplier {
		return nil, errors.New("batch_image_hold_multiplier must be >= batch_image_discount_multiplier")
	}
	if input.VideoRateIndependent != nil {
		group.VideoRateIndependent = *input.VideoRateIndependent
	}
	if input.VideoRateMultiplier != nil {
		if *input.VideoRateMultiplier < 0 {
			return nil, errors.New("video_rate_multiplier must be >= 0")
		}
		group.VideoRateMultiplier = *input.VideoRateMultiplier
	}
	if input.PeakRateEnabled != nil {
		group.PeakRateEnabled = *input.PeakRateEnabled
	}
	if input.PeakStart != nil {
		group.PeakStart = *input.PeakStart
	}
	if input.PeakEnd != nil {
		group.PeakEnd = *input.PeakEnd
	}
	if input.PeakRateMultiplier != nil {
		group.PeakRateMultiplier = *input.PeakRateMultiplier
	}
	// 先归一化（非订阅分组——含本次更新转为非订阅——静默清空高峰配置，清洗停用状态下的脏字段），
	// 再收敛校验：Update 可能只传部分 peak 字段，需对合并后的最终配置统一校验，
	// 防止单独修改 start/end 导致最终 start>=end 等非法配置入库。与 CreateGroup 同一收口。
	group.PeakRateEnabled, group.PeakStart, group.PeakEnd, group.PeakRateMultiplier = NormalizePeakRateConfig(group.SubscriptionType, group.PeakRateEnabled, group.PeakStart, group.PeakEnd, group.PeakRateMultiplier)
	if err := ValidatePeakRateConfig(group.SubscriptionType, group.PeakRateEnabled, group.PeakStart, group.PeakEnd, group.PeakRateMultiplier); err != nil {
		return nil, infraerrors.BadRequest("INVALID_PEAK_RATE_CONFIG", err.Error())
	}
	if input.ProfitControlEnabled != nil {
		group.ProfitControlEnabled = *input.ProfitControlEnabled
	}
	if input.ProfitMinMargin != nil {
		group.ProfitMinMargin = *input.ProfitMinMargin
	}
	if input.ProfitSafetyBuffer != nil {
		group.ProfitSafetyBuffer = *input.ProfitSafetyBuffer
	}
	// 利润控制与高峰同一收口：按合并后的最终平台归一化（转到不支持平台时静默重置），
	// 再对合并后的最终配置统一校验，防止部分字段更新拼出非法组合入库。
	group.ProfitControlEnabled, group.ProfitMinMargin, group.ProfitSafetyBuffer = NormalizeProfitControlConfig(group.Platform, group.ProfitControlEnabled, group.ProfitMinMargin, group.ProfitSafetyBuffer)
	if err := ValidateProfitControlConfig(group.Platform, group.ProfitControlEnabled, group.ProfitMinMargin, group.ProfitSafetyBuffer); err != nil {
		return nil, err
	}
	if input.ImagePrice1K != nil {
		group.ImagePrice1K = normalizePrice(input.ImagePrice1K)
	}
	if input.ImagePrice2K != nil {
		group.ImagePrice2K = normalizePrice(input.ImagePrice2K)
	}
	if input.ImagePrice4K != nil {
		group.ImagePrice4K = normalizePrice(input.ImagePrice4K)
	}
	if input.VideoPrice480P != nil {
		group.VideoPrice480P = normalizePrice(input.VideoPrice480P)
	}
	if input.VideoPrice720P != nil {
		group.VideoPrice720P = normalizePrice(input.VideoPrice720P)
	}
	if input.VideoPrice1080P != nil {
		group.VideoPrice1080P = normalizePrice(input.VideoPrice1080P)
	}
	// nil = leave unchanged; empty map = clear per-model prices.
	if input.VideoModelPrices != nil {
		group.VideoModelPrices = NormalizeVideoModelPrices(input.VideoModelPrices)
	}
	if input.WebSearchPricePerCall != nil {
		group.WebSearchPricePerCall = normalizePrice(input.WebSearchPricePerCall)
	}
	if input.SearchPricePer1k != nil {
		group.SearchPricePer1k = normalizePrice(input.SearchPricePer1k)
	}
	if input.AudioRealtimePricePerMin != nil {
		group.AudioRealtimePricePerMin = normalizePrice(input.AudioRealtimePricePerMin)
	}
	if input.AudioTTSPricePerMillionChars != nil {
		group.AudioTTSPricePerMillionChars = normalizePrice(input.AudioTTSPricePerMillionChars)
	}
	if input.AudioSTTPricePerHour != nil {
		group.AudioSTTPricePerHour = normalizePrice(input.AudioSTTPricePerHour)
	}

	// Claude Code 客户端限制
	if input.ClaudeCodeOnly != nil {
		group.ClaudeCodeOnly = *input.ClaudeCodeOnly
	}
	if input.FallbackGroupID != nil {
		// 校验降级分组
		if *input.FallbackGroupID > 0 {
			if err := s.validateFallbackGroup(ctx, id, *input.FallbackGroupID); err != nil {
				return nil, err
			}
			group.FallbackGroupID = input.FallbackGroupID
		} else {
			// 传入 0 或负数表示清除降级分组
			group.FallbackGroupID = nil
		}
	}
	fallbackOnInvalidRequest := group.FallbackGroupIDOnInvalidRequest
	if input.FallbackGroupIDOnInvalidRequest != nil {
		if *input.FallbackGroupIDOnInvalidRequest > 0 {
			fallbackOnInvalidRequest = input.FallbackGroupIDOnInvalidRequest
		} else {
			fallbackOnInvalidRequest = nil
		}
	}
	if fallbackOnInvalidRequest != nil {
		if err := s.validateFallbackGroupOnInvalidRequest(ctx, id, group.Platform, group.SubscriptionType, *fallbackOnInvalidRequest); err != nil {
			return nil, err
		}
	}
	group.FallbackGroupIDOnInvalidRequest = fallbackOnInvalidRequest

	// 模型路由配置
	if input.ModelRouting != nil {
		group.ModelRouting = input.ModelRouting
	}
	if input.ModelRoutingEnabled != nil {
		group.ModelRoutingEnabled = *input.ModelRoutingEnabled
	}
	if input.MCPXMLInject != nil {
		group.MCPXMLInject = *input.MCPXMLInject
	}

	// 支持的模型系列（仅 antigravity 平台使用）
	if input.SupportedModelScopes != nil {
		group.SupportedModelScopes = *input.SupportedModelScopes
	}

	// OpenAI Messages 调度配置
	if input.AllowMessagesDispatch != nil {
		group.AllowMessagesDispatch = *input.AllowMessagesDispatch
	}
	if input.AllowLive != nil {
		group.AllowLive = *input.AllowLive
	}
	if input.ForceOpenAIFast != nil {
		group.ForceOpenAIFast = *input.ForceOpenAIFast
	}
	if input.FreeOpenAIFast != nil {
		group.FreeOpenAIFast = *input.FreeOpenAIFast
	}
	if input.RequireOAuthOnly != nil {
		group.RequireOAuthOnly = *input.RequireOAuthOnly
	}
	if input.RequirePrivacySet != nil {
		group.RequirePrivacySet = *input.RequirePrivacySet
	}
	if input.DefaultMappedModel != nil {
		group.DefaultMappedModel = *input.DefaultMappedModel
	}
	if input.MessagesDispatchModelConfig != nil {
		group.MessagesDispatchModelConfig = normalizeOpenAIMessagesDispatchModelConfig(*input.MessagesDispatchModelConfig)
	}
	if input.ModelAllowlist != nil {
		modelAllowlist, err := normalizeGroupModelAllowlist(*input.ModelAllowlist)
		if err != nil {
			return nil, err
		}
		group.ModelAllowlist = modelAllowlist
	}
	if input.CodexModelsManifestConfig != nil {
		group.CodexModelsManifestConfig = *input.CodexModelsManifestConfig
	}
	if input.RPMLimit != nil {
		group.RPMLimit = *input.RPMLimit
	}
	if input.MaxReasoningEffort != nil {
		maxReasoningEffort, err := normalizeMaxReasoningEffortForPlatform(group.Platform, *input.MaxReasoningEffort)
		if err != nil {
			return nil, infraerrors.Newf(http.StatusBadRequest, "INVALID_MAX_REASONING_EFFORT", "%v", err)
		}
		group.MaxReasoningEffort = maxReasoningEffort
	}
	if input.MaxReasoningEffortOverLimit != nil {
		maxReasoningEffortOverLimit, err := normalizeMaxReasoningEffortOverLimitForPlatform(group.Platform, *input.MaxReasoningEffortOverLimit)
		if err != nil {
			return nil, infraerrors.Newf(http.StatusBadRequest, "INVALID_MAX_REASONING_EFFORT_OVER_LIMIT", "%v", err)
		}
		group.MaxReasoningEffortOverLimit = maxReasoningEffortOverLimit
	}
	if input.ReasoningEffortMappings != nil {
		reasoningEffortMappings, err := NormalizeReasoningEffortMappings(group.Platform, *input.ReasoningEffortMappings)
		if err != nil {
			return nil, infraerrors.Newf(http.StatusBadRequest, "INVALID_REASONING_EFFORT_MAPPING", "%v", err)
		}
		group.ReasoningEffortMappings = reasoningEffortMappings
	}
	sanitizeGroupMessagesDispatchFields(group)
	sanitizeGroupOpenAIFast(group)
	if group.Platform != PlatformOpenAI && group.Platform != PlatformComposite {
		group.AllowLive = false
	}
	sanitizeGroupReasoningEffortPolicy(group)
	// 固定账号 manifest 配置：按最终平台归一化（切出 openai 平台时静默归零，
	// 与 ForceOpenAIFast 同一收口）；校验仅在本次显式携带配置时进行，
	// 避免脏 ID 阻塞无关字段更新。
	group.CodexModelsManifestConfig = normalizeCodexModelsManifestConfig(group.Platform, group.CodexModelsManifestConfig)
	if input.CodexModelsManifestConfig != nil {
		if err := s.validateCodexModelsManifestConfig(ctx, id, group.CodexModelsManifestConfig); err != nil {
			return nil, err
		}
	}

	if err := s.groupRepo.Update(ctx, group); err != nil {
		return nil, err
	}

	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByGroupID(ctx, id)
	}

	// 平台变了就失效渠道缓存：该缓存持有 groupID → platform，而渠道定价 / 模型映射 /
	// 模型白名单都按平台严格隔离。不失效的话，缓存最长 10 分钟仍按旧平台匹配，
	// 期间定价查不到会静默回落到 LiteLLM 价格表、映射与白名单也不生效。
	if group.Platform != previousPlatform && s.channelCacheInvalidator != nil {
		s.channelCacheInvalidator.InvalidateCache()
	}

	// 如果指定了复制账号的源分组，同步绑定（替换当前分组的账号）
	if len(input.CopyAccountsFromGroupIDs) > 0 {
		// 去重源分组 IDs
		seen := make(map[int64]struct{})
		uniqueSourceGroupIDs := make([]int64, 0, len(input.CopyAccountsFromGroupIDs))
		for _, srcGroupID := range input.CopyAccountsFromGroupIDs {
			// 校验：源分组不能是自身
			if srcGroupID == id {
				return nil, fmt.Errorf("cannot copy accounts from self")
			}
			// 去重
			if _, exists := seen[srcGroupID]; !exists {
				seen[srcGroupID] = struct{}{}
				uniqueSourceGroupIDs = append(uniqueSourceGroupIDs, srcGroupID)
			}
		}

		// 校验源分组的平台是否与当前分组一致
		for _, srcGroupID := range uniqueSourceGroupIDs {
			srcGroup, err := s.groupRepo.GetByIDLite(ctx, srcGroupID)
			if err != nil {
				return nil, fmt.Errorf("source group %d not found: %w", srcGroupID, err)
			}
			if !canCopyAccountsFromGroupPlatform(group.Platform, srcGroup.Platform) {
				return nil, fmt.Errorf("source group %d platform mismatch: expected %s, got %s", srcGroupID, group.Platform, srcGroup.Platform)
			}
		}

		// 获取所有源分组的账号（去重）
		accountIDsToCopy, err := s.groupRepo.GetAccountIDsByGroupIDs(ctx, uniqueSourceGroupIDs)
		if err != nil {
			return nil, fmt.Errorf("failed to get accounts from source groups: %w", err)
		}

		// 先清空当前分组的所有账号绑定
		if _, err := s.groupRepo.DeleteAccountGroupsByGroupID(ctx, id); err != nil {
			return nil, fmt.Errorf("failed to clear existing account bindings: %w", err)
		}

		// require_oauth_only: 过滤掉 apikey 类型账号
		if group.RequireOAuthOnly && groupSupportsOAuthOnlyFilter(group.Platform) && len(accountIDsToCopy) > 0 {
			accounts, err := s.accountRepo.GetByIDs(ctx, accountIDsToCopy)
			if err != nil {
				return nil, fmt.Errorf("failed to fetch accounts for oauth filter: %w", err)
			}
			oauthIDs := make(map[int64]struct{}, len(accounts))
			for _, acc := range accounts {
				if acc.Type != AccountTypeAPIKey {
					oauthIDs[acc.ID] = struct{}{}
				}
			}
			var filtered []int64
			for _, aid := range accountIDsToCopy {
				if _, ok := oauthIDs[aid]; ok {
					filtered = append(filtered, aid)
				}
			}
			accountIDsToCopy = filtered
		}

		// 再绑定源分组的账号
		if len(accountIDsToCopy) > 0 {
			if err := s.groupRepo.BindAccountsToGroup(ctx, id, accountIDsToCopy); err != nil {
				return nil, fmt.Errorf("failed to bind accounts to group: %w", err)
			}
		}
	}

	return group, nil
}

func normalizeGroupModelPricing(platform string, pricing []ChannelModelPricing) ([]ChannelModelPricing, error) {
	out := make([]ChannelModelPricing, len(pricing))
	for i := range pricing {
		out[i] = pricing[i].Clone()
		out[i].ID = 0
		out[i].ChannelID = 0
		if out[i].TimePricing != nil && len(out[i].TimePricing.Periods) > 0 {
			return nil, infraerrors.BadRequest(
				"GROUP_MODEL_TIME_PRICING_UNSUPPORTED",
				"group model pricing does not support time pricing",
			)
		}
		if strings.TrimSpace(out[i].Platform) == "" {
			out[i].Platform = platform
		}
		for j := range out[i].Models {
			out[i].Models[j] = strings.TrimSpace(out[i].Models[j])
		}
		if len(out[i].Models) == 0 {
			return nil, infraerrors.New(http.StatusBadRequest, "GROUP_MODEL_PRICING_MODELS_REQUIRED", "group model pricing entry requires at least one model")
		}
	}
	if err := validatePricingEntries(out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *adminServiceImpl) DeleteGroup(ctx context.Context, id int64) error {
	return s.deleteGroup(ctx, id, false)
}

func (s *adminServiceImpl) DeleteGroupIfEmpty(ctx context.Context, id int64) error {
	return s.deleteGroup(ctx, id, true)
}

func (s *adminServiceImpl) deleteGroup(ctx context.Context, id int64, requireEmpty bool) error {
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		group, err := s.groupRepo.GetByIDLite(ctx, id)
		if err != nil {
			return err
		}
		if err := s.validateSimpleModeGroupAccess(group); err != nil {
			return err
		}
	}
	if requireEmpty && s.emptyGroupDeleteRepo == nil {
		return fmt.Errorf("guarded group deletion is unavailable")
	}

	var groupKeys []string
	if s.authCacheInvalidator != nil {
		keys, err := s.apiKeyRepo.ListKeysByGroupID(ctx, id)
		if err == nil {
			groupKeys = keys
		}
	}

	var affectedUserIDs []int64
	var err error
	if requireEmpty {
		affectedUserIDs, err = s.emptyGroupDeleteRepo.DeleteCascadeIfEmpty(ctx, id)
	} else {
		affectedUserIDs, err = s.groupRepo.DeleteCascade(ctx, id)
	}
	if err != nil {
		return err
	}
	// 注意：user_group_rate_multipliers 表通过外键 ON DELETE CASCADE 自动清理

	// 事务成功后，异步失效受影响用户的订阅缓存
	if len(affectedUserIDs) > 0 && s.billingCacheService != nil {
		groupID := id
		go func() {
			cacheCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for _, userID := range affectedUserIDs {
				if err := s.billingCacheService.InvalidateSubscription(cacheCtx, userID, groupID); err != nil {
					logger.LegacyPrintf("service.admin", "invalidate subscription cache failed: user_id=%d group_id=%d err=%v", userID, groupID, err)
				}
			}
		}()
	}
	if s.authCacheInvalidator != nil {
		for _, key := range groupKeys {
			s.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, key)
		}
	}

	return nil
}

func (s *adminServiceImpl) GetGroupAPIKeys(ctx context.Context, groupID int64, page, pageSize int) ([]APIKey, int64, error) {
	params := pagination.PaginationParams{Page: page, PageSize: pageSize}
	keys, result, err := s.apiKeyRepo.ListByGroupID(ctx, groupID, params)
	if err != nil {
		return nil, 0, err
	}
	return keys, result.Total, nil
}

func (s *adminServiceImpl) GetGroupRateMultipliers(ctx context.Context, groupID int64) ([]UserGroupRateEntry, error) {
	if err := s.ValidateSimpleModeGroupOperation(AdminGroupOperationMultiplier); err != nil {
		return nil, err
	}
	if s.userGroupRateRepo == nil {
		return nil, nil
	}
	return s.userGroupRateRepo.GetByGroupID(ctx, groupID)
}

func (s *adminServiceImpl) ClearGroupRateMultipliers(ctx context.Context, groupID int64) error {
	if err := s.ValidateSimpleModeGroupOperation(AdminGroupOperationMultiplier); err != nil {
		return err
	}
	if s.userGroupRateRepo == nil {
		return nil
	}
	return s.userGroupRateRepo.DeleteByGroupID(ctx, groupID)
}

func (s *adminServiceImpl) BatchSetGroupRateMultipliers(ctx context.Context, groupID int64, entries []GroupRateMultiplierInput) error {
	if err := s.ValidateSimpleModeGroupOperation(AdminGroupOperationMultiplier); err != nil {
		return err
	}
	if s.userGroupRateRepo == nil {
		return nil
	}
	for _, e := range entries {
		if e.RateMultiplier <= 0 {
			return fmt.Errorf("rate_multiplier must be > 0 (user_id=%d)", e.UserID)
		}
	}
	return s.userGroupRateRepo.SyncGroupRateMultipliers(ctx, groupID, entries)
}

func (s *adminServiceImpl) ClearGroupRPMOverrides(ctx context.Context, groupID int64) error {
	if err := s.ValidateSimpleModeGroupOperation(AdminGroupOperationRPMOverride); err != nil {
		return err
	}
	if s.userGroupRateRepo == nil {
		return nil
	}
	if err := s.userGroupRateRepo.ClearGroupRPMOverrides(ctx, groupID); err != nil {
		return err
	}
	// RPM override 已嵌入 auth cache snapshot (v7)，变更后必须失效相关缓存。
	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByGroupID(ctx, groupID)
	}
	return nil
}

func (s *adminServiceImpl) BatchSetGroupRPMOverrides(ctx context.Context, groupID int64, entries []GroupRPMOverrideInput) error {
	if err := s.ValidateSimpleModeGroupOperation(AdminGroupOperationRPMOverride); err != nil {
		return err
	}
	if s.userGroupRateRepo == nil {
		return nil
	}
	for _, e := range entries {
		if e.RPMOverride != nil && *e.RPMOverride < 0 {
			return infraerrors.BadRequest("INVALID_RPM_OVERRIDE", fmt.Sprintf("rpm_override must be >= 0 (user_id=%d)", e.UserID))
		}
	}
	if err := s.userGroupRateRepo.SyncGroupRPMOverrides(ctx, groupID, entries); err != nil {
		return err
	}
	// RPM override 已嵌入 auth cache snapshot (v7)，变更后必须失效相关缓存。
	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByGroupID(ctx, groupID)
	}
	return nil
}

func (s *adminServiceImpl) UpdateGroupSortOrders(ctx context.Context, updates []GroupSortOrderUpdate) error {
	if err := s.ValidateSimpleModeGroupOperation(AdminGroupOperationSort); err != nil {
		return err
	}
	return s.groupRepo.UpdateSortOrders(ctx, updates)
}

// AdminUpdateAPIKeyGroupID 管理员修改 API Key 分组绑定
// groupID: nil=不修改, 指向0=解绑, 指向正整数=绑定到目标分组
//
// 与关联表的同步（关键修正）：单个 group_id 在用户侧与前端契约里的定义是
// 「等价于一元素的 group_ids」（见 resolveRequestedGroupIDs：return []int64{primary}，
// frontend/src/api/__tests__/keys.multiGroup.spec.ts："a single `group_id` is equivalent
// to a one-element `group_ids`"）。因此本接口也必须整体替换绑定集合：
//   - 绑定 B → api_key_groups = [(B,0)]，旧候选全部退出；
//   - 解绑（0） → 主分组列置 NULL + 关联表清空。
//
// 此前只写 api_keys.group_id 而不动关联表，导致「把 key 迁出分组 A」不生效：
// 认证快照会把列值与残留候选归并成 [B,A,C]，A 仍参与决议与计费（倍率/组级 RPM/
// usage_logs.group_id 落回 A），且列与表永久漂移。现在两条写路径共享同一集合语义。
func (s *adminServiceImpl) AdminUpdateAPIKeyGroupID(ctx context.Context, keyID int64, groupID *int64) (*AdminUpdateAPIKeyGroupIDResult, error) {
	apiKey, err := s.apiKeyRepo.GetByID(ctx, keyID)
	if err != nil {
		return nil, err
	}

	if groupID == nil {
		// nil 表示不修改，直接返回
		return &AdminUpdateAPIKeyGroupIDResult{APIKey: apiKey}, nil
	}

	if *groupID < 0 {
		return nil, infraerrors.BadRequest("INVALID_GROUP_ID", "group_id must be non-negative")
	}

	result := &AdminUpdateAPIKeyGroupIDResult{}

	if *groupID == 0 {
		// 0 表示解绑分组（不修改 user_allowed_groups，避免影响用户其他 Key）
		apiKey.GroupID = nil
		apiKey.Group = nil
		apiKey.GroupIDs = nil
	} else {
		// 验证目标分组存在且状态为 active
		group, err := s.groupRepo.GetByID(ctx, *groupID)
		if err != nil {
			return nil, err
		}
		if group.Status != StatusActive {
			return nil, infraerrors.BadRequest("GROUP_NOT_ACTIVE", "target group is not active")
		}
		// 订阅类型分组：用户须持有该分组的有效订阅才可绑定
		if group.IsSubscriptionType() {
			if s.userSubRepo == nil {
				return nil, infraerrors.InternalServer("SUBSCRIPTION_REPOSITORY_UNAVAILABLE", "subscription repository is not configured")
			}
			if _, err := s.userSubRepo.GetActiveByUserIDAndGroupID(ctx, apiKey.UserID, *groupID); err != nil {
				if errors.Is(err, ErrSubscriptionNotFound) {
					return nil, infraerrors.BadRequest("SUBSCRIPTION_REQUIRED", "user does not have an active subscription for this group")
				}
				return nil, err
			}
		}

		gid := *groupID
		apiKey.GroupID = &gid
		apiKey.Group = group
		apiKey.GroupIDs = []int64{gid}

		// 专属标准分组：使用事务保证「添加分组权限」与「更新 API Key」的原子性
		if group.IsExclusive && !group.IsSubscriptionType() {
			opCtx := ctx
			var tx *dbent.Tx
			if s.entClient == nil {
				logger.LegacyPrintf("service.admin", "Warning: entClient is nil, skipping transaction protection for exclusive group binding")
			} else {
				var txErr error
				tx, txErr = s.entClient.Tx(ctx)
				if txErr != nil {
					return nil, fmt.Errorf("begin transaction: %w", txErr)
				}
				defer func() { _ = tx.Rollback() }()
				opCtx = dbent.NewTxContext(ctx, tx)
			}

			if addErr := s.userRepo.AddGroupToAllowedGroups(opCtx, apiKey.UserID, gid); addErr != nil {
				return nil, fmt.Errorf("add group to user allowed groups: %w", addErr)
			}
			if err := s.apiKeyRepo.Update(opCtx, apiKey, APIKeyUpdateFields{GroupID: true}); err != nil {
				return nil, fmt.Errorf("update api key: %w", err)
			}
			// 关联表同步（与主分组列同事务时刻之后）：整体替换为单元素集合。
			// 不并入 ent 事务的原因与 AdminUpdateAPIKeyGroupIDs 相同：ReplaceAPIKeyGroups
			// 走原生 SQL 执行器、不感知 ent tx。失败时不回滚已提交的授权与列更新，
			// 留下的不一致是「候选集未收敛」——与旧行为同形态，admin 重试即可自愈。
			if err := s.replaceAPIKeyGroupsForAdmin(opCtx, apiKey.ID, []int64{gid}, &gid); err != nil {
				return nil, fmt.Errorf("update api key groups: %w", err)
			}
			if tx != nil {
				if err := tx.Commit(); err != nil {
					return nil, fmt.Errorf("commit transaction: %w", err)
				}
			}

			result.AutoGrantedGroupAccess = true
			result.GrantedGroupID = &gid
			result.GrantedGroupName = group.Name

			// 失效认证缓存（在事务提交后执行）
			if s.authCacheInvalidator != nil {
				s.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, apiKey.Key)
			}

			result.APIKey = apiKey
			return result, nil
		}
	}

	// 非专属分组 / 解绑：无需事务，单步更新即可
	if err := s.apiKeyRepo.Update(ctx, apiKey, APIKeyUpdateFields{GroupID: true}); err != nil {
		return nil, fmt.Errorf("update api key: %w", err)
	}

	// 关联表同步：绑定→整体替换为 [groupID]；解绑→清空。
	// 顺序口径与 AdminUpdateAPIKeyGroupIDs 一致（先列后表）：列写失败时直接返回，
	// 不会出现「表已收敛、列仍旧值」的反向不一致。
	if err := s.replaceAPIKeyGroupsForAdmin(ctx, apiKey.ID, apiKey.GroupIDs, apiKey.GroupID); err != nil {
		return nil, fmt.Errorf("update api key groups: %w", err)
	}

	// 失效认证缓存
	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, apiKey.Key)
	}

	result.APIKey = apiKey
	return result, nil
}

// AdminUpdateAPIKeyGroupIDs 管理员整体替换 API Key 的分组绑定集合（新增能力，
// 与 AdminUpdateAPIKeyGroupID 并存：后者只改主分组，本方法改整个集合）。
//
// 契约：
//   - groupIDs 为 nil → 不修改；
//   - 空数组 → 解绑全部（主分组列置空、关联表清空）；
//   - 非空 → 集合第一个元素作为主分组（写入 api_keys.group_id），其余作为候选分组；
//   - 每个分组都必须存在且 active；专属/订阅型分组保持既有自动授权与校验逻辑。
func (s *adminServiceImpl) AdminUpdateAPIKeyGroupIDs(ctx context.Context, keyID int64, groupIDs []int64) (*AdminUpdateAPIKeyGroupIDResult, error) {
	apiKey, err := s.apiKeyRepo.GetByID(ctx, keyID)
	if err != nil {
		return nil, err
	}

	normalized := normalizeAPIKeyGroupIDsForService(groupIDs)
	if normalized == nil {
		normalized = []int64{}
	}

	result := &AdminUpdateAPIKeyGroupIDResult{}

	if len(normalized) == 0 {
		// 解绑全部：清空主分组列与关联表。
		apiKey.GroupID = nil
		apiKey.Group = nil
		apiKey.GroupIDs = nil
		if err := s.apiKeyRepo.Update(ctx, apiKey, APIKeyUpdateFields{GroupID: true}); err != nil {
			return nil, fmt.Errorf("update api key: %w", err)
		}
		if err := s.replaceAPIKeyGroupsForAdmin(ctx, apiKey.ID, nil, nil); err != nil {
			return nil, fmt.Errorf("update api key groups: %w", err)
		}
		if s.authCacheInvalidator != nil {
			s.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, apiKey.Key)
		}
		result.APIKey = apiKey
		return result, nil
	}

	// 校验集合内每个分组存在且 active（与单分组路径同口径）；同时记录首个可自动授权的专属分组。
	// P1③：集合内订阅类型必须一致（全订阅或全标准），与用户侧 validateBindableGroups 同口径。
	groupsByID := make(map[int64]*Group, len(normalized))
	var (
		firstGroup      *Group
		firstGroupIsSub bool
	)
	for _, groupID := range normalized {
		group, err := s.groupRepo.GetByID(ctx, groupID)
		if err != nil {
			return nil, err
		}
		if group.Status != StatusActive {
			return nil, infraerrors.BadRequest("GROUP_NOT_ACTIVE", fmt.Sprintf("group %d is not active", groupID))
		}
		if group.IsSubscriptionType() {
			if s.userSubRepo == nil {
				return nil, infraerrors.InternalServer("SUBSCRIPTION_REPOSITORY_UNAVAILABLE", "subscription repository is not configured")
			}
			if _, err := s.userSubRepo.GetActiveByUserIDAndGroupID(ctx, apiKey.UserID, groupID); err != nil {
				if errors.Is(err, ErrSubscriptionNotFound) {
					return nil, infraerrors.BadRequest("SUBSCRIPTION_REQUIRED", fmt.Sprintf("user does not have an active subscription for group %d", groupID))
				}
				return nil, err
			}
		}
		isSub := group.IsSubscriptionType()
		if firstGroup == nil {
			firstGroup = group
			firstGroupIsSub = isSub
		} else if isSub != firstGroupIsSub {
			return nil, infraerrors.BadRequest("MIXED_GROUP_SUBSCRIPTION_TYPE",
				fmt.Sprintf("cannot mix subscription and standard groups in one api key (group %d conflicts with group %d)", groupID, firstGroup.ID))
		}
		groupsByID[groupID] = group
	}

	primaryGroupID := normalized[0]
	primaryGroup := groupsByID[primaryGroupID]
	apiKey.GroupID = &primaryGroupID
	apiKey.Group = primaryGroup
	apiKey.GroupIDs = normalized

	// 写入顺序（P2）：关联表（原生 SQL，独立连接池）先行；随后把「专属分组自动授权」
	// 与「主分组列更新」包进同一个 ent 事务（与单分组路径 admin_group.go 同口径）。
	//
	// 为什么不是全量原子：ReplaceAPIKeyGroups 走仓储的原生 SQL 执行器，不感知 ent tx，
	// 无法与 ent 操作并入同一事务。因此这里的目标不是「全原子」，而是**让任何失败都退化
	// 为安全侧**：
	//   - 关联表写失败 → 直接返回，授权尚未发生，绝无 user_allowed_groups 泄漏；
	//   - 事务失败回滚 → 授权与主分组列一起回滚，仅剩「候选集合已更新、主分组列未更新」的
	//     不一致；该状态下鉴权/决议读的是候选集合（均为上面校验过的 active 分组），不产生越权，
	//     admin 重试即可自愈（Replace 幂等 + 事务重做）。
	// 旧实现把授权放在最前且无事务，一旦后续写失败就会永久留下专属分组授权，
	// 用户随后可自行合法绑定——这正是本段要消除的泄漏。
	if err := s.replaceAPIKeyGroupsForAdmin(ctx, apiKey.ID, normalized, &primaryGroupID); err != nil {
		return nil, fmt.Errorf("update api key groups: %w", err)
	}

	// 收集需要自动授权的专属标准分组（与单分组路径一致）。
	exclusiveGrants := make([]int64, 0, len(normalized))
	for _, groupID := range normalized {
		group := groupsByID[groupID]
		if group == nil || !group.IsExclusive || group.IsSubscriptionType() {
			continue
		}
		exclusiveGrants = append(exclusiveGrants, groupID)
	}

	needTx := len(exclusiveGrants) > 0
	var tx *dbent.Tx
	opCtx := ctx
	if needTx {
		if s.entClient == nil {
			logger.LegacyPrintf("service.admin", "Warning: entClient is nil, skipping transaction protection for exclusive group binding")
		} else {
			txErr := error(nil)
			tx, txErr = s.entClient.Tx(ctx)
			if txErr != nil {
				return nil, fmt.Errorf("begin transaction: %w", txErr)
			}
			defer func() { _ = tx.Rollback() }()
			opCtx = dbent.NewTxContext(ctx, tx)
		}
	}

	for _, groupID := range exclusiveGrants {
		if err := s.userRepo.AddGroupToAllowedGroups(opCtx, apiKey.UserID, groupID); err != nil {
			return nil, fmt.Errorf("add group to user allowed groups: %w", err)
		}
		if result.GrantedGroupID == nil {
			result.AutoGrantedGroupAccess = true
			gid := groupID
			result.GrantedGroupID = &gid
			result.GrantedGroupName = groupsByID[groupID].Name
		}
	}

	if err := s.apiKeyRepo.Update(opCtx, apiKey, APIKeyUpdateFields{GroupID: true}); err != nil {
		return nil, fmt.Errorf("update api key: %w", err)
	}

	if tx != nil {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit transaction: %w", err)
		}
	}

	// 失效认证缓存（事务提交后执行）。
	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, apiKey.Key)
	}

	result.APIKey = apiKey
	return result, nil
}

// replaceAPIKeyGroupsForAdmin 通过窄接口调用关联表整体替换（仓储不支持时静默跳过，
// 保持对测试替身/旧实现的后向兼容）。
func (s *adminServiceImpl) replaceAPIKeyGroupsForAdmin(ctx context.Context, apiKeyID int64, groupIDs []int64, primaryGroupID *int64) error {
	repo, ok := s.apiKeyRepo.(APIKeyGroupBindingRepository)
	if !ok || repo == nil {
		return nil
	}
	return repo.ReplaceAPIKeyGroups(ctx, apiKeyID, groupIDs, primaryGroupID)
}

// AdminResetAPIKeyRateLimitUsage resets all API key rate-limit usage windows.
func (s *adminServiceImpl) AdminResetAPIKeyRateLimitUsage(ctx context.Context, keyID int64) (*APIKey, error) {
	apiKey, err := s.apiKeyRepo.GetByID(ctx, keyID)
	if err != nil {
		return nil, err
	}
	apiKey.Usage5h = 0
	apiKey.Usage1d = 0
	apiKey.Usage7d = 0
	apiKey.Window5hStart = nil
	apiKey.Window1dStart = nil
	apiKey.Window7dStart = nil
	if err := s.apiKeyRepo.Update(ctx, apiKey, APIKeyUpdateFields{RateLimitUsage: true}); err != nil {
		return nil, fmt.Errorf("reset api key rate limit usage: %w", err)
	}
	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, apiKey.Key)
	}
	if s.billingCacheService != nil {
		_ = s.billingCacheService.InvalidateAPIKeyRateLimit(ctx, apiKey.ID)
	}
	return apiKey, nil
}

// ReplaceUserGroup 替换用户的专属分组
func (s *adminServiceImpl) ReplaceUserGroup(ctx context.Context, userID, oldGroupID, newGroupID int64) (*ReplaceUserGroupResult, error) {
	if oldGroupID == newGroupID {
		return nil, infraerrors.BadRequest("SAME_GROUP", "old and new group must be different")
	}

	// 验证新分组存在且为活跃的专属标准分组
	newGroup, err := s.groupRepo.GetByID(ctx, newGroupID)
	if err != nil {
		return nil, err
	}
	if newGroup.Status != StatusActive {
		return nil, infraerrors.BadRequest("GROUP_NOT_ACTIVE", "target group is not active")
	}
	if !newGroup.IsExclusive {
		return nil, infraerrors.BadRequest("GROUP_NOT_EXCLUSIVE", "target group is not exclusive")
	}
	if newGroup.IsSubscriptionType() {
		return nil, infraerrors.BadRequest("GROUP_IS_SUBSCRIPTION", "subscription groups are not supported for replacement")
	}

	// 事务保证原子性
	if s.entClient == nil {
		return nil, fmt.Errorf("entClient is nil, cannot perform group replacement")
	}
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	opCtx := dbent.NewTxContext(ctx, tx)

	// 1. 授予新分组权限
	if err := s.userRepo.AddGroupToAllowedGroups(opCtx, userID, newGroupID); err != nil {
		return nil, fmt.Errorf("add new group to allowed groups: %w", err)
	}

	// 2. 迁移绑定旧分组的 Key 到新分组
	migrated, err := s.apiKeyRepo.UpdateGroupIDByUserAndGroup(opCtx, userID, oldGroupID, newGroupID)
	if err != nil {
		return nil, fmt.Errorf("migrate api keys: %w", err)
	}

	// 3. 移除旧分组权限
	if err := s.userRepo.RemoveGroupFromUserAllowedGroups(opCtx, userID, oldGroupID); err != nil {
		return nil, fmt.Errorf("remove old group from allowed groups: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}

	// 失效该用户所有 Key 的认证缓存
	if s.authCacheInvalidator != nil {
		keys, keyErr := s.apiKeyRepo.ListKeysByUserID(ctx, userID)
		if keyErr == nil {
			for _, k := range keys {
				s.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, k)
			}
		}
	}

	return &ReplaceUserGroupResult{MigratedKeys: migrated}, nil
}
