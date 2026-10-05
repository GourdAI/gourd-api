package service

import (
	"context"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

const (
	modelRateLimitsKey                 = "model_rate_limits"
	antigravityGeminiModelRateLimitKey = "antigravity:gemini"
	openAIImageGenerationRateLimitKey  = "openai:image_generation"
	openAICodexSparkRateLimitReason    = "openai_codex_spark_rate_limit"
	// anthropicFableRateLimitKey 是 Anthropic 7d_oi（Fable 专属 7d 窗口）限流的
	// 家族级 scope：命中后所有 Fable 变体（含 [1m] 等后缀）都不再调度到该账号。
	anthropicFableRateLimitKey = "claude-fable-5"
)

// isRateLimitActiveForKey 检查指定 key 的限流是否生效
func (a *Account) isRateLimitActiveForKey(key string) bool {
	resetAt := a.modelRateLimitResetAt(key)
	return resetAt != nil && time.Now().Before(*resetAt)
}

// getRateLimitRemainingForKey 获取指定 key 的限流剩余时间，0 表示未限流或已过期
func (a *Account) getRateLimitRemainingForKey(key string) time.Duration {
	resetAt := a.modelRateLimitResetAt(key)
	if resetAt == nil {
		return 0
	}
	remaining := time.Until(*resetAt)
	if remaining > 0 {
		return remaining
	}
	return 0
}

func (a *Account) isModelRateLimitedWithContext(ctx context.Context, requestedModel string) bool {
	for _, key := range a.modelRateLimitKeysForRequest(ctx, requestedModel) {
		if a.isRateLimitActiveForKey(key) {
			return true
		}
	}
	return false
}

// GetModelRateLimitRemainingTime 获取模型限流剩余时间
// 返回 0 表示未限流或已过期
func (a *Account) GetModelRateLimitRemainingTime(requestedModel string) time.Duration {
	return a.GetModelRateLimitRemainingTimeWithContext(context.Background(), requestedModel)
}

func (a *Account) GetModelRateLimitRemainingTimeWithContext(ctx context.Context, requestedModel string) time.Duration {
	remaining := time.Duration(0)
	for _, key := range a.modelRateLimitKeysForRequest(ctx, requestedModel) {
		if keyRemaining := a.getRateLimitRemainingForKey(key); keyRemaining > remaining {
			remaining = keyRemaining
		}
	}
	return remaining
}

func (a *Account) modelRateLimitKeysForRequest(ctx context.Context, requestedModel string) []string {
	if a == nil {
		return nil
	}

	raw := strings.TrimSpace(requestedModel)
	if raw == "" {
		return nil
	}

	modelKey := a.GetMappedModel(raw)
	if a.Platform == PlatformAntigravity {
		modelKey = resolveFinalAntigravityModelKey(ctx, a, raw)
	}
	modelKey = strings.TrimSpace(modelKey)
	// Antigravity 的映射表是白名单：未配置的模型 mapAntigravityModel 返回空串，
	// 而写侧 (modelRateLimitKeyForUpstreamModelNotFound) 在解析不出时**回退到原始名**。
	// 读侧必须做同样的回退：此前在这里直接 return nil，会让下面的双键兜底在
	// Antigravity 平台整体失效（实测：写 key="claude-opus-4-8"、读 keys=nil）。
	if modelKey == "" {
		modelKey = raw
	}

	// 写入侧存在两套模型名口径：管理员自定义临时不可调度规则
	// (triggerTempUnschedulable) 记录的是客户端请求的原始模型名，而
	// model-not-found / Codex Spark / 生图冷却
	// (modelRateLimitKeyForUpstreamModelNotFound) 记录的是账号映射后的上游模型名。
	// 只查映射名时，前者在配了 model_mapping 的账号上永远命中不了：冷却写进了 DB
	// 却拦不住调度，同一账号被反复选中并再次撞上上游 404。因此两个键都要查。
	keys := make([]string, 0, 4)
	appendKey := func(key string) {
		if key = strings.TrimSpace(key); key != "" && !containsString(keys, key) {
			keys = append(keys, key)
		}
	}
	appendKey(modelKey)
	appendKey(raw)
	// Antigravity 的 429/503 冷却写入口 (setModelRateLimitByModelName) 用的是上游
	// error metadata 里的**官方模型 ID**（normalize 后，不带 -thinking 后缀）。
	// 别名 + thinking 组合下，本函数只会得到 [官方名-thinking, 别名] 两个键，
	// 恰好漏写官方名本身 → 冷却永远读不到（实测：写 claude-sonnet-4-5，
	// 读 [claude-sonnet-4-5-thinking, sonnet]）。客户端直接用官方名时本就能命中
	// （raw 在 keys 里），所以补上「不带 thinking 后缀的映射名」只是让别名用户
	// 与直接名用户获得一致的拦截能力，不新增误封。
	if a.Platform == PlatformAntigravity {
		appendKey(normalizeAntigravityModelName(mapAntigravityModel(a, raw)))
	}
	// 家族级 scope：原始名、映射名与官方名任一命中家族即纳入（两个写入口径都可能落在家族 key 上）。
	matchesAny := func(predicate func(string) bool) bool {
		for _, name := range keys {
			if predicate(name) {
				return true
			}
		}
		return false
	}
	switch a.Platform {
	case PlatformAntigravity:
		if matchesAny(isAntigravityGeminiModel) {
			appendKey(antigravityGeminiModelRateLimitKey)
		}
	case PlatformOpenAI:
		if matchesAny(func(name string) bool {
			return openAIImageGenerationRateLimitApplies(ctx, name, name)
		}) {
			appendKey(openAIImageGenerationRateLimitKey)
		}
	case PlatformAnthropic:
		if matchesAny(isAnthropicFableModel) {
			appendKey(anthropicFableRateLimitKey)
		}
	}
	return keys
}

// isAnthropicFableModel 判断是否为 Fable 模型家族（claude-fable-5、claude-fable-5[1m] 等变体）
func isAnthropicFableModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "fable")
}

func openAIImageGenerationRateLimitApplies(ctx context.Context, requestedModel, modelKey string) bool {
	if isOpenAIImageGenerationModel(requestedModel) || isOpenAIImageGenerationModel(modelKey) {
		return true
	}
	return OpenAIImageGenerationIntentFromContext(ctx)
}

func WithOpenAIImageGenerationIntent(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, ctxkey.OpenAIImageGenerationIntent, true)
}

func OpenAIImageGenerationIntentFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, ok := ctx.Value(ctxkey.OpenAIImageGenerationIntent).(bool)
	return ok && enabled
}

// WithOpenAIImagesEndpoint 标记请求从 /v1/images/* 专用生图端点入站。
func WithOpenAIImagesEndpoint(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, ctxkey.OpenAIImagesEndpoint, true)
}

// OpenAIImagesEndpointFromContext 报告请求是否来自 /v1/images/*。
func OpenAIImagesEndpointFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, ok := ctx.Value(ctxkey.OpenAIImagesEndpoint).(bool)
	return ok && enabled
}

func resolveFinalAntigravityModelKey(ctx context.Context, account *Account, requestedModel string) string {
	modelKey := mapAntigravityModel(account, requestedModel)
	if modelKey == "" {
		return ""
	}
	// thinking 会影响 Antigravity 最终模型名（例如 claude-sonnet-4-5 -> claude-sonnet-4-5-thinking）
	if enabled, ok := ThinkingEnabledFromContext(ctx); ok {
		modelKey = applyThinkingModelSuffix(modelKey, enabled)
	}
	return modelKey
}

func isAntigravityGeminiModel(model string) bool {
	return strings.HasPrefix(normalizeAntigravityModelName(model), "gemini-")
}

func antigravityModelRateLimitKeys(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	keys := []string{model}
	if isAntigravityGeminiModel(model) && model != antigravityGeminiModelRateLimitKey {
		keys = append(keys, antigravityGeminiModelRateLimitKey)
	}
	return keys
}

func (a *Account) modelRateLimitResetAt(scope string) *time.Time {
	if a == nil || a.Extra == nil || scope == "" {
		return nil
	}
	rawLimits, ok := a.Extra[modelRateLimitsKey].(map[string]any)
	if !ok {
		return nil
	}
	rawLimit, ok := rawLimits[scope].(map[string]any)
	if !ok {
		return nil
	}
	resetAtRaw, ok := rawLimit["rate_limit_reset_at"].(string)
	if !ok || strings.TrimSpace(resetAtRaw) == "" {
		return nil
	}
	resetAt, err := time.Parse(time.RFC3339, resetAtRaw)
	if err != nil {
		return nil
	}
	return &resetAt
}

func setAccountModelRateLimitSnapshot(account *Account, scope string, resetAt time.Time, reason string, now time.Time) {
	if account == nil || strings.TrimSpace(scope) == "" {
		return
	}
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	limits, ok := account.Extra[modelRateLimitsKey].(map[string]any)
	if !ok {
		limits = make(map[string]any)
		account.Extra[modelRateLimitsKey] = limits
	}
	payload := map[string]any{
		"rate_limited_at":     now.UTC().Format(time.RFC3339),
		"rate_limit_reset_at": resetAt.UTC().Format(time.RFC3339),
	}
	if reason = strings.TrimSpace(reason); reason != "" {
		payload["reason"] = reason
	}
	limits[scope] = payload
}
