//go:build unit

package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 主计费路径（repo.Apply）的订阅拆分断言。
//
// 修复前：cmd.SubscriptionID 只有一行，整笔 ActualCost 记到「最早到期」那份钱包，
// 越界部分随该行过期一起消失 —— 实测 A(10,1天)+B(10,30天)、cost=15 时每人漏 5 USD。
// 修复后：事务内锁定读该用户全部生效钱包，再按「先到期先消耗」逐行累加。

const (
	lockWalletsSQL = `(?s)SELECT id, expires_at, total_limit_usd, total_usage_usd\s+` +
		`FROM user_subscriptions\s+WHERE user_id = \$1\s+AND deleted_at IS NULL\s+` +
		`AND status = 'active'\s+AND expires_at > NOW\(\)\s+ORDER BY expires_at ASC\s+FOR UPDATE`

	incrementSubscriptionSQL = `(?s)UPDATE user_subscriptions\s+SET\s+` +
		`total_usage_usd = total_usage_usd \+ \$1,\s+updated_at = NOW\(\)\s+` +
		`WHERE id = \$2\s+AND deleted_at IS NULL`
)

func walletRows(now time.Time) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "expires_at", "total_limit_usd", "total_usage_usd"}).
		AddRow(int64(20), now.Add(time.Hour), 10.0, 0.0).
		AddRow(int64(21), now.Add(30*24*time.Hour), 10.0, 0.0)
}

func TestApplySubscriptionCost_SplitsAcrossWalletsInExpiryOrder(t *testing.T) {
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)

	now := time.Now()
	mock.ExpectQuery(lockWalletsSQL).WithArgs(int64(7)).
		WillReturnRows(walletRows(now))

	// 关键：必须发出两条 UPDATE（A 记满 10、B 记 5）。
	// 旧实现只会是一条 15 记到 A —— 那条多出来的 5 会随 A 过期蒸发。
	mock.ExpectExec(incrementSubscriptionSQL).WithArgs(10.0, int64(20)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(incrementSubscriptionSQL).WithArgs(5.0, int64(21)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	cmd := &service.UsageBillingCommand{UserID: 7, SubscriptionCost: 15}
	primary := int64(20)
	cmd.SubscriptionID = &primary // 调用方快照里的首选钱包，仅作锁不到行时的兜底

	fallback, err := applySubscriptionCost(ctx, tx, cmd)
	require.NoError(t, err)
	require.Zero(t, fallback, "两份钱包装得下时不应回落余额")
	require.NoError(t, tx.Commit())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestApplySubscriptionCost_LocksRowsForUpdate(t *testing.T) {
	// FOR UPDATE 是拆分正确性的前提：没有行锁，并发请求会各自读到旧用量、
	// 把同一份额度花两遍。这里用 ExpectationsWereMet 强制 SQL 必须含 FOR UPDATE。
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	mock.ExpectQuery(lockWalletsSQL).WithArgs(int64(8)).
		WillReturnRows(walletRows(time.Now()))
	mock.ExpectExec(incrementSubscriptionSQL).WithArgs(3.0, int64(20)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	fallback, err := applySubscriptionCost(ctx, tx,
		&service.UsageBillingCommand{UserID: 8, SubscriptionCost: 3})
	require.NoError(t, err)
	require.Zero(t, fallback)
	require.NoError(t, tx.Commit())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestApplySubscriptionCost_OverrunStillRecordedNotDropped(t *testing.T) {
	// 装不下时整笔记到首选钱包（用量合法越界，后续被预检 429），
	// 而不是丢弃 —— 请求已发生，丢弃等于白送。
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	mock.ExpectQuery(lockWalletsSQL).WithArgs(int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "expires_at", "total_limit_usd", "total_usage_usd"}).
			AddRow(int64(30), time.Now().Add(time.Hour), 1.0, 0.0).
			AddRow(int64(31), time.Now().Add(2*time.Hour), 1.0, 0.0))
	mock.ExpectExec(incrementSubscriptionSQL).WithArgs(100.0, int64(30)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	fallback, err := applySubscriptionCost(ctx, tx,
		&service.UsageBillingCommand{UserID: 9, SubscriptionCost: 100})
	require.NoError(t, err)
	require.Zero(t, fallback, "越界也要记到订阅钱包，不能回落")
	require.NoError(t, tx.Commit())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestApplySubscriptionCost_UnlimitedWalletBackfills(t *testing.T) {
	// 有限额榨干后余量落到不限额钱包（total_limit_usd 为 NULL）。
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	mock.ExpectQuery(lockWalletsSQL).WithArgs(int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "expires_at", "total_limit_usd", "total_usage_usd"}).
			AddRow(int64(40), time.Now().Add(time.Hour), 5.0, 5.0).   // 已榨干
			AddRow(int64(41), time.Now().Add(2*time.Hour), nil, 0.0)) // 不限额
	mock.ExpectExec(incrementSubscriptionSQL).WithArgs(4.0, int64(41)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	fallback, err := applySubscriptionCost(ctx, tx,
		&service.UsageBillingCommand{UserID: 10, SubscriptionCost: 4})
	require.NoError(t, err)
	require.Zero(t, fallback)
	require.NoError(t, tx.Commit())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestApplySubscriptionCost_FallsBackToSnapshotWhenNoRowsLocked(t *testing.T) {
	// 锁不到任何生效行（预检后到期/被撤销）：沿用调用方快照归属，
	// 让「行不存在」如实报错，而不是静默当成记账成功。
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	mock.ExpectQuery(lockWalletsSQL).WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "expires_at", "total_limit_usd", "total_usage_usd"}))
	mock.ExpectExec(incrementSubscriptionSQL).WithArgs(2.0, int64(55)).
		WillReturnError(sql.ErrNoRows)

	primary := int64(55)
	fallback, err := applySubscriptionCost(ctx, tx, &service.UsageBillingCommand{
		UserID: 11, SubscriptionCost: 2, SubscriptionID: &primary,
	})
	require.ErrorIs(t, err, sql.ErrNoRows, "真实 DB 故障必须上抛（该回滚重试），不得静默回落掩盖故障")
	require.Zero(t, fallback)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 【资金口径】锁不到生效行、也没有调用方快照可归时，**不得**再返回
// ErrSubscriptionNotFound：那个 error 会在 tx.Commit() 前上抛，使整个计费事务（含
// balance 扣减与 dedup claim）一并回滚 —— 而上游成本已真实发生、响应已发出，
// 客户端重试还会再花一次。结果是平台分文未得。现改为把全额上报为余额回落。
func TestApplySubscriptionCost_NoRowsNoSnapshotFallsBackToBalance(t *testing.T) {
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	mock.ExpectQuery(lockWalletsSQL).WithArgs(int64(12)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "expires_at", "total_limit_usd", "total_usage_usd"}))

	fallback, applyErr := applySubscriptionCost(ctx, tx,
		&service.UsageBillingCommand{UserID: 12, SubscriptionCost: 2})
	require.NoError(t, applyErr, "锁不到行不得让整事务回滚")
	require.InDelta(t, 2.0, fallback, 1e-9, "全额必须上报为余额回落，不能静默丢失")
	require.NoError(t, mock.ExpectationsWereMet())
}

// 【P0-3 回归】软删竞态：预检（L1 陈旧切片）后该订阅被管理员撤销（写 deleted_at），
// 事务内锁定读返回 0 行，兜底归行的 UPDATE 带 deleted_at IS NULL 守卫→affected=0。
// 旧实现直拒 ErrSubscriptionNotFound，整事务回滚（连带 balance 未扣）= 真实成本白送。
// 新实现必须把这笔钱全量转成余额回落。
func TestApplySubscriptionCost_SoftDeletedFallbackFallsBackToBalance(t *testing.T) {
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	mock.ExpectQuery(lockWalletsSQL).WithArgs(int64(13)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "expires_at", "total_limit_usd", "total_usage_usd"}))
	// 兜底归行：执行成功但 affected=0（软删行被 WHERE 过滤掉）
	mock.ExpectExec(incrementSubscriptionSQL).WithArgs(7.0, int64(77)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	primary := int64(77)
	fallback, applyErr := applySubscriptionCost(ctx, tx, &service.UsageBillingCommand{
		UserID: 13, SubscriptionCost: 7, SubscriptionID: &primary,
	})
	require.NoError(t, applyErr, "软删竞态不得让整事务回滚")
	require.InDelta(t, 7.0, fallback, 1e-9, "写不进钱包的钱必须全量回落余额")
	require.NoError(t, mock.ExpectationsWereMet())
}
