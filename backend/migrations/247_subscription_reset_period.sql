-- 247_subscription_reset_period.sql
-- 订阅支持「周期重置额度」+「用户自助领取」。
--
-- 动机（2026-10-21 需求）：管理员想配置「每天发 1 美元」这类免费订阅——
-- 金额自定义、周期可选（日/周/月）、过期即作废、注册后自动绑定，
-- 同时这个套餐能一键关闭。旧模型（迁移 246）是一次性总额池，
-- 花完为止、不随日/周/月滚动，也没有「用户自己领」的入口。
--
-- 本迁移加四列，全部带默认值，对存量数据零行为变化：
--
--   1. subscription_plans.reset_period   —— 套餐声明的重置周期（none/daily/weekly/monthly）
--   2. subscription_plans.self_claimable —— 该套餐是否允许用户自助领取
--   3. user_subscriptions.reset_period   —— 发放时的周期**快照**（改套餐不回溯）
--   4. user_subscriptions.usage_window_start —— 当前窗口起点
--
-- 关键设计：为什么是「窗口起点 + 读时归一」而不是「定时任务清零」
-- ---------------------------------------------------------------
-- 定时任务清零有两个无法回避的缺陷：
--   (a) 任务没跑起来（实例全挂/刚重启/多实例抢锁失败）时，用户当天就领不到钱，
--       而这属于「少发」而非「多发」，用户侧看起来就是莫名其妙不能用了；
--   (b) 清零与计费并发：清零 UPDATE 与 IncrementUsage 的 += 竞争，
--       会在窗口边界把跨窗口的那一笔重复计入或抹掉。
-- 因此这里改为「懒重置」：窗口起点只作为**判据**，真正生效点在读取处做归一
-- （见 service.UserSubscription.NormalizeUsageWindow）：
--   - 读路径（仓储 List*ByUserID / 计费事务锁定读 / 聚合缓存）看到
--     window_start 已跨过周期边界时，把 total_usage_usd 视为 0；
--   - 计费事务在加锁读之前用条件 UPDATE 推窗口起点（WHERE 里带旧窗口起点，
--     天然幂等且互斥），命中者负责把该行 total_usage_usd 归零；
--   - 后台批处理只是「兜底清理」，跑不跑都不影响正确性（只影响库里的陈旧值）。
-- 这样「一天 1 美元」在语义上等于：每个自然日窗口内，累计用量独立计算。
--
-- 为什么重置周期不是「循环发放订阅行」
-- -----------------------------------------
-- 每周期新建一行 user_subscriptions 会让表按 用户×周期数 膨胀（1 万用户 × 每天
-- = 一年 365 万行），且「先到期先消耗」的分配会被大量历史行稀释。用同一行 +
-- 窗口起点，行数恒定，且复用已稳定的计费链路（拆分/聚合/缓存键全部不用改）。
--
-- 时间基准：窗口按 **UTC** 自然日/周（周起点周一）/月计算，与
-- user_platform_quotas 的窗口函数保持同一口径，避免两套「一天」定义打架。
--
-- 重入安全：全部 IF NOT EXISTS；不加 CHECK 约束（应用层已归一，加约束会让
-- 存量脏值在迁移期直接失败）。

-- ---------------------------------------------------------------------------
-- 1. 套餐：重置周期 + 是否可自助领取
-- ---------------------------------------------------------------------------
ALTER TABLE subscription_plans
    ADD COLUMN IF NOT EXISTS reset_period VARCHAR(16) NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS self_claimable BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN subscription_plans.reset_period IS
    '额度重置周期：none=一次性(花完为止) / daily / weekly / monthly；发放时快照进订阅行，改套餐不回溯';
COMMENT ON COLUMN subscription_plans.self_claimable IS
    '是否允许用户自助领取（免费套餐用）：true 时用户可在订阅页一键领取，无需管理员逐个分配';

-- ---------------------------------------------------------------------------
-- 2. 订阅行：周期快照 + 窗口起点
-- ---------------------------------------------------------------------------
ALTER TABLE user_subscriptions
    ADD COLUMN IF NOT EXISTS reset_period VARCHAR(16) NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS usage_window_start TIMESTAMPTZ;

COMMENT ON COLUMN user_subscriptions.reset_period IS
    '重置周期快照(none/daily/weekly/monthly)：none 时 total_usage_usd 永不清零（一次性总额池）';
COMMENT ON COLUMN user_subscriptions.usage_window_start IS
    '当前额度窗口起点(UTC)：窗口跨过周期边界后该行的 total_usage_usd 视为 0；NULL 表示首次使用，按创建时刻起算';

-- ---------------------------------------------------------------------------
-- 3. 索引
-- ---------------------------------------------------------------------------
-- 后台兜底批处理：找出「所有仍在生效、且配了重置周期」的行，按窗口起点分批扫描。
-- 谓词必须只含 IMMUTABLE 表达式（不能用 now()），「是否该重置」由查询语句
-- 自身的 usage_window_start 比较承担。
CREATE INDEX IF NOT EXISTS idx_user_subscriptions_reset_pending
    ON user_subscriptions (reset_period, usage_window_start)
    WHERE deleted_at IS NULL AND status = 'active' AND reset_period <> 'none';

-- 用户端「可领取的套餐」列表：只扫在售 + 可自助领取。
CREATE INDEX IF NOT EXISTS idx_subscription_plans_self_claimable
    ON subscription_plans (self_claimable)
    WHERE self_claimable = TRUE;
