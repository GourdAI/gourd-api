package service

// 订阅 = 「个人额度钱包」（2026-10-03 产品定案，见 .gwork/SUBSCRIPTION_WALLET_SPEC.md）。
//
// 与旧模型的根本差别：
//   - 旧：按 (user, group) 开槽位，槽位上挂日/周/月三档滚动窗口限额，窗口需要激活与重置；
//   - 新：按 user 持有**多份**独立订阅，每份是一个**一次性总额池**（total_limit_usd /
//     total_usage_usd），花完为止，到期作废，不随任何周期滚动重置。
//
// 因此本文件不再有任何「窗口」概念：没有激活、没有重置、没有跨 0 点/跨月对齐，
// 也不需要 groupRepo 参与校验（订阅不授予分组准入，分组权限一律走原有分组校验）。
//
// 消耗顺序「先到期先消耗」+ 单笔跨订阅拆分由 AllocateSubscriptionUsage 负责（纯函数）；
// 记账入口是 AllocateAndRecordUsage：判定用聚合、记账用逐行。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/dgraph-io/ristretto"
	"golang.org/x/sync/singleflight"
)

// MaxExpiresAt is the maximum allowed expiration date (year 2099)
// This prevents time.Time JSON serialization errors (RFC 3339 requires year <= 9999)
var MaxExpiresAt = time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC)

// MaxValidityDays is the maximum allowed validity days for subscriptions (100 years)
const MaxValidityDays = 36500

var (
	ErrSubscriptionNotFound        = infraerrors.NotFound("SUBSCRIPTION_NOT_FOUND", "subscription not found")
	ErrSubscriptionExpired         = infraerrors.Forbidden("SUBSCRIPTION_EXPIRED", "subscription has expired")
	ErrSubscriptionSuspended       = infraerrors.Forbidden("SUBSCRIPTION_SUSPENDED", "subscription is suspended")
	ErrSubscriptionAlreadyExists   = infraerrors.Conflict("SUBSCRIPTION_ALREADY_EXISTS", "subscription already exists for this user")
	ErrSubscriptionAssignConflict  = infraerrors.Conflict("SUBSCRIPTION_ASSIGN_CONFLICT", "subscription exists but request conflicts with existing assignment semantics")
	ErrSubscriptionNotRevoked      = infraerrors.Conflict("SUBSCRIPTION_NOT_REVOKED", "subscription is not revoked")
	ErrSubscriptionRestoreConflict = infraerrors.Conflict("SUBSCRIPTION_RESTORE_CONFLICT", "an active subscription already exists for this user")
	ErrSubscriptionNilInput        = infraerrors.BadRequest("SUBSCRIPTION_NIL_INPUT", "subscription input cannot be nil")
	ErrAdjustWouldExpire           = infraerrors.BadRequest("ADJUST_WOULD_EXPIRE", "adjustment would result in expired subscription (remaining days must be > 0)")
	// 旧「日/周/月三档窗口 + 订阅制分组」错误变量已删除（契约 4/5）：
	// 钱包化后限额只有一个口径 ErrSubscriptionQuotaExhausted（见
	// billing_cache_service.go），日/周/月限额在语义上已不存在，
	// 分组也不再是订阅槽位依据。调用点（含两处中间件）一律改用后者。
)

// SubscriptionService 订阅服务（个人额度钱包）
type SubscriptionService struct {
	// groupRepo 仅为减少 wiring 改动而保留的字段。订阅已不绑定分组、也不授予分组准入，
	// 本服务不再用它做任何存废/类型校验（契约 6：groups.subscription_type 只用于
	// 「高峰时段倍率」的启用条件，与订阅判定无关）。
	groupRepo           GroupRepository
	userSubRepo         UserSubscriptionRepository
	billingCacheService *BillingCacheService
	entClient           *dbent.Client

	// L1 缓存：加速中间件热路径的订阅查询。
	// key = sub:<userID>，value = 该用户全部生效订阅的切片（按 expires_at 升序，
	// 即消耗顺序）或 subCacheNegative 负哨兵。
	subCacheL1     *ristretto.Cache
	subCacheGroup  singleflight.Group
	subCacheTTL    time.Duration
	subCacheJitter int // 抖动百分比

	maintenanceQueue *SubscriptionMaintenanceQueue
	now              func() time.Time
}

// NewSubscriptionService 创建订阅服务（签名保持不变，groupRepo 参数继续接收但不再使用）
func NewSubscriptionService(groupRepo GroupRepository, userSubRepo UserSubscriptionRepository, billingCacheService *BillingCacheService, entClient *dbent.Client, cfg *config.Config) *SubscriptionService {
	svc := &SubscriptionService{
		groupRepo:           groupRepo,
		userSubRepo:         userSubRepo,
		billingCacheService: billingCacheService,
		entClient:           entClient,
		now:                 time.Now,
	}
	svc.initSubCache(cfg)
	svc.initMaintenanceQueue(cfg)
	svc.StartSubCacheInvalidationSubscriber(context.Background())
	return svc
}

// currentTime 取当前时刻；now 钩子未注入（零值构造、直接赋值字段的老测试写法）时回退 time.Now。
func (s *SubscriptionService) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *SubscriptionService) initMaintenanceQueue(cfg *config.Config) {
	if cfg == nil {
		return
	}
	mc := cfg.SubscriptionMaintenance
	if mc.WorkerCount <= 0 || mc.QueueSize <= 0 {
		return
	}
	s.maintenanceQueue = NewSubscriptionMaintenanceQueue(mc.WorkerCount, mc.QueueSize)
}

// Stop stops the maintenance worker pool.
//
// 钱包化后已没有「窗口激活/重置」这类后台维护任务需要排队（总额池不滚动，
// 到期状态由 SubscriptionExpiryService 批量落库）。队列与 Stop 保留是为了
// 不打断既有的 wiring / 优雅关闭流程，删除它会牵动其他文件。
func (s *SubscriptionService) Stop() {
	if s == nil {
		return
	}
	if s.maintenanceQueue != nil {
		s.maintenanceQueue.Stop()
	}
}

// initSubCache 初始化订阅 L1 缓存
func (s *SubscriptionService) initSubCache(cfg *config.Config) {
	if cfg == nil {
		return
	}
	sc := cfg.SubscriptionCache
	if sc.L1Size <= 0 || sc.L1TTLSeconds <= 0 {
		return
	}
	cache, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: int64(sc.L1Size) * 10,
		MaxCost:     int64(sc.L1Size),
		BufferItems: 64,
	})
	if err != nil {
		log.Printf("Warning: failed to init subscription L1 cache: %v", err)
		return
	}
	s.subCacheL1 = cache
	s.subCacheTTL = time.Duration(sc.L1TTLSeconds) * time.Second
	s.subCacheJitter = sc.JitterPercent
}

// subCacheKey 生成订阅缓存 key（热路径，避免 fmt.Sprintf 开销）。
// 钱包化后 key 只有一个坐标：user。旧版的 "sub:<uid>:<gid>" 分组段随槽位模型一起消失，
// 该字符串同时作为跨实例失效消息（pubsub）的载荷，必须与 billing 侧
// Redis 键 billing:sub:<userID> 保持同一口径。
func subCacheKey(userID int64) string {
	return "sub:" + strconv.FormatInt(userID, 10)
}

// subCacheNegative 是「无订阅」的空哨兵：避免未持有订阅的用户每次网关请求都回源 DB。
// 分配/导入订阅时会按同一 key 失效，因此不会长时间遮蔽新建的订阅。
//
// 负哨兵在新模型下依然必要：绝大多数调用方根本没钱包，中间件每个请求都要问一次
// 「这个用户走订阅还是走余额」，没有哨兵就等于每请求多一次 DB 往返。
type subCacheNegative struct{}

// subNegativeTTL 负缓存上限：取 L1 TTL 与 30s 的较小值。
// 负缓存只能短：钱包是「管理员一点就生效」的东西，TTL 太长会表现为「分配了但没生效」。
func (s *SubscriptionService) subNegativeTTL() time.Duration {
	const negativeMaxTTL = 30 * time.Second
	if s.subCacheTTL <= 0 {
		return negativeMaxTTL
	}
	if s.subCacheTTL < negativeMaxTTL {
		return s.subCacheTTL
	}
	return negativeMaxTTL
}

// jitteredTTL 为 TTL 添加抖动，避免集中过期
func (s *SubscriptionService) jitteredTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 || s.subCacheJitter <= 0 {
		return ttl
	}
	pct := s.subCacheJitter
	if pct > 100 {
		pct = 100
	}
	delta := float64(pct) / 100
	factor := 1 - delta + rand.Float64()*(2*delta)
	if factor <= 0 {
		return ttl
	}
	return time.Duration(float64(ttl) * factor)
}

// InvalidateSubCache 失效指定用户的订阅 L1 缓存
func (s *SubscriptionService) InvalidateSubCache(userID int64) {
	if s.subCacheL1 == nil {
		return
	}
	s.subCacheL1.Del(subCacheKey(userID))
}

// InvalidateSubCacheSync 失效订阅 L1 缓存并等待 Ristretto 删除操作生效。
//
// Ristretto 的 Del() 是异步入队的：只 Del 不 Wait，紧随其后的 Get 仍可能读到旧值。
// 「改完立刻回读/立刻放行请求」的路径必须用这个同步版本。
func (s *SubscriptionService) InvalidateSubCacheSync(userID int64) {
	s.invalidateSubCacheKeySync(subCacheKey(userID))
}

func (s *SubscriptionService) invalidateSubCacheKeySync(key string) {
	if s.subCacheL1 == nil {
		return
	}
	s.subCacheL1.Del(key)
	s.subCacheL1.Wait()
}

// StartSubCacheInvalidationSubscriber 启动跨实例订阅 L1 缓存失效订阅。
func (s *SubscriptionService) StartSubCacheInvalidationSubscriber(ctx context.Context) {
	if s.billingCacheService == nil || s.subCacheL1 == nil {
		return
	}
	if err := s.billingCacheService.SubscribeSubscriptionCacheInvalidation(ctx, func(cacheKey string) {
		s.invalidateSubCacheKeySync(cacheKey)
	}); err != nil {
		log.Printf("Warning: failed to start subscription cache invalidation subscriber: %v", err)
	}
}

// invalidateSubscriptionCaches 一次性失效该用户的全部订阅缓存：
// 本实例 L1 → 共享（Redis）钱包聚合缓存 → 广播给其他实例清 L1。
//
// 缓存坐标只有 user，所以「失效哪一份订阅」不再重要：钱包任何一行变了，
// 整个切片都必须作废，否则会出现「已花完的行还在切片里」的假余额。
func (s *SubscriptionService) invalidateSubscriptionCaches(userID int64) error {
	s.InvalidateSubCacheSync(userID)
	if s.billingCacheService == nil {
		return nil
	}

	cacheCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.billingCacheService.InvalidateSubscription(cacheCtx, userID); err != nil {
		return fmt.Errorf("invalidate billing subscription cache: %w", err)
	}
	if err := s.billingCacheService.PublishSubscriptionCacheInvalidation(cacheCtx, subCacheKey(userID)); err != nil {
		return fmt.Errorf("publish subscription cache invalidation: %w", err)
	}
	return nil
}

// AssignSubscriptionInput 分配订阅输入。
//
// PlanID 决定幂等坐标：
//   - 非 nil：同一 (user, plan) 视为同一份钱包，重复分配 = 续期/改额度；
//   - nil：管理员手工发放（无套餐），按「该用户是否已有生效钱包」判定。
//
// TotalLimitUSD 是唯一额度入口：nil = 本次不改额度（兼容兑换码/支付续费只延有效期）；
// 非 nil 且 >0 = 设为该额度；非 nil 且 <=0 = 改为不限额钱包。
// 注意：不限额钱包不会接管扣费（见 SubscriptionWalletTakesOver），这是防资损的刻意行为。
type AssignSubscriptionInput struct {
	UserID       int64
	PlanID       *int64
	ValidityDays int
	AssignedBy   int64
	Notes        string

	TotalLimitUSD *float64
}

// AssignSubscription 分配订阅给用户（不允许重复分配：命中既有钱包时按幂等/冲突语义处理）
func (s *SubscriptionService) AssignSubscription(ctx context.Context, input *AssignSubscriptionInput) (*UserSubscription, error) {
	sub, _, err := s.assignSubscriptionWithReuse(ctx, input)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

// AssignOrExtendSubscription 分配或续期订阅（用于兑换码等场景）
// 如果用户已有可续期的钱包：
//   - 未过期：从当前过期时间累加天数
//   - 已过期：从当前时间开始计算新的过期时间，并激活订阅
//
// 如果没有钱包：创建新订阅
//
// 续费只延长有效期：总额池是一次性的（产品定案 4），到期作废，
// 续期**不清零** total_usage_usd。想「重新发钱」只能改额度或新开一份钱包。
func (s *SubscriptionService) AssignOrExtendSubscription(ctx context.Context, input *AssignSubscriptionInput) (*UserSubscription, bool, error) {
	return s.assignOrExtendSubscription(ctx, input, false)
}

func (s *SubscriptionService) assignOrExtendSubscription(ctx context.Context, input *AssignSubscriptionInput, deferCacheInvalidation bool) (*UserSubscription, bool, error) {
	// 订阅不绑定分组，不做任何分组存废/类型校验（契约 6）。
	existingSub, err := s.findAssignmentTarget(ctx, input.UserID, input.PlanID)
	if err != nil {
		return nil, false, err
	}

	validityDays := normalizeAssignValidityDays(input.ValidityDays)

	// 已有钱包，执行续期（在事务中完成所有更新）
	if existingSub != nil {
		if err := s.updateExistingSubscriptionTerm(ctx, existingSub.ID, validityDays, input.Notes, false); err != nil {
			return nil, false, err
		}
		if err := s.applyAssignedLimit(ctx, existingSub, input); err != nil {
			return nil, false, err
		}

		// 失效订阅缓存
		s.maybeInvalidateAssignmentCaches(input.UserID, deferCacheInvalidation)

		// 返回更新后的订阅
		sub, err := s.userSubRepo.GetByID(ctx, existingSub.ID)
		return sub, true, err // true 表示是续期
	}

	// 没有钱包，创建新订阅
	sub, err := s.createSubscription(ctx, input)
	if err != nil {
		return nil, false, err
	}

	// 失效订阅缓存
	s.maybeInvalidateAssignmentCaches(input.UserID, deferCacheInvalidation)

	return sub, false, nil // false 表示是新建
}

// applyAssignedLimit 把分配入参里的总额度写到钱包上。
//
// 语义：nil = 保持原值不变（兑换码/支付续费只延有效期，不该顺手改钱）；
// 非 nil = 覆盖总额度，其中 <=0 归一为「不限额」。
//
// 必须走 UpdateAssignedLimit（只写额度列），绝不能用整行 Update 代替：
// Update 会绝对写入 total_usage_usd，与并发 IncrementUsage 竞争时，会把期间累加上去的
// 用量静默写回旧值（用量虚低 = 变相提权，是真资损路径）。
// 额度的唯一写入口只有两个：Create（发放时写快照）与 UpdateAssignedLimit（改额度）。
func (s *SubscriptionService) applyAssignedLimit(ctx context.Context, sub *UserSubscription, input *AssignSubscriptionInput) error {
	if input.TotalLimitUSD == nil {
		return nil
	}
	if err := s.userSubRepo.UpdateAssignedLimit(ctx, sub.ID, input.TotalLimitUSD); err != nil {
		return err
	}
	return s.invalidateSubscriptionCaches(sub.UserID)
}

func (s *SubscriptionService) maybeInvalidateAssignmentCaches(userID int64, deferred bool) {
	// Payment fulfillment owns an outer transaction and performs a synchronous
	// invalidation after commit. Invalidating inside that transaction can reload
	// the pre-commit subscription into cache.
	if deferred {
		return
	}

	s.InvalidateSubCache(userID)
	if s.billingCacheService != nil {
		go func() {
			cacheCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.billingCacheService.InvalidateSubscription(cacheCtx, userID)
		}()
	}
}

// updateExistingSubscriptionTerm 延长（或复活）一份钱包的有效期。
//
// assignmentSemantics=true 表示「管理员按分配语义调用」：此时 suspended 的钱包
// 不得被自动唤醒，过期判定也要显式包含 status=expired。
func (s *SubscriptionService) updateExistingSubscriptionTerm(
	ctx context.Context,
	subscriptionID int64,
	validityDays int,
	notes string,
	assignmentSemantics bool,
) error {
	return s.withSubscriptionUpdateTx(ctx, func(txCtx context.Context) error {
		// 行锁内取快照：下面的「整行 Update（复活路径）」会绝对写 total_usage_usd，
		// 只有在持锁状态读到的用量才与库里一致，否则会把并发 IncrementUsage 吞掉。
		existingSub, err := s.userSubRepo.GetByIDForUpdate(txCtx, subscriptionID)
		if err != nil {
			return fmt.Errorf("lock subscription for renewal: %w", err)
		}
		if assignmentSemantics && existingSub.Status == SubscriptionStatusSuspended {
			return nil
		}

		now := s.currentTime()
		isExpired := !existingSub.ExpiresAt.After(now)
		if assignmentSemantics {
			isExpired = existingSub.Status == SubscriptionStatusExpired ||
				(existingSub.Status != SubscriptionStatusSuspended && !existingSub.ExpiresAt.After(now))
		}
		newExpiresAt := existingSub.ExpiresAt.AddDate(0, 0, validityDays)
		if isExpired {
			newExpiresAt = now.AddDate(0, 0, validityDays)
		}
		if newExpiresAt.After(MaxExpiresAt) {
			newExpiresAt = MaxExpiresAt
		}
		if assignmentSemantics && strings.TrimSpace(existingSub.Notes) == strings.TrimSpace(notes) {
			notes = ""
		}

		if isExpired {
			renewed := renewedSubscriptionTerm(existingSub, notes, now, newExpiresAt)
			if err := s.userSubRepo.Update(txCtx, renewed); err != nil {
				return fmt.Errorf("renew expired subscription: %w", err)
			}
			return nil
		}

		// 更新过期时间
		if err := s.userSubRepo.ExtendExpiry(txCtx, existingSub.ID, newExpiresAt); err != nil {
			return fmt.Errorf("extend subscription: %w", err)
		}

		// 如果订阅被暂停，恢复为 active 状态
		if existingSub.Status != SubscriptionStatusActive {
			if err := s.userSubRepo.UpdateStatus(txCtx, existingSub.ID, SubscriptionStatusActive); err != nil {
				return fmt.Errorf("update subscription status: %w", err)
			}
		}

		// 追加备注
		if notes != "" {
			if err := s.userSubRepo.UpdateNotes(txCtx, existingSub.ID, appendSubscriptionNotes(existingSub.Notes, notes)); err != nil {
				return fmt.Errorf("update subscription notes: %w", err)
			}
		}

		return nil
	})
}

func (s *SubscriptionService) withSubscriptionUpdateTx(ctx context.Context, fn func(context.Context) error) error {
	if dbent.TxFromContext(ctx) != nil {
		return fn(ctx)
	}
	if s.entClient == nil {
		return fn(ctx)
	}

	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	txCtx := dbent.NewTxContext(ctx, tx)

	if err := fn(txCtx); err != nil {
		_ = tx.Rollback()
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// renewedSubscriptionTerm 组装「过期钱包被复活」后的整行快照。
//
// 只改 StartsAt/ExpiresAt/Status/Notes 四项：
//   - total_usage_usd **原样带回**：产品定案 4，总额池一次性，续费只是延长有效期，
//     不清零用量（清零等于凭空再发一笔钱）；因为整行 Update 会绝对写这一列，
//     这里的值来自 GetByIDForUpdate 的行锁快照，与并发 IncrementUsage 串行化，不会丢；
//   - total_limit_usd 故意不在这条路径上改写（仓储层 Update 也不写额度列），
//     额度的唯一写入口是 Create / UpdateAssignedLimit，否则事务开始时的旧额度快照
//     会覆盖掉管理员刚提交的新额度。
func renewedSubscriptionTerm(existingSub *UserSubscription, notes string, startsAt, expiresAt time.Time) *UserSubscription {
	renewed := *existingSub
	renewed.StartsAt = startsAt
	renewed.ExpiresAt = expiresAt
	renewed.Status = SubscriptionStatusActive
	renewed.Notes = appendSubscriptionNotes(existingSub.Notes, notes)
	return &renewed
}

func appendSubscriptionNotes(existingNotes, newNotes string) string {
	if newNotes == "" {
		return existingNotes
	}
	if existingNotes == "" {
		return newNotes
	}
	return existingNotes + "\n" + newNotes
}

// createSubscription 创建新钱包（内部方法）
func (s *SubscriptionService) createSubscription(ctx context.Context, input *AssignSubscriptionInput) (*UserSubscription, error) {
	validityDays := normalizeAssignValidityDays(input.ValidityDays)

	now := s.currentTime()
	expiresAt := now.AddDate(0, 0, validityDays)
	if expiresAt.After(MaxExpiresAt) {
		expiresAt = MaxExpiresAt
	}

	sub := &UserSubscription{
		UserID:     input.UserID,
		PlanID:     input.PlanID,
		StartsAt:   now,
		ExpiresAt:  expiresAt,
		Status:     SubscriptionStatusActive,
		AssignedAt: now,
		Notes:      input.Notes,
		CreatedAt:  now,
		UpdatedAt:  now,
		// 额度归一化：<=0 与 nil 同义（不限额），避免库里落 0 后管理端展示“$0”误导。
		// 套餐额度在这里落快照：之后改 subscription_plans.total_limit_usd 不回溯。
		TotalLimitUSD: normalizeSubLimit(input.TotalLimitUSD),
	}
	// 只有当 AssignedBy > 0 时才设置（0 表示系统分配，如兑换码）
	if input.AssignedBy > 0 {
		sub.AssignedBy = &input.AssignedBy
	}

	if err := s.userSubRepo.Create(ctx, sub); err != nil {
		return nil, err
	}

	// 重新获取完整订阅信息（包含关联）
	return s.userSubRepo.GetByID(ctx, sub.ID)
}

// BulkAssignSubscriptionInput 批量分配订阅输入
type BulkAssignSubscriptionInput struct {
	UserIDs      []int64
	PlanID       *int64
	ValidityDays int
	AssignedBy   int64
	Notes        string

	// TotalLimitUSD 语义同 AssignSubscriptionInput：
	// nil = 不动已有额度；非 nil = 覆盖总额度（<=0 归一为不限额）。
	TotalLimitUSD *float64
}

// BulkAssignResult 批量分配结果
type BulkAssignResult struct {
	SuccessCount  int
	CreatedCount  int
	ReusedCount   int
	FailedCount   int
	Subscriptions []UserSubscription
	Errors        []string
	Statuses      map[int64]string
}

// BulkAssignSubscription 批量分配订阅
func (s *SubscriptionService) BulkAssignSubscription(ctx context.Context, input *BulkAssignSubscriptionInput) (*BulkAssignResult, error) {
	result := &BulkAssignResult{
		Subscriptions: make([]UserSubscription, 0),
		Errors:        make([]string, 0),
		Statuses:      make(map[int64]string),
	}

	for _, userID := range input.UserIDs {
		sub, reused, err := s.assignSubscriptionWithReuse(ctx, &AssignSubscriptionInput{
			UserID:        userID,
			PlanID:        input.PlanID,
			ValidityDays:  input.ValidityDays,
			AssignedBy:    input.AssignedBy,
			Notes:         input.Notes,
			TotalLimitUSD: input.TotalLimitUSD,
		})
		if err != nil {
			result.FailedCount++
			result.Errors = append(result.Errors, fmt.Sprintf("user %d: %v", userID, err))
			result.Statuses[userID] = "failed"
		} else {
			result.SuccessCount++
			result.Subscriptions = append(result.Subscriptions, *sub)
			if reused {
				result.ReusedCount++
				result.Statuses[userID] = "reused"
			} else {
				result.CreatedCount++
				result.Statuses[userID] = "created"
			}
		}
	}

	return result, nil
}

func (s *SubscriptionService) assignSubscriptionWithReuse(ctx context.Context, input *AssignSubscriptionInput) (*UserSubscription, bool, error) {
	// 订阅不绑定分组：不再有 groupRepo.GetByID + IsSubscriptionType 校验分支，
	// 「订阅制分组」不再是订阅槽位依据（契约 2）。

	// 检查是否已存在钱包；若已存在，则按幂等成功返回现有钱包
	existingSub, err := s.findAssignmentTarget(ctx, input.UserID, input.PlanID)
	if err != nil {
		return nil, false, err
	}
	if existingSub != nil {
		now := s.currentTime()
		if isRenewableUnderAssignment(existingSub, now) {
			validityDays := normalizeAssignValidityDays(input.ValidityDays)
			if err := s.updateExistingSubscriptionTerm(ctx, existingSub.ID, validityDays, input.Notes, true); err != nil {
				return nil, false, err
			}
			// 续期同时带上额度：一并写回（否则“调额度+续期”会默默只续期）。
			if err := s.applyAssignedLimit(ctx, existingSub, input); err != nil {
				return nil, false, err
			}
			s.maybeInvalidateAssignmentCaches(input.UserID, false)
			renewed, getErr := s.userSubRepo.GetByID(ctx, existingSub.ID)
			return renewed, true, getErr
		}
		if conflictReason, conflict := detectAssignSemanticConflict(existingSub, input); conflict {
			return nil, false, ErrSubscriptionAssignConflict.WithMetadata(map[string]string{
				"conflict_reason": conflictReason,
			})
		}
		// 已存在且仍有效：若本次带了额度则更新额度（“改额度”无需重建钱包）。
		if err := s.applyAssignedLimit(ctx, existingSub, input); err != nil {
			return nil, false, err
		}
		return existingSub, true, nil
	}

	sub, err := s.createSubscription(ctx, input)
	if err != nil {
		return nil, false, err
	}

	// 失效订阅缓存：新钱包必须立刻推翻「该用户无订阅」的负哨兵。
	s.maybeInvalidateAssignmentCaches(input.UserID, false)

	return sub, false, nil
}

// findAssignmentTarget 解析本次分配应当落到哪一份既有钱包上（找不到则返回 nil = 新建）。
//
// 幂等坐标（2026-10-03 定案）：
//   - PlanID 非空：按 (user, plan) 寻址，取该套餐最近一次发放的那份（含已过期/已暂停，
//     由调用方决定「复活续期」还是「幂等返回」）。同一套餐重复购买由支付链路新开钱包，
//     不经过这里。
//   - PlanID 为空（管理员手工发放）：按「用户已有生效钱包」寻址，多份生效钱包里取
//     到期最晚的一份作为主钱包；没有任何生效钱包时视为新发放。
//
// ListByUserID 已排除软删除行，所以「已撤销」的钱包不会被当作分配目标。
func (s *SubscriptionService) findAssignmentTarget(ctx context.Context, userID int64, planID *int64) (*UserSubscription, error) {
	subs, err := s.userSubRepo.ListByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}
	now := s.currentTime()

	var target *UserSubscription
	for i := range subs {
		sub := &subs[i]
		if planID != nil {
			if sub.PlanID == nil || *sub.PlanID != *planID {
				continue
			}
			if target == nil || sub.CreatedAt.After(target.CreatedAt) {
				target = sub
			}
			continue
		}
		if sub.Status != SubscriptionStatusActive || !sub.ExpiresAt.After(now) {
			continue
		}
		if target == nil || sub.ExpiresAt.After(target.ExpiresAt) {
			target = sub
		}
	}
	return target, nil
}

// isRenewableUnderAssignment 判定按分配语义看这份钱包是否已经失效、需要重新发放周期。
// 口径沿用原实现：status=expired，或仍是 active 但时间上已到期；
// suspended（人工停掉）不算，避免一次普通分配把被暂停的钱包自动唤醒。
func isRenewableUnderAssignment(sub *UserSubscription, now time.Time) bool {
	return sub.Status == SubscriptionStatusExpired ||
		(sub.Status != SubscriptionStatusSuspended && !sub.ExpiresAt.After(now))
}

// hasAssignedLimits 本次请求是否携带额度（即管理员的「调额度」意图）。
func hasAssignedLimits(input *AssignSubscriptionInput) bool {
	return input != nil && input.TotalLimitUSD != nil
}

// detectAssignSemanticConflict 判定「重复分配」是否与现有钱包语义冲突。
//
// 重要不变式（勿删）：带额度的请求视为「调额度」，必须直接放行。本函数只在
// 「钱包仍有效 + 不重建」的分支里被调用，那条分支既不续期也不写备注，
// 却拿 validity_days / notes 做幂等冲突判定，会直接挡死常规运维：
//   - 任何被续期过的钱包（ExpiresAt != StartsAt+N）改不动额度；
//   - 任何带备注的钱包（兑换码/支付订单都会写备注）同样改不动。
//
// 不带额度的请求（纯幂等重复分配）仍保留严格判定。
// 钱包化后，比较字段从「日/周/月三档窗口」收敛为「总额度」一个口径：
// 是否带额度看 input.TotalLimitUSD，不再看任何窗口字段。
func detectAssignSemanticConflict(existing *UserSubscription, input *AssignSubscriptionInput) (string, bool) {
	if existing == nil || input == nil {
		return "", false
	}

	// 带额度的请求视为「调额度」：见上方不变式，直接判定为无冲突。
	if hasAssignedLimits(input) {
		return "", false
	}

	normalizedDays := normalizeAssignValidityDays(input.ValidityDays)
	if !existing.StartsAt.IsZero() {
		expectedExpiresAt := existing.StartsAt.AddDate(0, 0, normalizedDays)
		if expectedExpiresAt.After(MaxExpiresAt) {
			expectedExpiresAt = MaxExpiresAt
		}
		if !existing.ExpiresAt.Equal(expectedExpiresAt) {
			return "validity_days_mismatch", true
		}
	}

	existingNotes := strings.TrimSpace(existing.Notes)
	inputNotes := strings.TrimSpace(input.Notes)
	if existingNotes != inputNotes {
		return "notes_mismatch", true
	}

	return "", false
}

func normalizeAssignValidityDays(days int) int {
	if days <= 0 {
		days = 30
	}
	if days > MaxValidityDays {
		days = MaxValidityDays
	}
	return days
}

// RevokeSubscription 撤销订阅
func (s *SubscriptionService) RevokeSubscription(ctx context.Context, subscriptionID int64) error {
	// 先获取订阅信息用于失效缓存
	sub, err := s.userSubRepo.GetByID(ctx, subscriptionID)
	if err != nil {
		return err
	}

	if err := s.userSubRepo.Delete(ctx, subscriptionID); err != nil {
		return err
	}

	if err := s.invalidateSubscriptionCaches(sub.UserID); err != nil {
		return err
	}

	return nil
}

// RestoreSubscription 恢复已撤销订阅
func (s *SubscriptionService) RestoreSubscription(ctx context.Context, subscriptionID int64) (*UserSubscription, error) {
	sub, err := s.userSubRepo.GetByIDIncludeDeleted(ctx, subscriptionID)
	if err != nil {
		return nil, err
	}
	if sub.DeletedAt == nil {
		return nil, ErrSubscriptionNotRevoked
	}

	// 保守闸门：用户此刻已持有另一份生效钱包时不复活。
	// 一个已被撤销的钱包等于「已作废的一次性总额度」，让它重新进入消耗序列
	// 会把撤销时免除掉的那笔钱再放出来（资损方向）；需要重新发钱应当走分配/改额度，
	// 而不是复活历史钱包。
	exists, err := s.userSubRepo.ExistsActiveByUserID(ctx, sub.UserID)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrSubscriptionRestoreConflict
	}

	restoredStatus := sub.Status
	now := s.currentTime()
	if restoredStatus == SubscriptionStatusActive && !sub.ExpiresAt.After(now) {
		restoredStatus = SubscriptionStatusExpired
	}

	restored, err := s.userSubRepo.Restore(ctx, subscriptionID, restoredStatus)
	if err != nil {
		return nil, err
	}

	if err := s.invalidateSubscriptionCaches(restored.UserID); err != nil {
		return nil, err
	}
	return restored, nil
}

// ExtendSubscription 调整订阅时长（正数延长，负数缩短）
func (s *SubscriptionService) ExtendSubscription(ctx context.Context, subscriptionID int64, days int) (*UserSubscription, error) {
	err := s.withSubscriptionUpdateTx(ctx, func(txCtx context.Context) error {
		// Lock the row before reading its expiry. Without this lock, concurrent
		// adjustments can both calculate from the same stale expiry and lose one
		// of the updates when they write the absolute timestamp.
		sub, err := s.userSubRepo.GetByIDForUpdate(txCtx, subscriptionID)
		if err != nil {
			return ErrSubscriptionNotFound
		}

		// 限制调整天数范围
		if days > MaxValidityDays {
			days = MaxValidityDays
		}
		if days < -MaxValidityDays {
			days = -MaxValidityDays
		}

		now := s.currentTime()
		isExpired := !sub.ExpiresAt.After(now)

		// 如果订阅已过期，不允许负向调整
		if isExpired && days < 0 {
			return infraerrors.BadRequest("CANNOT_SHORTEN_EXPIRED", "cannot shorten an expired subscription")
		}

		// 计算新的过期时间
		var newExpiresAt time.Time
		if isExpired {
			// 已过期：从当前时间开始增加天数
			newExpiresAt = now.AddDate(0, 0, days)
		} else {
			// 未过期：从原过期时间增加/减少天数
			newExpiresAt = sub.ExpiresAt.AddDate(0, 0, days)
		}

		if newExpiresAt.After(MaxExpiresAt) {
			newExpiresAt = MaxExpiresAt
		}

		// 检查新的过期时间必须大于当前时间
		if !newExpiresAt.After(now) {
			return ErrAdjustWouldExpire
		}

		if err := s.userSubRepo.ExtendExpiry(txCtx, subscriptionID, newExpiresAt); err != nil {
			return err
		}

		// 如果订阅已过期，恢复为active状态
		if sub.Status == SubscriptionStatusExpired {
			if err := s.userSubRepo.UpdateStatus(txCtx, subscriptionID, SubscriptionStatusActive); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sub, err := s.userSubRepo.GetByID(ctx, subscriptionID)
	if err != nil {
		return nil, err
	}

	// 失效订阅缓存（到期时刻变了，「先到期先消耗」的顺序也必须重算）
	s.InvalidateSubCache(sub.UserID)
	if s.billingCacheService != nil {
		userID := sub.UserID
		go func() {
			cacheCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.billingCacheService.InvalidateSubscription(cacheCtx, userID)
		}()
	}

	return sub, nil
}

// GetByID 根据ID获取订阅
func (s *SubscriptionService) GetByID(ctx context.Context, id int64) (*UserSubscription, error) {
	return s.userSubRepo.GetByID(ctx, id)
}

// GetActiveSubscriptions 获取用户全部生效订阅（钱包列表）。
// 使用 L1 缓存 + singleflight 加速中间件热路径。
// 返回缓存对象的浅拷贝，调用方可安全修改字段而不会污染缓存或触发 data race。
//
// 缓存值设计（关键，勿改）：
//   - key 只有一个坐标 "sub:<userID>"，value 是 []*UserSubscription，
//     顺序沿用仓储层的 expires_at 升序 = 「先到期先消耗」顺序，调用方不必再排序；
//   - 命中时逐个 *UserSubscription 解引用后复制（cp := *sub），因为下游会就地改
//     Status / TotalUsageUSD 之类的标量字段（见本文件的 normalizeSubscriptionStatus
//     一类内存修正，以及计费侧的预扣演算）。直接把缓存指针交出去 = 并发改缓存对象，
//     既有 data race，也会让一次临时修正永久污染该用户的缓存；
//     注意：这是浅拷贝，指针字段（TotalLimitUSD/PlanID/Plan/User）仍然共享，
//     调用方不得透过它们写入；需要改额度请改自己新建的值。
//   - 无生效订阅写 subCacheNegative 负哨兵（短 TTL）：订阅模式判定对每个网关请求
//     都要问一次，未持有钱包的用户不该每次打 DB。
//
// 无生效订阅时返回 ErrSubscriptionNotFound（与旧 GetActiveSubscription 口径一致）。
func (s *SubscriptionService) GetActiveSubscriptions(ctx context.Context, userID int64) ([]*UserSubscription, error) {
	key := subCacheKey(userID)

	if s.subCacheL1 != nil {
		if v, ok := s.subCacheL1.Get(key); ok {
			if _, neg := v.(*subCacheNegative); neg {
				// 负哨兵：该用户当前确无生效订阅（仅因本用户 miss 而写入）。
				return nil, ErrSubscriptionNotFound
			}
			if subs, ok := v.([]*UserSubscription); ok {
				if len(subs) == 0 {
					return nil, ErrSubscriptionNotFound
				}
				return copySubscriptionSlice(subs), nil
			}
		}
	}

	// singleflight 防止并发击穿
	value, err, _ := s.subCacheGroup.Do(key, func() (any, error) {
		subs, err := s.userSubRepo.ListActiveByUserID(ctx, userID)
		if err != nil {
			// 只对「确实没有订阅」做负缓存；DB/上下文故障不能缓存，
			// 否则一次抖动会把用户锁在「无钱包」状态里最长一个 TTL。
			if s.subCacheL1 != nil && errors.Is(err, ErrSubscriptionNotFound) {
				_ = s.subCacheL1.SetWithTTL(key, &subCacheNegative{}, 1, s.subNegativeTTL())
			}
			return nil, err // 直接透传 repo 已翻译的错误（其他错误原样返回）
		}
		if len(subs) == 0 {
			// 空结果同样写短 TTL 负哨兵：仓储层「查不到」返回空切片而不是错误。
			if s.subCacheL1 != nil {
				_ = s.subCacheL1.SetWithTTL(key, &subCacheNegative{}, 1, s.subNegativeTTL())
			}
			return nil, ErrSubscriptionNotFound
		}

		// 仓储返回的是值切片，这里逐元素取出稳定指针后整体入缓存，
		// 缓存里只存这一份规范副本，所有对外返回值都走浅拷贝。
		cached := make([]*UserSubscription, 0, len(subs))
		for i := range subs {
			sub := subs[i]
			cached = append(cached, &sub)
		}
		if s.subCacheL1 != nil {
			ttl := s.jitteredTTL(s.subCacheTTL)
			_ = s.subCacheL1.SetWithTTL(key, cached, 1, ttl)
		}
		return cached, nil
	})
	if err != nil {
		return nil, err
	}
	// singleflight 返回的也是缓存指针，需要浅拷贝
	subs, ok := value.([]*UserSubscription)
	if !ok || len(subs) == 0 {
		return nil, ErrSubscriptionNotFound
	}
	return copySubscriptionSlice(subs), nil
}

// copySubscriptionSlice 返回逐个浅拷贝后的新切片（保持原有顺序 = 消耗顺序）。
func copySubscriptionSlice(cached []*UserSubscription) []*UserSubscription {
	out := make([]*UserSubscription, len(cached))
	for i, sub := range cached {
		cp := *sub
		out[i] = &cp
	}
	return out
}

// ListUserSubscriptions 获取用户的所有订阅
func (s *SubscriptionService) ListUserSubscriptions(ctx context.Context, userID int64) ([]UserSubscription, error) {
	subs, err := s.userSubRepo.ListByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}
	normalizeSubscriptionStatus(subs)
	return subs, nil
}

// ListActiveUserSubscriptions 获取用户的所有有效订阅（按 expires_at 升序 = 消耗顺序）
func (s *SubscriptionService) ListActiveUserSubscriptions(ctx context.Context, userID int64) ([]UserSubscription, error) {
	return s.userSubRepo.ListActiveByUserID(ctx, userID)
}

// List 获取所有订阅（分页，支持按 user / plan / status 筛选和排序）
func (s *SubscriptionService) List(ctx context.Context, page, pageSize int, userID, planID *int64, status, sortBy, sortOrder string) ([]UserSubscription, *pagination.PaginationResult, error) {
	params := pagination.PaginationParams{Page: page, PageSize: pageSize}
	subs, pag, err := s.userSubRepo.List(ctx, params, userID, planID, status, sortBy, sortOrder)
	if err != nil {
		return nil, nil, err
	}
	normalizeSubscriptionStatus(subs)
	return subs, pag, nil
}

// normalizeSubscriptionStatus 根据实际过期时间修正状态（仅影响返回数据，不影响数据库）
// 这确保前端显示正确的状态，即使定时任务尚未更新数据库。
// 钱包模型没有任何滚动窗口需要在这里修正（总额池不重置，用量就是实时累计值）。
func normalizeSubscriptionStatus(subs []UserSubscription) {
	now := time.Now()
	for i := range subs {
		sub := &subs[i]
		if sub.Status == SubscriptionStatusActive && !sub.ExpiresAt.After(now) {
			sub.Status = SubscriptionStatusExpired
		}
	}
}

// startOfDay 返回给定时间所在日期的零点（保持原时区）。
// 订阅逻辑已不再需要窗口对齐；保留是因为仓库内其他代码/测试仍复用这个包级助手。
func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// CheckUsageLimits 检查钱包额度是否还能容纳 additionalCost（返回错误如果超限）
// 用于中间件/记账前的快速预检查，additionalCost 通常为 0。
//
// 不限额钱包恒通过：它不会接管扣费（见 SubscriptionWalletTakesOver），
// 真正的「余额兜底」判定在计费侧，这里不该拦它。
func (s *SubscriptionService) CheckUsageLimits(ctx context.Context, sub *UserSubscription, additionalCost float64) error {
	if sub == nil {
		return ErrSubscriptionNilInput
	}
	if !sub.CheckLimit(additionalCost) {
		return ErrSubscriptionQuotaExhausted
	}
	return nil
}

// AdminResetQuota 管理员手动清零这份钱包的已用额度。
//
// 只清用量、不动额度（额度的写入口仍然只有 Create / UpdateAssignedLimit）。
// ResetUsage 是「绝对写 0」，与并发 IncrementUsage 没有 CAS 保护：重置之后紧接着的
// 请求用量从 0 重新开始。这正是该运维按钮的产品语义（明确免除历史消费），
// 但请知悉：重置与在途请求之间不保证顺序，最坏情况是少记一笔。
func (s *SubscriptionService) AdminResetQuota(ctx context.Context, subscriptionID int64) (*UserSubscription, error) {
	sub, err := s.userSubRepo.GetByID(ctx, subscriptionID)
	if err != nil {
		return nil, err
	}
	if err := s.userSubRepo.ResetUsage(ctx, sub.ID); err != nil {
		return nil, err
	}
	// Invalidate L1 ristretto cache. Ristretto's Del() is asynchronous by design,
	// so call Wait() immediately after to flush pending operations and guarantee
	// the deleted key is not returned on the very next Get() call.
	s.InvalidateSubCacheSync(sub.UserID)
	if s.billingCacheService != nil {
		_ = s.billingCacheService.InvalidateSubscription(ctx, sub.UserID)
	}
	// Return the refreshed subscription from DB
	return s.userSubRepo.GetByID(ctx, subscriptionID)
}

// RecordUsage 记录使用量到订阅（逐行原子累加，不做限额判断）
func (s *SubscriptionService) RecordUsage(ctx context.Context, subscriptionID int64, costUSD float64) error {
	return s.userSubRepo.IncrementUsage(ctx, subscriptionID, costUSD)
}

// AllocateAndRecordUsage 是网关的记账入口：把一笔费用按「先到期先消耗」拆开并逐行落库。
//
// 返回实际写入的分配明细；现有钱包装不下这笔费用时返回 ErrSubscriptionQuotaExhausted
// （调用方据此拒绝请求或回落余额）。costUSD <= 0 视为无需记账，返回空明细 + nil。
//
// 这里刻意**绕过** GetActiveSubscriptions 的 L1 缓存、直接读 DB：分配依据的是各行
// 已committed 的 total_usage_usd，拿缓存里的旧用量来算会把钱拆到一个其实已花完的
// 钱包上（该行超额、另一行有余量却被拒 → 双向错账）。缓存只服务准入预检查。
//
// 逐行累加不包在同一事务里：单行 IncrementUsage 自身原子；若中途失败（例如并发撤销），
// 已写入的部分会随 error 一起返回，便于调用方对账/补偿，避免静默丢钱。
func (s *SubscriptionService) AllocateAndRecordUsage(ctx context.Context, userID int64, costUSD float64) ([]SubscriptionAllocation, error) {
	if costUSD <= 0 {
		return nil, nil
	}

	subs, err := s.userSubRepo.ListActiveByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(subs) == 0 {
		return nil, ErrSubscriptionNotFound
	}

	holders := make([]*UserSubscription, 0, len(subs))
	for i := range subs {
		sub := subs[i]
		holders = append(holders, &sub)
	}

	allocations, ok := AllocateSubscriptionUsage(holders, costUSD)
	if !ok {
		return nil, ErrSubscriptionQuotaExhausted
	}

	written := make([]SubscriptionAllocation, 0, len(allocations))
	for _, alloc := range allocations {
		if err := s.userSubRepo.IncrementUsage(ctx, alloc.SubscriptionID, alloc.Amount); err != nil {
			// 行已不存在/已被撤销：已写的部分如实返回，错误原样上抛。
			if errors.Is(err, ErrSubscriptionNotFound) {
				err = fmt.Errorf("subscription %d disappeared while recording usage: %w", alloc.SubscriptionID, err)
			}
			return written, err
		}
		written = append(written, alloc)
	}

	// 用量变了，该用户的钱包切片 L1 必须作废，否则下一个请求会拿旧用量做拆分依据。
	// 这里故意只走「本地 L1 + 异步失效 Redis 聚合缓存」，不发跨实例 pubsub 广播：
	// 本函数在每个网关请求上都会跑，每请求多一次 Redis publish 不划算；其他实例
	// 依靠短 L1 TTL 自愈，而权威分配始终直读 DB（上面已解释），不会因此错扣。
	s.maybeInvalidateAssignmentCaches(userID, false)
	return written, nil
}

// SubscriptionProgress 订阅（钱包）进度
type SubscriptionProgress struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	ExpiresAt     time.Time `json:"expires_at"`
	ExpiresInDays int       `json:"expires_in_days"`
	TotalLimitUSD *float64  `json:"total_limit_usd"` // nil = 不限额
	TotalUsageUSD float64   `json:"total_usage_usd"`
	RemainingUSD  *float64  `json:"remaining_usd"`
	Percentage    float64   `json:"percentage"`
	Unlimited     bool      `json:"unlimited"`
}

// GetSubscriptionProgress 获取订阅使用进度（纯总额池，不再查分组）
func (s *SubscriptionService) GetSubscriptionProgress(ctx context.Context, subscriptionID int64) (*SubscriptionProgress, error) {
	sub, err := s.userSubRepo.GetByID(ctx, subscriptionID)
	if err != nil {
		return nil, ErrSubscriptionNotFound
	}
	return s.calculateProgress(sub), nil
}

// calculateProgress 根据已加载的订阅数据计算使用进度（纯内存，无 DB 查询、无分组依赖）。
//
// 展示口径：TotalLimitUSD 为 nil 即「不限额钱包」，RemainingUSD 也为 nil，
// Percentage 保持 0（没有分母，前端按 Unlimited 字段显示「不限额」而不是 100%）。
func (s *SubscriptionService) calculateProgress(sub *UserSubscription) *SubscriptionProgress {
	progress := &SubscriptionProgress{
		ID:            sub.ID,
		Name:          sub.DisplayName(),
		ExpiresAt:     sub.ExpiresAt,
		ExpiresInDays: sub.DaysRemaining(),
		TotalLimitUSD: sub.EffectiveTotalLimit(),
		TotalUsageUSD: sub.TotalUsageUSD,
		RemainingUSD:  sub.RemainingUSD(),
		Unlimited:     sub.IsUnlimited(),
	}
	if limit := progress.TotalLimitUSD; limit != nil && *limit > 0 {
		percentage := sub.TotalUsageUSD / *limit * 100
		if percentage > 100 {
			percentage = 100
		}
		if percentage < 0 {
			percentage = 0
		}
		progress.Percentage = percentage
	}
	return progress
}

// GetUserSubscriptionsWithProgress 获取用户所有订阅及进度（按消耗顺序返回）
func (s *SubscriptionService) GetUserSubscriptionsWithProgress(ctx context.Context, userID int64) ([]SubscriptionProgress, error) {
	// ListActiveByUserID 已 eager-load 套餐边（Plan），展示名直接取套餐名；
	// 订阅不绑定分组，这里不需要任何分组查询或「分组边缺失则跳过」的保守分支。
	subs, err := s.userSubRepo.ListActiveByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}

	progresses := make([]SubscriptionProgress, 0, len(subs))
	for i := range subs {
		sub := &subs[i]
		progresses = append(progresses, *s.calculateProgress(sub))
	}

	return progresses, nil
}

// ValidateSubscription 验证订阅是否有效
func (s *SubscriptionService) ValidateSubscription(ctx context.Context, sub *UserSubscription) error {
	if sub.Status == SubscriptionStatusExpired {
		return ErrSubscriptionExpired
	}
	if sub.Status == SubscriptionStatusSuspended {
		return ErrSubscriptionSuspended
	}
	if sub.IsExpired() {
		// 更新状态
		_ = s.userSubRepo.UpdateStatus(ctx, sub.ID, SubscriptionStatusExpired)
		return ErrSubscriptionExpired
	}
	return nil
}
