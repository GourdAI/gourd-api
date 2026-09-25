//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// api_key_groups_single_bind_regression_test.go 锁定 P0 修正：
// 单分组管理接口 AdminUpdateAPIKeyGroupID 必须同步关联表 api_key_groups。
//
// 修正前的缺陷：该接口只写 api_keys.group_id，关联表残留旧候选。认证快照会把
// 列值与残留候选归并（normalizeAPIKeyGroupIDs 产出 [新主, 旧候选...]），于是
// 「把 key 迁出分组 A」这一管理动作不生效：A 仍参与生效分组决议，计费倍率、
// 组级 RPM、usage_logs.group_id 都可能落回 A，且列与表永久漂移。
//
// 语义依据（与用户侧/前端契约一致，非本测试自创）：
//   - resolveRequestedGroupIDs：单个 group_id 等价于一元素 group_ids；
//   - frontend keys.multiGroup.spec.ts："a single `group_id` is equivalent to a
//     one-element `group_ids`"。
// 因此绑定=整体替换为 [groupID]，解绑=清空。

// groupBindingRepoStub 在分组更新替身之上补齐 APIKeyGroupBindingRepository，
// 用于断言关联表替换确实被调用（内嵌替身未实现该接口时会被静默跳过 → 假绿）。
type groupBindingRepoStub struct {
	*apiKeyRepoStubForGroupUpdate
	replaceGroupIDs    []int64
	replacePrimary     *int64
	replaceCalled      int
	replaceAPIKeyIDArg int64
	replaceErr         error
}

func newGroupBindingRepoStub(key *APIKey) *groupBindingRepoStub {
	return &groupBindingRepoStub{apiKeyRepoStubForGroupUpdate: &apiKeyRepoStubForGroupUpdate{key: key}}
}

func (s *groupBindingRepoStub) ListGroupIDsByAPIKeyID(context.Context, int64) ([]int64, error) {
	return nil, nil
}

func (s *groupBindingRepoStub) ReplaceAPIKeyGroups(_ context.Context, apiKeyID int64, groupIDs []int64, primaryGroupID *int64) error {
	s.replaceCalled++
	s.replaceAPIKeyIDArg = apiKeyID
	s.replaceGroupIDs = groupIDs
	s.replacePrimary = primaryGroupID
	return s.replaceErr
}

// 编译期确认替身满足窄接口（否则 replaceAPIKeyGroupsForAdmin 会静默跳过，测试失去意义）。
var (
	_ APIKeyGroupBindingRepository = (*groupBindingRepoStub)(nil)
	_ APIKeyRepository             = (*groupBindingRepoStub)(nil)
)

func TestAdminUpdateAPIKeyGroupID_ReplacesBindingWithSingleGroup(t *testing.T) {
	existing := &APIKey{ID: 1, Key: "sk-test", GroupID: int64Ptr(11), GroupIDs: []int64{11, 12}}
	repo := newGroupBindingRepoStub(existing)
	groupRepo := &groupRepoStubForGroupUpdate{group: &Group{ID: 10, Name: "Pro", Status: StatusActive}}
	cache := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{apiKeyRepo: repo, groupRepo: groupRepo, authCacheInvalidator: cache}

	got, err := svc.AdminUpdateAPIKeyGroupID(context.Background(), 1, int64Ptr(10))
	require.NoError(t, err)
	require.Equal(t, int64(10), *got.APIKey.GroupID)

	require.Equal(t, 1, repo.replaceCalled, "关联表必须被替换，否则旧候选 A/C 仍参与决议")
	require.Equal(t, int64(1), repo.replaceAPIKeyIDArg)
	require.Equal(t, []int64{10}, repo.replaceGroupIDs, "单个 group_id 等价一元素集合：旧候选必须全部退出")
	require.NotNil(t, repo.replacePrimary)
	require.Equal(t, int64(10), *repo.replacePrimary)

	// 返回体与快照读源也必须收敛为单元素。
	require.Equal(t, []int64{10}, got.APIKey.GroupIDs)
	require.Equal(t, []string{"sk-test"}, cache.keys)
}

func TestAdminUpdateAPIKeyGroupID_UnbindClearsBinding(t *testing.T) {
	existing := &APIKey{ID: 1, Key: "sk-test", GroupID: int64Ptr(11), GroupIDs: []int64{11, 12}}
	repo := newGroupBindingRepoStub(existing)
	groupRepo := &groupRepoStubForGroupUpdate{group: &Group{ID: 11, Status: StatusActive}}
	svc := &adminServiceImpl{apiKeyRepo: repo, groupRepo: groupRepo, authCacheInvalidator: &authCacheInvalidatorStub{}}

	got, err := svc.AdminUpdateAPIKeyGroupID(context.Background(), 1, int64Ptr(0))
	require.NoError(t, err)
	require.Nil(t, got.APIKey.GroupID)
	require.Nil(t, got.APIKey.Group)

	require.Equal(t, 1, repo.replaceCalled, "解绑必须同时清空关联表")
	require.Empty(t, repo.replaceGroupIDs)
	require.Nil(t, repo.replacePrimary)
}

func TestAdminUpdateAPIKeyGroupID_ExclusiveGroupAlsoReplacesBinding(t *testing.T) {
	existing := &APIKey{ID: 1, UserID: 42, Key: "sk-test", GroupID: int64Ptr(11), GroupIDs: []int64{11, 12}}
	repo := newGroupBindingRepoStub(existing)
	groupRepo := &groupRepoStubForGroupUpdate{group: &Group{
		ID: 10, Name: "Exclusive", Status: StatusActive, IsExclusive: true, SubscriptionType: SubscriptionTypeStandard,
	}}
	userRepo := &userRepoStubForGroupUpdate{}
	svc := &adminServiceImpl{apiKeyRepo: repo, groupRepo: groupRepo, userRepo: userRepo, authCacheInvalidator: &authCacheInvalidatorStub{}}

	got, err := svc.AdminUpdateAPIKeyGroupID(context.Background(), 1, int64Ptr(10))
	require.NoError(t, err)
	require.True(t, got.AutoGrantedGroupAccess)
	require.True(t, userRepo.addGroupCalled)
	require.Equal(t, 1, repo.replaceCalled, "专属分组分支同样必须同步关联表")
	require.Equal(t, []int64{10}, repo.replaceGroupIDs)
}

func TestAdminUpdateAPIKeyGroupID_BindingReplaceFailurePropagates(t *testing.T) {
	existing := &APIKey{ID: 1, Key: "sk-test", GroupID: int64Ptr(11)}
	repo := newGroupBindingRepoStub(existing)
	repo.replaceErr = errors.New("db down")
	groupRepo := &groupRepoStubForGroupUpdate{group: &Group{ID: 10, Status: StatusActive}}
	cache := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{apiKeyRepo: repo, groupRepo: groupRepo, authCacheInvalidator: cache}

	_, err := svc.AdminUpdateAPIKeyGroupID(context.Background(), 1, int64Ptr(10))
	require.Error(t, err, "关联表写失败必须上报，不能静默留下列/表漂移")
	require.Empty(t, cache.keys, "失败路径不得声称缓存已失效")
}

// 替身未实现窄接口时（旧仓储/测试桩）必须保持可用：静默跳过而非 panic。
func TestAdminUpdateAPIKeyGroupID_RepoWithoutBindingInterfaceStillWorks(t *testing.T) {
	existing := &APIKey{ID: 1, Key: "sk-test", GroupID: nil}
	repo := &apiKeyRepoStubForGroupUpdate{key: existing}
	groupRepo := &groupRepoStubForGroupUpdate{group: &Group{ID: 10, Status: StatusActive}}
	svc := &adminServiceImpl{apiKeyRepo: repo, groupRepo: groupRepo, authCacheInvalidator: &authCacheInvalidatorStub{}}

	got, err := svc.AdminUpdateAPIKeyGroupID(context.Background(), 1, int64Ptr(10))
	require.NoError(t, err)
	require.Equal(t, int64(10), *got.APIKey.GroupID)
}

// ---------------------------------------------------------------------------
// 定价准入并集口径（P0 修正 2）
// ---------------------------------------------------------------------------

// PricingCandidateGroups 必须保证生效分组参与判定，且候选缺失时退化为单分组。
func TestPricingCandidateGroups(t *testing.T) {
	primary := &Group{ID: 1, Name: "A"}
	secondary := &Group{ID: 2, Name: "B"}

	// 候选集含生效分组：原样返回，不重排（顺序即语义）。
	key := &APIKey{GroupID: int64Ptr(2), Group: secondary, Groups: []*Group{primary, secondary}}
	require.Equal(t, []*Group{primary, secondary}, PricingCandidateGroups(key))

	// 生效分组不在候选集内（覆写到未物化分组的边缘形态）：前置补入，绝不丢失。
	key = &APIKey{GroupID: int64Ptr(9), Group: &Group{ID: 9, Name: "eff"}, Groups: []*Group{primary}}
	got := PricingCandidateGroups(key)
	require.Len(t, got, 2)
	require.Equal(t, int64(9), got[0].ID, "生效分组必须参与判定")
	require.Equal(t, int64(1), got[1].ID)

	// 快照未物化候选：退化为单分组（与修正前逐字一致，保证向后兼容）。
	key = &APIKey{GroupID: int64Ptr(1), Group: primary}
	require.Equal(t, []*Group{primary}, PricingCandidateGroups(key))

	// 无分组上下文：nil（闸门退化为只查全局价）。
	require.Nil(t, PricingCandidateGroups(&APIKey{}))
	require.Nil(t, PricingCandidateGroups(nil))
}
