package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestAccountRepositoryListDueUpstreamBillingProbeAccountsBoundsQuery(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	var capturedSQL string
	mock.ExpectQuery("WITH candidates AS").
		WithArgs(now, 20).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	repo := newAccountRepositoryWithSQL(nil, captureQuerySQL{db: db, captured: &capturedSQL}, nil)

	accounts, err := repo.ListDueUpstreamBillingProbeAccounts(context.Background(), now, 20)

	require.NoError(t, err)
	require.Empty(t, accounts)
	normalized := normalizeSQLWhitespace(capturedSQL)
	require.Contains(t, normalized, "deleted_at IS NULL")
	require.Contains(t, normalized, "status = 'active'")
	// 候选 SQL 必须按平台白名单收窄：否则存量 enabled=true 的 WorkBuddy 账号
	// （无 credentials.api_key，不具备探测资格）会占满批次名额、饿死合格账号。
	// 白名单镜像自 service.IsUpstreamBillingProbeIdentity，漂移由
	// TestUpstreamBillingProbePlatformsSQLMirrorsServiceWhitelist 兜底。
	require.Contains(t, normalized, "platform IN ("+upstreamBillingProbePlatformsSQL+")")
	require.Contains(t, normalized, "type = 'apikey'")
	require.Contains(t, normalized, `extra @> '{"upstream_billing_probe_enabled": true}'::jsonb`)
	require.Contains(t, normalized, "jsonb_path_query_first_tz")
	require.Contains(t, normalized, `'(\.[0-9]{6})[0-9]+(Z|[+-][0-9]{2}:[0-9]{2})$'`)
	require.Contains(t, normalized, "parsed AS MATERIALIZED")
	require.Contains(t, normalized, "parsed_next_probe_at::timestamptz <= $1")
	require.Contains(t, normalized, "LIMIT $2")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountRepositoryListDueUpstreamBillingProbeAccountsRejectsNonPositiveLimit(t *testing.T) {
	repo := newAccountRepositoryWithSQL(nil, nil, nil)

	accounts, err := repo.ListDueUpstreamBillingProbeAccounts(context.Background(), time.Now(), 0)

	require.NoError(t, err)
	require.Empty(t, accounts)
}
