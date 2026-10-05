-- 246_subscription_wallet_model.sql
-- 订阅重构为「个人额度钱包」：去掉 group_id，额度模型改为单一总额池。
--
-- 产品定案（2026-10-03 拍板，共 7 项）：
--   1. 订阅不绑定分组、不授予任何分组准入，只管理额度；
--   2. 「订阅制分组」(groups.subscription_type='subscription') 不再作为订阅槽位依据；
--   3. 额度改为对齐 new-api 的单总额池（amount_total / amount_used 语义）；
--   4. 总额度是一次性的：花完为止，有效期到期后剩余作废，不随日/周/月滚动重置；
--   5. 一个用户可持多份订阅，同一套餐重复购买 = 新增一份独立订阅；
--   6. 消耗顺序「先到期先消耗」，单笔费用可跨订阅拆分；
--   7. 套餐改额度不回溯已发放的订阅（发放时写快照）。
--
-- 为什么删 group_id 而不是保留：group_id 原本承担两个职责——
--   (a) 槽位键 (user_id, group_id)：区分个人订阅与分组专属订阅；
--   (b) 「买了哪个包」的凭据（订阅制分组时代）。
--   两者都随本次决策失效。保留一个永远只写 0 的列会让「订阅=分组通行证」的
--   误读反复发生（历史上已因此产生 5 处越权设计，见 4bfcd05 的撤销）。
--
-- 「买了哪个包」改由 plan_id 记录（可空：管理员手工发放的订阅没有套餐）。
--
-- 数据搬移：本实例无存量订阅（用户确认「从没建过订阅」），但迁移仍写成对
-- 任意存量安全的形状——旧三档额度取最粗粒度（月>周>日）作为总额池，
-- 旧用量取三者最大值，保证不会把已花的钱读成 0（那等于变相提权）。
--
-- 重入安全：全部 IF EXISTS / IF NOT EXISTS / DO 块判存在。
-- 注意：不使用 CREATE INDEX CONCURRENTLY，因此可安全跑在事务内（runner 默认包裹）。
-- 自检：本文件内所有 CREATE INDEX 的 WHERE 谓词必须只含 IMMUTABLE 表达式
--       （不得出现 now()/current_timestamp/会话级函数），否则整条迁移必然失败。

-- ---------------------------------------------------------------------------
-- 1. 新列：plan_id / 总额池额度 / 总额池用量
-- ---------------------------------------------------------------------------
ALTER TABLE user_subscriptions
    ADD COLUMN IF NOT EXISTS plan_id BIGINT,
    ADD COLUMN IF NOT EXISTS total_limit_usd DECIMAL(20, 8),
    ADD COLUMN IF NOT EXISTS total_usage_usd DECIMAL(20, 10) NOT NULL DEFAULT 0;

COMMENT ON COLUMN user_subscriptions.plan_id IS
    '来源套餐(可空)：仅用于展示与追溯；额度以本行 total_limit_usd 快照为准，套餐改额度不回溯';
COMMENT ON COLUMN user_subscriptions.total_limit_usd IS
    '订阅总额度(USD)：一次性总额池，花完为止；NULL 或 <=0 表示不限额（有效期到期为止）';
COMMENT ON COLUMN user_subscriptions.total_usage_usd IS
    '总额度已消耗(USD)：随请求原子累加，订阅过期/撤销不回滚';

-- ---------------------------------------------------------------------------
-- 2. 搬移旧数据（旧列存在时才做，保证重入与「已升级到位」的库都安全）
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name = 'user_subscriptions' AND column_name = 'monthly_limit_usd') THEN
        UPDATE user_subscriptions
        SET total_limit_usd = COALESCE(
                monthly_limit_usd,
                weekly_limit_usd,
                daily_limit_usd
            )
        WHERE total_limit_usd IS NULL;
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name = 'user_subscriptions' AND column_name = 'monthly_usage_usd') THEN
        UPDATE user_subscriptions
        SET total_usage_usd = GREATEST(daily_usage_usd, weekly_usage_usd, monthly_usage_usd)
        WHERE total_usage_usd = 0;
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- 3. 解除旧索引 / 唯一约束
--    这些索引定义在 (group_id) 或含 group_id 的复合键上，列删掉前必须先删索引。
--    逐个判存在，避免在未跑过对应迁移的库上报错。
-- ---------------------------------------------------------------------------
DROP INDEX IF EXISTS user_subscriptions_user_group_unique_active;
DROP INDEX IF EXISTS usersubscription_user_id_group_id;
DROP INDEX IF EXISTS idx_user_subscriptions_group_id;
DROP INDEX IF EXISTS idx_user_subscriptions_personal_active;
DROP INDEX IF EXISTS idx_user_subscriptions_user_status_expires_active;

ALTER TABLE user_subscriptions DROP CONSTRAINT IF EXISTS user_subscriptions_user_id_group_id_key;

-- ---------------------------------------------------------------------------
-- 4. 删除 group_id 与旧三档额度/用量/窗口列
-- ---------------------------------------------------------------------------
ALTER TABLE user_subscriptions
    DROP COLUMN IF EXISTS group_id,
    DROP COLUMN IF EXISTS daily_limit_usd,
    DROP COLUMN IF EXISTS weekly_limit_usd,
    DROP COLUMN IF EXISTS monthly_limit_usd,
    DROP COLUMN IF EXISTS daily_usage_usd,
    DROP COLUMN IF EXISTS weekly_usage_usd,
    DROP COLUMN IF EXISTS monthly_usage_usd,
    DROP COLUMN IF EXISTS daily_window_start,
    DROP COLUMN IF EXISTS weekly_window_start,
    DROP COLUMN IF EXISTS monthly_window_start;

-- ---------------------------------------------------------------------------
-- 5. 新索引
-- ---------------------------------------------------------------------------
-- 网关热路径：按用户取全部活跃钱包（先到期先消耗 → 按 expires_at 升序）。
-- 【谓词不得含 now()】partial index 的谓词必须是 IMMUTABLE 表达式，而 now() 是
-- STABLE —— 带它建索引会被 PostgreSQL 直接拒绝（42P17 "functions in index
-- predicate must be marked IMMUTABLE"）。本文件跑在事务里（runner 默认 BeginTx），
-- 该错误会让 246 整条回滚：新列不存在、旧列仍在，而运行期 SQL 已只认新列，
-- 服务永远起不来。「未过期」这一条件由各查询语句自身的 expires_at > NOW()
-- 谓词承担；索引仍按 (user_id, expires_at) 有序输出，扫描时顺带过滤即可。
CREATE INDEX IF NOT EXISTS idx_user_subscriptions_wallet_active
    ON user_subscriptions (user_id, expires_at)
    WHERE deleted_at IS NULL AND status = 'active';

-- 补回 062 迁移里被 DROP 的复合索引（口径保持一致：WHERE deleted_at IS NULL）。
-- 它服务的是「扫描全量 active 订阅」的后台批处理（BatchUpdateExpiredStatus、
-- 过期提醒遍历），条件比上面的热路径索引宽（不加 expires_at 过滤，因为要找出
-- 已过期的行）；若只留热路径索引，这两条批处理会退化成全表扫描。
CREATE INDEX IF NOT EXISTS idx_user_subscriptions_user_status_expires_active
    ON user_subscriptions (user_id, status, expires_at)
    WHERE deleted_at IS NULL;

-- plan_id 追溯（可空列，部分索引跳过 NULL）。
CREATE INDEX IF NOT EXISTS idx_user_subscriptions_plan_id
    ON user_subscriptions (plan_id)
    WHERE plan_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 6. plan_id 外键：套餐为硬删除（见 ent/schema/subscription_plan.go 注释），
--    删套餐会级联抹掉订阅的来源凭据。这里刻意用 ON DELETE SET NULL：
--    「下架商品」不应连带销毁「用户已购买的额度记录」。
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    con RECORD;
BEGIN
    FOR con IN
        SELECT c.conname AS name
        FROM pg_constraint c
        JOIN pg_attribute a
            ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
        WHERE c.contype = 'f'
          AND c.conrelid = 'user_subscriptions'::regclass
          AND a.attname = 'plan_id'
    LOOP
        EXECUTE format('ALTER TABLE user_subscriptions DROP CONSTRAINT %I', con.name);
    END LOOP;
END
$$;

ALTER TABLE user_subscriptions
    ADD CONSTRAINT user_subscriptions_plan_id_fkey
    FOREIGN KEY (plan_id) REFERENCES subscription_plans (id) ON DELETE SET NULL;

-- ---------------------------------------------------------------------------
-- 7. 套餐额度列（决策 3/7：额度长在套餐上，购买时快照进订阅行）
-- ---------------------------------------------------------------------------
ALTER TABLE subscription_plans
    ADD COLUMN IF NOT EXISTS total_limit_usd DECIMAL(20, 8);

COMMENT ON COLUMN subscription_plans.total_limit_usd IS
    '套餐总额度(USD)：购买后快照进订阅行；NULL 或 <=0 表示该套餐不限量';
