package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// normalizeSQL 折叠空白，便于做跨行断言。
func normalizeSQL245(content string) string {
	return strings.Join(strings.Fields(content), " ")
}

// 迁移 244：订阅额度落位到订阅行 + 允许个人订阅（group_id=0）。
// 这三件事缺一个就跑不通：列不存在则无法写额度；外键还在则 group_id=0 插不进去。
func TestSubscriptionPersonalQuotaMigration(t *testing.T) {
	content, err := FS.ReadFile("244_subscription_personal_quota.sql")
	require.NoError(t, err)
	sql := normalizeSQL245(string(content))

	// 1. 三个自有额度列，可空（NULL = 不限额）
	for _, col := range []string{"daily_limit_usd", "weekly_limit_usd", "monthly_limit_usd"} {
		require.Contains(t, sql,
			"ADD COLUMN IF NOT EXISTS "+col+" DECIMAL(20, 8)",
			"缺少可空额度列 "+col)
		require.Contains(t, sql,
			"COMMENT ON COLUMN user_subscriptions."+col,
			"额度列 "+col+" 缺少注释")
	}

	// 2. 必须解除 group_id -> groups 的外键（否则哨兵值 0 违反引用完整性）
	require.Contains(t, sql, "c.conrelid = 'user_subscriptions'::regclass")
	require.Contains(t, sql, `a.attname = 'group_id'`)
	require.Contains(t, sql, "DROP CONSTRAINT")

	// 3. group_id 默认 0
	require.Contains(t, sql, "ALTER COLUMN group_id SET DEFAULT 0")

	// 幂等：不允许出现无 IF EXISTS 保护的破坏性语句
	require.NotContains(t, sql, "DROP CONSTRAINT user_subscriptions.group_id",
		"硬编码约束名不幂等，必须走 pg_constraint 动态发现")
}

// 迁移 245：把分组额度搬迁到订阅行，且必须做到「不覆盖已设值」+「可精确回滚」。
func TestSubscriptionQuotaBackfillMigration(t *testing.T) {
	content, err := FS.ReadFile("245_backfill_subscription_quota.sql")
	require.NoError(t, err)
	sql := normalizeSQL245(string(content))

	// 审计表：回滚依赖它，缺了就只能全表清空（会误伤管理员手工设置的额度）
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS subscription_quota_backfill_245")
	require.Contains(t, sql, "ON CONFLICT (subscription_id) DO NOTHING")

	// 迁移必须自述「无自动 down 通道」，避免运维误以为存在 make migrate-down
	require.Contains(t, sql, "无自动 down 通道")

	// 只搬「三列仍为 NULL」的订阅：这是幂等与不覆盖的关键守卫
	require.Contains(t, sql, "us.daily_limit_usd IS NULL")
	require.Contains(t, sql, "us.weekly_limit_usd IS NULL")
	require.Contains(t, sql, "us.monthly_limit_usd IS NULL")

	// 排除个人订阅（无分组可继承）与软删除行
	require.Contains(t, sql, "us.group_id <> 0")
	require.Contains(t, sql, "us.deleted_at IS NULL")

	// 只搬 >0 的额度：0/NULL/负数都视为不限额，不能写进订阅行。
	// 必须是 CASE WHEN > 0 而不是 NULLIF(x,0)——后者挡得住 0，挡不住历史脏数据里的负数。
	for _, col := range []string{"daily", "weekly", "monthly"} {
		require.Contains(t, sql,
			"CASE WHEN COALESCE(g."+col+"_limit_usd, 0) > 0 THEN g."+col+"_limit_usd END",
			col+" 额度未做「仅正数才搬迁」的防护")
		require.NotContains(t, sql, "NULLIF(g."+col+"_limit_usd, 0)",
			col+" 用 NULLIF 只挡 0、挡不住负数额度")
	}

	// 只搬当前生效的订阅：过期/暂停的订阅续期复活后应跟随当时的分组额度，
	// 而不是冻结在迁移那天的历史值。
	require.Contains(t, sql, "us.status = 'active'")
	require.Contains(t, sql, "us.expires_at > NOW()")

	// UPDATE ... FROM 的目标表列必须写在 WHERE 里：PostgreSQL 禁止在 FROM 子句的
	// join 条件里引用被更新表（报错 invalid reference to FROM-clause entry）。
	// 只约束 UPDATE 语句块：候选查询（INSERT ... SELECT）里的 JOIN ... ON 是合法的。
	updateBlock := sql[strings.Index(sql, "UPDATE user_subscriptions us"):]
	require.NotEmpty(t, updateBlock, "没找到落位的 UPDATE 语句")
	require.NotContains(t, updateBlock, " ON ",
		"UPDATE...FROM 子句不得用 ON 引用目标表，条件应放进 WHERE")
	require.Contains(t, updateBlock, "WHERE b.subscription_id = us.id",
		"落位 UPDATE 缺少基于 WHERE 的目标表关联")
}

// 回滚判据必须取自审计表自身，不能依赖分组「当前」值：
// 分组在搬迁后调过价（迁移注释里警告的常态）会导致判据双向失效——
// 手工设成与分组巧合相等的值被误清，分组改过值的行又漏清。
func TestSubscriptionQuotaBackfillRollbackUsesAuditedValues(t *testing.T) {
	content, err := FS.ReadFile("245_backfill_subscription_quota.sql")
	require.NoError(t, err)
	sql := normalizeSQL245(string(content))

	// 审计表必须存下搬迁时写入的三个值，否则回滚无从判断「哪些值是本次写的」
	for _, col := range []string{"daily", "weekly", "monthly"} {
		require.Contains(t, sql, "backfilled_"+col+"_usd DECIMAL(20, 8)",
			"审计表缺少 "+col+" 搬迁值列，回滚无法精确判定")
	}

	// 落位的值必须来自审计表（保证「记录值 == 写入值」）
	for _, col := range []string{"daily", "weekly", "monthly"} {
		require.Contains(t, sql, "= b.backfilled_"+col+"_usd",
			col+" 落位值未取自审计表，审计记录可能与实际写入不一致")
	}

	// 回滚判据：对比审计值，而不是对比分组当前值
	for _, col := range []string{"daily", "weekly", "monthly"} {
		require.Contains(t, sql,
			"us."+col+"_limit_usd IS NOT DISTINCT FROM b.backfilled_"+col+"_usd",
			col+" 回滚判据未使用审计值")
		require.NotContains(t, sql,
			"us."+col+"_limit_usd IS NOT DISTINCT FROM NULLIF",
			col+" 回滚判据依赖分组当前值，分组调价后会误清/漏清")
	}
}

// 两个迁移都不允许使用 CONCURRENTLY（事务内会报错，runner 也会直接拒绝启动）。
func TestSubscriptionQuotaMigrationsAreTransactionSafe(t *testing.T) {
	for _, name := range []string{
		"244_subscription_personal_quota.sql",
		"245_backfill_subscription_quota.sql",
	} {
		content, err := FS.ReadFile(name)
		require.NoError(t, err, name)
		require.NotContains(t, strings.ToUpper(string(content)), "CONCURRENTLY",
			name+": 普通迁移不得包含 CONCURRENTLY（需改名为 *_notx.sql）")
	}
}
