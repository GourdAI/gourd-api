package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/ent/usersubscription"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

func TestUserSubscriptionGetByIDForUpdateLocksRow(t *testing.T) {
	var capturedSQL string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureEntQueryMatcher{actual: &capturedSQL}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := NewUserSubscriptionRepository(client)
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery("locked subscription").WillReturnRows(
		sqlmock.NewRows(usersubscription.Columns).AddRow(
			// 列顺序严格按 usersubscription.Columns（共 14 列，2026-10-03 钱包重构后）：
			// id, created_at, updated_at, deleted_at, user_id, plan_id, starts_at,
			// expires_at, status, total_limit_usd, total_usage_usd, assigned_by,
			// assigned_at, notes
			int64(7), now, now, nil, int64(11), int64(13), now, now.AddDate(0, 0, 30), "active",
			// 总额度 NULL = 不限额；已用 0
			nil, 0.0,
			nil, now, "renewal",
		),
	)

	sub, err := repo.GetByIDForUpdate(context.Background(), 7)
	require.NoError(t, err)
	require.Equal(t, int64(7), sub.ID)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Contains(t, strings.ToUpper(normalizeSQLWhitespace(capturedSQL)), "FOR UPDATE")
}
