-- 241_api_key_groups.sql
-- 一个 API Key 绑定多个分组：新增 api_key_groups 关联表。
--
-- 设计契约（不可偏移）：
--   * api_keys.group_id 继续保存「主分组」（= group_ids[0]），单值列保留作兼容与主分组真相源；
--   * 候选分组集合由 api_key_groups 决定，按 sort_order 升序、主分组优先；
--   * 认证快照（auth cache）必须随「关联表变更」与「分组配置变更」失效，否则请求期决议会
--     静默拿到陈旧分组配置。既有 outbox 触发器（184/186/193）以 api_keys.group_id 命中
--     为条件收集待失效 key，关联表改绑后不再触发，因此本迁移必须重写为
--     「group_id 命中 OR 关联表命中」，并新增 api_key_groups 自身变更的触发器。
--   * 直接复用既有 enqueue_auth_cache_invalidation(raw_key) / auth_cache_invalidation_outbox，
--     不另造失效机制。
--
-- 重入安全：全部 IF NOT EXISTS / ON CONFLICT / CREATE OR REPLACE。

-- ── 1. 关联表 ────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS api_key_groups (
    api_key_id BIGINT      NOT NULL REFERENCES api_keys (id) ON DELETE CASCADE,
    group_id   BIGINT      NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    sort_order INTEGER     NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (api_key_id, group_id)
);

CREATE INDEX IF NOT EXISTS idx_api_key_groups_group_id
    ON api_key_groups (group_id);

CREATE INDEX IF NOT EXISTS idx_api_key_groups_api_key_sort
    ON api_key_groups (api_key_id, sort_order);

COMMENT ON TABLE api_key_groups IS
    'API Key ↔ 分组多绑定；主分组 = sort_order 最小（并列取 group_id 最小），并与 api_keys.group_id 保持一致';

-- ── 2. 回填：现有 key 的主分组作为唯一候选（仅未软删）───────────────────────
-- 只回填 deleted_at IS NULL 的行，保证单分组 key 行为与今天完全一致。
INSERT INTO api_key_groups (api_key_id, group_id, sort_order)
SELECT k.id, k.group_id, 0
FROM api_keys AS k
WHERE k.group_id IS NOT NULL
  AND k.deleted_at IS NULL
ON CONFLICT (api_key_id, group_id) DO NOTHING;

-- ── 3. 关联表变更 → 失效对应 key 的认证快照 ─────────────────────────────────
-- helper：把单个 api_key_id 的（未软删）key 写进 outbox。
CREATE OR REPLACE FUNCTION enqueue_api_key_groups_key_invalidation(target_api_key_id BIGINT)
RETURNS VOID
LANGUAGE plpgsql
AS $$
BEGIN
    IF target_api_key_id IS NULL THEN
        RETURN;
    END IF;
    INSERT INTO auth_cache_invalidation_outbox (cache_key)
    SELECT encode(sha256(convert_to(k.key, 'UTF8')), 'hex')
    FROM api_keys AS k
    WHERE k.id = target_api_key_id
      AND k.deleted_at IS NULL
      AND k.key <> '';
END;
$$;

CREATE OR REPLACE FUNCTION enqueue_api_key_groups_auth_cache_invalidation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    -- 显式分支：INSERT 触发器里不得访问 OLD（PL/pgSQL 会报 record "old" is not assigned yet）。
    IF TG_OP = 'DELETE' THEN
        PERFORM enqueue_api_key_groups_key_invalidation(OLD.api_key_id);
        RETURN OLD;
    ELSIF TG_OP = 'INSERT' THEN
        PERFORM enqueue_api_key_groups_key_invalidation(NEW.api_key_id);
        RETURN NEW;
    END IF;

    -- UPDATE：值未变则不失效；绑定的 key 换了则新旧两个 key 都失效。
    IF OLD.api_key_id IS NOT DISTINCT FROM NEW.api_key_id
       AND OLD.group_id IS NOT DISTINCT FROM NEW.group_id
       AND OLD.sort_order IS NOT DISTINCT FROM NEW.sort_order THEN
        RETURN NEW;
    END IF;
    IF OLD.api_key_id IS DISTINCT FROM NEW.api_key_id THEN
        PERFORM enqueue_api_key_groups_key_invalidation(OLD.api_key_id);
    END IF;
    PERFORM enqueue_api_key_groups_key_invalidation(NEW.api_key_id);
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_api_key_groups_auth_cache_invalidation ON api_key_groups;
CREATE TRIGGER trg_api_key_groups_auth_cache_invalidation
AFTER INSERT OR UPDATE OR DELETE ON api_key_groups
FOR EACH ROW EXECUTE FUNCTION enqueue_api_key_groups_auth_cache_invalidation();

-- ── 4. groups 变更 → 失效所有（主分组或关联表）绑定该分组的 key ─────────────
-- 基于 193_group_profit_control_auth_cache_invalidation.sql 的最新函数体，
-- 只把「k.group_id = target_group_id」扩展为「OR 关联表命中」，其余字段比较一律保留。
CREATE OR REPLACE FUNCTION enqueue_group_auth_cache_invalidation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    target_group_id BIGINT;
BEGIN
    target_group_id := OLD.id;
    IF TG_OP = 'UPDATE'
       AND OLD.status IS NOT DISTINCT FROM NEW.status
       AND OLD.is_exclusive IS NOT DISTINCT FROM NEW.is_exclusive
       AND OLD.allow_image_generation IS NOT DISTINCT FROM NEW.allow_image_generation
       AND OLD.platform IS NOT DISTINCT FROM NEW.platform
       AND OLD.subscription_type IS NOT DISTINCT FROM NEW.subscription_type
       AND OLD.rate_multiplier IS NOT DISTINCT FROM NEW.rate_multiplier
       AND OLD.peak_rate_enabled IS NOT DISTINCT FROM NEW.peak_rate_enabled
       AND OLD.peak_start IS NOT DISTINCT FROM NEW.peak_start
       AND OLD.peak_end IS NOT DISTINCT FROM NEW.peak_end
       AND OLD.peak_rate_multiplier IS NOT DISTINCT FROM NEW.peak_rate_multiplier
       AND OLD.profit_control_enabled IS NOT DISTINCT FROM NEW.profit_control_enabled
       AND OLD.profit_min_margin IS NOT DISTINCT FROM NEW.profit_min_margin
       AND OLD.profit_safety_buffer IS NOT DISTINCT FROM NEW.profit_safety_buffer
       AND OLD.deleted_at IS NOT DISTINCT FROM NEW.deleted_at THEN
        RETURN NEW;
    END IF;

    INSERT INTO auth_cache_invalidation_outbox (cache_key)
    SELECT encode(sha256(convert_to(k.key, 'UTF8')), 'hex')
    FROM api_keys AS k
    WHERE k.deleted_at IS NULL
      AND k.key <> ''
      AND (
            k.group_id = target_group_id
            OR EXISTS (
                SELECT 1 FROM api_key_groups AS akg
                WHERE akg.api_key_id = k.id AND akg.group_id = target_group_id
            )
          );
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

-- ── 5. user_allowed_groups 变更 → 同样扩展为「主分组或关联表命中」───────────
-- 基于 184_auth_cache_invalidation_outbox.sql 的最新函数体，仅扩展分组命中条件。
CREATE OR REPLACE FUNCTION enqueue_allowed_group_auth_cache_invalidation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    target_user_id BIGINT;
    target_group_id BIGINT;
BEGIN
    IF TG_OP = 'UPDATE'
       AND (OLD.user_id IS DISTINCT FROM NEW.user_id
            OR OLD.group_id IS DISTINCT FROM NEW.group_id) THEN
        IF EXISTS (
            SELECT 1 FROM groups g
            WHERE g.id = OLD.group_id AND g.is_exclusive = TRUE
        ) THEN
            INSERT INTO auth_cache_invalidation_outbox (cache_key)
            SELECT encode(sha256(convert_to(k.key, 'UTF8')), 'hex')
            FROM api_keys AS k
            WHERE k.user_id = OLD.user_id
              AND k.deleted_at IS NULL
              AND k.key <> ''
              AND (
                    k.group_id = OLD.group_id
                    OR EXISTS (
                        SELECT 1 FROM api_key_groups AS akg
                        WHERE akg.api_key_id = k.id AND akg.group_id = OLD.group_id
                    )
                  );
        END IF;
        target_user_id := NEW.user_id;
        target_group_id := NEW.group_id;
    ELSIF TG_OP = 'UPDATE' THEN
        RETURN NEW;
    ELSIF TG_OP = 'INSERT' THEN
        target_user_id := NEW.user_id;
        target_group_id := NEW.group_id;
    ELSE
        target_user_id := OLD.user_id;
        target_group_id := OLD.group_id;
    END IF;

    IF EXISTS (
        SELECT 1 FROM groups g
        WHERE g.id = target_group_id AND g.is_exclusive = TRUE
    ) THEN
        INSERT INTO auth_cache_invalidation_outbox (cache_key)
        SELECT encode(sha256(convert_to(k.key, 'UTF8')), 'hex')
        FROM api_keys AS k
        WHERE k.user_id = target_user_id
          AND k.deleted_at IS NULL
          AND k.key <> ''
          AND (
                k.group_id = target_group_id
                OR EXISTS (
                    SELECT 1 FROM api_key_groups AS akg
                    WHERE akg.api_key_id = k.id AND akg.group_id = target_group_id
                )
              );
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;
