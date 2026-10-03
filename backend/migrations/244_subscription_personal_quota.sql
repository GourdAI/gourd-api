-- 244_subscription_personal_quota.sql
-- 订阅额度下沉到订阅行本身，并允许「个人订阅」不绑定任何分组。
--
-- 动机：原实现下额度只能配在分组（groups.daily/weekly/monthly_limit_usd）上，
-- 于是"给张三 5 美元/天、给李四 20 美元/天"必须先建两个分组，再把人分别关联进去。
-- 管理员想做的只是"给这个人一份日/周/月额度"，模型由用户自己选。
--
-- 本迁移做三件事：
-- 1. user_subscriptions 增加自有额度三列（DECIMAL(20,8)，NULL=不限额）。
--    生效优先级（见 service.UserSubscription.EffectiveDailyLimit）：
--      订阅自有额度 > 归属分组额度 > 不限额
--    即老数据（三列全 NULL）行为完全不变，仍按分组额度扣费。
-- 2. group_id 允许写 0 作为「个人订阅」哨兵值：不归属任何分组、
--    对该用户的全部 API Key/分组通用，只按上面三列的额度扣费。
--    因此必须解除指向 groups(id) 的外键 —— 0 不是真实分组，保留外键会直接插入失败。
-- 3. group_id 补 DEFAULT 0（与仓储层「未指定即 0」的写入路径保持一致，便于原生 SQL 直接插入）。
--    注：ent schema 仍保留 group_id 必填 + 指向 groups 的 edge（因哨兵值 0 与
--    非 NULL 列兼容：仓储层总是显式 SetGroupID(sub.GroupID)，因此 Required 检查
--    以「是否被赋值」为准，写 0 可通过），本仓库未启用 ent auto-migrate
--    （已核实无 Schema.Create()/auto.Migrate() 调用点），schema 与库结构的差异
--    仅为注释层面，不会在启动时回滚 DEFAULT 0。
--
-- 为什么用 0 而不是 NULL：ent 把该列生成为 Go 的 int64（非指针）。
-- 改成 NULLable 会把字段类型变成 *int64，波及全部读写点与既有比较逻辑，
-- 收益仅是语义纯洁，代价是一次跨包的全域改造 —— 不划算。
-- 0 在 groups 表里永远不可能是主键（BIGSERIAL 从 1 起），因此是安全的哨兵值。
--
-- 解除外键的安全性：分组删除时的订阅清理本就在应用层完成
-- （group_repo.go 的 DeleteByGroupID / 软删除会把该组订阅一并置 deleted_at），
-- 依赖外键 CASCADE 只是兜底；用量累加的 SQL 也已改为显式校验
-- "group_id = 0 OR group_id 指向一个未删除的分组"，不再依赖 JOIN groups 来过滤。
--
-- 重入安全：全部使用 IF NOT EXISTS / DO 块判存在，可重复执行。

-- ---------------------------------------------------------------------------
-- 1. 订阅自有额度
-- ---------------------------------------------------------------------------
ALTER TABLE user_subscriptions
    ADD COLUMN IF NOT EXISTS daily_limit_usd   DECIMAL(20, 8),
    ADD COLUMN IF NOT EXISTS weekly_limit_usd  DECIMAL(20, 8),
    ADD COLUMN IF NOT EXISTS monthly_limit_usd DECIMAL(20, 8);

COMMENT ON COLUMN user_subscriptions.daily_limit_usd IS
    '订阅自有日限额(USD)：非 NULL 时优先于分组日限额；NULL 或 <=0 表示不限额';
COMMENT ON COLUMN user_subscriptions.weekly_limit_usd IS
    '订阅自有周限额(USD)：非 NULL 时优先于分组周限额；NULL 或 <=0 表示不限额';
COMMENT ON COLUMN user_subscriptions.monthly_limit_usd IS
    '订阅自有月限额(USD)：非 NULL 时优先于分组月限额；NULL 或 <=0 表示不限额';

-- ---------------------------------------------------------------------------
-- 2. 解除 group_id -> groups(id) 外键，使 group_id = 0（个人订阅）可入库
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
        JOIN pg_class t ON t.oid = c.confrelid
        WHERE c.contype = 'f'
          AND c.conrelid = 'user_subscriptions'::regclass
          AND a.attname = 'group_id'
          AND t.relname = 'groups'
    LOOP
        EXECUTE format('ALTER TABLE user_subscriptions DROP CONSTRAINT %I', con.name);
        RAISE NOTICE '已解除外键 user_subscriptions.group_id -> groups.id: %', con.name;
    END LOOP;
END
$$;

-- ---------------------------------------------------------------------------
-- 3. group_id 默认 0（个人订阅槽位）
-- ---------------------------------------------------------------------------
ALTER TABLE user_subscriptions
    ALTER COLUMN group_id SET DEFAULT 0;

-- 个人订阅按 (user_id, group_id=0) 命中，既有 user_id 索引已足够；
-- 补一个 partial 索引专供网关热路径查询活跃的个人订阅。
CREATE INDEX IF NOT EXISTS idx_user_subscriptions_personal_active
    ON user_subscriptions (user_id, expires_at)
    WHERE group_id = 0 AND deleted_at IS NULL AND status = 'active';
