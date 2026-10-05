package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

// APIKeyRateLimitCacheData holds rate limit usage data cached in Redis.
type APIKeyRateLimitCacheData struct {
	Usage5h  float64 `json:"usage_5h"`
	Usage1d  float64 `json:"usage_1d"`
	Usage7d  float64 `json:"usage_7d"`
	Window5h int64   `json:"window_5h"` // unix timestamp, 0 = not started
	Window1d int64   `json:"window_1d"`
	Window7d int64   `json:"window_7d"`
}

// UserPlatformQuotaKey 标识一个 user×platform，用于脏集出入与批量读。
type UserPlatformQuotaKey struct {
	UserID   int64
	Platform string
}

// UserPlatformQuotaCacheEntry Redis hash 反序列化结果。
//
// SchemaVersion 用于向后兼容：
//   - 0（旧 entry，无 SchemaVersion 字段）→ 视为 cache MISS，强制 refresh
//   - 1（当前版本）→ 包含 limits 和 window_start，可免 DB 查询
//
// limit 字段为 nil 表示"无限额"（DB 中对应列为 NULL）。
const UserPlatformQuotaCacheSchemaV1 = int64(1)

type UserPlatformQuotaCacheEntry struct {
	DailyUsageUSD   float64
	WeeklyUsageUSD  float64
	MonthlyUsageUSD float64
	Version         int64
	SchemaVersion   int64

	// 以下字段仅在 SchemaVersion >= 1 时有效
	DailyLimitUSD   *float64
	WeeklyLimitUSD  *float64
	MonthlyLimitUSD *float64

	DailyWindowStart   *time.Time
	WeeklyWindowStart  *time.Time
	MonthlyWindowStart *time.Time
}

// BillingCache defines cache operations for billing service
type BillingCache interface {
	// Balance operations
	GetUserBalance(ctx context.Context, userID int64) (float64, error)
	SetUserBalance(ctx context.Context, userID int64, balance float64) error
	DeductUserBalance(ctx context.Context, userID int64, amount float64) error
	InvalidateUserBalance(ctx context.Context, userID int64) error

	// Subscription operations（钱包模型：按 user 单键聚合，不再有分组槽位）
	GetSubscriptionCache(ctx context.Context, userID int64) (*SubscriptionCacheData, error)
	SetSubscriptionCache(ctx context.Context, userID int64, data *SubscriptionCacheData) error
	UpdateSubscriptionUsage(ctx context.Context, userID int64, cost float64) error
	InvalidateSubscriptionCache(ctx context.Context, userID int64) error

	// API Key rate limit operations
	GetAPIKeyRateLimit(ctx context.Context, keyID int64) (*APIKeyRateLimitCacheData, error)
	SetAPIKeyRateLimit(ctx context.Context, keyID int64, data *APIKeyRateLimitCacheData) error
	UpdateAPIKeyRateLimitUsage(ctx context.Context, keyID int64, cost float64) error
	InvalidateAPIKeyRateLimit(ctx context.Context, keyID int64) error

	// user × platform quota 缓存
	GetUserPlatformQuotaCache(ctx context.Context, userID int64, platform string) (*UserPlatformQuotaCacheEntry, bool, error)
	SetUserPlatformQuotaCache(ctx context.Context, userID int64, platform string, entry *UserPlatformQuotaCacheEntry, ttl time.Duration) error
	DeleteUserPlatformQuotaCache(ctx context.Context, userID int64, platform string) error
	// IncrUserPlatformQuotaUsageCache 在缓存命中时累加用量；缓存未命中（key 不存在）静默返回 nil。
	// markDirty=true 时将该 key 的 member 写入 Redis 脏集，供 flusher 批量回写 DB。
	IncrUserPlatformQuotaUsageCache(ctx context.Context, userID int64, platform string, cost float64, ttl time.Duration, markDirty bool) error

	// 脏集读写，供 flusher 使用。
	PopDirtyUserPlatformQuotaKeys(ctx context.Context, n int) ([]UserPlatformQuotaKey, error)
	ReaddDirtyUserPlatformQuotaKeys(ctx context.Context, keys []UserPlatformQuotaKey) error
	BatchGetUserPlatformQuotaCache(ctx context.Context, keys []UserPlatformQuotaKey) ([]*UserPlatformQuotaCacheEntry, error)
}

// ModelPricing 模型价格配置（per-token价格，与LiteLLM格式一致）
type ModelPricing struct {
	InputPricePerToken                 float64  // 每token输入价格 (USD)
	InputPricePerTokenPriority         float64  // priority service tier 下每token输入价格 (USD)
	ImageInputPricePerToken            float64  // 图片输入 token 价格 (USD)，用于多模态 embedding 等图文不同价场景；为 0 时回退到 InputPricePerToken
	ImageCacheReadPricePerToken        float64  // 图片缓存输入价格；无独立价格时沿用缓存读取价
	OutputPricePerToken                float64  // 每token输出价格 (USD)
	OutputPricePerTokenPriority        float64  // priority service tier 下每token输出价格 (USD)
	CacheCreationPricePerToken         float64  // 缓存创建每token价格 (USD)
	CacheCreationPricePerTokenPriority float64  // priority service tier 下缓存创建每token价格 (USD)
	CacheCreationPriceExplicit         bool     // 是否由渠道/区间定价显式设定（为 true 时即使 == 0 也不回退）
	CacheReadPricePerToken             float64  // 缓存读取每token价格 (USD)
	CacheReadPricePerTokenPriority     float64  // priority service tier 下缓存读取每token价格 (USD)
	FastMultiplier                     *float64 // 渠道显式 Fast/priority 倍率；nil 时沿用模型目录行为
	FlexMultiplier                     *float64 // 渠道显式 Flex 倍率；nil 时沿用默认行为
	MaxReasoningEffortMultiplier       *float64 // max 推理等级的额度/计费倍率；nil 时沿用模型默认行为
	CacheCreation5mPrice               float64  // 5分钟缓存创建每token价格 (USD)
	CacheCreation1hPrice               float64  // 1小时缓存创建每token价格 (USD)
	SupportsCacheBreakdown             bool     // 是否支持详细的缓存分类
	LongContextInputThreshold          int      // 超过阈值后按整次会话提升输入价格
	LongContextThresholdInclusive      bool     // 达到阈值即应用（xAI）；默认保持严格大于以兼容既有模型
	LongContextInputMultiplier         float64  // 长上下文整次会话输入倍率
	LongContextOutputMultiplier        float64  // 长上下文整次会话输出倍率
	ImageOutputPricePerToken           float64  // 图片输出 token 价格 (USD)
	ImageOutputPriceExplicit           bool     // 是否由渠道定价显式设定（为 true 时即使 == 0 也不回退）
}

func normalizeBillingServiceTier(serviceTier string) string {
	return strings.ToLower(strings.TrimSpace(serviceTier))
}

func usePriorityServiceTierPricing(serviceTier string, pricing *ModelPricing) bool {
	if pricing == nil {
		return false
	}
	tier := normalizeBillingServiceTier(serviceTier)
	if tier != "priority" && tier != "fast" {
		return false
	}
	if pricing.FastMultiplier != nil {
		return false
	}
	return pricing.InputPricePerTokenPriority > 0 || pricing.OutputPricePerTokenPriority > 0 ||
		pricing.CacheCreationPricePerTokenPriority > 0 || pricing.CacheReadPricePerTokenPriority > 0
}

func serviceTierCostMultiplier(serviceTier string) float64 {
	switch normalizeBillingServiceTier(serviceTier) {
	case "priority", "fast", OpenAIFastTierUltrafast:
		return 2.0
	case "flex":
		return 0.5
	default:
		return 1.0
	}
}

func configuredServiceTierMultiplier(serviceTier string, pricing *ModelPricing) float64 {
	if pricing != nil {
		switch normalizeBillingServiceTier(serviceTier) {
		case "priority", "fast":
			if pricing.FastMultiplier != nil {
				return *pricing.FastMultiplier
			}
		case "flex":
			if pricing.FlexMultiplier != nil {
				return *pricing.FlexMultiplier
			}
		}
	}
	return serviceTierCostMultiplier(serviceTier)
}

func pricingWithPriorityMultiplier(base *ModelPricing, multiplier float64) *ModelPricing {
	if base == nil {
		return nil
	}
	cloned := *base
	cloned.InputPricePerTokenPriority = cloned.InputPricePerToken * multiplier
	cloned.OutputPricePerTokenPriority = cloned.OutputPricePerToken * multiplier
	cloned.CacheCreationPricePerTokenPriority = cloned.CacheCreationPricePerToken * multiplier
	cloned.CacheReadPricePerTokenPriority = cloned.CacheReadPricePerToken * multiplier
	return &cloned
}

// UsageTokens 使用的token数量
type UsageTokens struct {
	InputTokens           int
	ImageInputTokens      int
	ImageCacheReadTokens  int
	OutputTokens          int
	CacheCreationTokens   int
	CacheReadTokens       int
	CacheCreation5mTokens int
	CacheCreation1hTokens int
	ImageOutputTokens     int
}

// CostBreakdown 费用明细
type CostBreakdown struct {
	InputCost                 float64 // 文本输入费用（不含图片输入，图片输入单独记入 ImageInputCost）
	ImageInputCost            float64 // 图片输入 token 费用（如 gpt-image-2 图片编辑）
	OutputCost                float64
	ImageOutputCost           float64
	CacheCreationCost         float64
	CacheReadCost             float64
	TotalCost                 float64
	ActualCost                float64 // 应用倍率后的实际费用
	BillingMode               string  // 计费模式（"token"/"per_request"/"image"），由 CalculateCostUnified 填充
	LongContextBillingApplied bool
}

func applyCostBreakdownMultiplier(cost *CostBreakdown, multiplier float64) {
	if cost == nil || multiplier == 1 {
		return
	}
	cost.InputCost *= multiplier
	cost.ImageInputCost *= multiplier
	cost.OutputCost *= multiplier
	cost.ImageOutputCost *= multiplier
	cost.CacheCreationCost *= multiplier
	cost.CacheReadCost *= multiplier
	cost.TotalCost *= multiplier
	cost.ActualCost *= multiplier
}

const claudeFable51MaxReasoningEffortMultiplier = 3.0

func isClaudeFable51Model(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, marker := range []string{"fable-5-1", "fable-5.1", "fable5.1", "fable51"} {
		if at := strings.Index(model, marker); at >= 0 {
			after := at + len(marker)
			if after == len(model) || model[after] < '0' || model[after] > '9' {
				return true
			}
		}
	}
	return false
}

func defaultMaxReasoningEffortMultiplier(model string) *float64 {
	if !isClaudeFable51Model(model) {
		return nil
	}
	multiplier := claudeFable51MaxReasoningEffortMultiplier
	return &multiplier
}

func maxReasoningEffortBillingMultiplier(model, effort string, pricing *ModelPricing) float64 {
	if NormalizeMaxReasoningEffort(effort) != "max" {
		return 1
	}
	if pricing != nil && pricing.MaxReasoningEffortMultiplier != nil && *pricing.MaxReasoningEffortMultiplier > 0 {
		return *pricing.MaxReasoningEffortMultiplier
	}
	if multiplier := defaultMaxReasoningEffortMultiplier(model); multiplier != nil {
		return *multiplier
	}
	return 1
}

func resolvedChannelTimeMultiplier(resolved *ResolvedPricing, at time.Time) float64 {
	if resolved == nil || resolved.Source != PricingSourceChannel || resolved.channelPricing == nil {
		return 1
	}
	return resolved.channelPricing.TimePricing.MultiplierAt(at)
}

// ErrModelPricingUnavailable indicates that none of the configured pricing
// sources can price the requested model.
var ErrModelPricingUnavailable = errors.New("pricing not found")

// ---- DeepSeek 官方低谷价（$/token）----
// 2026-09-10 官方公告：DeepSeek-V4.1-Flash（新名 deepseek-flash）大幅降价，
// Flash 低谷价降为 $0.15/$0.60/$0.003 per MTok（输入缓存未命中/输出/缓存命中）；
// deepseek-v4-pro 名义价格暂不变，但自北京时间 2026-09-14 12:00（04:00 UTC）起
// 其请求被上游路由到 V4.1-Flash 并按 Flash 价计费（见 deepseekProBilledAsFlash）。
// Source: https://api-docs.deepseek.com/news/news260910
//
//	https://api-docs.deepseek.com/quick_start/pricing
//
// 高峰价 = 2× 低谷价；高峰时段 01:00–04:00 与 06:00–10:00 UTC（仅工作日），
// 北京时间周六/周日全天低谷。时段判定见 deepseekPeakMultiplierAt。
const (
	deepseekFlashOffPeakInputPrice  = 1.5e-7  // $0.15 per MTok (cache miss)
	deepseekFlashOffPeakOutputPrice = 6.0e-7  // $0.60 per MTok
	deepseekFlashOffPeakCacheRead   = 3e-9    // $0.003 per MTok (cache hit)
	deepseekProOffPeakInputPrice    = 6.6e-7  // $0.66 per MTok (cache miss)
	deepseekProOffPeakOutputPrice   = 1.98e-6 // $1.98 per MTok
	deepseekProOffPeakCacheRead     = 2.2e-8  // $0.022 per MTok (cache hit)
)

// isDeepSeekModel 判断模型名是否为 DeepSeek 模型（大小写不敏感）。
// 任意 deepseek- 前缀均视为 DeepSeek 模型：官方模型（v4-flash / v4-pro /
// v4-flash-vision-exp）按各自价卡计价，其余 deepseek-*（含已停服的
// deepseek-chat / deepseek-reasoner 与未知型号）统一按 flash 价兜底，
// 避免计费中断；新名字由 fallback warn 日志（每模型每进程一条）暴露，
// 运营者据此更新价卡。
func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek-")
}

// deepseekPeakMultiplierAt 返回指定时刻的 DeepSeek 官方峰谷定价因子。
// 官方口径（2026-08-23 起生效）：高峰价 = 2× 低谷价；高峰时段为
// 01:00–04:00 与 06:00–10:00 UTC（半开区间），仅工作日；
// 周末（北京时间周六/周日）全天低谷。北京时间用固定 +8 偏移（无夏令时）。
func deepseekPeakMultiplierAt(now time.Time) float64 {
	beijing := now.In(time.FixedZone("Asia/Shanghai", 8*3600))
	switch beijing.Weekday() {
	case time.Saturday, time.Sunday:
		return 1.0
	}
	switch h := now.UTC().Hour(); {
	case h >= 1 && h < 4, h >= 6 && h < 10:
		return 2.0
	}
	return 1.0
}

// deepseekProRoutesToFlashAt：官方公告自北京时间 2026-09-14 12:00（04:00 UTC）起，
// 所有 deepseek-v4-pro 请求被上游路由到 V4.1-Flash 并按 Flash 价计费（直至未来
// V4.1 Pro 上线）。Source: https://api-docs.deepseek.com/news/news260910
var deepseekProRoutesToFlashAt = time.Date(2026, 9, 14, 4, 0, 0, 0, time.UTC)

// deepseekProBilledAsFlash 报告指定计费时点 deepseek-v4-pro 是否已按 Flash 价
// 计费：计费时点到达或晚于切换时点返回 true；零值时点回退当前时刻（与峰谷
// 倍率的取时点方式一致，见 calculateTokenCost）。
func deepseekProBilledAsFlash(pricingAt time.Time) bool {
	if pricingAt.IsZero() {
		pricingAt = timezone.Now()
	}
	return !pricingAt.Before(deepseekProRoutesToFlashAt)
}

// isDeepSeekProModel 判断模型名是否归入 deepseek-v4-pro 档（含版本化名称，
// 如 deepseek-v4-pro-0813）。
func isDeepSeekProModel(model string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(model)), "deepseek-v4-pro")
}

// BillingService 计费服务
type BillingService struct {
	cfg            *config.Config
	pricingService *PricingService
	fallbackPrices map[string]*ModelPricing // 硬编码回退价格

	// fallbackWarnSeen 记录已打过 fallback 警告日志的(已小写化)模型名,
	// 让 "[Billing] Using fallback pricing" 每个模型每进程最多打一条,
	// 避免热路径上每请求刷屏(issue #3394)。零值即可用,无需在构造函数初始化。
	fallbackWarnSeen sync.Map
}

// NewBillingService 创建计费服务实例
func NewBillingService(cfg *config.Config, pricingService *PricingService) *BillingService {
	s := &BillingService{
		cfg:            cfg,
		pricingService: pricingService,
		fallbackPrices: make(map[string]*ModelPricing),
	}

	// 初始化硬编码回退价格（当动态价格不可用时使用）
	s.initFallbackPricing()

	return s
}

// initFallbackPricing 初始化硬编码回退价格（当动态价格不可用时使用）
// 价格单位：USD per token（与LiteLLM格式一致）
func (s *BillingService) initFallbackPricing() {
	// Claude 4.5 Opus
	s.fallbackPrices["claude-opus-4.5"] = &ModelPricing{
		InputPricePerToken:         5e-6,    // $5 per MTok
		OutputPricePerToken:        25e-6,   // $25 per MTok
		CacheCreationPricePerToken: 6.25e-6, // $6.25 per MTok
		CacheReadPricePerToken:     0.5e-6,  // $0.50 per MTok
		SupportsCacheBreakdown:     false,
	}

	// Claude 4 Sonnet
	s.fallbackPrices["claude-sonnet-4"] = &ModelPricing{
		InputPricePerToken:         3e-6,    // $3 per MTok
		OutputPricePerToken:        15e-6,   // $15 per MTok
		CacheCreationPricePerToken: 3.75e-6, // $3.75 per MTok
		CacheReadPricePerToken:     0.3e-6,  // $0.30 per MTok
		SupportsCacheBreakdown:     false,
	}

	// Claude 3.5 Sonnet
	s.fallbackPrices["claude-3-5-sonnet"] = &ModelPricing{
		InputPricePerToken:         3e-6,    // $3 per MTok
		OutputPricePerToken:        15e-6,   // $15 per MTok
		CacheCreationPricePerToken: 3.75e-6, // $3.75 per MTok
		CacheReadPricePerToken:     0.3e-6,  // $0.30 per MTok
		SupportsCacheBreakdown:     false,
	}

	// Claude 3.5 Haiku
	s.fallbackPrices["claude-3-5-haiku"] = &ModelPricing{
		InputPricePerToken:         1e-6,    // $1 per MTok
		OutputPricePerToken:        5e-6,    // $5 per MTok
		CacheCreationPricePerToken: 1.25e-6, // $1.25 per MTok
		CacheReadPricePerToken:     0.1e-6,  // $0.10 per MTok
		SupportsCacheBreakdown:     false,
	}

	// Claude 3 Opus
	s.fallbackPrices["claude-3-opus"] = &ModelPricing{
		InputPricePerToken:         15e-6,    // $15 per MTok
		OutputPricePerToken:        75e-6,    // $75 per MTok
		CacheCreationPricePerToken: 18.75e-6, // $18.75 per MTok
		CacheReadPricePerToken:     1.5e-6,   // $1.50 per MTok
		SupportsCacheBreakdown:     false,
	}

	// Claude 3 Haiku
	s.fallbackPrices["claude-3-haiku"] = &ModelPricing{
		InputPricePerToken:         0.25e-6, // $0.25 per MTok
		OutputPricePerToken:        1.25e-6, // $1.25 per MTok
		CacheCreationPricePerToken: 0.3e-6,  // $0.30 per MTok
		CacheReadPricePerToken:     0.03e-6, // $0.03 per MTok
		SupportsCacheBreakdown:     false,
	}

	// Claude 4.6 Opus (与4.5同价)
	s.fallbackPrices["claude-opus-4.6"] = s.fallbackPrices["claude-opus-4.5"]

	// Claude 4.7 Opus (暂与4.6同价，待官方定价更新)
	s.fallbackPrices["claude-opus-4.7"] = s.fallbackPrices["claude-opus-4.6"]

	// Claude 4.8 Opus / Claude Opus 5（标准 $5/$25，Fast $10/$50 per MTok）。
	// 缺少这两条时 getFallbackPricing 会掉到 claude-3-opus（$15/$75），造成 3 倍超收。
	s.fallbackPrices["claude-opus-4.8"] = pricingWithPriorityMultiplier(s.fallbackPrices["claude-opus-4.7"], 2)
	s.fallbackPrices["claude-opus-5"] = pricingWithPriorityMultiplier(s.fallbackPrices["claude-opus-4.8"], 2)

	// Claude Fable 5.x uses the same input/output and cache-write prices, while
	// Fable 5.1 reduces cache reads from $1 to $0.25 per MTok.
	s.fallbackPrices["claude-fable-5"] = &ModelPricing{
		InputPricePerToken:         10e-6,
		OutputPricePerToken:        50e-6,
		CacheCreationPricePerToken: 12.5e-6,
		CacheCreation5mPrice:       12.5e-6,
		CacheCreation1hPrice:       20e-6,
		CacheReadPricePerToken:     1e-6,
		SupportsCacheBreakdown:     true,
	}
	s.fallbackPrices["claude-fable-5-1"] = &ModelPricing{
		InputPricePerToken:         10e-6,
		OutputPricePerToken:        50e-6,
		CacheCreationPricePerToken: 12.5e-6,
		CacheCreation5mPrice:       12.5e-6,
		CacheCreation1hPrice:       20e-6,
		CacheReadPricePerToken:     0.25e-6,
		SupportsCacheBreakdown:     true,
	}

	// Gemini 3.1 Pro
	s.fallbackPrices["gemini-3.1-pro"] = &ModelPricing{
		InputPricePerToken:         2e-6,   // $2 per MTok
		OutputPricePerToken:        12e-6,  // $12 per MTok
		CacheCreationPricePerToken: 2e-6,   // $2 per MTok
		CacheReadPricePerToken:     0.2e-6, // $0.20 per MTok
		SupportsCacheBreakdown:     false,
	}

	// Gemini 3.6 Flash (Google AI pricing: $1.50 input / $7.50 output /
	// $0.15 cached input per MTok). Antigravity's -high/-low/-medium/-tiered
	// aliases are matched below so unavailable remote pricing never records
	// token-bearing requests at $0.
	s.fallbackPrices["gemini-3.6-flash"] = &ModelPricing{
		InputPricePerToken:     1.5e-6,
		OutputPricePerToken:    7.5e-6,
		CacheReadPricePerToken: 0.15e-6,
		SupportsCacheBreakdown: false,
	}

	// Gemini 3.7 Flash (Google AI pricing: $0.75 input / $3.75 output /
	// $0.075 cached input per MTok, promotional through 2026-12-31; official
	// rates double to $1.50/$7.50/$0.15 from 2027-01-01). Antigravity's
	// -high/-low/-medium/-tiered aliases are matched below so unavailable
	// remote pricing never records token-bearing requests at $0.
	s.fallbackPrices["gemini-3.7-flash"] = &ModelPricing{
		InputPricePerToken:     0.75e-6,
		OutputPricePerToken:    3.75e-6,
		CacheReadPricePerToken: 0.075e-6,
		SupportsCacheBreakdown: false,
	}

	// Gemini 3.8 Flash (Google AI pricing: $0.75 input / $3.75 output /
	// $0.075 cached input per MTok, promotional through 2026-12-31; official
	// rates double to $1.50/$7.50/$0.15 from 2027-01-01). Antigravity's
	// -high/-low/-medium/-tiered aliases are matched below so unavailable
	// remote pricing never records token-bearing requests at $0.
	s.fallbackPrices["gemini-3.8-flash"] = &ModelPricing{
		InputPricePerToken:     0.75e-6,
		OutputPricePerToken:    3.75e-6,
		CacheReadPricePerToken: 0.075e-6,
		SupportsCacheBreakdown: false,
	}

	// OpenAI GPT-5.4（业务指定价格）
	s.fallbackPrices["gpt-5.4"] = &ModelPricing{
		InputPricePerToken:             2.5e-6,  // $2.5 per MTok
		InputPricePerTokenPriority:     5e-6,    // $5 per MTok
		OutputPricePerToken:            15e-6,   // $15 per MTok
		OutputPricePerTokenPriority:    30e-6,   // $30 per MTok
		CacheCreationPricePerToken:     2.5e-6,  // $2.5 per MTok
		CacheReadPricePerToken:         0.25e-6, // $0.25 per MTok
		CacheReadPricePerTokenPriority: 0.5e-6,  // $0.5 per MTok
		SupportsCacheBreakdown:         false,
	}
	// OpenAI GPT-5.5 官方价格；Fast 为标准价 2.5 倍。
	// Source: https://platform.openai.com/docs/pricing
	s.fallbackPrices["gpt-5.5"] = pricingWithPriorityMultiplier(&ModelPricing{
		InputPricePerToken:  5e-6,
		OutputPricePerToken: 30e-6,
		// 官方未列独立 cache-write 价；内部出现 cache creation token 时按输入价兜底。
		CacheCreationPricePerToken: 5e-6,
		CacheReadPricePerToken:     0.5e-6,
		SupportsCacheBreakdown:     false,
	}, 2.5)
	// GPT-5.5 Pro 当前不提供 Fast；保留标准、Flex 和长上下文 fallback 价格。
	s.fallbackPrices["gpt-5.5-pro"] = &ModelPricing{
		InputPricePerToken:  30e-6,
		OutputPricePerToken: 180e-6,
		// 官方未列独立 cached-input/cache-write 价；内部出现对应 token 时按输入价兜底。
		CacheCreationPricePerToken: 30e-6,
		CacheReadPricePerToken:     30e-6,
		SupportsCacheBreakdown:     false,
	}

	s.fallbackPrices["gpt-6-astra"] = &ModelPricing{
		InputPricePerToken:                 10e-6,
		InputPricePerTokenPriority:         20e-6,
		OutputPricePerToken:                50e-6,
		OutputPricePerTokenPriority:        100e-6,
		CacheCreationPricePerToken:         12.5e-6,
		CacheCreationPricePerTokenPriority: 25e-6,
		CacheReadPricePerToken:             1e-6,
		CacheReadPricePerTokenPriority:     2e-6,
		LongContextInputThreshold:          272_000,
		LongContextInputMultiplier:         2,
		LongContextOutputMultiplier:        1.5,
	}

	// OpenAI GPT-5.6 官方价格（USD/token）。缓存写入为输入价的 1.25 倍。
	s.fallbackPrices["gpt-5.6-sol"] = &ModelPricing{
		InputPricePerToken:                 5e-6,
		InputPricePerTokenPriority:         10e-6,
		OutputPricePerToken:                30e-6,
		OutputPricePerTokenPriority:        60e-6,
		CacheCreationPricePerToken:         6.25e-6,
		CacheCreationPricePerTokenPriority: 12.5e-6,
		CacheReadPricePerToken:             0.5e-6,
		CacheReadPricePerTokenPriority:     1e-6,
	}
	s.fallbackPrices["gpt-5.6-terra"] = &ModelPricing{
		InputPricePerToken:                 2e-6,
		InputPricePerTokenPriority:         4e-6,
		OutputPricePerToken:                12e-6,
		OutputPricePerTokenPriority:        24e-6,
		CacheCreationPricePerToken:         2.5e-6,
		CacheCreationPricePerTokenPriority: 5e-6,
		CacheReadPricePerToken:             0.2e-6,
		CacheReadPricePerTokenPriority:     0.4e-6,
	}
	s.fallbackPrices["gpt-5.6-luna"] = &ModelPricing{
		InputPricePerToken:                 0.2e-6,
		InputPricePerTokenPriority:         0.4e-6,
		OutputPricePerToken:                1.2e-6,
		OutputPricePerTokenPriority:        2.4e-6,
		CacheCreationPricePerToken:         0.25e-6,
		CacheCreationPricePerTokenPriority: 0.5e-6,
		CacheReadPricePerToken:             0.02e-6,
		CacheReadPricePerTokenPriority:     0.04e-6,
	}

	s.fallbackPrices["gpt-5.4-mini"] = &ModelPricing{
		InputPricePerToken:     7.5e-7,
		OutputPricePerToken:    4.5e-6,
		CacheReadPricePerToken: 7.5e-8,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["gpt-5.4-nano"] = &ModelPricing{
		InputPricePerToken:     2e-7,
		OutputPricePerToken:    1.25e-6,
		CacheReadPricePerToken: 2e-8,
		SupportsCacheBreakdown: false,
	}
	// OpenAI GPT-5.2（本地兜底）
	s.fallbackPrices["gpt-5.2"] = &ModelPricing{
		InputPricePerToken:             1.75e-6,
		InputPricePerTokenPriority:     3.5e-6,
		OutputPricePerToken:            14e-6,
		OutputPricePerTokenPriority:    28e-6,
		CacheCreationPricePerToken:     1.75e-6,
		CacheReadPricePerToken:         0.175e-6,
		CacheReadPricePerTokenPriority: 0.35e-6,
		SupportsCacheBreakdown:         false,
	}
	// Codex 族兜底统一按 GPT-5.3 Codex 价格计费
	s.fallbackPrices["gpt-5.3-codex"] = &ModelPricing{
		InputPricePerToken:             1.5e-6, // $1.5 per MTok
		InputPricePerTokenPriority:     3e-6,   // $3 per MTok
		OutputPricePerToken:            12e-6,  // $12 per MTok
		OutputPricePerTokenPriority:    24e-6,  // $24 per MTok
		CacheCreationPricePerToken:     1.5e-6, // $1.5 per MTok
		CacheReadPricePerToken:         0.15e-6,
		CacheReadPricePerTokenPriority: 0.3e-6,
		SupportsCacheBreakdown:         false,
	}

	// ============================================================
	// 国产 LLM 兜底定价（数据源：各家官方定价页/USD 口径）
	// 顺序：DeepSeek → 智谱 GLM → 月之暗面 Kimi → MiniMax
	// 覆盖逻辑见同文件 getFallbackPricing()
	// ============================================================

	// ---- DeepSeek 系列 ----
	// Source: https://api-docs.deepseek.com/quick_start/pricing
	// 官方口径（2026-09-10 公告降价后）：现行模型为 deepseek-flash（=
	// DeepSeek-V4.1-Flash，旧名 deepseek-v4-flash 兼容路由）/ deepseek-v4-pro /
	// deepseek-v4-flash-vision-exp；deepseek-chat / deepseek-reasoner 已停止服务，
	// 其余 deepseek-*（含未知型号）统一按 flash 价兜底（见 getFallbackPricing），
	// 避免计费中断。
	// 以下均为官方低谷价；高峰价 = 2× 低谷价（高峰时段 01:00–04:00
	// 与 06:00–10:00 UTC，仅工作日；北京时间周六/周日全天低谷），见 deepseekPeakMultiplierAt。
	s.fallbackPrices["deepseek-v4-pro"] = &ModelPricing{
		InputPricePerToken:     deepseekProOffPeakInputPrice,  // $0.66 per MTok (cache miss, off-peak)
		OutputPricePerToken:    deepseekProOffPeakOutputPrice, // $1.98 per MTok
		CacheReadPricePerToken: deepseekProOffPeakCacheRead,   // $0.022 per MTok (cache hit)
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["deepseek-v4-flash"] = &ModelPricing{
		InputPricePerToken:     deepseekFlashOffPeakInputPrice,  // $0.15 per MTok (cache miss, off-peak)
		OutputPricePerToken:    deepseekFlashOffPeakOutputPrice, // $0.60 per MTok
		CacheReadPricePerToken: deepseekFlashOffPeakCacheRead,   // $0.003 per MTok (cache hit)
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["deepseek-v4-flash-vision-exp"] = &ModelPricing{
		InputPricePerToken:     deepseekFlashOffPeakInputPrice,
		OutputPricePerToken:    deepseekFlashOffPeakOutputPrice,
		CacheReadPricePerToken: deepseekFlashOffPeakCacheRead,
		SupportsCacheBreakdown: false,
	}

	// ---- 智谱 GLM（Z.AI）----
	// Source: https://docs.z.ai/guides/overview/pricing (USD per 1M tokens)
	// 注意：CacheReadPricePerToken 即"缓存命中"价格，CacheCreationPricePerToken 留空（智谱未公开写入价，按 0 处理）。
	// GLM-4.6 与 GLM-4.5 在 z.ai 国际版上定价一致；GLM-4.5 国内按 ¥0.8/¥2，汇率换算后约 $0.112/$0.28，与国际版 $0.6/$2.2 不同，本分支采用国际版 USD 口径与现有 Claude/GPT 一致。
	// GLM-5.3 / GLM-5.2 与 GLM-5.1 在 z.ai 上同价。
	// GLM-5.3-Flash 列表价 $0.15/$0.50（2026-09-09 前五折促销，此处按列表价，与其它模型口径一致）。
	s.fallbackPrices["glm-5.3-flash"] = &ModelPricing{
		InputPricePerToken:     0.15e-6, // $0.15 per MTok
		OutputPricePerToken:    0.5e-6,  // $0.50 per MTok
		CacheReadPricePerToken: 0.03e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-5.3"] = &ModelPricing{
		InputPricePerToken:     1.4e-6, // $1.40 per MTok
		OutputPricePerToken:    4.4e-6, // $4.40 per MTok
		CacheReadPricePerToken: 0.26e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-5.2"] = &ModelPricing{
		InputPricePerToken:     1.4e-6, // $1.40 per MTok
		OutputPricePerToken:    4.4e-6, // $4.40 per MTok
		CacheReadPricePerToken: 0.26e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-5.1"] = &ModelPricing{
		InputPricePerToken:     1.4e-6, // $1.40 per MTok
		OutputPricePerToken:    4.4e-6, // $4.40 per MTok
		CacheReadPricePerToken: 0.26e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-5"] = &ModelPricing{
		InputPricePerToken:     1e-6, // $1.00 per MTok
		OutputPricePerToken:    3.2e-6,
		CacheReadPricePerToken: 0.2e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-5-turbo"] = &ModelPricing{
		InputPricePerToken:     1.2e-6,
		OutputPricePerToken:    4e-6,
		CacheReadPricePerToken: 0.24e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4.7"] = &ModelPricing{
		InputPricePerToken:     0.6e-6, // $0.60 per MTok
		OutputPricePerToken:    2.2e-6,
		CacheReadPricePerToken: 0.11e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4.7-flashx"] = &ModelPricing{
		InputPricePerToken:     0.07e-6, // $0.07 per MTok
		OutputPricePerToken:    0.4e-6,
		CacheReadPricePerToken: 0.01e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4.6"] = &ModelPricing{
		InputPricePerToken:     0.6e-6, // $0.60 per MTok
		OutputPricePerToken:    2.2e-6,
		CacheReadPricePerToken: 0.11e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4.5"] = &ModelPricing{
		InputPricePerToken:     0.6e-6, // $0.60 per MTok
		OutputPricePerToken:    2.2e-6,
		CacheReadPricePerToken: 0.11e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4.5-x"] = &ModelPricing{
		InputPricePerToken:     2.2e-6, // $2.20 per MTok
		OutputPricePerToken:    8.9e-6,
		CacheReadPricePerToken: 0.45e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4.5-air"] = &ModelPricing{
		InputPricePerToken:     0.2e-6, // $0.20 per MTok
		OutputPricePerToken:    1.1e-6,
		CacheReadPricePerToken: 0.03e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4.5-airx"] = &ModelPricing{
		InputPricePerToken:     1.1e-6,
		OutputPricePerToken:    4.5e-6,
		CacheReadPricePerToken: 0.22e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4-32b-0414-128k"] = &ModelPricing{
		InputPricePerToken:     0.1e-6, // $0.10 per MTok
		OutputPricePerToken:    0.1e-6,
		SupportsCacheBreakdown: false,
	}
	// GLM-4.5-Flash / GLM-4.7-Flash 在 z.ai 上为 Free，保留 zero-cost entry 防止未知 alias 误计费。
	s.fallbackPrices["glm-4.5-flash"] = &ModelPricing{
		InputPricePerToken:     0,
		OutputPricePerToken:    0,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4.7-flash"] = &ModelPricing{
		InputPricePerToken:     0,
		OutputPricePerToken:    0,
		SupportsCacheBreakdown: false,
	}

	// ---- 月之暗面 Kimi（K 系列）----
	// Source: https://platform.moonshot.cn/docs/pricing/overview (元/百万 tokens 口径)
	//       交叉验证：https://www.tmtpost.com/7961404.html (USD 口径)
	// Moonshot V1 (¥2/¥5/¥10 多 tier) 公开页未直接标注 USD 价，本分支不覆盖，避免误计价。
	// K2-0905 / K2-0711 官方页面未保留定价，不覆盖。
	// Kimi K3 国际站 USD 价目：https://platform.kimi.ai/docs/pricing/chat-k3.md
	// Kimi Code bare aliases（k3 / k3-256k）官方无按 token 价目；复用 API Platform
	// kimi-k3 档位作代理计费 fallback（同 kimi-for-coding 对 K2.6 的处理口径）。
	s.fallbackPrices["kimi-k3"] = &ModelPricing{
		InputPricePerToken:     3e-6,    // $3.00 per MTok (cache miss)
		OutputPricePerToken:    15e-6,   // $15.00 per MTok
		CacheReadPricePerToken: 0.30e-6, // $0.30 per MTok (cache hit)
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["kimi-k2.6"] = &ModelPricing{
		InputPricePerToken:     0.95e-6, // $0.95 per MTok (cache miss)
		OutputPricePerToken:    4e-6,    // $4.00 per MTok
		CacheReadPricePerToken: 0.15e-6, // $0.15 per MTok (cache hit, ¥1.10)
		SupportsCacheBreakdown: false,
	}
	// kimi-for-coding 走 Kimi Coding endpoint，按当前 K2.6 coding 档位兜底计费。
	s.fallbackPrices["kimi-for-coding"] = &ModelPricing{
		InputPricePerToken:     0.95e-6,
		OutputPricePerToken:    4e-6,
		CacheReadPricePerToken: 0.15e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["kimi-k2.5"] = &ModelPricing{
		InputPricePerToken:     0.60e-6, // $0.60 per MTok
		OutputPricePerToken:    3e-6,    // $3.00 per MTok
		CacheReadPricePerToken: 0.098e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["kimi-k2-thinking"] = &ModelPricing{
		InputPricePerToken:     0.56e-6, // ¥4/百万 ≈ $0.56
		OutputPricePerToken:    2.24e-6, // ¥16/百万
		CacheReadPricePerToken: 0.14e-6, // ¥1/百万
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["kimi-k2"] = &ModelPricing{
		InputPricePerToken:     0.56e-6, // ¥4/百万
		OutputPricePerToken:    2.24e-6, // ¥16/百万
		CacheReadPricePerToken: 0.14e-6, // ¥1/百万
		SupportsCacheBreakdown: false,
	}

	// ---- MiniMax M 系列 ----
	// Source: https://platform.minimax.io/docs/guides/pricing-paygo
	// 注意：MiniMax M3 在 >512K context 时价格翻倍，本兜底采用 ≤512K 标准 tier（保守口径，对用户有利）。
	// 如需支持长上下文 multiplier，可后续参考 GPT-5.4 模式扩展 LongContextXxx 字段。
	s.fallbackPrices["minimax-m3"] = &ModelPricing{
		InputPricePerToken:     0.60e-6, // $0.60 per MTok (≤512K standard tier, 含 50% 永久折扣前原价 $1.20)
		OutputPricePerToken:    2.40e-6,
		CacheReadPricePerToken: 0.12e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["minimax-m2.7"] = &ModelPricing{
		InputPricePerToken:     0.30e-6, // $0.30 per MTok
		OutputPricePerToken:    1.20e-6,
		CacheReadPricePerToken: 0.06e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["minimax-m2.7-highspeed"] = &ModelPricing{
		InputPricePerToken:     0.60e-6,
		OutputPricePerToken:    2.40e-6,
		CacheReadPricePerToken: 0.06e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["minimax-m2.5"] = &ModelPricing{
		InputPricePerToken:     0.30e-6,
		OutputPricePerToken:    1.20e-6,
		CacheReadPricePerToken: 0.03e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["minimax-m2.1"] = &ModelPricing{
		InputPricePerToken:     0.30e-6,
		OutputPricePerToken:    1.20e-6,
		CacheReadPricePerToken: 0.03e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["minimax-m2"] = &ModelPricing{
		InputPricePerToken:     0.30e-6,
		OutputPricePerToken:    1.20e-6,
		CacheReadPricePerToken: 0.03e-6,
		SupportsCacheBreakdown: false,
	}

	// ---- 火山方舟 豆包 Embedding（多模态向量化）----
	// doubao-embedding-vision 图文向量化：上游 usage 回传 prompt_tokens_details.{text_tokens,image_tokens}，
	// 按量付费官方价 文本 ¥0.7/MTok、图片 ¥1.8/MTok；汇率口径 ÷7.14（与本表其他国产模型一致，¥1≈$0.14）。
	// embedding 无 output，OutputPricePerToken 置 0。
	s.fallbackPrices["doubao-embedding-vision"] = &ModelPricing{
		InputPricePerToken:      0.098e-6, // ¥0.7/MTok ≈ $0.098（文本输入）
		ImageInputPricePerToken: 0.252e-6, // ¥1.8/MTok ≈ $0.252（图片输入）
		OutputPricePerToken:     0,
		SupportsCacheBreakdown:  false,
	}

	// ---- 火山方舟 豆包 Seed 对话/编程模型 ----
	// Trae 渠道目录以品牌原始写法下发型号（Doubao-Seed-2.1-Pro、Doubao-Seed-2.0-Code、
	// seed-code-pro-0430），方舟/百炼侧此前无任何兜底卡，导致这些名字在定价链四处
	// （分组卡→渠道卡→目录→兜底卡）全部查无 → 定价闸门判未定价 → /v1/models 与模型
	// 广场整批剔除、入口同时 404（2026-09-29 报障，与 2026-09-28 Qoder 别名同构）。
	// 汇率口径 ÷7.14（¥1≈$0.14），与上方 doubao-embedding-vision 及本表其他国产模型一致；
	// 价目取官方「中国内地」基础档（最低输入阶梯），与 qmodel/q37fmodel 的「基础档价」
	// 口径一致，更长上下文由运营者按需在渠道/分组价卡上加档覆盖。
	s.fallbackPrices["doubao-seed-2.1-pro"] = &ModelPricing{
		InputPricePerToken:     0.84e-6,  // ¥6/MTok ≈ $0.84
		OutputPricePerToken:    4.20e-6,  // ¥30/MTok ≈ $4.20
		CacheReadPricePerToken: 0.168e-6, // ¥1.2/MTok ≈ $0.168（缓存命中）
		SupportsCacheBreakdown: false,
	}
	// Doubao-Seed-2.0-Code（¥3.2/¥16、缓存 ¥0.64，≤32K 档）。同时承接 Trae 的
	// seed-code-pro-0430：该型号无独立公开价目，按豆包编程模型现役档计价，
	// 取有缓存价的 2.0-Code 卡而非更旧的 Seed-Code（¥1.2/¥8、缓存价零一手证据），
	// 偏保守且不因缓存价缺失把命中 token 记成 $0。
	s.fallbackPrices["doubao-seed-2.0-code"] = &ModelPricing{
		InputPricePerToken:     0.448e-6, // ¥3.2/MTok ≈ $0.448
		OutputPricePerToken:    2.24e-6,  // ¥16/MTok ≈ $2.24
		CacheReadPricePerToken: 0.09e-6,  // ¥0.64/MTok ≈ $0.09
		SupportsCacheBreakdown: false,
	}
	// Doubao-Seed-2.1-Turbo：官方发布口径「价格仅为 2.1 Pro 的一半」→ ¥3/¥15，
	// 缓存命中按同比例 ¥0.6。单独建卡是为了让家族默认规则能把它从旗舰档卡里
	// 排除出去（否则未来出现的 turbo 名会被按 Pro 多收一倍）。
	s.fallbackPrices["doubao-seed-2.1-turbo"] = &ModelPricing{
		InputPricePerToken:     0.42e-6,  // ¥3/MTok ≈ $0.42
		OutputPricePerToken:    2.10e-6,  // ¥15/MTok ≈ $2.10
		CacheReadPricePerToken: 0.084e-6, // ¥0.6/MTok ≈ $0.084
		SupportsCacheBreakdown: false,
	}

	// ---- 千问（Trae 目录写法）----
	// qwen-3.7-plus 不在这里单列：它与 Qoder 的 qmodel（Qwen3.7-Plus）是同一模型，
	// 按 2026-09-22「同款模型对齐」口径复用那张卡，避免同一模型出现两份价目。
	// 其余三个写法在本表零命中，且 qwen 系列刻意不做子串兜底（见上方国产 LLM 注释），
	// 所以必须显式建卡 + 显式规则，否则依旧被闸门剪掉。
	s.fallbackPrices["qwen-3.6-plus"] = &ModelPricing{
		InputPricePerToken:     0.28e-6,  // ¥2/MTok ≈ $0.28（≤256K 档）
		OutputPricePerToken:    1.68e-6,  // ¥12/MTok ≈ $1.68
		CacheReadPricePerToken: 0.028e-6, // 显式缓存命中按输入 10% ≈ $0.028
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["qwen-3.5"] = &ModelPricing{
		InputPricePerToken:     0.112e-6, // ¥0.8/MTok ≈ $0.112（qwen3.5-plus ≤128K 档）
		OutputPricePerToken:    0.672e-6, // ¥4.8/MTok ≈ $0.672
		CacheReadPricePerToken: 0.011e-6, // 显式缓存命中 ¥0.08/MTok ≈ $0.011
		SupportsCacheBreakdown: false,
	}
	// Trae 的 qwen3-coder 为裸名（无 -plus/-flash 后缀）。按 coder-plus 档计价：
	// 裸名无法判定子档，取家族内已取价的最高档保守计费，与 auto 借最高档同口径，
	// 确保智能/省略命名的 coder 流量不以 $0 白偷。带档后缀的子档（-flash / -next /
	// -30b-a3b）价格低于 coder-plus，由 isCheaperQwenVariant 排除后维持原有「无价」，
	// 需要精细差异时由渠道/分组价卡覆盖。
	s.fallbackPrices["qwen3-coder"] = &ModelPricing{
		InputPricePerToken:     0.56e-6,  // ¥4/MTok ≈ $0.56（coder-plus ≤32K 档）
		OutputPricePerToken:    2.24e-6,  // ¥16/MTok ≈ $2.24
		CacheReadPricePerToken: 0.112e-6, // 输入缓存命中 ¥0.8/MTok ≈ $0.112
		SupportsCacheBreakdown: false,
	}

	// step-5-preview（阶跃星辰 Step 5 Preview，Trae solo_agent 表可见项）。
	// 官方开放平台定价页未在本次可取到，取 Artificial Analysis 记录的等值
	// （¥7/¥20 测得 $1.00/$2.70）与仓内一致的 ÷7.14 汇率口径换算：¥7/¥20。
	// 缓存命中官方标 ¥0.35。该模型 2026-09-29 实测在 solo_agent 下可调用，
	// 不建卡会被定价闸门剔除出 /v1/models 与模型广场（同 2026-09-29 报障形态）。
	s.fallbackPrices["step-5-preview"] = &ModelPricing{
		InputPricePerToken:     0.98e-6,  // ¥7/MTok ≈ $0.98
		OutputPricePerToken:    2.80e-6,  // ¥20/MTok ≈ $2.80
		CacheReadPricePerToken: 0.049e-6, // ¥0.35/MTok ≈ $0.049
		SupportsCacheBreakdown: false,
	}

	// xAI Grok 4.5: $2 input / $0.30 cached input / $6 output below 200k;
	// long-context rates are $4 / $0.60 / $12 (>=200k prompt tokens).
	s.fallbackPrices["grok-4.5"] = &ModelPricing{
		InputPricePerToken:            2e-6,
		OutputPricePerToken:           6e-6,
		CacheReadPricePerToken:        0.3e-6,
		SupportsCacheBreakdown:        false,
		LongContextInputThreshold:     200000,
		LongContextThresholdInclusive: true,
		LongContextInputMultiplier:    2,
		LongContextOutputMultiplier:   2,
	}

	// xAI Grok 4.6: $2 input / $0.50 cached input / $6 output below 200k;
	// long-context rates are $4 / $1 / $12 (>=200k prompt tokens).
	s.fallbackPrices["grok-4.6"] = &ModelPricing{
		InputPricePerToken:            2e-6,
		OutputPricePerToken:           6e-6,
		CacheReadPricePerToken:        0.5e-6,
		SupportsCacheBreakdown:        false,
		LongContextInputThreshold:     200000,
		LongContextThresholdInclusive: true,
		LongContextInputMultiplier:    2,
		LongContextOutputMultiplier:   2,
	}

	// xAI Grok 4.3: $1.25 input / $0.20 cached / $2.50 output below 200k;
	// long-context rates are $2.50 / $0.40 / $5.
	s.fallbackPrices["grok-4.3"] = &ModelPricing{
		InputPricePerToken:            1.25e-6,
		OutputPricePerToken:           2.5e-6,
		CacheReadPricePerToken:        0.2e-6,
		SupportsCacheBreakdown:        false,
		LongContextInputThreshold:     200000,
		LongContextThresholdInclusive: true,
		LongContextInputMultiplier:    2,
		LongContextOutputMultiplier:   2,
	}
	// Grok 4.20 variants share the official $1.25 / $0.20 / $2.50 card
	// (and $2.50 / $0.40 / $5 long-context rates) with Grok 4.3.
	s.fallbackPrices["grok-4.20"] = &ModelPricing{
		InputPricePerToken:            1.25e-6,
		OutputPricePerToken:           2.5e-6,
		CacheReadPricePerToken:        0.2e-6,
		SupportsCacheBreakdown:        false,
		LongContextInputThreshold:     200000,
		LongContextThresholdInclusive: true,
		LongContextInputMultiplier:    2,
		LongContextOutputMultiplier:   2,
	}

	// Keep legacy Grok 3 Mini requests on their own historical xAI price card;
	// otherwise the generic Grok fallback bills them as Grok 4.5.
	s.fallbackPrices["grok-3-mini"] = &ModelPricing{
		InputPricePerToken:     0.30e-6,
		OutputPricePerToken:    0.50e-6,
		CacheReadPricePerToken: 0.075e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["grok-3-mini-fast"] = &ModelPricing{
		InputPricePerToken:     0.60e-6,
		OutputPricePerToken:    4e-6,
		CacheReadPricePerToken: 0.15e-6,
		SupportsCacheBreakdown: false,
	}
	// xAI Grok Build 0.1 (official docs: $1 input / $0.20 cached input /
	// $2 output per MTok). Composer is available only through Grok Build and
	// has no standalone public API rate card, so its aliases use this coding
	// model rate instead of silently billing at zero.
	s.fallbackPrices["grok-build-0.1"] = &ModelPricing{
		InputPricePerToken:            1e-6,
		OutputPricePerToken:           2e-6,
		CacheReadPricePerToken:        0.2e-6,
		SupportsCacheBreakdown:        false,
		LongContextInputThreshold:     200000,
		LongContextThresholdInclusive: true,
		LongContextInputMultiplier:    2,
		LongContextOutputMultiplier:   2,
	}

	// ---- Qoder 平台模型代号价卡（14 项，与 qoder.go DefaultQoderModelIDs 对齐）----
	// Qoder 上游以代号标识模型：客户端请求什么代号，计费就按什么代号查价。
	// 价卡口径（2026-09-22 运营者拍板「同款模型对齐」）：
	//   - dmodel/dfmodel           → DeepSeek-V4-Pro / DeepSeek-Flash（与 deepseek-* 同卡）
	//   - gmodel/gfmodel/gm51model → GLM-5.3 / GLM-5.3-Flash / GLM-5.2（z.ai 国际价）
	//   - kmodel_latest/kmodel     → Kimi-K3 / K2.6 档（K2.8-Preview 无独立公开价目）
	//   - mmodel                   → MiniMax-M2.7
	//   - qmodel_38max/qfmodel     → Qwen3.8-Max / Qwen3.8-Flash（百炼国际站 USD 价）
	//   - qmodel_latest/qmodel/q37fmodel → Qwen3.7-Max / Plus / Flash（基础档价；
	//     Qwen3.7-Flash 官方阶梯 0.030/0.130 → 0.200/0.800，兜底取基础档）
	//   - auto                     → 平台最高档（Kimi-K3 卡 $3/$15）保守计价，
	//     智能路由不得以 $0 白嫖；该名同时覆盖 WorkBuddy 的同名 auto 入口。
	// 展示名/别名（Qwen3.8-Flash → qfmodel 等）不在表内单列：由 getFallbackPricing
	// 末位的闭集归一（normalizeQoderModelKey）按官方 key 取价，避免两份价卡表。
	s.fallbackPrices["dmodel"] = &ModelPricing{
		InputPricePerToken:     deepseekProOffPeakInputPrice,
		OutputPricePerToken:    deepseekProOffPeakOutputPrice,
		CacheReadPricePerToken: deepseekProOffPeakCacheRead,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["dfmodel"] = &ModelPricing{
		InputPricePerToken:     deepseekFlashOffPeakInputPrice,
		OutputPricePerToken:    deepseekFlashOffPeakOutputPrice,
		CacheReadPricePerToken: deepseekFlashOffPeakCacheRead,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["gmodel"] = &ModelPricing{
		InputPricePerToken:     1.4e-6, // $1.40 per MTok
		OutputPricePerToken:    4.4e-6, // $4.40 per MTok
		CacheReadPricePerToken: 0.26e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["gfmodel"] = &ModelPricing{
		InputPricePerToken:     0.15e-6, // $0.15 per MTok
		OutputPricePerToken:    0.5e-6,  // $0.50 per MTok
		CacheReadPricePerToken: 0.03e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["gm51model"] = &ModelPricing{
		InputPricePerToken:     1.4e-6,
		OutputPricePerToken:    4.4e-6,
		CacheReadPricePerToken: 0.26e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["kmodel_latest"] = &ModelPricing{
		InputPricePerToken:     3e-6,    // $3.00 per MTok (cache miss)
		OutputPricePerToken:    15e-6,   // $15.00 per MTok
		CacheReadPricePerToken: 0.30e-6, // $0.30 per MTok (cache hit)
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["kmodel"] = &ModelPricing{
		InputPricePerToken:     0.95e-6, // $0.95 per MTok (cache miss，K2.6 档)
		OutputPricePerToken:    4e-6,    // $4.00 per MTok
		CacheReadPricePerToken: 0.15e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["mmodel"] = &ModelPricing{
		InputPricePerToken:     0.30e-6, // $0.30 per MTok
		OutputPricePerToken:    1.20e-6,
		CacheReadPricePerToken: 0.06e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["qmodel_38max"] = &ModelPricing{
		InputPricePerToken:     2e-6, // $2.00 per MTok（百炼国际站）
		OutputPricePerToken:    6e-6, // $6.00 per MTok
		CacheReadPricePerToken: 0.2e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["qfmodel"] = &ModelPricing{
		InputPricePerToken:     0.15e-6, // $0.15 per MTok
		OutputPricePerToken:    0.47e-6, // $0.47 per MTok
		CacheReadPricePerToken: 0.015e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["qmodel_latest"] = &ModelPricing{
		InputPricePerToken:     2.5e-6, // $2.50 per MTok
		OutputPricePerToken:    7.5e-6, // $7.50 per MTok
		CacheReadPricePerToken: 0.25e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["qmodel"] = &ModelPricing{
		InputPricePerToken:     0.4e-6, // $0.40 per MTok（≤256K 基础档）
		OutputPricePerToken:    1.6e-6, // $1.60 per MTok
		CacheReadPricePerToken: 0.04e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["q37fmodel"] = &ModelPricing{
		InputPricePerToken:     0.03e-6, // $0.030 per MTok（≤32K 基础档）
		OutputPricePerToken:    0.13e-6, // $0.130 per MTok
		CacheReadPricePerToken: 0.003e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["auto"] = &ModelPricing{
		InputPricePerToken:     3e-6,    // $3.00 per MTok（平台最高档=Kimi-K3 卡）
		OutputPricePerToken:    15e-6,   // $15.00 per MTok
		CacheReadPricePerToken: 0.30e-6, // $0.30 per MTok
		SupportsCacheBreakdown: false,
	}
}

// getFallbackPricing 根据模型系列获取回退价格
func (s *BillingService) getFallbackPricing(model string) *ModelPricing {
	// TrimSpace 与定价闸门（model_pricing_gate.go）保持同一口径：闸门 trim 后放行、
	// 本函数不 trim 会让带首尾空白的名字在计费链上查无价（fail-closed），而
	// trae_models.go 已证实上游确实会下发带空白的 config_name。
	modelLower := strings.ToLower(strings.TrimSpace(model))

	// Qoder 平台模型代号（精确匹配，不参与子串规则）：代号集合封闭且与
	// qoder.go DefaultQoderModelIDs 对齐，请求什么代号就查什么代号。
	// 必须在其它子串规则之前判断：代号无品牌前缀（如 auto/dmodel），
	// 落到下方任何 Contains 链都有误命中风险。
	// 展示名/别名（qwen3.8-flash 等）的闭集归一放在链末尾（见函数尾部），
	// 避免抢在其它家族规则之前改变既有匹配顺序。
	if pricing, ok := s.fallbackPrices[modelLower]; ok && isQoderModelCode(modelLower) {
		return pricing
	}

	// 按模型系列匹配
	if isClaudeFable51Model(modelLower) {
		return s.fallbackPrices["claude-fable-5-1"]
	}
	if strings.Contains(modelLower, "fable-5") || strings.Contains(modelLower, "fable5") {
		return s.fallbackPrices["claude-fable-5"]
	}
	if strings.Contains(modelLower, "opus") {
		// "opus-5" 必须先判：不能用裸 "5" 匹配，否则 claude-opus-4-5 会被误判。
		if strings.Contains(modelLower, "opus-5") || strings.Contains(modelLower, "opus5") {
			return s.fallbackPrices["claude-opus-5"]
		}
		if strings.Contains(modelLower, "4.8") || strings.Contains(modelLower, "4-8") {
			return s.fallbackPrices["claude-opus-4.8"]
		}
		if strings.Contains(modelLower, "4.7") || strings.Contains(modelLower, "4-7") {
			return s.fallbackPrices["claude-opus-4.7"]
		}
		if strings.Contains(modelLower, "4.6") || strings.Contains(modelLower, "4-6") {
			return s.fallbackPrices["claude-opus-4.6"]
		}
		if strings.Contains(modelLower, "4.5") || strings.Contains(modelLower, "4-5") {
			return s.fallbackPrices["claude-opus-4.5"]
		}
		return s.fallbackPrices["claude-3-opus"]
	}
	if strings.Contains(modelLower, "sonnet") {
		if strings.Contains(modelLower, "4") && !strings.Contains(modelLower, "3") {
			return s.fallbackPrices["claude-sonnet-4"]
		}
		return s.fallbackPrices["claude-3-5-sonnet"]
	}
	if strings.Contains(modelLower, "haiku") {
		if strings.Contains(modelLower, "3-5") || strings.Contains(modelLower, "3.5") {
			return s.fallbackPrices["claude-3-5-haiku"]
		}
		return s.fallbackPrices["claude-3-haiku"]
	}
	// Claude 未知型号统一回退到 Sonnet，避免计费中断。
	if strings.Contains(modelLower, "claude") {
		return s.fallbackPrices["claude-sonnet-4"]
	}
	if strings.Contains(modelLower, "gemini-3.1-pro") || strings.Contains(modelLower, "gemini-3-1-pro") {
		return s.fallbackPrices["gemini-3.1-pro"]
	}
	if strings.Contains(modelLower, "gemini-3.6-flash") || strings.Contains(modelLower, "gemini-3-6-flash") {
		return s.fallbackPrices["gemini-3.6-flash"]
	}
	if strings.Contains(modelLower, "gemini-3.7-flash") || strings.Contains(modelLower, "gemini-3-7-flash") {
		return s.fallbackPrices["gemini-3.7-flash"]
	}
	if strings.Contains(modelLower, "gemini-3.8-flash") || strings.Contains(modelLower, "gemini-3-8-flash") {
		return s.fallbackPrices["gemini-3.8-flash"]
	}

	// DeepSeek 系列：官方模型 V4 Pro/Flash（含 vision-exp）按各自价卡；
	// 其余 deepseek-*（含已停服的 deepseek-chat / deepseek-reasoner 与未知型号）
	// 统一按 flash 价兜底，避免计费中断。新名字由 fallback warn 日志
	// （每模型每进程一条）暴露，运营者据此更新价卡。
	// "deepseek-v4-flash-vision-exp" 含 "deepseek-v4-flash" 子串，显式分支置于 flash 之前，语义清晰。
	if strings.Contains(modelLower, "deepseek-v4-flash-vision-exp") {
		return s.fallbackPrices["deepseek-v4-flash-vision-exp"]
	}
	if strings.Contains(modelLower, "deepseek-v4-flash") {
		return s.fallbackPrices["deepseek-v4-flash"]
	}
	if strings.Contains(modelLower, "deepseek-v4-pro") {
		return s.fallbackPrices["deepseek-v4-pro"]
	}
	if strings.HasPrefix(modelLower, "deepseek-") {
		return s.fallbackPrices["deepseek-v4-flash"]
	}

	// ---- 国产 LLM 兜底匹配 ----
	// 匹配策略：长 key 优先（具体模型 → 系列 / 厂商），未知型号不回退以避免误计价。
	// 与 DeepSeek 一样采用"白名单"语义：未在本表命中的国产模型 alias 一律不返回兜底价。

	// 智谱 GLM（z.ai 公开 SKU：glm-5.3 / glm-5.3-flash / glm-5.2 / glm-5.1 / glm-5 / glm-5-turbo / glm-4.7 / glm-4.6 / glm-4.5 等）
	// 匹配顺序：先判别最高 tier，再依次降级。
	// 注意：带小数点的型号必须排在裸 "glm-5" 之前，否则会被 strings.Contains 抢走；
	// glm-5.3-flash 必须排在 glm-5.3 之前（前者包含后者子串）。
	if strings.Contains(modelLower, "glm-5.3-flash") || strings.Contains(modelLower, "glm-5.3flash") {
		return s.fallbackPrices["glm-5.3-flash"]
	}
	if strings.Contains(modelLower, "glm-5.3") {
		return s.fallbackPrices["glm-5.3"]
	}
	if strings.Contains(modelLower, "glm-5.2") {
		return s.fallbackPrices["glm-5.2"]
	}
	if strings.Contains(modelLower, "glm-5.1") {
		return s.fallbackPrices["glm-5.1"]
	}
	if strings.Contains(modelLower, "glm-5-turbo") || strings.Contains(modelLower, "glm-5turbo") {
		return s.fallbackPrices["glm-5-turbo"]
	}
	if strings.Contains(modelLower, "glm-5") {
		return s.fallbackPrices["glm-5"]
	}
	if strings.Contains(modelLower, "glm-4.7-flashx") {
		return s.fallbackPrices["glm-4.7-flashx"]
	}
	if strings.Contains(modelLower, "glm-4.7-flash") {
		return s.fallbackPrices["glm-4.7-flash"]
	}
	if strings.Contains(modelLower, "glm-4.7") {
		return s.fallbackPrices["glm-4.7"]
	}
	if strings.Contains(modelLower, "glm-4.6") {
		return s.fallbackPrices["glm-4.6"]
	}
	if strings.Contains(modelLower, "glm-4.5-flash") {
		return s.fallbackPrices["glm-4.5-flash"]
	}
	if strings.Contains(modelLower, "glm-4.5-x") || strings.Contains(modelLower, "glm-4.5x") {
		return s.fallbackPrices["glm-4.5-x"]
	}
	if strings.Contains(modelLower, "glm-4.5-airx") || strings.Contains(modelLower, "glm-4.5airx") {
		return s.fallbackPrices["glm-4.5-airx"]
	}
	if strings.Contains(modelLower, "glm-4.5-air") || strings.Contains(modelLower, "glm-4.5air") {
		return s.fallbackPrices["glm-4.5-air"]
	}
	if strings.Contains(modelLower, "glm-4.5") {
		return s.fallbackPrices["glm-4.5"]
	}
	if strings.Contains(modelLower, "glm-4-32b") {
		return s.fallbackPrices["glm-4-32b-0414-128k"]
	}

	// 月之暗面 Kimi（kimi-k3 / k3 / k3-256k / kimi-k2.6 / kimi-for-coding / kimi-k2.5 / kimi-k2-thinking / kimi-k2）
	// K2-0905 / K2-0711 官方未保留定价，不进入 fallback。
	// K3 规则置于 K2 前：API Platform 仅官方 kimi-k3（及 / 路径后缀）；
	// Code bare aliases 仅精确 k3 / k3-256k 或 /k3|/k3-256k 后缀，避免 kimi-k30 等未知型号误命中。
	// 注意：kimi-k3[1m] 是 Claude Code 上下文选择语法，不是 Kimi API 模型 ID，不进入 fallback。
	if strings.Contains(modelLower, "kimi-for-coding") {
		return s.fallbackPrices["kimi-for-coding"]
	}
	if modelLower == "kimi-k3" || strings.HasSuffix(modelLower, "/kimi-k3") ||
		modelLower == "k3" || modelLower == "k3-256k" ||
		strings.HasSuffix(modelLower, "/k3") || strings.HasSuffix(modelLower, "/k3-256k") {
		return s.fallbackPrices["kimi-k3"]
	}
	if strings.Contains(modelLower, "kimi-k2.6") || strings.Contains(modelLower, "kimi-k2-6") {
		return s.fallbackPrices["kimi-k2.6"]
	}
	if strings.Contains(modelLower, "kimi-k2.5") || strings.Contains(modelLower, "kimi-k2-5") {
		return s.fallbackPrices["kimi-k2.5"]
	}
	if strings.Contains(modelLower, "kimi-k2-thinking") || strings.Contains(modelLower, "kimi-k2-thinking-") {
		return s.fallbackPrices["kimi-k2-thinking"]
	}
	if strings.Contains(modelLower, "kimi-k2") || strings.Contains(modelLower, "kimi/k2") {
		return s.fallbackPrices["kimi-k2"]
	}

	// MiniMax M 系列（M3 / M2.7 / M2.5 / M2.1 / M2；含 highspeed 变体）
	if strings.Contains(modelLower, "minimax-m3") {
		return s.fallbackPrices["minimax-m3"]
	}
	if strings.Contains(modelLower, "minimax-m2.7-highspeed") || strings.Contains(modelLower, "minimax-m2-7-highspeed") {
		return s.fallbackPrices["minimax-m2.7-highspeed"]
	}
	if strings.Contains(modelLower, "minimax-m2.7") || strings.Contains(modelLower, "minimax-m2-7") {
		return s.fallbackPrices["minimax-m2.7"]
	}
	if strings.Contains(modelLower, "minimax-m2.5") || strings.Contains(modelLower, "minimax-m2-5") {
		return s.fallbackPrices["minimax-m2.5"]
	}
	if strings.Contains(modelLower, "minimax-m2.1") || strings.Contains(modelLower, "minimax-m2-1") {
		return s.fallbackPrices["minimax-m2.1"]
	}
	if strings.Contains(modelLower, "minimax-m2") || strings.Contains(modelLower, "minimax-m-2") {
		return s.fallbackPrices["minimax-m2"]
	}

	// 阶跃星辰 Step（Trae solo_agent 目录可见项 step-5-preview）。
	// 只认完整型号名而不收 step- 宽前缀：step 在很多无关名字里作子串出现（如
	// 第三方包装名），且 step-1/step-3 等旧代价格低一个量级，宽匹配会静默多收。
	if strings.Contains(modelLower, "step-5-preview") {
		return s.fallbackPrices["step-5-preview"]
	}

	// 火山方舟 豆包 Embedding（多模态向量化）。
	// most-specific-first：放在未来任何 doubao-embedding / doubao 宽匹配之前。
	// 覆盖带版本后缀的别名（如 doubao-embedding-vision-251215）。
	if strings.Contains(modelLower, "doubao-embedding-vision") {
		return s.fallbackPrices["doubao-embedding-vision"]
	}

	// 火山方舟 豆包 Seed 对话/编程模型（Trae 渠道目录写法）。
	// 分派与全部护栏集中在 doubaoSeedFamilyFallback 内部：上一版把护栏挂在个别
	// `||` 分支上，短路让 seed-code / seed-2.0-code / seed-2.1-pro / seed-2.1-turbo
	// 四条完全不过护栏（doubao-seed-2.0-code2video 按 token、
	// doubao-seed-2.1-pro-flash 按旗舰档……都是多收下游客户的钱）。
	if pricing, decided := s.doubaoSeedFamilyFallback(modelLower); decided {
		return pricing
	}

	// 千问（Trae 目录写法 qwen-3.x-plus / qwen-3.5 / qwen3-coder）。
	// 百炼官方 Model ID 是 qwen3.7-plus（qwen 与数字之间无连字符），而 Trae 下发的是
	// qwen-3.7-plus，两者差一个连字符；Qoder 的闭集别名表只收官方拼法，所以这里必须
	// 把 Trae 写法接上：qwen-3.7-plus 复用 qmodel（同款模型对齐），其余三个走新建卡。
	// 护栏，缺一不可：
	//  1. 不收更宽的 qwen 前缀族：qwen-plus / qwen-max / qwen-turbo 等存量写法此前
	//     故意处于无价状态，本次报障只涉及 Trae 目录内实际存在的型号。
	//  2. qwen-3.7-plus / qwen-3.6-plus 两条已要求字面带 -plus，更便宜的 flash 变体
	//     （qwen3.5-flash、qwen3.6-flash）天然不会命中。
	//  3. qwen-3.5 与 qwen3-coder 是裸名/短名，必须显式排除更便宜的子档（见
	//     isCheaperQwenVariant），否则 coder-flash/next/30b 会被按 coder-plus 多收。
	// 两种拼法都接：带连字符的是 Trae 目录写法（上轮闸门 404 的直接受害项），
	// 不带的是百炼官方 Model ID（同时也会被函数末位的 Qoder 闭集归一接住）。
	// 保留双入口是有意的冗余（计费闸门宁可多一层命中，不可漏），但两条都必须
	// 过 isCheaperQwenVariant：否则 qwen3.7-plus-flash 这类拼接写法会被按 plus 档多收。
	if (strings.Contains(modelLower, "qwen-3.7-plus") || strings.Contains(modelLower, "qwen3.7-plus")) &&
		!isCheaperQwenVariant(modelLower) {
		return s.fallbackPrices["qmodel"]
	}
	if (strings.Contains(modelLower, "qwen-3.6-plus") || strings.Contains(modelLower, "qwen3.6-plus")) &&
		!isCheaperQwenVariant(modelLower) {
		return s.fallbackPrices["qwen-3.6-plus"]
	}
	// qwen-3.5 是裸名（Trae 目录写法，无档位后缀），必须额外排除 coder/max：
	// 百炼的 qwen3.5-coder / qwen3.5-max 价格与 qwen3.5-plus 不同档（coder 档
	// ¥4/¥16 是这里的 5 倍），若被裸名规则抢走会按 1/5 的价格少收；把它们留作
	// 「无价」维持报障前的原状，需要上架时由渠道/分组价卡精确覆盖。
	if (strings.Contains(modelLower, "qwen-3.5") || strings.Contains(modelLower, "qwen3.5")) &&
		!isCheaperQwenVariant(modelLower) &&
		!strings.Contains(modelLower, "coder") && !strings.Contains(modelLower, "max") &&
		!hasQwenOpenWeightSpec(modelLower) {
		return s.fallbackPrices["qwen-3.5"]
	}
	if (strings.Contains(modelLower, "qwen3-coder") || strings.Contains(modelLower, "qwen-3-coder")) &&
		!isCheaperQwenVariant(modelLower) && !hasQwenOpenWeightSpec(modelLower) {
		return s.fallbackPrices["qwen3-coder"]
	}

	// OpenAI（GPT-5 / Codex 族）：仅匹配已知型号，避免未知 OpenAI 型号误计价。
	if normalized := normalizeKnownOpenAICodexModel(modelLower); normalized != "" {
		switch normalized {
		case "gpt-6-astra":
			return s.fallbackPrices["gpt-6-astra"]
		case "gpt-5.6-sol":
			return s.fallbackPrices["gpt-5.6-sol"]
		case "gpt-5.6-terra":
			return s.fallbackPrices["gpt-5.6-terra"]
		case "gpt-5.6-luna":
			return s.fallbackPrices["gpt-5.6-luna"]
		case "gpt-5.5-pro":
			return s.fallbackPrices["gpt-5.5-pro"]
		case "gpt-5.5":
			return s.fallbackPrices["gpt-5.5"]
		case "gpt-5.4-mini":
			return s.fallbackPrices["gpt-5.4-mini"]
		case "gpt-5.4-nano":
			return s.fallbackPrices["gpt-5.4-nano"]
		case "gpt-5.4":
			return s.fallbackPrices["gpt-5.4"]
		case "gpt-5.2":
			return s.fallbackPrices["gpt-5.2"]
		case "gpt-5.3-codex", "gpt-5.3-codex-spark":
			return s.fallbackPrices["gpt-5.3-codex"]
		}
	}

	switch modelLower {
	case "grok", "grok-latest", "grok-4.6", "grok-4.6-latest":
		return s.fallbackPrices["grok-4.6"]
	case "grok-4.5", "grok-4.5-latest":
		return s.fallbackPrices["grok-4.5"]
	case "grok-3-mini":
		return s.fallbackPrices["grok-3-mini"]
	case "grok-3-mini-fast":
		return s.fallbackPrices["grok-3-mini-fast"]
	case "grok-4.3":
		return s.fallbackPrices["grok-4.3"]
	case "grok-4.20-0309-reasoning",
		"grok-4.20-0309-non-reasoning",
		"grok-4.20-multi-agent-0309",
		"grok-4.20-reasoning",
		"grok-4.20-non-reasoning":
		return s.fallbackPrices["grok-4.20"]
	case "grok-build", "grok-build-latest", "grok-build-0.1", "grok-composer", "grok-composer-2.5-fast", "composer-2.5":
		return s.fallbackPrices["grok-build-0.1"]
	}

	// Unknown Grok text IDs (grok-5, dated snapshots, provider-prefixed) inherit
	// the current default text card so a new model cannot ship unbilled.
	if pricing := s.grokUnknownTextFamilyFallback(modelLower); pricing != nil {
		return pricing
	}

	// 末位兜底：Qoder 展示名/别名（qwen3.8-flash、Qwen3.8-Max 等）闭集归一后
	// 按官方 key 取价卡。
	//
	// 这些展示名不在上方任何子串规则里（qwen 系列刻意不做子串兜底，防误计价），
	// 而转发/计费出站链早已把它们归一为官方 key（normalizeQoderModelKey）：
	// 定价链若不归一，模型广场、/v1/models 与管理端候选会因「查无价」把 qwen
	// 系列整批剔除（准入中间件同时 404），与真实计费口径倒挂（2026-09-28 报障）。
	// 只认闭集命中的官方 key，未知值不猜测，维持原有 nil 语义。
	if normalized := normalizeQoderModelKey(modelLower); normalized != modelLower {
		if pricing, ok := s.fallbackPrices[normalized]; ok && isQoderModelCode(normalized) {
			return pricing
		}
	}

	return nil
}

// doubaoSeedFamilyFallback 分派火山方舟豆包 Seed 对话/编程型号的兜底价卡。
// decided=true 表示该名字属于豆包 Seed 命名线、已由本函数定论（定论可以是
// 「无价」）；decided=false 表示与豆包家族无关，交回调用方继续匹配。
//
// 全部护栏在此一次性排好，不再挂在个别子串分支上：//
//  1. 家族判据只认 doubao-seed* / seed-2* / seed-code* 三种写法，不收宽泛的
//     doubao- 前缀（存量 doubao-pro / doubao-1.5-* 价格低一个量级，必须维持
//     原有匹配语义，不得进入本函数被判家族默认）。
//  2. 媒体族（seedance/seedream/embedding/image/video/audio/speech/asr/tts/vision）
//     一律无价——它们按张/按秒计费，命中任何 token 价卡都是错收。这一条必须先于
//     下面所有型号判定，否则 seed-code-asr / seed-2.0-code2video / 2.1-turbo-asr
//     会被子串规则短路抢走。
//  3. turbo 有独立卡，必须排在「更便宜子档」护栏之前（turbo 本身就是低档名）。
//  4. 未知代际（doubao-seed-2.0 / 2.0-pro / 2.5-*）一律无价，与家族默认的代际
//     判据严格对称；否则未来代会被上一代价格静默少收。新型号由
//     TestTraeFallbackPricingCoversCatalog 在 CI 里拓出来，而不是在生产里默默错价。
func (s *BillingService) doubaoSeedFamilyFallback(model string) (*ModelPricing, bool) {
	if !isDoubaoSeedFamilyModel(model) {
		return nil, false
	}
	if isDoubaoMediaFamilyModel(model) {
		return nil, true
	}
	if strings.Contains(model, "seed-2.1-turbo") || strings.Contains(model, "seed2.1turbo") {
		return s.fallbackPrices["doubao-seed-2.1-turbo"], true
	}
	// 编程档：seed-code 子串覆盖历史名 seed-code-pro-0430；当前代的 -code 后缀
	// （如 Doubao-Seed-2.1-Code）不接就会落到旗舰默认，¥6/¥30 vs ¥3.2/¥16 多收一倍。
	if strings.Contains(model, "seed-2.0-code") || strings.Contains(model, "seed-code") ||
		(strings.HasPrefix(model, "doubao-seed-2.1") && strings.Contains(model, "code")) {
		if isCheaperDoubaoVariant(model) {
			return nil, true
		}
		return s.fallbackPrices["doubao-seed-2.0-code"], true
	}
	// 更便宜子档（flash/lite/mini/thinking/上一代 1.5、1.6）一律无价，不得按旗舰档多收。
	if isCheaperDoubaoVariant(model) {
		return nil, true
	}
	// Seed-Evolving：官方定价页（ai.volcengine.com/model）标输入 ¥6/百万、输出 ¥30/百万，
	// 与 2.1 Pro 完全同档；Trae 侧它的 before_consumption_rate 也是 0.8，与 2.1 Pro 一致
	// （两路证据互证）。名字里不含 2.1/pro/code 任何一个子串，不显式接住就会落到本函数
	// 末尾的「家族接管但判无价」出口，于是被定价闸门整批剔除（2026-09-30 把它加入
	// DefaultTraeModelIDs 后由 TestTraeFallbackPricingCoversCatalog 拓出）。
	// 位置在便宜子档护栏之后，所以 seed-evolving-flash 这类未来名仍先被判无价。
	if strings.Contains(model, "seed-evolving") || strings.Contains(model, "seedevolving") {
		return s.fallbackPrices["doubao-seed-2.1-pro"], true
	}
	if strings.Contains(model, "seed-2.1-pro") || strings.Contains(model, "seed2.1pro") ||
		strings.HasPrefix(model, "doubao-seed-2.1") {
		return s.fallbackPrices["doubao-seed-2.1-pro"], true
	}
	return nil, true
}

// isDoubaoSeedFamilyModel 是 doubaoSeedFamilyFallback 的入口判据（输入需已小写）。
// 故意不收宽泛的 doubao- 前缀；也包含 doubao-seedance/seedream 等媒体名，它们
// 会在下一步被媒体护栏拦下（宁可接管后判无价，也不让它们漏到后面的宽匹配里）。
func isDoubaoSeedFamilyModel(model string) bool {
	return strings.HasPrefix(model, "doubao-seed") ||
		strings.HasPrefix(model, "seed-2") ||
		strings.Contains(model, "seed-code")
}

// hasQwenOpenWeightSpec 报告 qwen 名字带了开源权重/部署规模后缀（如
// …-480b-a35b-instruct、qwen3-14b-base、-vl、-ocr、-omni、-preview）。百炼商用
// API 的 plus/coder 档与开源权重档不同价（开源侧普遍更便宜或按时长计费），
// 裸名规则（qwen-3.5 / qwen3-coder）不能把它们混为同一价卡。规模判据取
// 「数字紧跟字母 b」这一形式而非穷举常量，因此 5b/0.6b/a35b/235b 都接得住，
// 新规模名不会静默混档。
func hasQwenOpenWeightSpec(model string) bool {
	for _, marker := range []string{"instruct", "base", "-vl", "vl-", "ocr", "omni", "preview"} {
		if strings.Contains(model, marker) {
			return true
		}
	}
	for i := 1; i < len(model); i++ {
		if model[i] == 'b' && model[i-1] >= '0' && model[i-1] <= '9' {
			return true
		}
	}
	return false
}

// isDoubaoMediaFamilyModel matches Volcengine Ark ids billed per image/video/
// audio/vector unit rather than per token, so a new doubao-* chat name cannot
// slip into the Seed family default card while a media model picks up token
// pricing. Mirrors isGrokMediaFamilyModel (same guard, same rationale).
func isDoubaoMediaFamilyModel(model string) bool {
	for _, marker := range []string{
		"seedance", "seedream", "embedding", "image", "video", "audio",
		"speech", "asr", "tts", "vision",
	} {
		if strings.Contains(model, marker) {
			return true
		}
	}
	return false
}

// isCheaperQwenVariant reports that a qwen id names a tier that is NOT the
// plus-tier card above (flash/turbo/next/minus/lite/mini/free/30b/embedding).
// Those names sit at a different price point, so they must stay unbilled
// (unchanged behaviour) rather than silently inherit the plus-tier card and
// mis-bill downstream customers. Note: "coder"/"max" are deliberately NOT in
// this list — the qwen3-coder card itself would self-exclude; they are
// excluded on the bare-name rule explicitly instead.
func isCheaperQwenVariant(model string) bool {
	for _, marker := range []string{"flash", "turbo", "next", "minus", "30b", "embedding", "lite", "mini", "free", "thinking"} {
		if strings.Contains(model, marker) {
			return true
		}
	}
	return false
}

// isCheaperDoubaoVariant reports that a doubao id names an explicitly cheaper
// tier than the Seed 2.1 Pro flagship card, so the family default must not
// inherit the flagship rate for it (over-billing downstream customers).
func isCheaperDoubaoVariant(model string) bool {
	for _, marker := range []string{"flash", "lite", "mini", "thinking", "turbo", "1.5", "1.6"} {
		if strings.Contains(model, marker) {
			return true
		}
	}
	return false
}

func (s *BillingService) grokUnknownTextFamilyFallback(model string) *ModelPricing {
	if s == nil || !isGrokUnknownTextFamilyModel(model) {
		return nil
	}
	return s.fallbackPrices["grok-4.6"]
}

func isGrokUnknownTextFamilyModel(model string) bool {
	native := strings.ToLower(strings.TrimSpace(xai.StripGrokProviderPrefix(model)))
	if isGrokMediaFamilyModel(native) {
		return false
	}
	switch {
	case native == "grok", native == "grok-latest":
		return true
	case strings.HasPrefix(native, "grok-build"),
		strings.HasPrefix(native, "grok-composer"),
		strings.HasPrefix(native, "composer-"):
		return true
	case len(native) > 5 && strings.HasPrefix(native, "grok-"):
		rest := native[len("grok-"):]
		return rest[0] >= '0' && rest[0] <= '9'
	default:
		return false
	}
}

// isGrokMediaFamilyModel matches ids that are billed per image/video/audio unit
// rather than per token, so version-numbered media ids (grok-2-image-1212,
// grok-5-video) cannot slip into the unknown-text fallback and pick up a token
// card. "vision" is deliberately absent: multimodal chat models are token billed.
func isGrokMediaFamilyModel(native string) bool {
	for _, marker := range []string{"imagine", "image", "video", "audio", "speech", "tts", "transcribe", "realtime"} {
		if strings.Contains(native, marker) {
			return true
		}
	}
	return false
}

// HasIdentifiedTokenPricing 判断模型能否在价格表中被"确定性识别"出 token 价格。
//
// 与 GetModelPricing 的关键区别：本函数拒绝按子串猜系列的兜底。GetModelPricing 会
// 让任意含 "haiku"/"opus"/"claude" 的名字（哪怕是不存在的型号）落到 getFallbackPricing
// 的系列兜底价上，因此凡是模型名来自外部、且"能查到价"会直接影响计费金额的场景
// （如按上游响应自报模型计费），都必须用本函数而不是 GetModelPricing 做准入判断。
func (s *BillingService) HasIdentifiedTokenPricing(model string) bool {
	if s == nil {
		return false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	if s.pricingService != nil {
		// 仅有图片价的条目不能用于 token 计费，口径与 GetModelPricing 保持一致。
		if pricing := s.pricingService.GetIdentifiedModelPricing(model); pricing != nil && !pricing.TokenPricingAbsent {
			return true
		}
	}
	pricing, ok := s.fallbackPrices[model]
	return ok && pricing != nil
}

// GetModelPricing 获取模型价格配置
func (s *BillingService) GetModelPricing(model string) (*ModelPricing, error) {
	// 无显式计费时点，DeepSeek pro→Flash 切换按当前时刻判定。
	return s.getModelPricingAt(model, timezone.Now())
}

// getModelPricingAt 是 GetModelPricing 的带计费时点内部变体：pricingAt 显式
// 驱动 DeepSeek pro→Flash 切换判定（切换点前 Pro 价、之后 Flash 价），使
// 展示/估算路径可与历史补账同刻复算，测试也能用固定时点钉住断言。
func (s *BillingService) getModelPricingAt(model string, pricingAt time.Time) (*ModelPricing, error) {
	// 标准化模型名称（转小写）
	model = strings.ToLower(model)

	// 1. 优先从动态价格服务获取
	if s.pricingService != nil {
		litellmPricing := s.pricingService.GetModelPricing(model)
		// 仅有图片价、无 token 价的条目（如 LiteLLM 的 imagen 类模型）不能用于
		// token 计费：直接返回会把 token 流量按 $0 计费。跳过后走 fallback，
		// 无 fallback 则 fail-closed（ErrModelPricingUnavailable）。
		// 图片计费路径（getDefaultImagePrice / getImageUnitPrice）直接读
		// PricingService，不受影响。
		if litellmPricing != nil && litellmPricing.TokenPricingAbsent {
			litellmPricing = nil
		}
		if litellmPricing != nil {
			// 启用 5m/1h 分类计费的条件：
			// 1. 存在 1h 价格
			// 2. 1h 价格 > 5m 价格（防止 LiteLLM 数据错误导致少收费）
			price5m := litellmPricing.CacheCreationInputTokenCost
			price1h := litellmPricing.CacheCreationInputTokenCostAbove1hr
			enableBreakdown := price1h > 0 && price1h > price5m
			return s.applyModelSpecificPricingPolicyEx(model, &ModelPricing{
				InputPricePerToken:                 litellmPricing.InputCostPerToken,
				InputPricePerTokenPriority:         litellmPricing.InputCostPerTokenPriority,
				OutputPricePerToken:                litellmPricing.OutputCostPerToken,
				OutputPricePerTokenPriority:        litellmPricing.OutputCostPerTokenPriority,
				CacheCreationPricePerToken:         litellmPricing.CacheCreationInputTokenCost,
				CacheCreationPricePerTokenPriority: litellmPricing.CacheCreationInputTokenCostPriority,
				CacheReadPricePerToken:             litellmPricing.CacheReadInputTokenCost,
				CacheReadPricePerTokenPriority:     litellmPricing.CacheReadInputTokenCostPriority,
				CacheCreation5mPrice:               price5m,
				CacheCreation1hPrice:               price1h,
				SupportsCacheBreakdown:             enableBreakdown,
				LongContextInputThreshold:          litellmPricing.LongContextInputTokenThreshold,
				// xAI 的长上下文阈值语义为"达到即进高档"（LiteLLM 同口径），其余提供商为严格大于。
				LongContextThresholdInclusive: strings.EqualFold(litellmPricing.LiteLLMProvider, "xai"),
				LongContextInputMultiplier:    litellmPricing.LongContextInputCostMultiplier,
				LongContextOutputMultiplier:   litellmPricing.LongContextOutputCostMultiplier,
				ImageInputPricePerToken:       litellmPricing.InputCostPerImageToken,
				ImageCacheReadPricePerToken:   litellmPricing.CacheReadInputImageTokenCost,
				ImageOutputPricePerToken:      litellmPricing.OutputCostPerImageToken,
			}, true, pricingAt), nil
		}
	}

	// 2. 使用硬编码回退价格
	fallback := s.getFallbackPricing(model)
	if fallback != nil {
		// 按模型名去重:每个模型每进程最多打一条 warn,避免热路径每请求刷屏（issue #3394）。
		// model 在函数入口已 ToLower,故 GLM-5.2 / glm-5.2 视为同一条目。
		if _, seen := s.fallbackWarnSeen.LoadOrStore(model, struct{}{}); !seen {
			log.Printf("[Billing] Using fallback pricing for model: %s", model)
		}
		return s.applyModelSpecificPricingPolicyEx(model, fallback, true, pricingAt), nil
	}

	return nil, fmt.Errorf("%w for model: %s", ErrModelPricingUnavailable, model)
}

// GetModelPricingWithChannel 获取模型定价，渠道配置的价格覆盖默认值
// 渠道存在时，未配置的图片输出价格归零（不回退到 LiteLLM）
func (s *BillingService) GetModelPricingWithChannel(model string, channelPricing *ChannelModelPricing) (*ModelPricing, error) {
	pricing, err := s.GetModelPricing(model)
	if err != nil {
		return nil, err
	}
	if channelPricing == nil {
		return pricing, nil
	}
	// 防止修改 fallbackPrices 中的共享指针
	cloned := *pricing
	pricing = &cloned
	applyChannelTokenPriceOverrides(pricing, channelPricing)
	pricing.FastMultiplier = channelPricing.FastMultiplier
	pricing.FlexMultiplier = channelPricing.FlexMultiplier
	if channelPricing.MaxReasoningEffortMultiplier != nil {
		pricing.MaxReasoningEffortMultiplier = channelPricing.MaxReasoningEffortMultiplier
	}
	if channelPricing.ImageOutputPrice != nil {
		pricing.ImageOutputPricePerToken = *channelPricing.ImageOutputPrice
	} else {
		pricing.ImageOutputPricePerToken = 0
	}
	pricing.ImageOutputPriceExplicit = true
	applyChannelImageInputPrice(channelPricing, pricing)
	return pricing, nil
}

// channelTierOverridePrice applies a Standard-tier override while preserving
// an explicit model-catalog Fast/Priority ratio. If the catalog has no tier
// price, generic service-tier defaults remain responsible for the fallback.
func channelTierOverridePrice(baseStandard, baseTier, channelStandard float64) float64 {
	if baseStandard > 0 && baseTier > 0 {
		return channelStandard * (baseTier / baseStandard)
	}
	return 0
}

func applyChannelTokenPriceOverrides(pricing *ModelPricing, channelPricing *ChannelModelPricing) {
	if pricing == nil || channelPricing == nil {
		return
	}
	if channelPricing.InputPrice != nil {
		priority := channelTierOverridePrice(pricing.InputPricePerToken, pricing.InputPricePerTokenPriority, *channelPricing.InputPrice)
		pricing.InputPricePerToken = *channelPricing.InputPrice
		pricing.InputPricePerTokenPriority = priority
	}
	if channelPricing.OutputPrice != nil {
		priority := channelTierOverridePrice(pricing.OutputPricePerToken, pricing.OutputPricePerTokenPriority, *channelPricing.OutputPrice)
		pricing.OutputPricePerToken = *channelPricing.OutputPrice
		pricing.OutputPricePerTokenPriority = priority
	}
	if channelPricing.CacheWritePrice != nil {
		priority := channelTierOverridePrice(pricing.CacheCreationPricePerToken, pricing.CacheCreationPricePerTokenPriority, *channelPricing.CacheWritePrice)
		pricing.CacheCreationPricePerToken = *channelPricing.CacheWritePrice
		pricing.CacheCreationPricePerTokenPriority = priority
		pricing.CacheCreationPriceExplicit = true
		pricing.CacheCreation5mPrice = *channelPricing.CacheWritePrice
		if channelPricing.CacheWrite1hPrice == nil {
			// Preserve the pre-split behavior for existing configurations: a lone
			// cache_write_price continues to override both TTL tiers.
			pricing.CacheCreation1hPrice = *channelPricing.CacheWritePrice
		}
	}
	if channelPricing.CacheWrite1hPrice != nil {
		pricing.CacheCreation1hPrice = *channelPricing.CacheWrite1hPrice
		pricing.SupportsCacheBreakdown = true
	}
	if channelPricing.CacheReadPrice != nil {
		priority := channelTierOverridePrice(pricing.CacheReadPricePerToken, pricing.CacheReadPricePerTokenPriority, *channelPricing.CacheReadPrice)
		pricing.CacheReadPricePerToken = *channelPricing.CacheReadPrice
		pricing.CacheReadPricePerTokenPriority = priority
	}
}

// --- 统一计费入口 ---

// CostInput 统一计费输入
type CostInput struct {
	Ctx                       context.Context
	Model                     string
	GroupID                   *int64 // 用于渠道定价查找
	Group                     *Group
	Tokens                    UsageTokens
	RequestCount              int     // 按次计费时使用
	UsageUnits                float64 // 音频等连续计量单位（分钟/小时/百万字符）
	SizeTier                  string  // 按次/图片模式的层级标签（"1K","2K","4K","HD" 等）
	RateMultiplier            float64
	PricingAt                 time.Time             // 渠道分时定价使用的计费时刻
	ServiceTier               string                // "priority","flex","" 等
	ReasoningEffort           string                // 最终转发的推理等级；max 可触发模型/渠道倍率
	Resolver                  *ModelPricingResolver // 定价解析器
	Resolved                  *ResolvedPricing      // 可选：预解析的定价结果（避免重复 Resolve 调用）
	LongContextBillingEnabled *bool
}

// CalculateCostUnified 统一计费入口，支持三种计费模式。
// 使用 ModelPricingResolver 解析定价，然后根据 BillingMode 分发计算。
func (s *BillingService) CalculateCostUnified(input CostInput) (*CostBreakdown, error) {
	if input.Resolver == nil {
		// 无 Resolver，回退到旧路径
		applyLongContextBilling := true
		if input.LongContextBillingEnabled != nil {
			applyLongContextBilling = *input.LongContextBillingEnabled
		}
		breakdown, err := s.calculateCostInternalWithPolicy(
			input.Model,
			input.Tokens,
			input.RateMultiplier,
			input.ServiceTier,
			nil,
			applyLongContextBilling,
		)
		if err == nil {
			applyCostBreakdownMultiplier(breakdown, maxReasoningEffortBillingMultiplier(input.Model, input.ReasoningEffort, nil))
		}
		return breakdown, err
	}

	// 优先使用预解析结果，避免重复 Resolve 调用
	resolved := input.Resolved
	if resolved == nil {
		resolved = input.Resolver.Resolve(input.Ctx, PricingInput{
			Model:   input.Model,
			GroupID: input.GroupID,
			Group:   input.Group,
		})
	}

	// 保存时强制 > 0；若仍有负数泄漏（缓存/迁移残留），按 0 处理避免按 1x 误扣。
	if input.RateMultiplier < 0 {
		input.RateMultiplier = 0
	}

	var breakdown *CostBreakdown
	var err error
	switch resolved.Mode {
	case BillingModePerRequest, BillingModeImage, BillingModeVideo:
		breakdown, err = s.calculatePerRequestCost(resolved, input)
	default: // BillingModeToken
		breakdown, err = s.calculateTokenCost(resolved, input)
	}
	if err == nil && breakdown != nil {
		breakdown.BillingMode = string(resolved.Mode)
		if breakdown.BillingMode == "" {
			breakdown.BillingMode = string(BillingModeToken)
		}
	}
	return breakdown, err
}

// calculateTokenCost 按 token 区间计费
func (s *BillingService) calculateTokenCost(resolved *ResolvedPricing, input CostInput) (*CostBreakdown, error) {
	totalContext := input.Tokens.InputTokens + input.Tokens.CacheCreationTokens + input.Tokens.CacheReadTokens

	// 分组开关是统一入口；账号 API 开关保留为额外开启能力，但 false 不否决分组配置。
	contextTierPricingEnabled := resolved.longContextPricingEnabled
	if input.LongContextBillingEnabled != nil && *input.LongContextBillingEnabled {
		contextTierPricingEnabled = true
	}

	pricingContext := totalContext
	if !contextTierPricingEnabled {
		// 渠道可能显式配置了第一档，也可能只配置高上下文档。用 1 token
		// 选择最低档；未命中时自然回退到渠道基础价。
		pricingContext = 1
	}
	pricing := input.Resolver.GetIntervalPricing(resolved, pricingContext)
	if pricing == nil {
		return nil, fmt.Errorf("no pricing available for model: %s: %w", input.Model, ErrModelPricingUnavailable)
	}

	// 计费时点：优先请求级 PricingAt（历史补账与 DeepSeek pro→Flash 切换判定
	// 同源），零值回退当前时刻。
	pricingAt := input.PricingAt
	if pricingAt.IsZero() {
		pricingAt = timezone.Now()
	}

	// 默认价卡（Source=LiteLLM）应用 DeepSeek 官方价强制覆盖（幂等，GetModelPricing
	// 内部已强制过）；分组/渠道自定义定价保留运营者配置，不强制覆盖官方价。
	pricing = s.applyModelSpecificPricingPolicyEx(input.Model, pricing, resolved.Source == PricingSourceLiteLLM, pricingAt)

	// DeepSeek 模型默认价卡按官方峰谷口径调整：高峰时段（01:00–04:00 与
	// 06:00–10:00 UTC，仅工作日；北京时间周末全天低谷）按 2× 低谷价计费。
	// 仅作用于默认价卡（Source=LiteLLM，无分组/渠道自定义定价）——分组/渠道
	// 自定义定价保持运营者语义，不叠加。先克隆再乘，避免污染共享 fallbackPrices 指针。
	if resolved.Source == PricingSourceLiteLLM && isDeepSeekModel(input.Model) {
		if mult := deepseekPeakMultiplierAt(pricingAt); mult > 1 {
			cloned := *pricing
			cloned.InputPricePerToken *= mult
			cloned.OutputPricePerToken *= mult
			cloned.CacheReadPricePerToken *= mult
			pricing = &cloned
		}
	}

	// 官方长上下文阶梯仅在无区间定价时应用（区间定价已包含上下文分层）。
	applyLongCtx := len(resolved.Intervals) == 0 && contextTierPricingEnabled

	breakdown := s.computeTokenBreakdown(pricing, input.Tokens, input.RateMultiplier, input.ServiceTier, applyLongCtx)
	applyCostBreakdownMultiplier(breakdown, resolvedChannelTimeMultiplier(resolved, input.PricingAt))
	applyCostBreakdownMultiplier(breakdown, maxReasoningEffortBillingMultiplier(input.Model, input.ReasoningEffort, pricing))
	return breakdown, nil
}

// computeTokenBreakdown 是 token 计费的核心逻辑，由 calculateTokenCost 和 calculateCostInternal 共用。
// applyLongCtx 控制是否检查长上下文定价（区间定价已自含上下文分层，不需要额外应用）。
func (s *BillingService) computeTokenBreakdown(
	pricing *ModelPricing, tokens UsageTokens,
	rateMultiplier float64, serviceTier string,
	applyLongCtx bool,
) *CostBreakdown {
	// 保存时强制 > 0；若仍有负数泄漏，按 0 处理避免按 1x 误扣。
	if rateMultiplier < 0 {
		rateMultiplier = 0
	}

	inputPrice := pricing.InputPricePerToken
	outputPrice := pricing.OutputPricePerToken
	cacheReadPrice := pricing.CacheReadPricePerToken
	cacheCreationPrice := pricing.CacheCreationPricePerToken
	cacheCreationMultiplier := 1.0
	tierMultiplier := 1.0

	if usePriorityServiceTierPricing(serviceTier, pricing) {
		if pricing.InputPricePerTokenPriority > 0 {
			inputPrice = pricing.InputPricePerTokenPriority
		}
		if pricing.OutputPricePerTokenPriority > 0 {
			outputPrice = pricing.OutputPricePerTokenPriority
		}
		if pricing.CacheReadPricePerTokenPriority > 0 {
			cacheReadPrice = pricing.CacheReadPricePerTokenPriority
		}
		if pricing.CacheCreationPricePerTokenPriority > 0 {
			cacheCreationPrice = pricing.CacheCreationPricePerTokenPriority
		}
	} else {
		tierMultiplier = configuredServiceTierMultiplier(serviceTier, pricing)
	}

	longContextPricingEligible := applyLongCtx && s.shouldApplySessionLongContextPricing(tokens, pricing)
	var baselineCost *CostBreakdown
	if longContextPricingEligible {
		baselineCost = s.computeTokenBreakdown(pricing, tokens, rateMultiplier, serviceTier, false)
		// 倍率 ≤0 表示该项未配置（目录/覆写条目可能只写了 input 或 output 一侧），
		// 按 1 计而不是乘 0：乘 0 会把超阈值请求的对应分项算成免费。
		longCtxInputMultiplier := longContextMultiplierOrOne(pricing.LongContextInputMultiplier)
		inputPrice *= longCtxInputMultiplier
		outputPrice *= longContextMultiplierOrOne(pricing.LongContextOutputMultiplier)
		// 缓存读取本质上是输入侧的复用，应与 input 一同应用长上下文倍率；
		// 否则 cache hit 越多，少计的费用越多（见 #2293）。
		cacheReadPrice *= longCtxInputMultiplier
		// 缓存创建（cache_write）也是输入侧操作，三档价格（标准 / 5m / 1h）
		// 都通过 computeCacheCreationCost 直接读取 pricing.*，不会经过这里
		// 的倍率修改，因此显式向下传一个倍率，避免长上下文场景下被漏乘。
		cacheCreationMultiplier = longCtxInputMultiplier
	}

	bd := &CostBreakdown{}
	// 分离图片输入 token 与文本输入 token（多模态 embedding、图片编辑等图文不同价场景）。
	// InputCost 仅计文本输入，图片输入费用单独记入 ImageInputCost，便于对账；总额不变。
	// ImageInputTokens 为 0 时（绝大多数 chat/vision 流量）走原始单价路径，行为不变。
	if tokens.ImageInputTokens > 0 {
		imageInputTokens := tokens.ImageInputTokens
		textInputTokens := tokens.InputTokens - imageInputTokens
		if textInputTokens < 0 {
			textInputTokens = 0
			imageInputTokens = tokens.InputTokens
		}
		imageInputPrice := pricing.ImageInputPricePerToken
		if imageInputPrice == 0 {
			// 未配置图片输入档时回退到文本 input 价（已含 priority / 长上下文调整）
			imageInputPrice = inputPrice
		}
		bd.InputCost = float64(textInputTokens) * inputPrice
		bd.ImageInputCost = float64(imageInputTokens) * imageInputPrice
	} else {
		bd.InputCost = float64(tokens.InputTokens) * inputPrice
	}

	// 分离图片输出 token 与文本输出 token
	textOutputTokens := tokens.OutputTokens - tokens.ImageOutputTokens
	if textOutputTokens < 0 {
		textOutputTokens = 0
	}
	bd.OutputCost = float64(textOutputTokens) * outputPrice

	// 图片输出 token 费用（独立费率）
	if tokens.ImageOutputTokens > 0 {
		imgPrice := pricing.ImageOutputPricePerToken
		if imgPrice == 0 && !pricing.ImageOutputPriceExplicit {
			imgPrice = outputPrice
		}
		bd.ImageOutputCost = float64(tokens.ImageOutputTokens) * imgPrice
	}

	// 缓存创建费用
	bd.CacheCreationCost = s.computeCacheCreationCost(pricing, tokens, cacheCreationPrice, cacheCreationMultiplier)

	bd.CacheReadCost = float64(tokens.CacheReadTokens) * cacheReadPrice
	if imageCached := min(max(tokens.ImageCacheReadTokens, 0), max(tokens.CacheReadTokens, 0)); imageCached > 0 && pricing.ImageCacheReadPricePerToken > 0 {
		bd.CacheReadCost = float64(tokens.CacheReadTokens-imageCached)*cacheReadPrice + float64(imageCached)*pricing.ImageCacheReadPricePerToken
	}

	if tierMultiplier != 1.0 {
		bd.InputCost *= tierMultiplier
		bd.ImageInputCost *= tierMultiplier
		bd.OutputCost *= tierMultiplier
		bd.ImageOutputCost *= tierMultiplier
		bd.CacheCreationCost *= tierMultiplier
		bd.CacheReadCost *= tierMultiplier
	}

	bd.TotalCost = bd.InputCost + bd.ImageInputCost + bd.OutputCost + bd.ImageOutputCost +
		bd.CacheCreationCost + bd.CacheReadCost
	bd.ActualCost = bd.TotalCost * rateMultiplier
	bd.LongContextBillingApplied = baselineCost != nil && bd.ActualCost > baselineCost.ActualCost

	return bd
}

// computeCacheCreationCost 计算缓存创建费用（支持 5m/1h 分类或标准计费）。
// multiplier 用于长上下文等场景下的整体价格缩放（普通调用传 1.0 即可）。
func (s *BillingService) computeCacheCreationCost(pricing *ModelPricing, tokens UsageTokens, price, multiplier float64) float64 {
	if pricing.SupportsCacheBreakdown && (pricing.CacheCreation5mPrice > 0 || pricing.CacheCreation1hPrice > 0) {
		cacheCreation5mTokens, cacheCreation1hTokens := normalizeCacheCreationBreakdown(tokens)
		if cacheCreation5mTokens == 0 && cacheCreation1hTokens == 0 && tokens.CacheCreationTokens > 0 {
			// API 未返回 ephemeral 明细，回退到全部按 5m 单价计费
			return float64(tokens.CacheCreationTokens) * pricing.CacheCreation5mPrice * multiplier
		}
		return float64(cacheCreation5mTokens)*pricing.CacheCreation5mPrice*multiplier +
			float64(cacheCreation1hTokens)*pricing.CacheCreation1hPrice*multiplier
	}
	return float64(tokens.CacheCreationTokens) * price * multiplier
}

// normalizeCacheCreationBreakdown caps contradictory 5m/1h details at an explicitly
// positive aggregate while retaining their reported ratio as closely as integer tokens allow.
func normalizeCacheCreationBreakdown(tokens UsageTokens) (int, int) {
	cacheCreation5mTokens := tokens.CacheCreation5mTokens
	cacheCreation1hTokens := tokens.CacheCreation1hTokens
	aggregate := tokens.CacheCreationTokens
	if cacheCreation5mTokens < 0 {
		cacheCreation5mTokens = 0
	}
	if cacheCreation1hTokens < 0 {
		cacheCreation1hTokens = 0
	}
	if aggregate <= 0 || (cacheCreation5mTokens <= aggregate && cacheCreation1hTokens <= aggregate-cacheCreation5mTokens) {
		return cacheCreation5mTokens, cacheCreation1hTokens
	}

	detailTotal := float64(cacheCreation5mTokens) + float64(cacheCreation1hTokens)
	normalized5mTokens := math.Round(float64(aggregate) * float64(cacheCreation5mTokens) / detailTotal)
	if normalized5mTokens >= float64(aggregate) {
		cacheCreation5mTokens = aggregate
	} else {
		cacheCreation5mTokens = int(normalized5mTokens)
	}
	return cacheCreation5mTokens, aggregate - cacheCreation5mTokens
}

// calculatePerRequestCost 按次/图片计费
func (s *BillingService) calculatePerRequestCost(resolved *ResolvedPricing, input CostInput) (*CostBreakdown, error) {
	units := input.UsageUnits
	if units <= 0 {
		count := input.RequestCount
		if count <= 0 {
			count = 1
		}
		units = float64(count)
	}

	var unitPrice float64

	if input.SizeTier != "" {
		unitPrice = input.Resolver.GetRequestTierPrice(resolved, input.SizeTier)
	}

	if unitPrice == 0 {
		totalContext := input.Tokens.InputTokens + input.Tokens.CacheCreationTokens + input.Tokens.CacheReadTokens
		unitPrice = input.Resolver.GetRequestTierPriceByContext(resolved, totalContext)
	}

	// 回退到默认按次价格
	if unitPrice == 0 {
		unitPrice = resolved.DefaultPerRequestPrice
	}

	totalCost := unitPrice * units
	actualCost := totalCost * input.RateMultiplier

	return &CostBreakdown{
		TotalCost:  totalCost,
		ActualCost: actualCost,
	}, nil
}

// CalculateCost 计算使用费用
func (s *BillingService) CalculateCost(model string, tokens UsageTokens, rateMultiplier float64) (*CostBreakdown, error) {
	return s.calculateCostInternal(model, tokens, rateMultiplier, "", nil)
}

func (s *BillingService) CalculateCostWithServiceTier(model string, tokens UsageTokens, rateMultiplier float64, serviceTier string) (*CostBreakdown, error) {
	return s.calculateCostInternal(model, tokens, rateMultiplier, serviceTier, nil)
}

func (s *BillingService) calculateCostWithServiceTierPolicy(
	model string,
	tokens UsageTokens,
	rateMultiplier float64,
	serviceTier string,
	longContextBillingEnabled bool,
) (*CostBreakdown, error) {
	return s.calculateCostInternalWithPolicy(model, tokens, rateMultiplier, serviceTier, nil, longContextBillingEnabled)
}

func (s *BillingService) calculateCostInternal(model string, tokens UsageTokens, rateMultiplier float64, serviceTier string, channelPricing *ChannelModelPricing) (*CostBreakdown, error) {
	return s.calculateCostInternalWithPolicy(model, tokens, rateMultiplier, serviceTier, channelPricing, true)
}

func (s *BillingService) calculateCostInternalWithPolicy(
	model string,
	tokens UsageTokens,
	rateMultiplier float64,
	serviceTier string,
	channelPricing *ChannelModelPricing,
	longContextBillingEnabled bool,
) (*CostBreakdown, error) {
	var pricing *ModelPricing
	var err error
	if channelPricing != nil {
		pricing, err = s.GetModelPricingWithChannel(model, channelPricing)
	} else {
		pricing, err = s.GetModelPricing(model)
	}
	if err != nil {
		return nil, err
	}

	return s.computeTokenBreakdown(pricing, tokens, rateMultiplier, serviceTier, longContextBillingEnabled), nil
}

// applyModelSpecificPricingPolicy 对目录数据做模型特定修正：DeepSeek 官方价
// 强制覆盖；GPT-5.6 缺 cache_write 价时按官方规则补 1.25 倍输入价；Fast/priority
// 档按业务倍率改写（本地/远程目录的 priority 价可能沿用官方旧口径）。长上下文
// 阶梯不在此处补齐：一律由目录数据（above_XXXk 折算或显式 long_context_* 字段）
// 驱动。强制 DeepSeek 官方价且无显式计费时点（pro→Flash 切换按当前时刻判定），
// 供无既有时点的策略修正场景与测试使用；计费/展示主路径分别经
// calculateTokenCost 与 getModelPricingAt 显式传时点，分组/渠道自定义定价
// 用 applyModelSpecificPricingPolicyEx 关闭强制，保留运营者配置。
func (s *BillingService) applyModelSpecificPricingPolicy(model string, pricing *ModelPricing) *ModelPricing {
	return s.applyModelSpecificPricingPolicyEx(model, pricing, true, time.Time{})
}

// applyModelSpecificPricingPolicyEx 与 applyModelSpecificPricingPolicy 相同，
// 但由调用方控制是否强制 DeepSeek 官方价（forceDeepSeekRates），并显式传入
// 计费时点 pricingAt（零值表示按当前时刻判定）。
// calculateTokenCost 对分组/渠道自定义定价（Source 非 LiteLLM）传 false：
// 强制覆盖会把运营者配置的售价盖回官方价，违反自定义定价语义。
func (s *BillingService) applyModelSpecificPricingPolicyEx(model string, pricing *ModelPricing, forceDeepSeekRates bool, pricingAt time.Time) *ModelPricing {
	if pricing == nil {
		return nil
	}
	// DeepSeek 模型：无论 JSON/远端价格表给什么价，一律强制官方低谷价
	// （Flash 三档为 2026-09-10 官方降价后口径）。这是覆盖远端旧价的关键——远端
	// 仓库不可改，生产会先拉到旧价，必须在此兜底修正；克隆后再覆盖，避免污染
	// 共享 fallbackPrices 指针。
	// 档位判定：含 "deepseek-v4-pro" 的版本化名称（如 deepseek-v4-pro-0813）归 pro 档，
	// 其余 deepseek-*（含已停服的 chat/reasoner 与未知型号）统一归 flash 档。
	// 2026-09-14 04:00 UTC 起上游把 pro 请求路由到 V4.1-Flash，pro 档改按
	// Flash 三档价计费；历史时点（早于切换时刻）仍按 Pro 价。
	// 高峰时段倍率不在本函数处理，由 calculateTokenCost 按 deepseekPeakMultiplierAt
	// 对默认价卡另行叠加（分组/渠道自定义定价不叠加）。
	if forceDeepSeekRates && isDeepSeekModel(model) {
		cloned := *pricing
		if isDeepSeekProModel(model) && !deepseekProBilledAsFlash(pricingAt) {
			cloned.InputPricePerToken = deepseekProOffPeakInputPrice
			cloned.OutputPricePerToken = deepseekProOffPeakOutputPrice
			cloned.CacheReadPricePerToken = deepseekProOffPeakCacheRead
		} else {
			// deepseek-flash（= V4.1-Flash）、deepseek-v4-flash /
			// deepseek-v4-flash-vision-exp 与其余 deepseek-* 共用 flash 价；
			// 切换时点之后的 pro 请求同样按 flash 价计费。
			cloned.InputPricePerToken = deepseekFlashOffPeakInputPrice
			cloned.OutputPricePerToken = deepseekFlashOffPeakOutputPrice
			cloned.CacheReadPricePerToken = deepseekFlashOffPeakCacheRead
		}
		return &cloned
	}
	normalized := normalizeKnownOpenAICodexModel(model)
	isGPT56 := isOpenAIGPT56Model(normalized)
	needsMaxReasoningEffortMultiplier := isClaudeFable51Model(model) && pricing.MaxReasoningEffortMultiplier == nil
	needsCacheCreationPolicy := isGPT56 && !pricing.CacheCreationPriceExplicit && (pricing.CacheCreationPricePerToken <= 0 ||
		(pricing.InputPricePerTokenPriority > 0 && pricing.CacheCreationPricePerTokenPriority <= 0))
	fastRatio := openAIModelFastPricingRatio(normalized)
	if !needsCacheCreationPolicy && fastRatio <= 0 && !needsMaxReasoningEffortMultiplier {
		return pricing
	}
	cloned := *pricing
	if needsMaxReasoningEffortMultiplier {
		cloned.MaxReasoningEffortMultiplier = defaultMaxReasoningEffortMultiplier(model)
	}
	if isGPT56 && !cloned.CacheCreationPriceExplicit {
		if cloned.CacheCreationPricePerToken <= 0 {
			cloned.CacheCreationPricePerToken = cloned.InputPricePerToken * 1.25
		}
		if cloned.CacheCreationPricePerTokenPriority <= 0 {
			cloned.CacheCreationPricePerTokenPriority = cloned.InputPricePerTokenPriority * 1.25
		}
	}
	if fastRatio > 0 {
		enforceOpenAIFastPricingRatio(&cloned, fastRatio)
	}
	return &cloned
}

// openAIModelFastPricingRatio 返回业务口径下 OpenAI GPT 模型 Fast/priority
// 的标准价倍率：gpt-5.6 / gpt-6-astra / gpt-5.4 为 2x，gpt-5.5 为 2.5x。未定义 Fast
// 档的模型（如 gpt-5.5-pro、gpt-5.4-mini/nano）返回 0。
func openAIModelFastPricingRatio(normalized string) float64 {
	switch normalized {
	case "gpt-5.4", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra":
		return 2.0
	case "gpt-5.5":
		return 2.5
	default:
		if isOpenAIGPT6AstraModel(normalized) {
			return 2.0
		}
		return 0
	}
}

// enforceOpenAIFastPricingRatio 把 priority 档价格改写为「标准价 × ratio」。
// 本地/远程 LiteLLM 目录可能只带官方旧口径（如 gpt-5.5 priority 仍标 2x），
// 直接采用会导致 Fast 模式少计费；这里按业务倍率兜底修正，且对已正确的
// fallback 条目（2x/2.5x）是幂等的。computeTokenBreakdown 在 priority 价格
// 存在时走显式档位价、不再叠加通用 tier 倍率，因此不会重复乘价。
func enforceOpenAIFastPricingRatio(pricing *ModelPricing, ratio float64) {
	if pricing == nil || ratio <= 0 {
		return
	}
	pricing.InputPricePerTokenPriority = pricing.InputPricePerToken * ratio
	pricing.OutputPricePerTokenPriority = pricing.OutputPricePerToken * ratio
	if pricing.CacheReadPricePerToken > 0 {
		pricing.CacheReadPricePerTokenPriority = pricing.CacheReadPricePerToken * ratio
	}
	if pricing.CacheCreationPricePerToken > 0 {
		pricing.CacheCreationPricePerTokenPriority = pricing.CacheCreationPricePerToken * ratio
	}
}

// longContextMultiplierOrOne 把未配置（≤0）的长上下文倍率归一为 1。
func longContextMultiplierOrOne(m float64) float64 {
	if m <= 0 {
		return 1
	}
	return m
}

func (s *BillingService) shouldApplySessionLongContextPricing(tokens UsageTokens, pricing *ModelPricing) bool {
	if pricing == nil || pricing.LongContextInputThreshold <= 0 {
		return false
	}
	if pricing.LongContextInputMultiplier <= 1 && pricing.LongContextOutputMultiplier <= 1 {
		return false
	}
	totalInputTokens := tokens.InputTokens + tokens.CacheCreationTokens + tokens.CacheReadTokens
	if pricing.LongContextThresholdInclusive {
		return totalInputTokens >= pricing.LongContextInputThreshold
	}
	return totalInputTokens > pricing.LongContextInputThreshold
}

// CalculateCostWithConfig 使用配置中的默认倍率计算费用
func (s *BillingService) CalculateCostWithConfig(model string, tokens UsageTokens) (*CostBreakdown, error) {
	multiplier := s.cfg.Default.RateMultiplier
	if multiplier <= 0 {
		multiplier = 1.0
	}
	return s.CalculateCost(model, tokens, multiplier)
}

// ListSupportedModels 列出所有支持的模型（现在总是返回true，因为有模糊匹配）
func (s *BillingService) ListSupportedModels() []string {
	models := make([]string, 0)
	// 返回回退价格支持的模型系列
	for model := range s.fallbackPrices {
		models = append(models, model)
	}
	return models
}

// IsModelSupported 检查模型是否支持（现在总是返回true，因为有模糊匹配回退）
func (s *BillingService) IsModelSupported(model string) bool {
	// 所有Claude模型都有回退价格支持
	modelLower := strings.ToLower(model)
	return strings.Contains(modelLower, "claude") ||
		strings.Contains(modelLower, "opus") ||
		strings.Contains(modelLower, "sonnet") ||
		strings.Contains(modelLower, "haiku")
}

// GetEstimatedCost 估算费用（用于前端展示）
func (s *BillingService) GetEstimatedCost(model string, estimatedInputTokens, estimatedOutputTokens int) (float64, error) {
	tokens := UsageTokens{
		InputTokens:  estimatedInputTokens,
		OutputTokens: estimatedOutputTokens,
	}

	breakdown, err := s.CalculateCostWithConfig(model, tokens)
	if err != nil {
		return 0, err
	}

	return breakdown.ActualCost, nil
}

// GetPricingServiceStatus 获取价格服务状态
func (s *BillingService) GetPricingServiceStatus() map[string]any {
	if s.pricingService != nil {
		return s.pricingService.GetStatus()
	}
	return map[string]any{
		"model_count":  len(s.fallbackPrices),
		"last_updated": "using fallback",
		"local_hash":   "N/A",
	}
}

// ForceUpdatePricing 强制更新价格数据
func (s *BillingService) ForceUpdatePricing() error {
	if s.pricingService != nil {
		return s.pricingService.ForceUpdate()
	}
	return fmt.Errorf("pricing service not initialized")
}

// ImagePriceConfig 图片计费配置
type ImagePriceConfig struct {
	Price1K *float64 // 1K 尺寸价格（nil 表示使用默认值）
	Price2K *float64 // 2K 尺寸价格（nil 表示使用默认值）
	Price4K *float64 // 4K 尺寸价格（nil 表示使用默认值）
}

// VideoPriceConfig 视频生成计费配置。所有价格均为**每秒**单价（USD/s），与 xAI 官方计费口径一致。
type VideoPriceConfig struct {
	Price480P  *float64 // 480p 每秒价格（nil 表示使用默认值）
	Price720P  *float64 // 720p 每秒价格（nil 表示使用默认值）
	Price1080P *float64 // 1080p 每秒价格（nil 表示使用默认值）
	// ModelPrices is optional per-model-family override: family → resolution → USD/s.
	// When set for a model, it wins over Price* flat columns for that model only.
	ModelPrices map[string]map[string]float64
}

const (
	defaultImageGenerationPrice = 0.134

	defaultGrokImagineImagePrice1K        = 0.02
	defaultGrokImagineImagePrice2K        = 0.02
	defaultGrokImagineImageQualityPrice1K = 0.05
	defaultGrokImagineImageQualityPrice2K = 0.07
	defaultGrokImagineImage20Price1K      = 0.06 // default quality is Medium
	defaultGrokImagineImage20Price2K      = 0.08

	// 视频默认价为 xAI 官方**每秒**输出价格（USD/s），总价 = 每秒价 × 时长（秒）。
	defaultGrokImagineVideoPrice480P    = 0.05
	defaultGrokImagineVideoPrice720P    = 0.07
	defaultGrokImagineVideo15Price480P  = 0.08
	defaultGrokImagineVideo15Price720P  = 0.14
	defaultGrokImagineVideo15Price1080P = 0.25

	// Codex alpha/search 网页搜索单次默认价：OpenAI 官方 web search 定价 $10/1000 次。
	defaultWebSearchPricePerCall = 0.01

	// xAI server-side web/X search and code execution are $5/1000 calls.
	defaultSearchPricePer1k = 5.0

	// Generic realtime defaults to think-fast-1.0; think-fast-2.0 can be
	// configured independently through per-model group/channel pricing.
	defaultAudioRealtimePricePerMin     = 0.05
	defaultAudioTTSPricePerMillionChars = 15.0
	defaultAudioSTTPricePerHour         = 0.10
)

// CalculateWebSearchCost 计算 Codex alpha/search 网页搜索按次费用。
// callCount: 搜索调用次数（每次请求为 1）
// groupPrice: 分组配置的单次价格（nil 表示使用默认价 0.01；0 表示免费）
// rateMultiplier: 分组费率倍数
func (s *BillingService) CalculateWebSearchCost(callCount int, groupPrice *float64, rateMultiplier float64) *CostBreakdown {
	if callCount <= 0 {
		return &CostBreakdown{}
	}
	unitPrice := defaultWebSearchPricePerCall
	if groupPrice != nil && *groupPrice >= 0 {
		unitPrice = *groupPrice
	}
	totalCost := unitPrice * float64(callCount)

	// 应用倍率（保存时强制 > 0；负数按 0 处理避免按 1x 误扣）
	if rateMultiplier < 0 {
		rateMultiplier = 0
	}
	return &CostBreakdown{
		TotalCost:   totalCost,
		ActualCost:  totalCost * rateMultiplier,
		BillingMode: string(BillingModePerRequest),
	}
}

// CalculateSearchCost bills search/tool invocations (e.g. web_search) per 1k calls.
// groupPricePer1k: nil → defaultSearchPricePer1k; explicit 0 → free; >0 → that rate.
func (s *BillingService) CalculateSearchCost(numCalls int, groupPricePer1k *float64, rateMultiplier float64) *CostBreakdown {
	if numCalls <= 0 {
		return &CostBreakdown{}
	}
	pricePer1k := defaultSearchPricePer1k
	if groupPricePer1k != nil {
		if *groupPricePer1k < 0 {
			return &CostBreakdown{}
		}
		pricePer1k = *groupPricePer1k
	}
	if pricePer1k == 0 {
		return &CostBreakdown{}
	}
	if rateMultiplier < 0 {
		rateMultiplier = 0
	}
	unit := pricePer1k / 1000.0
	total := unit * float64(numCalls)
	return &CostBreakdown{
		TotalCost:   total,
		ActualCost:  total * rateMultiplier,
		BillingMode: string(BillingModePerRequest),
	}
}

type audioPriceConfig struct {
	RealtimePerMin *float64
	TTSPerMChars   *float64
	STTPerHour     *float64
}

// CalculateAudioCost supports realtime (per min), tts (per M chars), stt (per hr).
// Missing group prices use defaults; explicit 0 means free for that mode.
func (s *BillingService) CalculateAudioCost(mode string, durationOrUnits float64, groupConfig *audioPriceConfig, rateMultiplier float64) *CostBreakdown {
	if durationOrUnits <= 0 {
		return &CostBreakdown{}
	}
	var unitPrice float64
	switch strings.ToLower(mode) {
	case "realtime":
		unitPrice = defaultAudioRealtimePricePerMin
		if groupConfig != nil && groupConfig.RealtimePerMin != nil {
			unitPrice = *groupConfig.RealtimePerMin
		}
	case "tts":
		unitPrice = defaultAudioTTSPricePerMillionChars
		if groupConfig != nil && groupConfig.TTSPerMChars != nil {
			unitPrice = *groupConfig.TTSPerMChars
		}
	case "stt":
		unitPrice = defaultAudioSTTPricePerHour
		if groupConfig != nil && groupConfig.STTPerHour != nil {
			unitPrice = *groupConfig.STTPerHour
		}
	default:
		return &CostBreakdown{}
	}
	if unitPrice <= 0 {
		return &CostBreakdown{}
	}
	if rateMultiplier < 0 {
		rateMultiplier = 0
	}
	total := unitPrice * durationOrUnits
	return &CostBreakdown{
		TotalCost:   total,
		ActualCost:  total * rateMultiplier,
		BillingMode: string(BillingModePerRequest),
	}
}

// CalculateImageCost 计算图片生成费用
// model: 请求的模型名称（用于获取 LiteLLM 默认价格）
// imageSize: 图片尺寸 "1K", "2K", "4K"
// imageCount: 生成的图片数量
// groupConfig: 分组配置的价格（可能为 nil，表示使用默认值）
// rateMultiplier: 费率倍数
func (s *BillingService) CalculateImageCost(model string, imageSize string, imageCount int, groupConfig *ImagePriceConfig, rateMultiplier float64) *CostBreakdown {
	if imageCount <= 0 {
		return &CostBreakdown{}
	}
	imageSize = NormalizeImageBillingTierOrDefault(imageSize)

	// 获取单价
	unitPrice := s.getImageUnitPrice(model, imageSize, groupConfig)

	// 计算总费用
	totalCost := unitPrice * float64(imageCount)

	// 应用倍率（保存时强制 > 0；负数按 0 处理避免按 1x 误扣）
	if rateMultiplier < 0 {
		rateMultiplier = 0
	}
	actualCost := totalCost * rateMultiplier

	return &CostBreakdown{
		TotalCost:   totalCost,
		ActualCost:  actualCost,
		BillingMode: string(BillingModeImage),
	}
}

// CalculateVideoCost 计算视频生成费用（按秒计费，与 xAI 口径一致）。
// model: 请求的模型名称（用于获取默认价格）
// resolution: 视频分辨率 "480p", "720p", "1080p"
// videoCount: 生成的视频数量
// durationSeconds: 单个视频时长（秒），<=0 时按上游默认时长计
// groupConfig: 分组配置的每秒价格（可能为 nil，表示使用默认值）
// rateMultiplier: 费率倍数
func (s *BillingService) CalculateVideoCost(model string, resolution string, videoCount int, durationSeconds int, groupConfig *VideoPriceConfig, rateMultiplier float64) *CostBreakdown {
	if videoCount <= 0 {
		return &CostBreakdown{}
	}
	resolution = NormalizeVideoBillingResolutionOrDefault(resolution)
	durationSeconds = NormalizeVideoBillingDurationSecondsOrDefault(durationSeconds)

	perSecondPrice := s.getVideoUnitPrice(model, resolution, groupConfig)
	totalCost := perSecondPrice * float64(durationSeconds) * float64(videoCount)

	if rateMultiplier < 0 {
		rateMultiplier = 0
	}
	actualCost := totalCost * rateMultiplier

	return &CostBreakdown{
		TotalCost:   totalCost,
		ActualCost:  actualCost,
		BillingMode: string(BillingModeVideo),
	}
}

// getImageUnitPrice 获取图片单价
func (s *BillingService) getImageUnitPrice(model string, imageSize string, groupConfig *ImagePriceConfig) float64 {
	// 优先使用分组配置的价格
	if groupConfig != nil {
		switch imageSize {
		case "1K":
			if groupConfig.Price1K != nil {
				return *groupConfig.Price1K
			}
		case "2K":
			if groupConfig.Price2K != nil {
				return *groupConfig.Price2K
			}
		case "4K":
			if groupConfig.Price4K != nil {
				return *groupConfig.Price4K
			}
		}
	}

	// 回退到 LiteLLM 默认价格
	return s.getDefaultImagePrice(model, imageSize)
}

func (s *BillingService) getVideoUnitPrice(model string, resolution string, groupConfig *VideoPriceConfig) float64 {
	// Order: (a) per-model map (b) flat group video_price_* (c) model-aware code defaults.
	if groupConfig != nil {
		if price := LookupVideoModelPrice(groupConfig.ModelPrices, model, resolution); price != nil {
			return *price
		}
		switch NormalizeVideoBillingResolutionOrDefault(resolution) {
		case VideoBillingResolution480P:
			if groupConfig.Price480P != nil {
				return *groupConfig.Price480P
			}
		case VideoBillingResolution720P:
			if groupConfig.Price720P != nil {
				return *groupConfig.Price720P
			}
		case VideoBillingResolution1080P:
			if groupConfig.Price1080P != nil {
				return *groupConfig.Price1080P
			}
		}
	}

	return s.getDefaultVideoPrice(model, resolution)
}

// getDefaultImagePrice 获取 LiteLLM 默认图片价格
func (s *BillingService) getDefaultImagePrice(model string, imageSize string) float64 {
	if price, ok := getDefaultGrokImagineImagePrice(model, imageSize); ok {
		return price
	}

	basePrice := 0.0

	// 从 PricingService 获取 output_cost_per_image
	if s.pricingService != nil {
		pricing := s.pricingService.GetModelPricing(model)
		if pricing != nil && pricing.OutputCostPerImage > 0 {
			basePrice = pricing.OutputCostPerImage
		}
	}

	// 如果没有找到价格，使用硬编码默认值（$0.134，来自 gemini-3-pro-image-preview）
	if basePrice <= 0 {
		basePrice = defaultImageGenerationPrice
	}

	// 2K 尺寸 1.5 倍，4K 尺寸翻倍
	if imageSize == "2K" {
		return basePrice * 1.5
	}
	if imageSize == "4K" {
		return basePrice * 2
	}

	return basePrice
}

func (s *BillingService) getDefaultVideoPrice(model string, resolution string) float64 {
	if price, ok := getDefaultGrokImagineVideoPrice(model, resolution); ok {
		return price
	}

	// The bundled LiteLLM schema does not expose an output video generation price.
	// Keep the historical model default as the fallback (interpreted as a per-second
	// rate; today only Grok models reach video billing, so this path is a safety net),
	// while letting group-level video prices override it independently from image prices.
	return s.getDefaultImagePrice(model, ImageBillingSize2K)
}

func getDefaultGrokImagineImagePrice(model string, imageSize string) (float64, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	switch model {
	case "grok-imagine-image-2.0":
		return getGrokImagineImageTierPrice(
			imageSize,
			defaultGrokImagineImage20Price1K,
			defaultGrokImagineImage20Price2K,
		), true
	case "grok-imagine-image-quality":
		return getGrokImagineImageTierPrice(
			imageSize,
			defaultGrokImagineImageQualityPrice1K,
			defaultGrokImagineImageQualityPrice2K,
		), true
	case "grok-imagine", "grok-imagine-image", "grok-imagine-edit":
		return getGrokImagineImageTierPrice(
			imageSize,
			defaultGrokImagineImagePrice1K,
			defaultGrokImagineImagePrice2K,
		), true
	default:
		return 0, false
	}
}

func getGrokImagineImageTierPrice(imageSize string, price1K float64, price2K float64) float64 {
	switch NormalizeImageBillingTierOrDefault(imageSize) {
	case ImageBillingSize1K:
		return price1K
	case ImageBillingSize2K, ImageBillingSize4K:
		return price2K
	default:
		return price2K
	}
}

func getDefaultGrokImagineVideoPrice(model string, resolution string) (float64, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(model, "grok-imagine-video-1.5"):
		switch NormalizeVideoBillingResolutionOrDefault(resolution) {
		case VideoBillingResolution480P:
			return defaultGrokImagineVideo15Price480P, true
		case VideoBillingResolution720P:
			return defaultGrokImagineVideo15Price720P, true
		case VideoBillingResolution1080P:
			return defaultGrokImagineVideo15Price1080P, true
		default:
			return defaultGrokImagineVideo15Price480P, true
		}
	case strings.HasPrefix(model, "grok-imagine-video"):
		switch NormalizeVideoBillingResolutionOrDefault(resolution) {
		case VideoBillingResolution480P:
			return defaultGrokImagineVideoPrice480P, true
		case VideoBillingResolution720P, VideoBillingResolution1080P:
			return defaultGrokImagineVideoPrice720P, true
		default:
			return defaultGrokImagineVideoPrice480P, true
		}
	default:
		return 0, false
	}
}
