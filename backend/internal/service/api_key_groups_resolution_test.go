//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// api_key_groups_resolution_test.go 覆盖「一个 API Key 绑定多个分组」的请求期生效分组决议。
//
// 必须覆盖的场景（任务冻结要求）：
//   ① 单分组 key 行为完全不变（回归保护）；
//   ② 多分组 key 候选全停用才拒；
//   ③ 决议按模型选中正确分组，并列时按账号优先级；
//   ④ 快照往返（snapshot→APIKey）后候选分组与主分组不丢失；
//   ⑤ 快照版本号不匹配时旧快照不被采用。

func testGroup(id int64, status string) *Group {
	return &Group{
		ID:       id,
		Name:     "group",
		Platform: PlatformOpenAI,
		Status:   status,
		Hydrated: true,
	}
}

// ① 单分组 key：候选唯一 → 直接采用，且 GroupID/Group 与历史完全一致。
func TestResolveEffectiveGroup_SingleGroupUnchanged(t *testing.T) {
	groupID := int64(11)
	group := testGroup(groupID, StatusActive)
	apiKey := &APIKey{
		ID:       1,
		GroupID:  &groupID,
		Group:    group,
		GroupIDs: []int64{groupID},
		Groups:   []*Group{group},
	}

	decision := ResolveEffectiveGroup(apiKey, "gpt-5", func(int64) (bool, int) { return true, 0 })

	require.NotNil(t, decision)
	require.Equal(t, groupID, decision.GroupID)
	require.Equal(t, EffectiveGroupReasonSingleCandidate, decision.Reason)
	require.Equal(t, group, decision.Group)

	// 就地覆写必须是无副作用的：单分组场景下覆写返回原对象（同一指针）。
	overwritten := EffectiveAPIKeyWithGroup(apiKey, decision.Group)
	require.Same(t, apiKey, overwritten, "单分组 key 的生效分组与当前分组一致时不得克隆，保证零额外分配")
	require.Equal(t, &groupID, overwritten.GroupID)
	require.Equal(t, group, overwritten.Group)
}

// ② 多分组 key：只要有一个候选可用即放行；全部停用才拒。
// 该用例直接驱动鉴权层的候选集合判定（validateAPIKeyGroupAvailable 的等价逻辑）。
func TestMultiGroupCandidateAvailability(t *testing.T) {
	groupID := int64(1)
	secondID := int64(2)

	t.Run("one active among disabled passes", func(t *testing.T) {
		active := testGroup(groupID, StatusActive)
		disabled := testGroup(secondID, StatusDisabled)
		apiKey := &APIKey{
			GroupID:  &groupID,
			Group:    disabled, // 主分组停用
			GroupIDs: []int64{groupID, secondID},
			Groups:   []*Group{disabled, active},
		}

		available := availableAPIKeyGroups(apiKey)
		require.Len(t, available, 1, "候选集合中应只有 1 个可用分组")
		require.Equal(t, groupID, available[0].ID)
	})

	t.Run("all disabled yields empty candidate set", func(t *testing.T) {
		apiKey := &APIKey{
			GroupID:  &groupID,
			Group:    testGroup(groupID, StatusDisabled),
			GroupIDs: []int64{groupID, secondID},
			Groups:   []*Group{testGroup(groupID, StatusDisabled), testGroup(secondID, StatusDisabled)},
		}
		require.Empty(t, availableAPIKeyGroups(apiKey), "候选全停用 → 无可用分组，鉴权层应拒绝")
	})

	t.Run("deleted and disabled mix yields empty candidate set", func(t *testing.T) {
		apiKey := &APIKey{
			GroupID:  &groupID,
			Group:    testGroup(groupID, "deleted"),
			GroupIDs: []int64{groupID, secondID},
			Groups:   []*Group{testGroup(groupID, "deleted"), testGroup(secondID, StatusDisabled)},
		}
		require.Empty(t, availableAPIKeyGroups(apiKey))
	})
}

// ③ 决议按模型选中正确分组；并列时按账号优先级。
func TestResolveEffectiveGroup_ModelAware(t *testing.T) {
	primaryID := int64(100)
	secondaryID := int64(200)
	primary := testGroup(primaryID, StatusActive)
	secondary := testGroup(secondaryID, StatusActive)

	apiKey := &APIKey{
		ID:       7,
		GroupID:  &primaryID, // 主分组 = 100
		Group:    primary,
		GroupIDs: []int64{primaryID, secondaryID},
		Groups:   []*Group{primary, secondary},
	}

	t.Run("picks the only group that can serve the model", func(t *testing.T) {
		probe := func(groupID int64) (bool, int) {
			// 主分组 100 无法服务；200 可以 → 必须切到 200。
			return groupID == secondaryID, 5
		}
		decision := ResolveEffectiveGroup(apiKey, "claude-opus-4", probe)
		require.Equal(t, secondaryID, decision.GroupID)
		require.Equal(t, EffectiveGroupReasonModelAware, decision.Reason)
		require.Equal(t, secondary, decision.Group)

		// 就地覆写：GroupID 与 Group 必须同步替换，GroupIDs/Groups 保持不变。
		overwritten := EffectiveAPIKeyWithGroup(apiKey, decision.Group)
		require.NotSame(t, apiKey, overwritten, "生效分组与当前分组不同必须克隆，避免污染共享快照对象")
		require.Equal(t, &secondaryID, overwritten.GroupID)
		require.Equal(t, secondary, overwritten.Group)
		require.Equal(t, []int64{primaryID, secondaryID}, overwritten.GroupIDs)
		require.Len(t, overwritten.Groups, 2)
		// 原对象不得被修改（共享快照 / L1 缓存必须保持干净）。
		require.Equal(t, &primaryID, apiKey.GroupID)
		require.Equal(t, primary, apiKey.Group)
	})

	t.Run("tie broken by account priority", func(t *testing.T) {
		// 两个分组都能服务，主分组优先级较差（10）→ 应选优先级更优（3）的次分组。
		probe := func(groupID int64) (bool, int) {
			if groupID == primaryID {
				return true, 10
			}
			return true, 3
		}
		decision := ResolveEffectiveGroup(apiKey, "claude-opus-4", probe)
		require.Equal(t, secondaryID, decision.GroupID, "并列时应按账号优先级（小者优先）选出更优分组")
		require.Equal(t, EffectiveGroupReasonModelAware, decision.Reason)
	})

	t.Run("tie broken in favor of primary group when priorities equal", func(t *testing.T) {
		probe := func(int64) (bool, int) { return true, 7 }
		decision := ResolveEffectiveGroup(apiKey, "claude-opus-4", probe)
		require.Equal(t, primaryID, decision.GroupID, "优先级并列时主分组优先（契约第 2 条）")
	})

	t.Run("no serviceable group falls back to primary", func(t *testing.T) {
		probe := func(int64) (bool, int) { return false, 0 }
		decision := ResolveEffectiveGroup(apiKey, "unknown-model", probe)
		require.Equal(t, primaryID, decision.GroupID, "无法判定时回退主分组")
		require.Equal(t, EffectiveGroupReasonPrimary, decision.Reason)
	})

	t.Run("empty model falls back to primary", func(t *testing.T) {
		decision := ResolveEffectiveGroup(apiKey, "", func(int64) (bool, int) { return true, 1 })
		require.Equal(t, primaryID, decision.GroupID)
		require.Equal(t, EffectiveGroupReasonPrimary, decision.Reason)
	})

	t.Run("primary group left the candidate set falls back to first candidate", func(t *testing.T) {
		apiKeyMissingPrimary := &APIKey{
			GroupID:  &primaryID, // 主分组 100 已不在候选集内
			Group:    primary,
			GroupIDs: []int64{secondaryID},
			Groups:   []*Group{secondary},
		}
		decision := ResolveEffectiveGroup(apiKeyMissingPrimary, "", nil)
		require.Equal(t, secondaryID, decision.GroupID, "主分组不在候选集时取候选集第一个（确定性排序）")
	})
}

// ④ 快照往返（snapshot→APIKey）后候选分组与主分组不丢失。
func TestAuthSnapshotRoundTripPreservesCandidateGroups(t *testing.T) {
	primaryID := int64(301)
	secondaryID := int64(302)
	primary := testGroup(primaryID, StatusActive)
	primary.RateMultiplier = 0.5
	primary.ProfitControlEnabled = true
	primary.ProfitMinMargin = 0.2
	primary.ProfitSafetyBuffer = 0.05
	secondary := testGroup(secondaryID, StatusActive)
	secondary.RateMultiplier = 0.8
	secondary.RPMLimit = 42

	apiKey := &APIKey{
		ID:       55,
		UserID:   9,
		Key:      "tk-round-trip",
		Name:     "round-trip",
		Status:   StatusActive,
		GroupID:  &primaryID,
		Group:    primary,
		GroupIDs: []int64{primaryID, secondaryID},
		Groups:   []*Group{primary, secondary},
		User:     &User{ID: 9, Status: StatusActive, Role: RoleUser},
	}

	svc := &APIKeyService{}

	snapshot := svc.snapshotFromAPIKey(context.Background(), apiKey)
	require.NotNil(t, snapshot)
	require.Equal(t, apiKeyAuthSnapshotVersion, snapshot.Version)
	require.Equal(t, []int64{primaryID, secondaryID}, snapshot.GroupIDs, "候选分组集合必须进入快照")
	require.Len(t, snapshot.Groups, 2, "候选分组对象必须进入快照")

	restored := svc.snapshotToAPIKey("tk-round-trip", snapshot)
	require.NotNil(t, restored)

	// 主分组：GroupID/Group 必须与快照一致。
	require.NotNil(t, restored.GroupID)
	require.Equal(t, primaryID, *restored.GroupID)
	require.NotNil(t, restored.Group)
	require.Equal(t, primaryID, restored.Group.ID)
	require.InDelta(t, 0.5, restored.Group.RateMultiplier, 1e-9)
	require.True(t, restored.Group.ProfitControlEnabled, "利润控制字段必须随快照往返（硬约束）")
	require.InDelta(t, 0.2, restored.Group.ProfitMinMargin, 1e-9)
	require.InDelta(t, 0.05, restored.Group.ProfitSafetyBuffer, 1e-9)

	// 候选分组：GroupIDs 与 Groups 必须完整往返，且与快照同序。
	require.Equal(t, []int64{primaryID, secondaryID}, restored.GroupIDs, "候选分组 ID 集合往返不得丢失")
	require.Len(t, restored.Groups, 2, "候选分组对象往返不得丢失")
	require.Equal(t, primaryID, restored.Groups[0].ID)
	require.Equal(t, secondaryID, restored.Groups[1].ID)
	require.InDelta(t, 0.8, restored.Groups[1].RateMultiplier, 1e-9, "次分组的计费倍率必须进入快照")
	require.Equal(t, 42, restored.Groups[1].RPMLimit, "次分组的组级 RPM 必须进入快照")

	// 往返后仍能正确决议：模型只能由次分组服务时切到次分组。
	probe := func(groupID int64) (bool, int) { return groupID == secondaryID, 1 }
	decision := ResolveEffectiveGroup(restored, "only-secondary", probe)
	require.Equal(t, secondaryID, decision.GroupID)
}

// ⑤ 快照版本号不匹配时旧快照不被采用（强制重新回源，避免新旧实例混读）。
func TestAuthSnapshotVersionMismatchRejected(t *testing.T) {
	svc := &APIKeyService{}

	stale := &APIKeyAuthCacheEntry{Snapshot: &APIKeyAuthSnapshot{
		Version:  apiKeyAuthSnapshotVersion - 1,
		APIKeyID: 1,
		UserID:   2,
		GroupID:  int64Ptr(3),
	}}

	apiKey, used, err := svc.applyAuthCacheEntry("sk-stale", stale)
	require.NoError(t, err)
	require.False(t, used, "版本号不匹配的旧快照不得被采用")
	require.Nil(t, apiKey)
}

// 附加：候选集合为空时的守卫行为（不应 panic，且决议结果为无候选）。
func TestResolveEffectiveGroupNoCandidates(t *testing.T) {
	decision := ResolveEffectiveGroup(nil, "m", nil)
	require.Equal(t, EffectiveGroupReasonNoCandidates, decision.Reason)
	require.Nil(t, decision.Group)

	empty := &APIKey{ID: 1, GroupID: nil}
	decision = ResolveEffectiveGroup(empty, "m", nil)
	require.Equal(t, EffectiveGroupReasonNoCandidates, decision.Reason)
}

// 附加：单分组 key 的候选集合归一化（主分组置首、去重、去非正值）。
func TestResolveEffectiveGroupCandidateNormalization(t *testing.T) {
	primaryID := int64(5)
	group := testGroup(primaryID, StatusActive)
	apiKey := &APIKey{
		GroupID:  &primaryID,
		Group:    group,
		GroupIDs: []int64{primaryID},
		Groups:   []*Group{group},
	}
	decision := ResolveEffectiveGroup(apiKey, "m", nil)
	require.Len(t, decision.Candidates, 1)
	require.Equal(t, primaryID, decision.Candidates[0].ID)
}
