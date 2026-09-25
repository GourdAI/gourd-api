package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/dgraph-io/ristretto"
)

const apiKeyAuthSnapshotVersion = 26 // v26: candidate group objects materialized in auth snapshot (v25 snapshots carried empty Groups and silently degraded multi-group resolution)

type apiKeyAuthCacheConfig struct {
	l1Size        int
	l1TTL         time.Duration
	l2TTL         time.Duration
	negativeTTL   time.Duration
	jitterPercent int
	singleflight  bool
}

func newAPIKeyAuthCacheConfig(cfg *config.Config) apiKeyAuthCacheConfig {
	if cfg == nil {
		return apiKeyAuthCacheConfig{}
	}
	auth := cfg.APIKeyAuth
	return apiKeyAuthCacheConfig{
		l1Size:        auth.L1Size,
		l1TTL:         time.Duration(auth.L1TTLSeconds) * time.Second,
		l2TTL:         time.Duration(auth.L2TTLSeconds) * time.Second,
		negativeTTL:   time.Duration(auth.NegativeTTLSeconds) * time.Second,
		jitterPercent: auth.JitterPercent,
		singleflight:  auth.Singleflight,
	}
}

func (c apiKeyAuthCacheConfig) l1Enabled() bool {
	return c.l1Size > 0 && c.l1TTL > 0
}

func (c apiKeyAuthCacheConfig) l2Enabled() bool {
	return c.l2TTL > 0
}

func (c apiKeyAuthCacheConfig) negativeEnabled() bool {
	return c.negativeTTL > 0
}

// jitterTTL 为缓存 TTL 添加抖动，避免多个请求在同一时刻同时过期触发集中回源。
// 这里直接使用 rand/v2 的顶层函数：并发安全，无需全局互斥锁。
func (c apiKeyAuthCacheConfig) jitterTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return ttl
	}
	if c.jitterPercent <= 0 {
		return ttl
	}
	percent := c.jitterPercent
	if percent > 100 {
		percent = 100
	}
	delta := float64(percent) / 100
	randVal := rand.Float64()
	factor := 1 - delta + randVal*(2*delta)
	if factor <= 0 {
		return ttl
	}
	return time.Duration(float64(ttl) * factor)
}

func (s *APIKeyService) initAuthCache(cfg *config.Config) {
	s.authCfg = newAPIKeyAuthCacheConfig(cfg)
	if s.authCfg.negativeEnabled() {
		negativeSize := defaultNegativeAuthCacheSize
		if s.authCfg.l1Size > 0 && s.authCfg.l1Size < negativeSize {
			negativeSize = s.authCfg.l1Size
		}
		cache, err := ristretto.NewCache(&ristretto.Config{
			NumCounters: int64(negativeSize) * 10,
			MaxCost:     int64(negativeSize),
			BufferItems: 64,
		})
		if err == nil {
			s.authNegativeCacheL1 = cache
		}
	}
	if s.authCfg.l1Enabled() {
		cache, err := ristretto.NewCache(&ristretto.Config{
			NumCounters: int64(s.authCfg.l1Size) * 10,
			MaxCost:     int64(s.authCfg.l1Size),
			BufferItems: 64,
		})
		if err == nil {
			s.authCacheL1 = cache
		}
	}
}

// StartAuthCacheInvalidationSubscriber starts the Pub/Sub subscriber for L1 cache invalidation.
// This should be called after the service is fully initialized.
func (s *APIKeyService) StartAuthCacheInvalidationSubscriber(ctx context.Context) {
	if s.cache == nil || (s.authCacheL1 == nil && s.authNegativeCacheL1 == nil) {
		return
	}
	s.authInvalidationStart.Do(func() {
		subscriberCtx, cancel := context.WithCancel(ctx)
		subscriberCtx = withAuthCacheSubscriptionReady(subscriberCtx, func() {
			s.authInvalidationConnected.Store(true)
		})
		s.authInvalidationCancel = cancel
		s.authInvalidationWG.Add(1)
		go func() {
			defer s.authInvalidationWG.Done()
			backoff := time.Second
			for {
				err := s.cache.SubscribeAuthCacheInvalidation(subscriberCtx, func(cacheKey string) {
					s.invalidateLocalAuthCache(cacheKey)
				})
				wasConnected := s.authInvalidationConnected.Swap(false)
				if subscriberCtx.Err() != nil {
					return
				}
				if wasConnected {
					backoff = time.Second
				}
				s.authInvalidationFailures.Add(1)
				if err == nil {
					err = errors.New("auth cache invalidation subscription closed")
				}
				slog.Warn("failed to start auth cache invalidation subscriber; retrying", "error", err, "retry_in", backoff)
				timer := time.NewTimer(backoff)
				select {
				case <-subscriberCtx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				if backoff < 30*time.Second {
					backoff *= 2
					if backoff > 30*time.Second {
						backoff = 30 * time.Second
					}
				}
			}
		}()
	})
}

func (s *APIKeyService) invalidateLocalAuthCache(cacheKey string) {
	if s == nil {
		return
	}
	if s.authCacheL1 != nil {
		s.authCacheL1.Del(cacheKey)
	}
	if s.authNegativeCacheL1 != nil {
		s.authNegativeCacheL1.Del(cacheKey)
	}
}

type AuthCacheInvalidationSubscriberHealth struct {
	Connected bool   `json:"connected"`
	Failures  uint64 `json:"failures"`
}

func (s *APIKeyService) AuthCacheInvalidationSubscriberHealth() AuthCacheInvalidationSubscriberHealth {
	if s == nil {
		return AuthCacheInvalidationSubscriberHealth{}
	}
	return AuthCacheInvalidationSubscriberHealth{
		Connected: s.authInvalidationConnected.Load(),
		Failures:  s.authInvalidationFailures.Load(),
	}
}

func (s *APIKeyService) StopAuthCacheInvalidationSubscriber() {
	if s == nil {
		return
	}
	s.authInvalidationStop.Do(func() {
		if s.authInvalidationCancel != nil {
			s.authInvalidationCancel()
		}
		s.authInvalidationWG.Wait()
	})
}

func (s *APIKeyService) authCacheKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (s *APIKeyService) getAuthCacheEntry(ctx context.Context, cacheKey string) (*APIKeyAuthCacheEntry, bool) {
	if s.authCacheL1 != nil {
		if val, ok := s.authCacheL1.Get(cacheKey); ok {
			if entry, ok := val.(*APIKeyAuthCacheEntry); ok {
				return entry, true
			}
		}
	}
	if s.authNegativeCacheL1 != nil {
		if val, ok := s.authNegativeCacheL1.Get(cacheKey); ok {
			if entry, ok := val.(*APIKeyAuthCacheEntry); ok && entry.NotFound {
				return entry, true
			}
		}
	}
	if s.cache == nil || !s.authCfg.l2Enabled() {
		return nil, false
	}
	entry, err := s.cache.GetAuthCache(ctx, cacheKey)
	if err != nil {
		return nil, false
	}
	s.setAuthCacheL1(cacheKey, entry)
	return entry, true
}

func (s *APIKeyService) setAuthCacheL1(cacheKey string, entry *APIKeyAuthCacheEntry) {
	if entry == nil {
		return
	}
	if entry.NotFound {
		if s.authNegativeCacheL1 != nil && s.authCfg.negativeTTL > 0 {
			_ = s.authNegativeCacheL1.SetWithTTL(cacheKey, entry, 1, s.authCfg.jitterTTL(s.authCfg.negativeTTL))
		}
		return
	}
	if s.authCacheL1 == nil {
		return
	}
	ttl := s.authCfg.l1TTL
	ttl = s.authCfg.jitterTTL(ttl)
	_ = s.authCacheL1.SetWithTTL(cacheKey, entry, 1, ttl)
}

func (s *APIKeyService) setAuthCacheEntry(ctx context.Context, cacheKey string, entry *APIKeyAuthCacheEntry, ttl time.Duration) {
	if entry == nil {
		return
	}
	s.setAuthCacheL1(cacheKey, entry)
	if s.cache == nil || !s.authCfg.l2Enabled() {
		return
	}
	_ = s.cache.SetAuthCache(ctx, cacheKey, entry, s.authCfg.jitterTTL(ttl))
}

func (s *APIKeyService) deleteAuthCache(ctx context.Context, cacheKey string) {
	if s.authCacheL1 != nil {
		s.authCacheL1.Del(cacheKey)
	}
	if s.authNegativeCacheL1 != nil {
		s.authNegativeCacheL1.Del(cacheKey)
	}
	if s.cache == nil {
		return
	}
	_ = s.cache.DeleteAuthCache(ctx, cacheKey)
	// Publish invalidation message to other instances
	_ = s.cache.PublishAuthCacheInvalidation(ctx, cacheKey)
}

func (s *APIKeyService) loadAuthCacheEntry(ctx context.Context, key, cacheKey string) (*APIKeyAuthCacheEntry, error) {
	apiKey, err := s.lookupAPIKeyForAuth(ctx, key)
	if err != nil {
		if errors.Is(err, ErrAPIKeyNotFound) {
			entry := &APIKeyAuthCacheEntry{NotFound: true}
			if s.authCfg.negativeEnabled() {
				// Invalid keys are attacker-controlled and high-cardinality. Keep their
				// negative entries in the bounded process-local cache; do not amplify
				// random-key scans into Redis writes on every instance.
				s.setAuthCacheL1(cacheKey, entry)
			}
			return entry, nil
		}
		return nil, fmt.Errorf("get api key: %w", err)
	}
	apiKey.Key = key
	snapshot := s.snapshotFromAPIKey(ctx, apiKey)
	if snapshot == nil {
		return nil, fmt.Errorf("get api key: %w", ErrAPIKeyNotFound)
	}
	entry := &APIKeyAuthCacheEntry{Snapshot: snapshot}
	s.setAuthCacheEntry(ctx, cacheKey, entry, s.authCfg.l2TTL)
	return entry, nil
}

func (s *APIKeyService) lookupAPIKeyForAuth(ctx context.Context, key string) (*APIKey, error) {
	if s == nil || s.apiKeyRepo == nil {
		return nil, ErrAPIKeyNotFound
	}
	if s.authLookupSlots == nil {
		return s.apiKeyRepo.GetByKeyForAuth(ctx, key)
	}
	s.authLookupTotal.Add(1)
	select {
	case s.authLookupSlots <- struct{}{}:
		s.authLookupInFlight.Add(1)
		defer func() {
			s.authLookupInFlight.Add(-1)
			<-s.authLookupSlots
		}()
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		s.authLookupRejected.Add(1)
		return nil, ErrAPIKeyAuthOverloaded
	}
	return s.apiKeyRepo.GetByKeyForAuth(ctx, key)
}

func (s *APIKeyService) applyAuthCacheEntry(key string, entry *APIKeyAuthCacheEntry) (*APIKey, bool, error) {
	if entry == nil {
		return nil, false, nil
	}
	if entry.NotFound {
		return nil, true, ErrAPIKeyNotFound
	}
	if entry.Snapshot == nil {
		return nil, false, nil
	}
	if entry.Snapshot.Version != apiKeyAuthSnapshotVersion {
		return nil, false, nil
	}
	return s.snapshotToAPIKey(key, entry.Snapshot), true, nil
}

// apiKeyGroupSnapshotFromGroup 将分组对象投影为认证快照分组（主分组与候选分组共用，
// 保证字段口径一致：漏字段会让下游静默失效，集成测试对账兜底）。
func apiKeyGroupSnapshotFromGroup(g *Group) *APIKeyAuthGroupSnapshot {
	if g == nil {
		return nil
	}
	return &APIKeyAuthGroupSnapshot{
		ID:                              g.ID,
		Name:                            g.Name,
		Platform:                        g.Platform,
		IsExclusive:                     g.IsExclusive,
		Status:                          g.Status,
		SubscriptionType:                g.SubscriptionType,
		RateMultiplier:                  g.RateMultiplier,
		DailyLimitUSD:                   g.DailyLimitUSD,
		WeeklyLimitUSD:                  g.WeeklyLimitUSD,
		MonthlyLimitUSD:                 g.MonthlyLimitUSD,
		AllowImageGeneration:            g.AllowImageGeneration,
		AllowBatchImageGeneration:       g.AllowBatchImageGeneration,
		ImageRateIndependent:            g.ImageRateIndependent,
		ImageRateMultiplier:             g.ImageRateMultiplier,
		ImagePrice1K:                    g.ImagePrice1K,
		ImagePrice2K:                    g.ImagePrice2K,
		ImagePrice4K:                    g.ImagePrice4K,
		VideoRateIndependent:            g.VideoRateIndependent,
		VideoRateMultiplier:             g.VideoRateMultiplier,
		VideoPrice480P:                  g.VideoPrice480P,
		VideoPrice720P:                  g.VideoPrice720P,
		VideoPrice1080P:                 g.VideoPrice1080P,
		VideoModelPrices:                NormalizeVideoModelPrices(g.VideoModelPrices),
		WebSearchPricePerCall:           g.WebSearchPricePerCall,
		SearchPricePer1k:                g.SearchPricePer1k,
		AudioRealtimePricePerMin:        g.AudioRealtimePricePerMin,
		AudioTTSPricePerMillionChars:    g.AudioTTSPricePerMillionChars,
		AudioSTTPricePerHour:            g.AudioSTTPricePerHour,
		LongContextPricingEnabled:       g.LongContextPricingEnabled,
		ModelPricing:                    g.ModelPricing,
		ClaudeCodeOnly:                  g.ClaudeCodeOnly,
		FallbackGroupID:                 g.FallbackGroupID,
		FallbackGroupIDOnInvalidRequest: g.FallbackGroupIDOnInvalidRequest,
		ModelRouting:                    g.ModelRouting,
		ModelRoutingEnabled:             g.ModelRoutingEnabled,
		MCPXMLInject:                    g.MCPXMLInject,
		SupportedModelScopes:            g.SupportedModelScopes,
		AllowMessagesDispatch:           g.AllowMessagesDispatch,
		AllowLive:                       g.AllowLive,
		ForceOpenAIFast:                 g.ForceOpenAIFast,
		FreeOpenAIFast:                  g.FreeOpenAIFast,
		DefaultMappedModel:              g.DefaultMappedModel,
		MessagesDispatchModelConfig:     g.MessagesDispatchModelConfig,
		ModelAllowlist:                  g.ModelAllowlist,
		CodexModelsManifestConfig:       g.CodexModelsManifestConfig,
		RPMLimit:                        g.RPMLimit,
		MaxReasoningEffort:              g.MaxReasoningEffort,
		MaxReasoningEffortOverLimit:     g.MaxReasoningEffortOverLimit,
		ReasoningEffortMappings:         g.ReasoningEffortMappings,
		PeakRateEnabled:                 g.PeakRateEnabled,
		PeakStart:                       g.PeakStart,
		PeakEnd:                         g.PeakEnd,
		PeakRateMultiplier:              g.PeakRateMultiplier,
		ProfitControlEnabled:            g.ProfitControlEnabled,
		ProfitMinMargin:                 g.ProfitMinMargin,
		ProfitSafetyBuffer:              g.ProfitSafetyBuffer,
	}
}

// apiKeyGroupFromSnapshot 把认证快照分组物化回领域对象（主分组与候选分组共用）。
func apiKeyGroupFromSnapshot(s *APIKeyAuthGroupSnapshot) *Group {
	if s == nil {
		return nil
	}
	return &Group{
		ID:                              s.ID,
		Name:                            s.Name,
		Platform:                        s.Platform,
		IsExclusive:                     s.IsExclusive,
		Status:                          s.Status,
		Hydrated:                        true,
		SubscriptionType:                s.SubscriptionType,
		RateMultiplier:                  s.RateMultiplier,
		DailyLimitUSD:                   s.DailyLimitUSD,
		WeeklyLimitUSD:                  s.WeeklyLimitUSD,
		MonthlyLimitUSD:                 s.MonthlyLimitUSD,
		AllowImageGeneration:            s.AllowImageGeneration,
		AllowBatchImageGeneration:       s.AllowBatchImageGeneration,
		ImageRateIndependent:            s.ImageRateIndependent,
		ImageRateMultiplier:             s.ImageRateMultiplier,
		ImagePrice1K:                    s.ImagePrice1K,
		ImagePrice2K:                    s.ImagePrice2K,
		ImagePrice4K:                    s.ImagePrice4K,
		VideoRateIndependent:            s.VideoRateIndependent,
		VideoRateMultiplier:             s.VideoRateMultiplier,
		VideoPrice480P:                  s.VideoPrice480P,
		VideoPrice720P:                  s.VideoPrice720P,
		VideoPrice1080P:                 s.VideoPrice1080P,
		VideoModelPrices:                NormalizeVideoModelPrices(s.VideoModelPrices),
		WebSearchPricePerCall:           s.WebSearchPricePerCall,
		SearchPricePer1k:                s.SearchPricePer1k,
		AudioRealtimePricePerMin:        s.AudioRealtimePricePerMin,
		AudioTTSPricePerMillionChars:    s.AudioTTSPricePerMillionChars,
		AudioSTTPricePerHour:            s.AudioSTTPricePerHour,
		LongContextPricingEnabled:       s.LongContextPricingEnabled,
		ModelPricing:                    s.ModelPricing,
		ClaudeCodeOnly:                  s.ClaudeCodeOnly,
		FallbackGroupID:                 s.FallbackGroupID,
		FallbackGroupIDOnInvalidRequest: s.FallbackGroupIDOnInvalidRequest,
		ModelRouting:                    s.ModelRouting,
		ModelRoutingEnabled:             s.ModelRoutingEnabled,
		MCPXMLInject:                    s.MCPXMLInject,
		SupportedModelScopes:            s.SupportedModelScopes,
		AllowMessagesDispatch:           s.AllowMessagesDispatch,
		AllowLive:                       s.AllowLive,
		ForceOpenAIFast:                 s.ForceOpenAIFast,
		FreeOpenAIFast:                  s.FreeOpenAIFast,
		DefaultMappedModel:              s.DefaultMappedModel,
		MessagesDispatchModelConfig:     s.MessagesDispatchModelConfig,
		ModelAllowlist:                  s.ModelAllowlist,
		CodexModelsManifestConfig:       s.CodexModelsManifestConfig,
		RPMLimit:                        s.RPMLimit,
		MaxReasoningEffort:              s.MaxReasoningEffort,
		MaxReasoningEffortOverLimit:     s.MaxReasoningEffortOverLimit,
		ReasoningEffortMappings:         s.ReasoningEffortMappings,
		PeakRateEnabled:                 s.PeakRateEnabled,
		PeakStart:                       s.PeakStart,
		PeakEnd:                         s.PeakEnd,
		PeakRateMultiplier:              s.PeakRateMultiplier,
		ProfitControlEnabled:            s.ProfitControlEnabled,
		ProfitMinMargin:                 s.ProfitMinMargin,
		ProfitSafetyBuffer:              s.ProfitSafetyBuffer,
	}
}

func (s *APIKeyService) snapshotFromAPIKey(ctx context.Context, apiKey *APIKey) *APIKeyAuthSnapshot {
	if apiKey == nil || apiKey.User == nil {
		return nil
	}
	snapshot := &APIKeyAuthSnapshot{
		Version:     apiKeyAuthSnapshotVersion,
		APIKeyID:    apiKey.ID,
		UserID:      apiKey.UserID,
		GroupID:     apiKey.GroupID,
		Name:        apiKey.Name,
		Status:      apiKey.Status,
		IPWhitelist: apiKey.IPWhitelist,
		IPBlacklist: apiKey.IPBlacklist,
		Quota:       apiKey.Quota,
		QuotaUsed:   apiKey.QuotaUsed,
		ExpiresAt:   apiKey.ExpiresAt,
		RateLimit5h: apiKey.RateLimit5h,
		RateLimit1d: apiKey.RateLimit1d,
		RateLimit7d: apiKey.RateLimit7d,
		User: APIKeyAuthUserSnapshot{
			ID:                         apiKey.User.ID,
			Status:                     apiKey.User.Status,
			Role:                       apiKey.User.Role,
			Balance:                    apiKey.User.Balance,
			Concurrency:                apiKey.User.Concurrency,
			AllowedGroups:              apiKey.User.AllowedGroups,
			Email:                      apiKey.User.Email,
			Username:                   apiKey.User.Username,
			BalanceNotifyEnabled:       apiKey.User.BalanceNotifyEnabled,
			RestrictPublicGroups:       apiKey.User.RestrictPublicGroups,
			BalanceNotifyThresholdType: apiKey.User.BalanceNotifyThresholdType,
			BalanceNotifyThreshold:     apiKey.User.BalanceNotifyThreshold,
			BalanceNotifyExtraEmails:   apiKey.User.BalanceNotifyExtraEmails,
			TotalRecharged:             apiKey.User.TotalRecharged,
			RPMLimit:                   apiKey.User.RPMLimit,
		},
	}

	// 填充 (user, group) RPM override —— snapshot 构建时查一次 DB，后续请求零 DB 往返。
	if apiKey.GroupID != nil && *apiKey.GroupID > 0 && s.userGroupRateRepo != nil {
		override, err := s.userGroupRateRepo.GetRPMOverrideByUserAndGroup(ctx, apiKey.UserID, *apiKey.GroupID)
		if err == nil && override != nil {
			snapshot.User.UserGroupRPMOverride = override
		}
		// 查询失败或无 override 时留 nil，checkRPM 会回退到 DB 查询
	}
	if apiKey.Group != nil {
		snapshot.Group = apiKeyGroupSnapshotFromGroup(apiKey.Group)
	}

	// 候选分组集合：主分组优先，其余按 apiKey.GroupIDs 顺序物化。
	// 单分组 key：GroupIDs=[主分组]、Groups=[主分组]，与 v24 语义完全一致。
	groupIDs := apiKey.GroupIDs
	if len(groupIDs) == 0 && apiKey.GroupID != nil && *apiKey.GroupID > 0 {
		groupIDs = []int64{*apiKey.GroupID}
	}
	if len(groupIDs) > 0 {
		snapshot.GroupIDs = normalizeGroupIDsForSnapshot(groupIDs, apiKey.Group)
		groups := make([]*APIKeyAuthGroupSnapshot, 0, len(snapshot.GroupIDs))
		for _, groupID := range snapshot.GroupIDs {
			if snapshot.Group != nil && snapshot.Group.ID == groupID {
				// 主分组直接复用已物化快照，保证与 Group 字段逐位一致。
				groups = append(groups, snapshot.Group)
				continue
			}
			for _, candidate := range apiKey.Groups {
				if candidate != nil && candidate.ID == groupID {
					groups = append(groups, apiKeyGroupSnapshotFromGroup(candidate))
					break
				}
			}
		}
		snapshot.Groups = groups
	}
	return snapshot
}

// normalizeGroupIDsForSnapshot 归一化快照候选分组顺序：主分组置首，其余保持原序去重。
// 生成快照时使用，确保快照内 GroupIDs[0] 恒为主分组（与 api_keys.group_id 语义一致）。
func normalizeGroupIDsForSnapshot(groupIDs []int64, primary *Group) []int64 {
	out := make([]int64, 0, len(groupIDs)+1)
	seen := make(map[int64]struct{}, len(groupIDs)+1)
	if primary != nil && primary.ID > 0 {
		seen[primary.ID] = struct{}{}
		out = append(out, primary.ID)
	}
	for _, id := range groupIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func (s *APIKeyService) snapshotToAPIKey(key string, snapshot *APIKeyAuthSnapshot) *APIKey {
	if snapshot == nil {
		return nil
	}
	apiKey := &APIKey{
		ID:          snapshot.APIKeyID,
		UserID:      snapshot.UserID,
		GroupID:     snapshot.GroupID,
		Key:         key,
		Name:        snapshot.Name,
		Status:      snapshot.Status,
		IPWhitelist: snapshot.IPWhitelist,
		IPBlacklist: snapshot.IPBlacklist,
		Quota:       snapshot.Quota,
		QuotaUsed:   snapshot.QuotaUsed,
		ExpiresAt:   snapshot.ExpiresAt,
		RateLimit5h: snapshot.RateLimit5h,
		RateLimit1d: snapshot.RateLimit1d,
		RateLimit7d: snapshot.RateLimit7d,
		User: &User{
			ID:                         snapshot.User.ID,
			Status:                     snapshot.User.Status,
			Role:                       snapshot.User.Role,
			Balance:                    snapshot.User.Balance,
			Concurrency:                snapshot.User.Concurrency,
			AllowedGroups:              snapshot.User.AllowedGroups,
			Email:                      snapshot.User.Email,
			Username:                   snapshot.User.Username,
			BalanceNotifyEnabled:       snapshot.User.BalanceNotifyEnabled,
			RestrictPublicGroups:       snapshot.User.RestrictPublicGroups,
			BalanceNotifyThresholdType: snapshot.User.BalanceNotifyThresholdType,
			BalanceNotifyThreshold:     snapshot.User.BalanceNotifyThreshold,
			BalanceNotifyExtraEmails:   snapshot.User.BalanceNotifyExtraEmails,
			TotalRecharged:             snapshot.User.TotalRecharged,
			RPMLimit:                   snapshot.User.RPMLimit,
			UserGroupRPMOverride:       snapshot.User.UserGroupRPMOverride,
		},
	}
	// 主分组对象与候选分组集合统一由快照物化（口径一致）。
	if snapshot.Group != nil {
		apiKey.Group = apiKeyGroupFromSnapshot(snapshot.Group)
	}
	if len(snapshot.GroupIDs) > 0 {
		apiKey.GroupIDs = append([]int64(nil), snapshot.GroupIDs...)
		candidates := make([]*Group, 0, len(snapshot.GroupIDs))
		for _, groupID := range snapshot.GroupIDs {
			if apiKey.Group != nil && apiKey.Group.ID == groupID {
				candidates = append(candidates, apiKey.Group)
				continue
			}
			matched := false
			for _, group := range snapshot.Groups {
				if group != nil && group.ID == groupID {
					candidates = append(candidates, apiKeyGroupFromSnapshot(group))
					matched = true
					break
				}
			}
			if !matched {
				// 候选快照缺失（历史快照或字段裁剪）：跳过，多分组决议会退化为可用候选子集。
				continue
			}
		}
		apiKey.Groups = candidates
	}
	s.compileAPIKeyIPRules(apiKey)
	return apiKey
}
