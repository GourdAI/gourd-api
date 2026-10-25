package repository

import (
	"context"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/ent/subscriptionplan"
	"github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/ent/usersubscription"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type userSubscriptionRepository struct {
	client *dbent.Client
}

func NewUserSubscriptionRepository(client *dbent.Client) service.UserSubscriptionRepository {
	return &userSubscriptionRepository{client: client}
}

func (r *userSubscriptionRepository) Create(ctx context.Context, sub *service.UserSubscription) error {
	if sub == nil {
		return service.ErrSubscriptionNilInput
	}

	client := clientFromContext(ctx, r.client)
	builder := client.UserSubscription.Create().
		SetUserID(sub.UserID).
		SetNillablePlanID(sub.PlanID).
		SetExpiresAt(sub.ExpiresAt).
		SetTotalUsageUsd(sub.TotalUsageUSD).
		SetNillableTotalLimitUsd(sub.TotalLimitUSD).
		SetNillableAssignedBy(sub.AssignedBy)

	// 重置周期快照：归一后写入（空串/未知值 → none，等于一次性总额池）。
	// 窗口起点只在配了周期时初始化，none 周期永远保持 NULL，
	// 避免看库时误以为「有窗口但从不重置」。
	resetPeriod := service.NormalizeSubscriptionResetPeriod(sub.ResetPeriod)
	builder.SetResetPeriod(resetPeriod)
	if service.IsRecurringSubscriptionResetPeriod(resetPeriod) {
		windowStart := sub.UsageWindowStart
		if windowStart.IsZero() {
			startsAt := sub.StartsAt
			if startsAt.IsZero() {
				startsAt = time.Now()
			}
			windowStart = service.SubscriptionWindowStart(resetPeriod, startsAt)
		}
		builder.SetUsageWindowStart(windowStart)
	}

	if sub.StartsAt.IsZero() {
		builder.SetStartsAt(time.Now())
	} else {
		builder.SetStartsAt(sub.StartsAt)
	}
	if sub.Status != "" {
		builder.SetStatus(sub.Status)
	}
	if !sub.AssignedAt.IsZero() {
		builder.SetAssignedAt(sub.AssignedAt)
	}
	// Keep compatibility with historical behavior: always store notes as a string value.
	builder.SetNotes(sub.Notes)

	created, err := builder.Save(ctx)
	if err == nil {
		applyUserSubscriptionEntityToService(sub, created)
	}
	return translatePersistenceError(err, nil, service.ErrSubscriptionAlreadyExists)
}

func (r *userSubscriptionRepository) GetByID(ctx context.Context, id int64) (*service.UserSubscription, error) {
	client := clientFromContext(ctx, r.client)
	m, err := client.UserSubscription.Query().
		Where(usersubscription.IDEQ(id)).
		WithUser().
		WithPlan().
		WithAssignedByUser().
		Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrSubscriptionNotFound, nil)
	}
	return userSubscriptionEntityToService(m), nil
}

func (r *userSubscriptionRepository) GetByIDForUpdate(ctx context.Context, id int64) (*service.UserSubscription, error) {
	client := clientFromContext(ctx, r.client)
	m, err := client.UserSubscription.Query().
		Where(usersubscription.IDEQ(id)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrSubscriptionNotFound, nil)
	}
	return userSubscriptionEntityToService(m), nil
}

func (r *userSubscriptionRepository) GetByIDIncludeDeleted(ctx context.Context, id int64) (*service.UserSubscription, error) {
	client := clientFromContext(ctx, r.client)
	queryCtx := mixins.SkipSoftDelete(ctx)
	m, err := client.UserSubscription.Query().
		Where(usersubscription.IDEQ(id)).
		WithUser().
		WithPlan().
		WithAssignedByUser().
		Only(queryCtx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrSubscriptionNotFound, nil)
	}
	return userSubscriptionEntityToServicePreserveStatus(m), nil
}

func (r *userSubscriptionRepository) Update(ctx context.Context, sub *service.UserSubscription) error {
	if sub == nil {
		return service.ErrSubscriptionNilInput
	}

	// 使用约束：本方法会绝对写入 total_usage_usd（见下方 SetTotalUsageUsd），调用方必须
	// 在同一事务内先 GetByIDForUpdate 取行锁，否则会与计费侧的原子累加竞争、
	// 把期间产生的用量写回旧值（等于变相提权）。仅适用于「整行换周期」的续期场景。
	client := clientFromContext(ctx, r.client)
	builder := client.UserSubscription.UpdateOneID(sub.ID).
		SetUserID(sub.UserID).
		SetNillablePlanID(sub.PlanID).
		SetStartsAt(sub.StartsAt).
		SetExpiresAt(sub.ExpiresAt).
		SetStatus(sub.Status).
		SetTotalUsageUsd(sub.TotalUsageUSD).
		SetNillableAssignedBy(sub.AssignedBy).
		SetAssignedAt(sub.AssignedAt).
		SetNotes(sub.Notes)

	// 额度列故意不在这里写：唯一的生产调用方是「过期订阅续期」（见
	// SubscriptionService.updateExistingSubscriptionTerm），续期不应改变额度；
	// 而它拿到的 sub 是事务开始时的快照，整行写回会吞掉期间已提交的
	// UpdateAssignedLimit 变更（管理员改额度被旧值覆盖）。
	// 额度的唯一写入口是 UpdateAssignedLimit（以及 Create）。
	updated, err := builder.Save(ctx)
	if err == nil {
		applyUserSubscriptionEntityToService(sub, updated)
		return nil
	}
	return translatePersistenceError(err, service.ErrSubscriptionNotFound, service.ErrSubscriptionAlreadyExists)
}

func (r *userSubscriptionRepository) Delete(ctx context.Context, id int64) error {
	// Match GORM semantics: deleting a missing row is not an error.
	client := clientFromContext(ctx, r.client)
	_, err := client.UserSubscription.Delete().Where(usersubscription.IDEQ(id)).Exec(ctx)
	return err
}

func (r *userSubscriptionRepository) Restore(ctx context.Context, subscriptionID int64, restoredStatus string) (*service.UserSubscription, error) {
	client := clientFromContext(ctx, r.client)
	queryCtx := mixins.SkipSoftDelete(ctx)
	_, err := client.UserSubscription.UpdateOneID(subscriptionID).
		SetStatus(restoredStatus).
		ClearDeletedAt().
		SetUpdatedAt(time.Now()).
		Save(queryCtx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrSubscriptionNotFound, service.ErrSubscriptionRestoreConflict)
	}
	return r.GetByID(ctx, subscriptionID)
}

func (r *userSubscriptionRepository) ListByUserID(ctx context.Context, userID int64) ([]service.UserSubscription, error) {
	client := clientFromContext(ctx, r.client)
	subs, err := client.UserSubscription.Query().
		Where(usersubscription.UserIDEQ(userID)).
		WithPlan().
		Order(dbent.Desc(usersubscription.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return userSubscriptionEntitiesToService(subs), nil
}

// ListActiveByUserID 按到期时间升序返回生效订阅，即「先到期先消耗」的顺序。
func (r *userSubscriptionRepository) ListActiveByUserID(ctx context.Context, userID int64) ([]service.UserSubscription, error) {
	client := clientFromContext(ctx, r.client)
	subs, err := client.UserSubscription.Query().
		Where(
			usersubscription.UserIDEQ(userID),
			usersubscription.StatusEQ(service.SubscriptionStatusActive),
			usersubscription.ExpiresAtGT(time.Now()),
		).
		WithPlan().
		Order(dbent.Asc(usersubscription.FieldExpiresAt)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return userSubscriptionEntitiesToService(subs), nil
}

func (r *userSubscriptionRepository) ExistsActiveByUserID(ctx context.Context, userID int64) (bool, error) {
	client := clientFromContext(ctx, r.client)
	return client.UserSubscription.Query().
		Where(
			usersubscription.UserIDEQ(userID),
			usersubscription.StatusEQ(service.SubscriptionStatusActive),
			usersubscription.ExpiresAtGT(time.Now()),
		).
		Exist(ctx)
}

// UpdateAssignedLimit 仅更新总额度列，不触碰用量/状态列。
// 语义：nil = 保持原值不变；非 nil 且 >0 = 设为该值；非 nil 且 <=0 = 改为不限额。
// 之所以不用「读整行-改-写整行」：Update 会整行覆盖，与并发 IncrementUsage 竞争时
// 会把期间累加上去的用量静默写回旧值（用户额度显示虚低 = 变相提权）。
func (r *userSubscriptionRepository) UpdateAssignedLimit(ctx context.Context, id int64, total *float64) error {
	if total == nil {
		return nil
	}
	client := clientFromContext(ctx, r.client)
	builder := client.UserSubscription.UpdateOneID(id)
	if *total > 0 {
		builder = builder.SetTotalLimitUsd(*total)
	} else {
		builder = builder.ClearTotalLimitUsd()
	}
	_, err := builder.Save(ctx)
	return translatePersistenceError(err, service.ErrSubscriptionNotFound, nil)
}

func (r *userSubscriptionRepository) ExtendExpiry(ctx context.Context, subscriptionID int64, newExpiresAt time.Time) error {
	client := clientFromContext(ctx, r.client)
	_, err := client.UserSubscription.UpdateOneID(subscriptionID).
		SetExpiresAt(newExpiresAt).
		Save(ctx)
	return translatePersistenceError(err, service.ErrSubscriptionNotFound, nil)
}

func (r *userSubscriptionRepository) UpdateStatus(ctx context.Context, subscriptionID int64, status string) error {
	client := clientFromContext(ctx, r.client)
	_, err := client.UserSubscription.UpdateOneID(subscriptionID).
		SetStatus(status).
		Save(ctx)
	return translatePersistenceError(err, service.ErrSubscriptionNotFound, nil)
}

func (r *userSubscriptionRepository) UpdateNotes(ctx context.Context, subscriptionID int64, notes string) error {
	client := clientFromContext(ctx, r.client)
	_, err := client.UserSubscription.UpdateOneID(subscriptionID).
		SetNotes(notes).
		Save(ctx)
	return translatePersistenceError(err, service.ErrSubscriptionNotFound, nil)
}

// ResetUsage 清零已用额度（管理员手动「重置用量」）。
func (r *userSubscriptionRepository) ResetUsage(ctx context.Context, id int64) error {
	client := clientFromContext(ctx, r.client)
	_, err := client.UserSubscription.UpdateOneID(id).
		SetTotalUsageUsd(0).
		Save(ctx)
	return translatePersistenceError(err, service.ErrSubscriptionNotFound, nil)
}

// IncrementUsage 原子性地累加订阅用量。
// 限额检查已在请求前由 BillingCacheService.CheckBillingEligibility 完成，
// 此处仅负责记录实际消费，确保消费数据的完整性。
//
// 【为什么先推窗口再累加】配了重置周期的行，窗口边界刚过时直接 += 会把本窗口
// 的用量加到上一天的累计值上（跳窗后就变成「已用 2$」）。先做一次带条件的
// 窗口重置（WHERE usage_window_start < 当前窗口起点），只有真正跨界的调用者
// 命中，然后再累加。不是单条 UPDATE 完成是因为窗口边界需要 Go 侧算（周一/月首）
// 而 PostgreSQL 的 date_trunc 行为无法在本机验证（无 PG 环境），
// 宁可用能单测的参数化写法，也不用未验证的方言表达式。
//
// 边界竞态：并发两个请求同时跳窗时，A 重置后累加、B 可能在 A 重置前已累加 →
// B 那笔随重置被抹掉（用户多得一笔额度）。这是「宁多少发不卡死用户」的取舍，
// 与订阅过期、钱包装不下时的口径一致（不能因边界竞态把用户的合法请求吃掉）。
func (r *userSubscriptionRepository) IncrementUsage(ctx context.Context, id int64, costUSD float64) error {
	if _, err := r.ResetUsageWindowIfExpired(ctx, id, time.Time{}); err != nil {
		return err
	}

	const updateSQL = `
		UPDATE user_subscriptions
		SET
			total_usage_usd = total_usage_usd + $1,
			updated_at = NOW()
		WHERE id = $2
			AND deleted_at IS NULL
	`

	client := clientFromContext(ctx, r.client)
	result, err := client.ExecContext(ctx, updateSQL, costUSD, id)
	if err != nil {
		return err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if affected > 0 {
		return nil
	}

	// affected == 0：订阅不存在或已删除
	return service.ErrSubscriptionNotFound
}

// ResetUsageWindowIfExpired 把已跨窗口边界的订阅用量归零，并把窗口起点推到当前窗口。
//
// newWindowStart 传零值时按 now 自行计算（IncrementUsage 走这条），
// 显式传入则使用给定值（后台批处理按统一 now 扫描，保证同一批次口径一致）。
//
// 条件写在 WHERE 里而不是先读再写：读-改-写会在并发下把同一窗口的用量清零两次，
// 也可能把刚累加的一笔抹掉；条件更新只有真正跨界的那个写入者 affected=1。
//
// 不处理 reset_period='none' 的行（WHERE 直接排除）：一次性总额池永不清零。
func (r *userSubscriptionRepository) ResetUsageWindowIfExpired(ctx context.Context, id int64, newWindowStart time.Time) (bool, error) {
	client := clientFromContext(ctx, r.client)

	// 只取 reset_period 一列（Select + Only），复用 ent 的软删除拦截器：
	// 已删行在这里直接报 NotFound，无需在 SQL 里再判 deleted_at。
	row, err := client.UserSubscription.Query().
		Where(usersubscription.IDEQ(id)).
		Select(usersubscription.FieldResetPeriod).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	period := normalizResetPeriodForWindow(row.ResetPeriod)
	if !service.IsRecurringSubscriptionResetPeriod(period) {
		return false, nil
	}

	now := time.Now().UTC()
	if newWindowStart.IsZero() {
		newWindowStart = service.SubscriptionWindowStart(period, now)
	}
	if newWindowStart.IsZero() {
		return false, nil
	}

	const updateSQL = `
		UPDATE user_subscriptions
		SET
			total_usage_usd = 0,
			usage_window_start = $2,
			updated_at = NOW()
		WHERE id = $1
			AND deleted_at IS NULL
			AND reset_period <> 'none'
			AND (usage_window_start IS NULL OR usage_window_start < $2)
	`
	result, err := client.ExecContext(ctx, updateSQL, id, newWindowStart)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// normalizResetPeriodForWindow 归一周期值（空串/未知值 → none）。
// 单独抽出来是为了在任何读取点都不会因脏值进入非预期的窗口分支。
func normalizResetPeriodForWindow(raw string) string {
	return service.NormalizeSubscriptionResetPeriod(raw)
}

// BatchResetExpiredUsageWindows 批量归零所有「配了重置周期且已跨窗口」的生效订阅。
//
// 正确性不依赖本方法：读路径（EffectiveWindowUsageUSD）已做懒归一，
// 这里只是避免库里的 total_usage_usd 长期停在上一窗口的值（否则后台报表、
// 管理员看到的用量全是虚高）。因此它跑不跑、跑多久不影响业务，
// 不设 leader lock 也不抢锁。
//
// 一次性按 reset_period 分组批量 UPDATE：日/周/月三种窗口各算一个目标起点，
// 避免 「WHERE 里带 now()」这种不可索引也无法参数化的写法。
func (r *userSubscriptionRepository) BatchResetExpiredUsageWindows(ctx context.Context, now time.Time) (int64, error) {
	client := clientFromContext(ctx, r.client)
	total := int64(0)

	periods := []string{
		service.SubscriptionResetPeriodDaily,
		service.SubscriptionResetPeriodWeekly,
		service.SubscriptionResetPeriodMonthly,
	}
	const updateSQL = `
		UPDATE user_subscriptions
		SET
			total_usage_usd = 0,
			usage_window_start = $2,
			updated_at = NOW()
		WHERE deleted_at IS NULL
			AND status = 'active'
			AND reset_period = $1
			AND (usage_window_start IS NULL OR usage_window_start < $2)
	`
	for _, period := range periods {
		windowStart := service.SubscriptionWindowStart(period, now)
		if windowStart.IsZero() {
			continue
		}
		result, err := client.ExecContext(ctx, updateSQL, period, windowStart)
		if err != nil {
			return total, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return total, err
		}
		total += affected
	}
	return total, nil
}

func (r *userSubscriptionRepository) BatchUpdateExpiredStatus(ctx context.Context) (int64, error) {
	client := clientFromContext(ctx, r.client)
	n, err := client.UserSubscription.Update().
		Where(
			usersubscription.StatusEQ(service.SubscriptionStatusActive),
			usersubscription.ExpiresAtLTE(time.Now()),
		).
		SetStatus(service.SubscriptionStatusExpired).
		Save(ctx)
	return int64(n), err
}

func (r *userSubscriptionRepository) List(ctx context.Context, params pagination.PaginationParams, userID, planID *int64, status, sortBy, sortOrder string) ([]service.UserSubscription, *pagination.PaginationResult, error) {
	client := clientFromContext(ctx, r.client)
	q := client.UserSubscription.Query()
	includeSoftDeleted := status == "" || status == service.SubscriptionStatusRevoked
	if userID != nil {
		q = q.Where(usersubscription.UserIDEQ(*userID))
	}
	if planID != nil {
		q = q.Where(usersubscription.PlanIDEQ(*planID))
	}

	// Status filtering with real-time expiration check
	now := time.Now()
	switch status {
	case service.SubscriptionStatusActive:
		// Active: status is active AND not yet expired
		q = q.Where(
			usersubscription.StatusEQ(service.SubscriptionStatusActive),
			usersubscription.ExpiresAtGT(now),
		)
	case service.SubscriptionStatusExpired:
		// Expired: status is expired OR (status is active but already expired)
		q = q.Where(
			usersubscription.Or(
				usersubscription.StatusEQ(service.SubscriptionStatusExpired),
				usersubscription.And(
					usersubscription.StatusEQ(service.SubscriptionStatusActive),
					usersubscription.ExpiresAtLTE(now),
				),
			),
		)
	case service.SubscriptionStatusRevoked:
		// Revoked is a DTO/API display state backed by user_subscriptions.deleted_at.
		q = q.Where(usersubscription.DeletedAtNotNil())
	case "":
		// No filter. Use SkipSoftDelete below so admin "all status" includes revoked history.
	default:
		// Other persisted status.
		q = q.Where(usersubscription.StatusEQ(status))
	}

	queryCtx := ctx
	if includeSoftDeleted {
		queryCtx = mixins.SkipSoftDelete(ctx)
	}

	total, err := q.Clone().Count(queryCtx)
	if err != nil {
		return nil, nil, err
	}

	if !includeSoftDeleted {
		q = q.WithUser().WithPlan().WithAssignedByUser()
	}

	// Determine sort field
	var field string
	switch sortBy {
	case "expires_at":
		field = usersubscription.FieldExpiresAt
	case "status":
		field = usersubscription.FieldStatus
	default:
		field = usersubscription.FieldCreatedAt
	}

	// Determine sort order (default: desc)
	if sortOrder == "asc" && sortBy != "" {
		q = q.Order(dbent.Asc(field))
	} else {
		q = q.Order(dbent.Desc(field))
	}

	subs, err := q.
		Offset(params.Offset()).
		Limit(params.Limit()).
		All(queryCtx)
	if err != nil {
		return nil, nil, err
	}

	result := userSubscriptionEntitiesToService(subs)
	if includeSoftDeleted {
		if err := r.attachUserSubscriptionRelations(ctx, result); err != nil {
			return nil, nil, err
		}
	}

	return result, paginationResultFromTotal(int64(total), params), nil
}

// Extra repository helpers (currently used only by integration tests).

func (r *userSubscriptionRepository) ListExpired(ctx context.Context) ([]service.UserSubscription, error) {
	client := clientFromContext(ctx, r.client)
	subs, err := client.UserSubscription.Query().
		Where(
			usersubscription.StatusEQ(service.SubscriptionStatusActive),
			usersubscription.ExpiresAtLTE(time.Now()),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return userSubscriptionEntitiesToService(subs), nil
}

func (r *userSubscriptionRepository) attachUserSubscriptionRelations(ctx context.Context, subs []service.UserSubscription) error {
	if len(subs) == 0 {
		return nil
	}

	userIDs := make([]int64, 0, len(subs))
	planIDs := make([]int64, 0, len(subs))
	assignedByIDs := make([]int64, 0, len(subs))
	for i := range subs {
		userIDs = append(userIDs, subs[i].UserID)
		if subs[i].PlanID != nil {
			planIDs = append(planIDs, *subs[i].PlanID)
		}
		if subs[i].AssignedBy != nil {
			assignedByIDs = append(assignedByIDs, *subs[i].AssignedBy)
		}
	}

	client := clientFromContext(ctx, r.client)
	users, err := client.User.Query().Where(user.IDIn(uniqueInt64s(userIDs)...)).All(ctx)
	if err != nil {
		return err
	}
	userByID := make(map[int64]*service.User, len(users))
	for _, u := range users {
		userByID[u.ID] = userEntityToService(u)
	}

	planByID := make(map[int64]*service.SubscriptionPlanInfo, len(planIDs))
	if len(planIDs) > 0 {
		plans, err := client.SubscriptionPlan.Query().
			Where(subscriptionplan.IDIn(uniqueInt64s(planIDs)...)).
			All(ctx)
		if err != nil {
			return err
		}
		for _, p := range plans {
			planByID[p.ID] = &service.SubscriptionPlanInfo{ID: p.ID, Name: p.Name}
		}
	}

	assignedByID := map[int64]*service.User{}
	if len(assignedByIDs) > 0 {
		assignedUsers, err := client.User.Query().Where(user.IDIn(uniqueInt64s(assignedByIDs)...)).All(ctx)
		if err != nil {
			return err
		}
		assignedByID = make(map[int64]*service.User, len(assignedUsers))
		for _, u := range assignedUsers {
			assignedByID[u.ID] = userEntityToService(u)
		}
	}

	for i := range subs {
		subs[i].User = userByID[subs[i].UserID]
		if subs[i].PlanID != nil {
			subs[i].Plan = planByID[*subs[i].PlanID]
		}
		if subs[i].AssignedBy != nil {
			subs[i].AssignedByUser = assignedByID[*subs[i].AssignedBy]
		}
	}
	return nil
}

func uniqueInt64s(values []int64) []int64 {
	seen := make(map[int64]struct{}, len(values))
	out := make([]int64, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func userSubscriptionEntityToService(m *dbent.UserSubscription) *service.UserSubscription {
	return userSubscriptionEntityToServiceWithStatusMapping(m, true)
}

func userSubscriptionEntityToServicePreserveStatus(m *dbent.UserSubscription) *service.UserSubscription {
	return userSubscriptionEntityToServiceWithStatusMapping(m, false)
}

func userSubscriptionEntityToServiceWithStatusMapping(m *dbent.UserSubscription, mapDeletedToRevoked bool) *service.UserSubscription {
	if m == nil {
		return nil
	}
	status := m.Status
	if mapDeletedToRevoked && m.DeletedAt != nil {
		status = service.SubscriptionStatusRevoked
	}
	out := &service.UserSubscription{
		ID:            m.ID,
		UserID:        m.UserID,
		PlanID:        m.PlanID,
		StartsAt:      m.StartsAt,
		ExpiresAt:     m.ExpiresAt,
		Status:        status,
		TotalLimitUSD: m.TotalLimitUsd,
		TotalUsageUSD: m.TotalUsageUsd,
		ResetPeriod:   service.NormalizeSubscriptionResetPeriod(m.ResetPeriod),
		AssignedBy:    m.AssignedBy,
		AssignedAt:    m.AssignedAt,
		Notes:         derefString(m.Notes),
		CreatedAt:     m.CreatedAt,
		UpdatedAt:     m.UpdatedAt,
		DeletedAt:     m.DeletedAt,
	}
	if m.UsageWindowStart != nil {
		out.UsageWindowStart = *m.UsageWindowStart
	}
	if m.Edges.User != nil {
		out.User = userEntityToService(m.Edges.User)
	}
	if m.Edges.Plan != nil {
		out.Plan = &service.SubscriptionPlanInfo{ID: m.Edges.Plan.ID, Name: m.Edges.Plan.Name}
	}
	if m.Edges.AssignedByUser != nil {
		out.AssignedByUser = userEntityToService(m.Edges.AssignedByUser)
	}
	return out
}

func userSubscriptionEntitiesToService(models []*dbent.UserSubscription) []service.UserSubscription {
	out := make([]service.UserSubscription, 0, len(models))
	for i := range models {
		if s := userSubscriptionEntityToService(models[i]); s != nil {
			out = append(out, *s)
		}
	}
	return out
}

func applyUserSubscriptionEntityToService(dst *service.UserSubscription, src *dbent.UserSubscription) {
	if dst == nil || src == nil {
		return
	}
	dst.ID = src.ID
	dst.CreatedAt = src.CreatedAt
	dst.UpdatedAt = src.UpdatedAt
}
