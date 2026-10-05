package repository

import (
	"context"
	"database/sql/driver"
	"regexp"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	sqlmock "github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 模型级限流的落库与快照同步必须**脱离请求生命周期**。
//
// 写冷却的典型时机是上游返回 404/429 之后，而那一刻客户端超时、主动断开或已切走
// 都是常规路径，请求 ctx 往往已经 cancel。跟随请求 ctx 会造成两种丢失：
//  1. UPDATE 本身失败 → 冷却根本没落库；
//  2. UPDATE 成功但随后的 Redis 快照同步失败 → DB 有冷却、调度候选池（读快照）
//     看不见 → 同一个号继续被选中、再撞一遍同样的上游错误
//     （即用户可见的「限流了还被调、反复报 404」）。
//
// 本文件是回归闸门：把 WithoutCancel 改回裸 ctx，这里必须立刻失败。

// 前提自证：已 cancel 的 ctx 走 database/sql 时确实会失败。
// 没有这条断言，下面的用例可能只是「永远绿」的空壳。
func TestPrecondition_CanceledContextFailsRawSQLOperation(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectExec(regexp.QuoteMeta("UPDATE accounts")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, execErr := db.ExecContext(ctx, "UPDATE accounts SET x = 1 WHERE id = $1", int64(1))
	require.Error(t, execErr, "前提：裸请求 ctx 必须会让写入失败，否则本组回归测试毫无意义")
}

func canceledContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, ctx.Err(), "前提：ctx 必须已 cancel")
	return ctx
}

func newModelRateLimitRepo(t *testing.T) (service.AccountRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close(); _ = db.Close() })
	// schedulerCache 传 nil：快照同步入口直接返回，本组用例只钉死「落库」这一段。
	return newAccountRepositoryWithSQL(client, db, nil), mock
}

func TestAccountRepository_SetModelRateLimit_CommittedAfterRequestContextCanceled(t *testing.T) {
	repo, mock := newModelRateLimitRepo(t)

	resetAt := time.Now().Add(30 * time.Minute).UTC()
	mock.ExpectExec(`(?s)`+regexp.QuoteMeta("UPDATE accounts SET")).
		WithArgs("claude-sonnet-4-5", sqlmock.AnyArg(), int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := repo.SetModelRateLimit(canceledContext(t), 42, "claude-sonnet-4-5", resetAt, "upstream_404_model_not_found")

	require.NoError(t, err, "客户端断开不得导致冷却写不进 DB")
	require.NoError(t, mock.ExpectationsWereMet(), "UPDATE 与 outbox 必须都真的执行过")
}

// jsonPayloadContains 是 sqlmock 的自定义参数匹配器：用来断言冷却载荷内容。
// reset_at 是动态值，不能用字面量比对，因此这里只校验关键子串。
type jsonPayloadContains []string

func (m jsonPayloadContains) Match(v driver.Value) bool {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case []byte:
		s = string(t)
	default:
		return false
	}
	for _, want := range m {
		if !strings.Contains(s, want) {
			return false
		}
	}
	return true
}

// 载荷必须带上正确的 reset_at 与 reason，否则读侧 isRateLimitActiveForKey 解析不到。
func TestAccountRepository_SetModelRateLimit_PayloadCarriesResetAtAndReason(t *testing.T) {
	repo, mock := newModelRateLimitRepo(t)

	resetAt := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	mock.ExpectExec(`(?s)`+regexp.QuoteMeta("UPDATE accounts SET")).
		WithArgs("claude-sonnet-4-5", jsonPayloadContains{
			`"rate_limit_reset_at":"2026-10-05T12:00:00Z"`,
			`"reason":"upstream_404_model_not_found"`,
		}, int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WillReturnResult(sqlmock.NewResult(1, 1))

	require.NoError(t, repo.SetModelRateLimit(canceledContext(t), 42, "claude-sonnet-4-5", resetAt, "upstream_404_model_not_found"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountRepository_ClearModelRateLimits_CommittedAfterRequestContextCanceled(t *testing.T) {
	repo, mock := newModelRateLimitRepo(t)

	mock.ExpectExec(`(?s)` + regexp.QuoteMeta("UPDATE accounts SET extra = COALESCE(extra")).
		WithArgs(int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := repo.ClearModelRateLimits(canceledContext(t), 42)

	require.NoError(t, err, "清除冷却同样不得被客户端断开打断，否则留下 DB 已清/快照仍冷却的反向不一致")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountRepository_SetModelRateLimit_AccountMissingStillReturnsNotFound(t *testing.T) {
	repo, mock := newModelRateLimitRepo(t)

	mock.ExpectExec(`(?s)` + regexp.QuoteMeta("UPDATE accounts SET")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// 行不存在（已删除/不存在）时不能被「脱离 ctx」改动吞掉既有错误语义。
	require.ErrorIs(t, repo.SetModelRateLimit(context.Background(), 404, "gpt-5.4", time.Now().Add(time.Minute)), service.ErrAccountNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

// WithoutCancel 只去掉取消信号、**保留 value**。这是本修复的安全前提：
// clientFromContext 依赖 ctx 里的 tx value，若丢失 value，冷却写入会跑到事务外，
// 破坏调用方原子性。
func TestContextOrBackground_KeepsValuesButDropsCancellation(t *testing.T) {
	type txKey struct{}

	parent, cancel := context.WithCancel(context.WithValue(context.Background(), txKey{}, "tx"))
	cancel()

	derived := contextOrBackground(parent)
	require.Error(t, parent.Err(), "前提：parent 已取消")
	require.NoError(t, derived.Err(), "脱离后不得携带取消错误")
	select {
	case <-derived.Done():
		t.Fatal("脱离后的 ctx 不应可取消")
	default:
	}
	require.Equal(t, "tx", derived.Value(txKey{}), "必须保留 value，否则会丢失事务上下文")
}

// ctx 为 nil 时不能返回 nil（context.WithoutCancel(nil) 得到 nil ctx，
// 后续 WithTimeout(nil) 会 panic）。
func TestContextOrBackground_NilFallsBackToBackground(t *testing.T) {
	derived := contextOrBackground(nil)
	require.NotNil(t, derived)
	require.NoError(t, derived.Err())
}

// writeBudget 必须防止两种方向的预算错配：
//  1. contextOrBackground 剥掉父 deadline（Go context.go:597-599 实测 ok=false），
//     若不取 min，调用方已声明的预算会被静默换成默认值；
//  2. 默认值也不能太大（2s 会把「客户端断开」换成「等锁超时」，症状不变）。
func TestWriteBudget(t *testing.T) {
	const fallback = 5 * time.Second

	t.Run("nil ctx 用默认值", func(t *testing.T) {
		require.Equal(t, fallback, writeBudget(nil, fallback))
	})

	t.Run("无 deadline 的 ctx 用默认值", func(t *testing.T) {
		require.Equal(t, fallback, writeBudget(context.Background(), fallback))
	})

	t.Run("调用方预算更长时取默认值而非叠加", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		got := writeBudget(ctx, fallback)
		require.InDelta(t, fallback.Seconds(), got.Seconds(), 0.5,
			"不能把调用方的 60s 当预算：本写入已脱离请求，应有自身上界")
	})

	t.Run("调用方预算更短时必须尊重它", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
		defer cancel()
		got := writeBudget(ctx, fallback)
		require.Less(t, got, fallback, "不得静默抬升调用方给出的短预算")
		require.Greater(t, got, 600*time.Millisecond)
	})

	// 已过期 ctx：remaining<=0 不能算出负预算（WithTimeout(负值) 会立刻到期，
	// 恰好重现「客户端断开→冷却写不进去」）。必须回退到默认值。
	t.Run("已过期 ctx 不得算出负预算", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		time.Sleep(50 * time.Millisecond)
		require.Error(t, ctx.Err(), "前提：ctx 已过期")
		require.Equal(t, fallback, writeBudget(ctx, fallback),
			"过期 ctx 上仍能拿到完整默认预算（配合 WithoutCancel）")
	})
}

// 端到端钉住两个预算常量的关系：权威写入必须宽于快照同步，
// 否则缓存量级的预算会误杀争行锁的落库写（本仓库引入过的回归）。
func TestModelRateLimitWriteBudgetIsWiderThanSnapshotSync(t *testing.T) {
	require.Greater(t, modelRateLimitWriteTimeout, schedulerSnapshotSyncTimeout,
		"冷却 UPDATE 需等账号计费自增释放行锁，预算必须大于快照同步的 2s")
}

// ClearModelRateLimitScopes 必须是**服务端按键路径删除**，而不是把调用方传进来的
// 整个 map 写回（UpdateExtra 的 `extra || $1` 是顶层整体替换，会抹掉并发其刚写入的
// 其它冷却）。本组用例钉住 SQL 形态与参数个数。
func TestAccountRepository_ClearModelRateLimitScopes(t *testing.T) {
	t.Run("多个键拼成一条 UPDATE 且只碰目标键", func(t *testing.T) {
		repo, mock := newModelRateLimitRepo(t)

		// 期望：COALESCE(extra,'{}') #- ARRAY['model_rate_limits',$2] #- ARRAY['model_rate_limits',$3]
		mock.ExpectExec(`(?s)UPDATE accounts SET extra = COALESCE\(extra, '\{\}'::jsonb\)`+
			` #- ARRAY\['model_rate_limits', \$2\]`+
			` #- ARRAY\['model_rate_limits', \$3\]`+
			`, updated_at = NOW\(\) WHERE id = \$1 AND deleted_at IS NULL`).
			WithArgs(int64(42), "AICredits", "claude-sonnet-4-5").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
			WillReturnResult(sqlmock.NewResult(1, 1))

		err := repo.ClearModelRateLimitScopes(canceledContext(t), 42, []string{"AICredits", "claude-sonnet-4-5"})
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet(), "必须只执行一次路径删除，不得写回整个 map")
	})

	t.Run("空白 scope 被过滤后只剩一个占位符", func(t *testing.T) {
		repo, mock := newModelRateLimitRepo(t)

		mock.ExpectExec(`(?s)UPDATE accounts SET extra = COALESCE\(extra, '\{\}'::jsonb\)`+
			` #- ARRAY\['model_rate_limits', \$2\]`+
			`, updated_at = NOW\(\) WHERE id = \$1 AND deleted_at IS NULL`).
			WithArgs(int64(42), "AICredits").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
			WillReturnResult(sqlmock.NewResult(1, 1))

		require.NoError(t, repo.ClearModelRateLimitScopes(context.Background(), 42, []string{"  ", "AICredits", ""}))
		require.NoError(t, mock.ExpectationsWereMet())
	})

	// 空列表不能发 UPDATE：否则会给不存在的目标写一次 updated_at 并触发快照重建。
	t.Run("空列表直接返回且不碰数据库", func(t *testing.T) {
		repo, mock := newModelRateLimitRepo(t)

		require.NoError(t, repo.ClearModelRateLimitScopes(context.Background(), 42, nil))
		require.NoError(t, repo.ClearModelRateLimitScopes(context.Background(), 42, []string{"", "  "}))
		require.NoError(t, mock.ExpectationsWereMet(), "不应产生任何 SQL")
	})

	t.Run("行不存在仍返回 NotFound", func(t *testing.T) {
		repo, mock := newModelRateLimitRepo(t)

		mock.ExpectExec(`(?s)UPDATE accounts SET extra = `).
			WillReturnResult(sqlmock.NewResult(0, 0))

		require.ErrorIs(t, repo.ClearModelRateLimitScopes(context.Background(), 404, []string{"AICredits"}),
			service.ErrAccountNotFound)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}
