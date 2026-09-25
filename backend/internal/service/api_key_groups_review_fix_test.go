//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// api_key_groups_review_fix_test.go 是本次 code review 修复的回归保护。
//
// 三条修复各自都必须有「改了必挂」的断言（上一轮的教训：决议测试用 primaryID=100
// < secondaryID=200，升序恰好等于勾选顺序，把主分组排序 bug 完美掩盖）：
//   P0  主分组 = 用户勾选顺序的第一个，不得重排为 ID 最小者；
//   P1① 决议层必须复查用户授权（CanBindGroup），授权被撤销的候选不得入选；
//   P1② 生效分组切换时必须清掉按主分组预物化的 UserGroupRPMOverride。

// ── P0：主分组保序 ────────────────────────────────────────────────────────────

// 前端按勾选顺序提交 group_ids，并以 group_ids[0] 作为主分组
// （frontend/src/api/keys.ts：payload.group_id = groupIds[0]）。
// 回归：用户先选 10 再选 3，主分组必须是 10，而不是 ID 更小的 3。
func TestResolveRequestedGroupIDs_PrimaryIsUserPickedOrder(t *testing.T) {
	groupID := int64(10)
	groupIDs := []int64{10, 3}

	resolved, primary, provided := resolveRequestedGroupIDs(&groupID, &groupIDs)

	require.True(t, provided)
	require.NotNil(t, primary)
	require.Equal(t, int64(10), *primary, "主分组必须是勾选顺序的第一个 group_ids[0]，不得换成 ID 最小者")
	require.Equal(t, []int64{10, 3}, resolved, "候选集合必须逐位保持勾选顺序")
}

// 去重/去非正值后仍保序，且主分组取归一化后的首个。
func TestResolveRequestedGroupIDs_KeepsOrderAfterDedup(t *testing.T) {
	groupIDs := []int64{9, 0, 4, 9, -2, 7}

	resolved, primary, provided := resolveRequestedGroupIDs(nil, &groupIDs)

	require.True(t, provided)
	require.Equal(t, []int64{9, 4, 7}, resolved, "去重去非正值后必须保持首次出现顺序")
	require.NotNil(t, primary)
	require.Equal(t, int64(9), *primary)
}

// 空数组 = 解绑全部（provided=true、集合与主分组均为空）。
func TestResolveRequestedGroupIDs_EmptyArrayUnbinds(t *testing.T) {
	empty := []int64{}

	resolved, primary, provided := resolveRequestedGroupIDs(nil, &empty)

	require.True(t, provided)
	require.Nil(t, resolved)
	require.Nil(t, primary)
}

// 只传 group_id（老客户端）：等价单元素列表，行为不变。
func TestResolveRequestedGroupIDs_SingleGroupIDUnchanged(t *testing.T) {
	groupID := int64(10)

	resolved, primary, provided := resolveRequestedGroupIDs(&groupID, nil)

	require.True(t, provided)
	require.Equal(t, []int64{10}, resolved)
	require.Equal(t, int64(10), *primary)
}

// ── P1①：决议层授权复查 ───────────────────────────────────────────────────────

// 专属分组的授权被撤销后，即使该分组状态 active 且「能服务模型」，
// 也不得被决议为生效分组（否则用户以已撤销的专属分组身份持续消费）。
func TestAvailableAPIKeyGroups_FiltersUnauthorizedExclusiveGroup(t *testing.T) {
	authorizedID := int64(1)
	revokedID := int64(2)

	authorized := testGroup(authorizedID, StatusActive)
	authorized.IsExclusive = true

	revoked := testGroup(revokedID, StatusActive)
	revoked.IsExclusive = true

	// 用户只被授权分组 1；分组 2 的授权已被 admin 撤销。
	user := &User{
		ID:                   42,
		Status:               StatusActive,
		AllowedGroups:        []int64{authorizedID},
		RestrictPublicGroups: true,
	}

	apiKey := &APIKey{
		ID:       7,
		User:     user,
		GroupID:  &authorizedID,
		Group:    authorized,
		GroupIDs: []int64{authorizedID, revokedID},
		Groups:   []*Group{authorized, revoked},
	}

	got := availableAPIKeyGroups(apiKey)

	require.Len(t, got, 1, "已撤销授权的专属分组必须被过滤出候选集")
	require.Equal(t, authorizedID, got[0].ID)

	// 端到端：即便探针声称「只有 revoked 分组能服务该模型」，决议也不得选它。
	probe := func(groupID int64) (bool, int) { return groupID == revokedID, 0 }
	decision := ResolveEffectiveGroup(apiKey, "some-model", probe)
	require.NotNil(t, decision.Group)
	require.Equal(t, authorizedID, decision.GroupID, "决议不得选中已撤销授权的分组（越权）")
}

// 非专属公开分组：用户未开启 RestrictPublicGroups 时可绑定，候选保留。
func TestAvailableAPIKeyGroups_KeepsPublicGroupForUnrestrictedUser(t *testing.T) {
	publicID := int64(5)
	public := testGroup(publicID, StatusActive)
	public.IsExclusive = false

	user := &User{ID: 42, Status: StatusActive, RestrictPublicGroups: false}
	apiKey := &APIKey{
		ID:       7,
		User:     user,
		GroupID:  &publicID,
		Group:    public,
		GroupIDs: []int64{publicID},
		Groups:   []*Group{public},
	}

	got := availableAPIKeyGroups(apiKey)
	require.Len(t, got, 1)
	require.Equal(t, publicID, got[0].ID)
}

// 订阅型分组不在决议层复查授权（订阅有效性由运行期订阅校验负责），
// 与鉴权层 validateAPIKeyGroupAllowed 对订阅型候选无条件放行的口径一致。
func TestAvailableAPIKeyGroups_SubscriptionGroupNotFilteredByCanBind(t *testing.T) {
	subID := int64(8)
	sub := testGroup(subID, StatusActive)
	sub.SubscriptionType = SubscriptionTypeSubscription
	sub.IsExclusive = true

	// 用户 AllowedGroups 为空，但订阅型分组仍应保留在候选集。
	user := &User{ID: 42, Status: StatusActive, RestrictPublicGroups: true}
	apiKey := &APIKey{
		ID:       7,
		User:     user,
		GroupID:  &subID,
		Group:    sub,
		GroupIDs: []int64{subID},
		Groups:   []*Group{sub},
	}

	got := availableAPIKeyGroups(apiKey)
	require.Len(t, got, 1, "订阅型分组的有效性由运行期订阅校验负责，决议层不按 CanBindGroup 过滤")
	require.Equal(t, subID, got[0].ID)
}

// User 为 nil 时 fail-open（保留既有单分组行为与测试替身兼容）。
func TestAvailableAPIKeyGroups_NilUserFailsOpen(t *testing.T) {
	id := int64(1)
	group := testGroup(id, StatusActive)
	group.IsExclusive = true

	apiKey := &APIKey{ID: 7, GroupID: &id, Group: group, GroupIDs: []int64{id}, Groups: []*Group{group}}

	got := availableAPIKeyGroups(apiKey)
	require.Len(t, got, 1, "User 为 nil 时跳过授权复查，保持既有行为")
}

// ── P1②：RPM override 随生效分组失效 ──────────────────────────────────────────

// 生效分组切换时，按主分组预物化的 UserGroupRPMOverride 必须被清掉，
// 迫使 checkRPM 按生效分组回查 DB；否则会用主分组的阈值卡生效分组的计数桶。
func TestEffectiveAPIKeyWithGroup_ClearsStaleRPMOverride(t *testing.T) {
	primaryID := int64(1)
	effectiveID := int64(2)
	primary := testGroup(primaryID, StatusActive)
	effective := testGroup(effectiveID, StatusActive)

	override := 0 // 主分组上该用户被设为「免检」
	sharedUser := &User{ID: 42, Status: StatusActive, UserGroupRPMOverride: &override}

	apiKey := &APIKey{ID: 7, User: sharedUser, GroupID: &primaryID, Group: primary}

	got := EffectiveAPIKeyWithGroup(apiKey, effective)

	require.NotSame(t, apiKey, got, "分组变化必须返回克隆，不得修改原对象")
	require.Equal(t, effectiveID, *got.GroupID)
	require.Equal(t, effective, got.Group)

	// 关键断言：override 必须被清空，且不能污染共享的 User（认证快照/L1 缓存）。
	require.NotNil(t, sharedUser.UserGroupRPMOverride, "原 User 不得被就地修改（共享快照对象必须干净）")
	require.Equal(t, 0, *sharedUser.UserGroupRPMOverride)
	require.NotSame(t, sharedUser, got.User, "User 必须被克隆后再清空 override")
	require.Nil(t, got.User.UserGroupRPMOverride, "生效分组切换后必须丢弃主分组的 RPM override")
	require.Equal(t, int64(42), got.User.ID, "克隆必须保留 User 的其余字段")
}

// 分组未变化时走热路径：直接复用原对象（不分配、不清 override）。
func TestEffectiveAPIKeyWithGroup_SameGroupReusesPointer(t *testing.T) {
	id := int64(1)
	group := testGroup(id, StatusActive)
	override := 30
	user := &User{ID: 42, Status: StatusActive, UserGroupRPMOverride: &override}
	apiKey := &APIKey{ID: 7, User: user, GroupID: &id, Group: group}

	got := EffectiveAPIKeyWithGroup(apiKey, group)

	require.Same(t, apiKey, got, "生效分组与当前分组一致时必须复用原指针（热路径优化）")
	require.NotNil(t, got.User.UserGroupRPMOverride)
}

// 原本就没有 override 时不必克隆 User（避免无谓分配）。
func TestEffectiveAPIKeyWithGroup_NoOverrideDoesNotCloneUser(t *testing.T) {
	primaryID := int64(1)
	effectiveID := int64(2)
	primary := testGroup(primaryID, StatusActive)
	effective := testGroup(effectiveID, StatusActive)
	user := &User{ID: 42, Status: StatusActive}
	apiKey := &APIKey{ID: 7, User: user, GroupID: &primaryID, Group: primary}

	got := EffectiveAPIKeyWithGroup(apiKey, effective)

	require.NotSame(t, apiKey, got)
	require.Same(t, user, got.User, "无 override 时不需克隆 User")
}
