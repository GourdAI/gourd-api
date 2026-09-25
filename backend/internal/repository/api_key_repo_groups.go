package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	dbgroup "github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/lib/pq"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// api_key_repo_groups.go 实现「一个 API Key 绑定多个分组」的关联表读写。
//
// 硬约束：本次改造禁止运行 ent codegen，因此关联表 api_key_groups 一律走原生 SQL
// （复用仓储层已有的 sql 执行器），不新增 ent schema。
//
// 语义：
//   - api_key_groups 是「候选分组集合」的唯一真相源；
//   - api_keys.group_id 仍是「主分组」列（= group_ids[0]，兼容与对外读数）；
//   - 替换为整体替换（先删后插），主分组永远排在最前（sort_order=0）。

// apiKeyGroupIDsForAuth 取回认证快照所需的候选分组 ID 集合：主分组优先，
// 其余按 (sort_order, group_id) 升序，保证确定性。
// 该方法在认证热路径上调用。
func (r *apiKeyRepository) apiKeyGroupIDsForAuth(ctx context.Context, apiKeyID int64, primaryGroupID *int64) ([]int64, error) {
	if apiKeyID <= 0 || r.sql == nil {
		// 无原生 SQL 执行器（仅注入 ent client 的测试桩）或非法 ID 时退化为单分组语义，
		// 保证单分组 key 行为与历史完全一致。
		return singleGroupFallback(primaryGroupID), nil
	}

	ids, err := r.queryGroupIDs(ctx, `
		SELECT group_id
		FROM api_key_groups
		WHERE api_key_id = $1
		ORDER BY sort_order ASC, group_id ASC`, apiKeyID)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		// 尚未回填或历史数据缺行：退化为 api_keys.group_id。
		return singleGroupFallback(primaryGroupID), nil
	}
	return normalizeAPIKeyGroupIDs(ids, primaryGroupID), nil
}

// ListGroupIDsByAPIKeyID 返回某 API Key 的绑定分组（主分组优先）。
func (r *apiKeyRepository) ListGroupIDsByAPIKeyID(ctx context.Context, apiKeyID int64) ([]int64, error) {
	if apiKeyID <= 0 || r.sql == nil {
		return nil, nil
	}
	return r.queryGroupIDs(ctx, `
		SELECT group_id
		FROM api_key_groups
		WHERE api_key_id = $1
		ORDER BY sort_order ASC, group_id ASC`, apiKeyID)
}

// ListGroupIDsByAPIKeyIDs 批量取回多个 key 的绑定分组，避免列表接口 N+1 查询。
func (r *apiKeyRepository) ListGroupIDsByAPIKeyIDs(ctx context.Context, apiKeyIDs []int64) (map[int64][]int64, error) {
	out := make(map[int64][]int64, len(apiKeyIDs))
	if r.sql == nil || len(apiKeyIDs) == 0 {
		return out, nil
	}
	rows, err := r.sql.QueryContext(ctx, `
		SELECT api_key_id, group_id
		FROM api_key_groups
		WHERE api_key_id = ANY($1::bigint[])
		ORDER BY api_key_id ASC, sort_order ASC, group_id ASC`, pq.Array(apiKeyIDs))
	if err != nil {
		if isUndefinedTableError(err) {
			return out, nil
		}
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var apiKeyID, groupID int64
		if err := rows.Scan(&apiKeyID, &groupID); err != nil {
			return nil, err
		}
		out[apiKeyID] = append(out[apiKeyID], groupID)
	}
	return out, rows.Err()
}

// ReplaceAPIKeyGroups 整体替换某 API Key 的分组绑定集合。
// primaryGroupID 是调用方决议出的主分组（同时写入 api_keys.group_id），
// 它会出现在 groupIDs 首位；primaryGroupID 为 nil 且 groupIDs 为空 = 解绑全部。
//
// 使用单条 data-modifying CTE 保证 DELETE 与 INSERT 在同一次语句执行内完成（原子）。
func (r *apiKeyRepository) ReplaceAPIKeyGroups(ctx context.Context, apiKeyID int64, groupIDs []int64, primaryGroupID *int64) error {
	if apiKeyID <= 0 || r.sql == nil {
		return nil
	}
	normalized := normalizeAPIKeyGroupIDs(groupIDs, primaryGroupID)

	var query string
	args := []any{apiKeyID}
	if len(normalized) == 0 {
		_, err := r.sql.ExecContext(ctx, `DELETE FROM api_key_groups WHERE api_key_id = $1`, apiKeyID)
		if isUndefinedTableError(err) {
			return nil
		}
		return err
	}

	values := make([]string, 0, len(normalized))
	for i, groupID := range normalized {
		values = append(values,
			fmt.Sprintf("($1, $%d::bigint, $%d::int)", len(args)+1, len(args)+2))
		args = append(args, groupID, i)
	}
	query = `WITH removed AS (
			DELETE FROM api_key_groups WHERE api_key_id = $1
		)
		INSERT INTO api_key_groups (api_key_id, group_id, sort_order)
		VALUES ` + strings.Join(values, ", ") + `
		ON CONFLICT (api_key_id, group_id) DO UPDATE SET sort_order = EXCLUDED.sort_order`

	_, err := r.sql.ExecContext(ctx, query, args...)
	if isUndefinedTableError(err) {
		return nil
	}
	return err
}

// CountAPIKeysByAnyGroupID 统计「主分组或任一绑定分组命中」的未软删 key 数量。
func (r *apiKeyRepository) CountAPIKeysByAnyGroupID(ctx context.Context, groupID int64) (int64, error) {
	if r.sql == nil {
		return 0, nil
	}
	var count int64
	err := scanSingleRow(ctx, r.sql, `
		SELECT COUNT(*)
		FROM api_keys AS k
		WHERE k.deleted_at IS NULL
		  AND (k.group_id = $1
		       OR EXISTS (SELECT 1 FROM api_key_groups AS akg
		                  WHERE akg.api_key_id = k.id AND akg.group_id = $1))`,
		[]any{groupID}, &count)
	if err != nil {
		if isUndefinedTableError(err) {
			return 0, nil
		}
		return 0, err
	}
	return count, nil
}

// ListAPIKeyStringsByAnyGroupID 返回「主分组或任一绑定分组命中」的未软删 key 明文。
// 仅供认证缓存失效使用（与既有 ListKeysByGroupID 同语义，扩展为 EXISTS）。
func (r *apiKeyRepository) ListAPIKeyStringsByAnyGroupID(ctx context.Context, groupID int64) ([]string, error) {
	if r.sql == nil {
		return nil, nil
	}
	rows, err := r.sql.QueryContext(ctx, `
		SELECT k.key
		FROM api_keys AS k
		WHERE k.deleted_at IS NULL
		  AND k.key <> ''
		  AND (k.group_id = $1
		       OR EXISTS (SELECT 1 FROM api_key_groups AS akg
		                  WHERE akg.api_key_id = k.id AND akg.group_id = $1))`, groupID)
	if err != nil {
		if isUndefinedTableError(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	keys := make([]string, 0, 16)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// RemoveGroupFromAllAPIKeys 从所有 key 的绑定集合中移除某分组，并同步主分组列：
// 主分组指向被删分组时回填剩余的、顺序最靠前的绑定分组；没有剩余绑定时清空。
// 返回被删除的关联行数（用于日志/统计）。
func (r *apiKeyRepository) RemoveGroupFromAllAPIKeys(ctx context.Context, groupID int64) (int64, error) {
	if r.sql == nil {
		return 0, nil
	}

	var affected int64
	err := r.execInTx(ctx, func(exec sqlExecutor) error {
		res, err := exec.ExecContext(ctx, `DELETE FROM api_key_groups WHERE group_id = $1`, groupID)
		if err != nil {
			return err
		}
		if n, rowsErr := res.RowsAffected(); rowsErr == nil {
			affected = n
		}

		// 需要回填主分组的 key：主分组 = 被删分组，且仍有其它绑定。
		if _, err := exec.ExecContext(ctx, `
			UPDATE api_keys AS k
			SET group_id = replacement.group_id, updated_at = NOW()
			FROM (
				SELECT DISTINCT ON (k2.id) k2.id AS api_key_id, akg.group_id
				FROM api_keys AS k2
				JOIN api_key_groups AS akg ON akg.api_key_id = k2.id
				WHERE k2.group_id = $1 AND k2.deleted_at IS NULL
				ORDER BY k2.id, akg.sort_order ASC, akg.group_id ASC
			) AS replacement
			WHERE k.id = replacement.api_key_id`, groupID); err != nil {
			return err
		}

		// 主分组 = 被删分组，且已无任何绑定：清空主分组（与 ClearGroupIDByGroupID 语义一致）。
		_, err = exec.ExecContext(ctx, `
			UPDATE api_keys
			SET group_id = NULL, updated_at = NOW()
			WHERE group_id = $1
			  AND deleted_at IS NULL
			  AND NOT EXISTS (
			        SELECT 1 FROM api_key_groups AS akg WHERE akg.api_key_id = api_keys.id
			      )`, groupID)
		return err
	})
	if isUndefinedTableError(err) {
		return 0, nil
	}
	return affected, err
}

// MigrateAPIKeyGroupsOnUserGroupChange 把某用户在 oldGroupID 上的绑定（关联表 + 主分组列）
// 迁移到 newGroupID，保持其余绑定与顺序不变。
func (r *apiKeyRepository) MigrateAPIKeyGroupsOnUserGroupChange(ctx context.Context, userID, oldGroupID, newGroupID int64) (int64, error) {
	if r.sql == nil {
		return 0, nil
	}

	var affected int64
	err := r.execInTx(ctx, func(exec sqlExecutor) error {
		// 先把「已有目标绑定」的旧行删除，避免主键冲突。
		if _, err := exec.ExecContext(ctx, `
			DELETE FROM api_key_groups AS akg
			USING api_keys AS k
			WHERE akg.api_key_id = k.id
			  AND k.user_id = $1
			  AND k.deleted_at IS NULL
			  AND akg.group_id = $2
			  AND EXISTS (
			        SELECT 1 FROM api_key_groups AS dup
			        WHERE dup.api_key_id = akg.api_key_id AND dup.group_id = $3
			      )`, userID, oldGroupID, newGroupID); err != nil {
			return err
		}

		// 再把剩余旧绑定改为新分组。
		res, err := exec.ExecContext(ctx, `
			UPDATE api_key_groups AS akg
			SET group_id = $3
			FROM api_keys AS k
			WHERE akg.api_key_id = k.id
			  AND k.user_id = $1
			  AND k.deleted_at IS NULL
			  AND akg.group_id = $2`, userID, oldGroupID, newGroupID)
		if err != nil {
			return err
		}
		if n, rowsErr := res.RowsAffected(); rowsErr == nil {
			affected = n
		}

		// 最后同步主分组列（与既有 UpdateGroupIDByUserAndGroup 语义一致）。
		_, err = exec.ExecContext(ctx, `
			UPDATE api_keys
			SET group_id = $3, updated_at = NOW()
			WHERE user_id = $1 AND group_id = $2 AND deleted_at IS NULL`,
			userID, oldGroupID, newGroupID)
		return err
	})
	if isUndefinedTableError(err) {
		return 0, nil
	}
	return affected, err
}

// ── 内部工具 ─────────────────────────────────────────────────────────────────

// apiKeyIDsByGroupID 返回关联表上绑定该分组的 key ID（用于与主分组列取并集）。
func (r *apiKeyRepository) apiKeyIDsByGroupID(ctx context.Context, groupID int64) ([]int64, error) {
	if r.sql == nil {
		return nil, nil
	}
	return r.queryGroupIDsForColumn(ctx, `SELECT api_key_id FROM api_key_groups WHERE group_id = $1`, groupID)
}

func (r *apiKeyRepository) queryGroupIDsForColumn(ctx context.Context, query string, groupID int64) ([]int64, error) {
	rows, err := r.sql.QueryContext(ctx, query, groupID)
	if err != nil {
		if isUndefinedTableError(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	ids := make([]int64, 0, 8)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// attachGroupIDs 为批量 key 补齐候选分组集合（主分组优先），供接口层输出 group_ids。
func (r *apiKeyRepository) attachGroupIDs(ctx context.Context, keys []service.APIKey) {
	if len(keys) == 0 || r.sql == nil {
		return
	}
	ids := make([]int64, 0, len(keys))
	for i := range keys {
		ids = append(ids, keys[i].ID)
	}
	bound, err := r.ListGroupIDsByAPIKeyIDs(ctx, ids)
	if err != nil {
		return
	}
	for i := range keys {
		groupIDs := bound[keys[i].ID]
		if len(groupIDs) == 0 {
			// 关联表无记录（未回填）：退化为主分组单元素集合。
			groupIDs = singleGroupFallback(keys[i].GroupID)
		}
		keys[i].GroupIDs = normalizeAPIKeyGroupIDs(groupIDs, keys[i].GroupID)
	}
}

func (r *apiKeyRepository) queryGroupIDs(ctx context.Context, query string, args ...any) ([]int64, error) {
	rows, err := r.sql.QueryContext(ctx, query, args...)
	if err != nil {
		if isUndefinedTableError(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	ids := make([]int64, 0, 4)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// execInTx 让多条原生 SQL 在同一事务内执行，避免中途失败留下半完成状态。
// 执行器不支持事务时（测试桩）退化为顺序执行。
func (r *apiKeyRepository) execInTx(ctx context.Context, fn func(exec sqlExecutor) error) error {
	if r == nil || r.sql == nil {
		return nil
	}
	beginner, ok := r.sql.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	if !ok {
		return fn(r.sql)
	}
	tx, err := beginner.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin api_key_groups tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit api_key_groups tx: %w", err)
	}
	return nil
}

// normalizeAPIKeyGroupIDs 去重 + 去除非正值 + 主分组置首 + 其余保持传入顺序。
//
// 顺序契约：groupIDs 的传入顺序即用户勾选顺序（service 层 normalizeAPIKeyGroupIDsForService
// 已保序），primaryGroupID 置首后其余元素不得重排——sort_order 直接由该顺序的下标决定，
// 而「模型不可知」请求会回退到主分组，顺序错了计费/RPM/日志分组就全错。
func normalizeAPIKeyGroupIDs(groupIDs []int64, primaryGroupID *int64) []int64 {
	seen := make(map[int64]struct{}, len(groupIDs)+1)
	out := make([]int64, 0, len(groupIDs)+1)

	if primaryGroupID != nil && *primaryGroupID > 0 {
		seen[*primaryGroupID] = struct{}{}
		out = append(out, *primaryGroupID)
	}
	for _, id := range groupIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// attachCandidateGroups 为单个 key 填充候选分组【对象】集合 apiKey.Groups。
//
// 背景（P0 修复）：请求期「生效分组」决议（ResolveEffectiveGroup）与鉴权层
// apiKeyGroupCandidates 都从 apiKey.Groups 读候选分组对象。此前 repo 层只填了
// GroupIDs，从未物化非主分组对象 → 快照 Groups 为空 → 反序列化时把「ID 不在
// 快照对象里」的候选全部跳过 → 决议永远退化为「只看主分组」。
//
// 语义：
//   - 与 GroupIDs 严格同序（主分组在前，其余保持勾选顺序）；
//   - 只物化仍存在的分组；已删除的分组在 GroupIDs 中保留（由鉴权层按语义报错）；
//   - 单分组 key 保持既有行为（不写 Groups），既有退化路径零变化；
//   - 失败时静默退化（保留 apiKey.Group 单元素集合由调用方兜底），不阻断认证热路径。
func (r *apiKeyRepository) attachCandidateGroups(ctx context.Context, apiKey *service.APIKey) {
	if apiKey == nil || r == nil || r.client == nil {
		return
	}
	if len(apiKey.GroupIDs) == 0 {
		return
	}

	// 主分组对象可能缺失（主分组已被软删除，WithGroup 无法带出）。
	// 此时仍必须物化其余候选分组：冻结契约要求「候选集任一可用即放行」，
	// 不因主分组被删就丢掉其他可用候选。
	primary := apiKey.Group
	primaryID := int64(0)
	if primary != nil {
		primaryID = primary.ID
	}

	// 收集需要额外物化的非主分组 ID（保持 GroupIDs 顺序）。
	missing := make([]int64, 0, len(apiKey.GroupIDs))
	for _, groupID := range apiKey.GroupIDs {
		if groupID <= 0 || groupID == primaryID {
			continue
		}
		missing = append(missing, groupID)
	}
	if len(missing) == 0 {
		// 单分组 key（或主分组是唯一候选）：保持既有行为（不写 Groups），
		// 由调用方与鉴权层按「主分组单元素」退化处理，避免改变存量路径的可观测行为。
		return
	}

	byID := make(map[int64]*service.Group, len(missing))
	for start := 0; start < len(missing); start += postgresParameterBatchSize {
		end := start + postgresParameterBatchSize
		if end > len(missing) {
			end = len(missing)
		}
		groups, err := r.client.Group.Query().Where(dbgroup.IDIn(missing[start:end]...)).All(ctx)
		if err != nil {
			// 认证热路径：物化失败不阻断请求；Groups 保持为空，
			// 决议退化为「主分组单元素」的既有兜底（与修复前行为一致）。
			return
		}
		for _, g := range groups {
			byID[g.ID] = groupEntityToService(g)
		}
	}

	candidates := make([]*service.Group, 0, len(apiKey.GroupIDs))
	for _, groupID := range apiKey.GroupIDs {
		if groupID <= 0 {
			continue
		}
		if groupID == primaryID {
			candidates = append(candidates, primary)
			continue
		}
		// 已删除/不可见的分组跳过：决议层因此绝不会选中它，
		// 与鉴权契约「至少一个可用即放行」配合工作。
		if g := byID[groupID]; g != nil {
			candidates = append(candidates, g)
		}
	}
	if len(candidates) == 0 {
		return
	}
	apiKey.Groups = candidates
}

func singleGroupFallback(primaryGroupID *int64) []int64 {
	if primaryGroupID == nil || *primaryGroupID <= 0 {
		return nil
	}
	return []int64{*primaryGroupID}
}

// isUndefinedTableError 识别「关联表不存在」的执行错误（PostgreSQL 42P01 / SQLite
// "no such table"），用于迁移尚未执行或轻量测试库上的优雅降级，避免认证链路因
// 关联表缺失而整体不可用。
func isUndefinedTableError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "42P01") {
		return true
	}
	if strings.Contains(msg, "api_key_groups") &&
		(strings.Contains(msg, "no such table") || strings.Contains(msg, "relation") || strings.Contains(msg, "does not exist")) {
		return true
	}
	return false
}

// 编译期断言：新增方法必须在 apiKeyRepository 上可用。
var _ interface {
	ListGroupIDsByAPIKeyID(ctx context.Context, apiKeyID int64) ([]int64, error)
	ReplaceAPIKeyGroups(ctx context.Context, apiKeyID int64, groupIDs []int64, primaryGroupID *int64) error
	CountAPIKeysByAnyGroupID(ctx context.Context, groupID int64) (int64, error)
	ListAPIKeyStringsByAnyGroupID(ctx context.Context, groupID int64) ([]string, error)
	RemoveGroupFromAllAPIKeys(ctx context.Context, groupID int64) (int64, error)
	MigrateAPIKeyGroupsOnUserGroupChange(ctx context.Context, userID, oldGroupID, newGroupID int64) (int64, error)
} = (*apiKeyRepository)(nil)

// 提示：本次新增方法通过 service 包内的窄接口（APIKeyGroupBindingRepository）
// 做类型断言调用，保持依赖方向为 service → repository。
