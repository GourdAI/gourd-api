package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

// UserSubscriptionRepository 是「个人额度钱包」的持久化契约。
//
// 2026-10-03 重构：订阅不再绑定分组，全部按 user_id 寻址。原先以
// (user_id, group_id) 为槽位键的方法（GetActiveByUserIDAndGroupID 等）
// 以及按分组统计/删除的方法（CountByGroupID / DeleteByGroupID 等）已废弃。
type UserSubscriptionRepository interface {
	Create(ctx context.Context, sub *UserSubscription) error
	GetByID(ctx context.Context, id int64) (*UserSubscription, error)
	GetByIDForUpdate(ctx context.Context, id int64) (*UserSubscription, error)
	GetByIDIncludeDeleted(ctx context.Context, id int64) (*UserSubscription, error)
	Update(ctx context.Context, sub *UserSubscription) error
	Delete(ctx context.Context, id int64) error
	Restore(ctx context.Context, subscriptionID int64, restoredStatus string) (*UserSubscription, error)

	ListByUserID(ctx context.Context, userID int64) ([]UserSubscription, error)
	// ListActiveByUserID 返回用户全部生效订阅，按 expires_at 升序，
	// 即「先到期先消耗」的默认消耗顺序。
	ListActiveByUserID(ctx context.Context, userID int64) ([]UserSubscription, error)
	List(ctx context.Context, params pagination.PaginationParams, userID, planID *int64, status, sortBy, sortOrder string) ([]UserSubscription, *pagination.PaginationResult, error)

	// ExistsActiveByUserID 报告用户是否持有任一生效订阅。
	// 「生效」口径与 ListActiveByUserID 一致：status=active 且未过期。
	// 软删除行由 SoftDeleteMixin 拦截器自动排除；这里不能退化成裸 Exist，
	// 否则过期/已撤销的订阅会被读成命中。
	ExistsActiveByUserID(ctx context.Context, userID int64) (bool, error)

	// UpdateAssignedLimit 仅更新总额度列（nil = 保持原值不变；非 nil 且 <=0 = 改为不限额），
	// 不触碰用量/状态列，避免整行覆盖吞掉并发 IncrementUsage 写入的用量。
	UpdateAssignedLimit(ctx context.Context, id int64, total *float64) error
	ExtendExpiry(ctx context.Context, subscriptionID int64, newExpiresAt time.Time) error
	UpdateStatus(ctx context.Context, subscriptionID int64, status string) error
	UpdateNotes(ctx context.Context, subscriptionID int64, notes string) error

	// ResetUsage 手动清零该订阅的已用额度（不动窗口起点）。
	ResetUsage(ctx context.Context, id int64) error
	// IncrementUsage 原子累加已用额度。
	IncrementUsage(ctx context.Context, id int64, costUSD float64) error

	// ResetUsageWindowIfExpired 把「已跨窗口边界」的订阅用量归零，并把窗口起点
	// 推到 newWindowStart。
	//
	// 必须做成条件更新（WHERE usage_window_start < newWindowStart），不能先读再写：
	// 读-改-存在两个并发请求间会把同一窗口的用量清零两次，也可能把刚累加的
	// 一笔抹掉。条件写在 WHERE 里，只有真正跨界的那个写入者 affected=1。
	// 返回 true 表示本次实际发生了重置。
	ResetUsageWindowIfExpired(ctx context.Context, id int64, newWindowStart time.Time) (bool, error)

	// BatchResetExpiredUsageWindows 批量归零「配了重置周期且已跨窗口」的生效订阅，
	// 供后台兑底任务调用。返回实际重置行数。
	// 正确性不依赖本方法：读路径已做懒归一，这里只是避免库里用量长期虚高。
	BatchResetExpiredUsageWindows(ctx context.Context, now time.Time) (int64, error)

	BatchUpdateExpiredStatus(ctx context.Context) (int64, error)
}
