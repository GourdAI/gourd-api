//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// api_key_repo_groups_integration_test.go 在真实 PostgreSQL 上验证「一个 API Key 绑定多个分组」：
//
//	① 迁移 241 建表 + 回填（单分组 key 自动得到一条关联行）；
//	② GetByKeyForAuth 取回候选分组集合（主分组优先）；
//	③ ReplaceAPIKeyGroups 整体替换（含解绑全部）；
//	④ 关联表变更触发认证缓存失效 outbox（迁移 241 重写的触发器）；
//	⑤ 分组配置变更（groups UPDATE）同时失效「关联表命中」的 key（EXISTS 语义扩展）。
//
// 这些是认证快照正确性的关键护栏：触发器漏改会让分组改配置后快照永久陈旧，
// 关联表读写口径不一致会让请求期决议拿到错分组。

func TestAPIKeyGroupsIntegration(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	// NewAPIKeyRepository 返回 service.APIKeyRepository 接口，关联表读写方法在具体类型上，
	// 这里通过窄接口断言获取（与 service 层同款做法，不扩大接口面）。
	apiKeyRepo := NewAPIKeyRepository(integrationEntClient, integrationDB)
	groupsRepo, ok := apiKeyRepo.(interface {
		ListGroupIDsByAPIKeyID(ctx context.Context, apiKeyID int64) ([]int64, error)
		ReplaceAPIKeyGroups(ctx context.Context, apiKeyID int64, groupIDs []int64, primaryGroupID *int64) error
	})
	require.True(t, ok, "repository 必须实现多分组关联表读写方法")

	primary := mustCreateGroup(t, integrationEntClient, &service.Group{
		Name:     fmt.Sprintf("multi-group-primary-%d", suffix),
		Platform: service.PlatformOpenAI,
		Status:   service.StatusActive,
	})
	secondary := mustCreateGroup(t, integrationEntClient, &service.Group{
		Name:     fmt.Sprintf("multi-group-secondary-%d", suffix),
		Platform: service.PlatformOpenAI,
		Status:   service.StatusActive,
	})
	user := mustCreateUser(t, integrationEntClient, &service.User{
		Email: fmt.Sprintf("multi-group-%d@example.com", suffix), Concurrency: 5,
	})

	primaryID := primary.ID
	secondaryID := secondary.ID
	keyValue := fmt.Sprintf("tk-multi-group-%d", suffix)
	key := &service.APIKey{
		UserID:  user.ID,
		GroupID: &primaryID,
		Key:     keyValue,
		Name:    "multi-group",
		Status:  service.StatusActive,
	}

	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM auth_cache_invalidation_outbox WHERE cache_key = encode(sha256(convert_to($1, 'UTF8')), 'hex')", keyValue)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE id = $1", key.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id = $1", user.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id = ANY($1::bigint[])", fmt.Sprintf("{%d,%d}", primaryID, secondaryID))
		require.NoError(t, err)
	})

	require.NoError(t, apiKeyRepo.Create(ctx, key))

	// ① 单分组回填：Create 不写关联表 → 认证查询必须退化为单分组语义（向后兼容）。
	got, err := apiKeyRepo.GetByKeyForAuth(ctx, keyValue)
	require.NoError(t, err)
	require.Equal(t, []int64{primaryID}, got.GroupIDs, "未写关联表时应退化为主分组单元素集合")

	// ② ReplaceAPIKeyGroups 写入多分组：主分组置首，其余按升序。
	require.NoError(t, groupsRepo.ReplaceAPIKeyGroups(ctx, key.ID, []int64{secondaryID, primaryID}, &primaryID))

	got, err = apiKeyRepo.GetByKeyForAuth(ctx, keyValue)
	require.NoError(t, err)
	require.Equal(t, []int64{primaryID, secondaryID}, got.GroupIDs, "候选分组集合必须主分组优先且确定性排序")
	require.NotNil(t, got.Group, "主分组对象必须继续带出（兼容 group 字段）")
	require.Equal(t, primaryID, got.Group.ID)

	// ②.5 【P0 回归】候选分组【对象】必须完整物化并与 GroupIDs 同序。
	// 历史缺口：repo 只填 GroupIDs 不填 Groups → 快照 Groups 为空 → 反序列化跳过
	// 非主分组 → 决议永远退化为「只看主分组」，多分组 key 的计费/RPM/日志全错。
	require.Len(t, got.Groups, 2, "候选分组对象必须完整物化（否则多分组决议永久失效）")
	require.Equal(t, primaryID, got.Groups[0].ID, "Groups[0] 必须是主分组")
	require.Equal(t, secondaryID, got.Groups[1].ID, "Groups[1] 必须与 GroupIDs 同序")
	require.Equal(t, service.PlatformOpenAI, got.Groups[1].Platform, "次分组对象必须水合完整字段（platform）")
	require.Equal(t, service.StatusActive, got.Groups[1].Status)
	require.True(t, got.Groups[1].Hydrated, "次分组对象必须标记已水合")

	// ListGroupIDsByAPIKeyID 与认证查询口径一致。
	listed, err := groupsRepo.ListGroupIDsByAPIKeyID(ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{primaryID, secondaryID}, listed)

	// ③ EXISTS 语义：任一绑定命中均计入。
	count, err := apiKeyRepo.CountByGroupID(ctx, secondaryID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, count, int64(1), "次分组应通过关联表命中")

	// ④ 关联表变更必须触发认证缓存失效（outbox 增加一条本 key 的记录）。
	outboxCountFor := func() int64 {
		var n int64
		require.NoError(t, integrationDB.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM auth_cache_invalidation_outbox WHERE cache_key = encode(sha256(convert_to($1, 'UTF8')), 'hex')",
			keyValue).Scan(&n))
		return n
	}
	beforeReplace := outboxCountFor()
	require.NoError(t, groupsRepo.ReplaceAPIKeyGroups(ctx, key.ID, []int64{secondaryID, primaryID}, &secondaryID))
	require.Greater(t, outboxCountFor(), beforeReplace,
		"关联表变更必须写入认证缓存失效 outbox（否则分组改绑后快照永久陈旧）")

	// ⑤ groups 配置变更必须失效「仅通过关联表绑定」该分组的 key。
	var groupID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT id FROM groups LIMIT 1").Scan(&groupID))
	beforeGroupUpdate := outboxCountFor()
	_, err = integrationDB.ExecContext(ctx, `
		UPDATE groups SET rate_multiplier = rate_multiplier + 0.01 WHERE id = $1`, secondaryID)
	require.NoError(t, err)
	require.Greater(t, outboxCountFor(), beforeGroupUpdate,
		"groups 配置变更必须同时失效关联表命中的 key（触发器已扩展为 EXISTS 语义）")

	// ⑥ 解绑全部：主分组列与关联表都清空。
	require.NoError(t, groupsRepo.ReplaceAPIKeyGroups(ctx, key.ID, nil, nil))
	listed, err = groupsRepo.ListGroupIDsByAPIKeyID(ctx, key.ID)
	require.NoError(t, err)
	require.Empty(t, listed, "解绑全部后关联表必须为空")
}
