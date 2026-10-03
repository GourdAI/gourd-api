-- 245_backfill_subscription_quota.sql
-- 把分组上的日/周/月额度落到该分组下每个订阅行自身（依赖迁移 244 新增的三列）。
--
-- 目的：迁移 244 之后额度可以写在订阅行上，但历史数据仍只在 groups 上。
-- 不落位的话，管理员想「只调张三的额度」就必须先给他补一份订阅额度，
-- 而没补的人继续继承分组额度 —— 同一分组里两种口径混着，最难排查。
-- 本迁移一次性把分组额度复制成每人自己的额度，之后每人独立可调。
--
-- 语义等价性：搬迁前后每个人的生效额度完全相同
-- （原来是「订阅为空 → 继承分组」，现在是「订阅值 = 分组值」），
-- 因为 service.UserSubscription.Effective*Limit 的优先级是 订阅自有 > 分组。
--
-- ⚠️ 搬迁带来的行为变化（必须知道）：
-- 搬迁后修改 **分组** 额度，不会再自动影响这些订阅（自有额度优先）。
-- 若日后要做分组级批量调价，请对目标分组的订阅重新下发额度
-- （POST /admin/subscriptions/bulk-assign 传三个 *_limit_usd）。
--
-- 只搬「当前生效」的订阅：status='active' 且未过期。
-- 过期/暂停的订阅不搬 —— 它们日后被续期复活时，应当跟随**当时**的分组额度，
-- 而不是冻结在迁移那天的历史值（否则续费用户拿到一份陈旧额度）。
--
-- 只搬 > 0 的额度：0/NULL/负数在原语义里都是「不限额」，一律写成 NULL。
-- 运行期 normalizeSubLimit 也会把 <=0 归一为不限额，但库里留脏值会让
-- SQL 直查与报表口径不一致，故在迁移层就挡住。
-- 个人订阅（group_id = 0）本就靠自有额度、无分组可继承，被 WHERE 天然排除。
--
-- 幂等与可回滚：
--   - 只处理「三个自有额度都还是 NULL」的订阅行，重复执行不会覆盖任何已有额度；
--   - 搬迁先写审计表（含搬迁值），再由审计表把值落到位，
--     因此「记录的值」与「写入的值」必然一致；
--   - 回滚判据是「订阅当前值 == 审计表记录的搬迁值」，不依赖分组当前值，
--     所以管理员此后手工调过的值不会被误清，分组后来调价也不影响回滚正确性。

-- ---------------------------------------------------------------------------
-- 1. 审计表：记下搬迁了哪些订阅、以及搬迁时写入的三个值
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS subscription_quota_backfill_245 (
    subscription_id        BIGINT        NOT NULL PRIMARY KEY,
    backfilled_daily_usd   DECIMAL(20, 8),
    backfilled_weekly_usd  DECIMAL(20, 8),
    backfilled_monthly_usd DECIMAL(20, 8),
    backfilled_at          TIMESTAMPTZ   NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE subscription_quota_backfill_245 IS
    '迁移 245 把分组额度复制到订阅行时的记录（含搬迁值），用于精确回滚';

-- ---------------------------------------------------------------------------
-- 2. 锁定候选并记录搬迁值
-- ---------------------------------------------------------------------------
-- CASE WHEN > 0 而非 NULLIF：NULLIF 只挡 0，挡不住历史脏数据里的负数额度。
INSERT INTO subscription_quota_backfill_245 (
    subscription_id, backfilled_daily_usd, backfilled_weekly_usd, backfilled_monthly_usd
)
SELECT us.id,
       CASE WHEN COALESCE(g.daily_limit_usd,   0) > 0 THEN g.daily_limit_usd   END,
       CASE WHEN COALESCE(g.weekly_limit_usd,  0) > 0 THEN g.weekly_limit_usd  END,
       CASE WHEN COALESCE(g.monthly_limit_usd, 0) > 0 THEN g.monthly_limit_usd END
FROM user_subscriptions us
JOIN groups g ON g.id = us.group_id
WHERE us.deleted_at IS NULL
  AND g.deleted_at IS NULL
  AND us.group_id <> 0
  -- 只搬当前生效的订阅（与 244 局部索引谓词、service.IsActive() 口径一致）
  AND us.status = 'active'
  AND us.expires_at > NOW()
  AND us.daily_limit_usd   IS NULL
  AND us.weekly_limit_usd  IS NULL
  AND us.monthly_limit_usd IS NULL
  AND (
        COALESCE(g.daily_limit_usd,   0) > 0
     OR COALESCE(g.weekly_limit_usd,  0) > 0
     OR COALESCE(g.monthly_limit_usd, 0) > 0
  )
ON CONFLICT (subscription_id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- 3. 落位：值取自审计表，保证与记录严格一致
-- ---------------------------------------------------------------------------
-- 目标表列一律放在 WHERE（UPDATE ... FROM 的 ON 里引用目标表是非法语法）。
-- 再次用「三列都仍为 NULL」把关：即使某订阅已在审计表里，
-- 只要它后来被人工设过额度，这里也不会覆盖。
UPDATE user_subscriptions us
SET daily_limit_usd   = b.backfilled_daily_usd,
    weekly_limit_usd  = b.backfilled_weekly_usd,
    monthly_limit_usd = b.backfilled_monthly_usd,
    updated_at        = NOW()
FROM subscription_quota_backfill_245 b
WHERE b.subscription_id = us.id
  AND us.deleted_at IS NULL
  AND us.daily_limit_usd   IS NULL
  AND us.weekly_limit_usd  IS NULL
  AND us.monthly_limit_usd IS NULL;

-- ---------------------------------------------------------------------------
-- 回滚（需手工执行，不在本迁移内；本仓库无自动 down 通道，无 make migrate-down）
-- ---------------------------------------------------------------------------
-- 只把「当前值仍等于搬迁时写入值」的订阅还原为继承态（NULL）：
--
-- UPDATE user_subscriptions us
-- SET daily_limit_usd   = NULL,
--     weekly_limit_usd  = NULL,
--     monthly_limit_usd = NULL,
--     updated_at        = NOW()
-- FROM subscription_quota_backfill_245 b
-- WHERE b.subscription_id = us.id
--   AND us.daily_limit_usd   IS NOT DISTINCT FROM b.backfilled_daily_usd
--   AND us.weekly_limit_usd  IS NOT DISTINCT FROM b.backfilled_weekly_usd
--   AND us.monthly_limit_usd IS NOT DISTINCT FROM b.backfilled_monthly_usd;
--
-- DROP TABLE IF EXISTS subscription_quota_backfill_245;
