//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 冻结契约：订阅是「额度钱包」，不是「分组通行证」。
//
// 个人订阅（group_id=0）只决定「按什么额度扣费」，绝不决定「能用哪些分组」。
// 一旦让它授予准入，任何持有个人额度的人都能绕开管理员的 user_allowed_groups
// 与 restrict_public_groups 限制，甚至绑定专属分组——那是权限扩张，不是产品语义。
// 本文件反向锁定这一点，防止将来被重新引入。

func newPersonalSubOnlyService(t *testing.T) (*APIKeyService, *visibilitySubRepo) {
	t.Helper()
	now := time.Now()
	personal := UserSubscription{
		UserID: 1, GroupID: 0,
		Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour),
	}
	user := &User{ID: 1, AllowedGroups: []int64{7}}
	subs := &visibilitySubRepo{subscriptions: []UserSubscription{personal}}
	groups := []Group{
		{ID: 7, Name: "granted"},
		{ID: 8, Name: "standard-exclusive", IsExclusive: true},
		{ID: 9, Name: "public-not-granted"},
		{ID: 10, Name: "sub-type-exclusive", IsExclusive: true, SubscriptionType: "subscription"},
	}
	svc := &APIKeyService{
		userRepo:    &visibilityUserRepo{user: user},
		userSubRepo: subs,
		groupRepo:   &visibilityGroupRepo{groups: groups},
	}
	return svc, subs
}

// GetAvailableGroups：个人订阅不改变任何分组判定。
// 基准：公开分组（非专属）本默认可绑（CanBindGroup 语义），专属分组需授权，
// 订阅型分组需该分组的专属订阅——个人订阅在这三条上均不加分。
func TestPersonalSubscription_GrantsNoGroupAdmission(t *testing.T) {
	svc, _ := newPersonalSubOnlyService(t)

	available, err := svc.GetAvailableGroups(context.Background(), 1)
	require.NoError(t, err)
	ids := make([]int64, 0, len(available))
	for i := range available {
		ids = append(ids, available[i].ID)
	}
	// 7：明确授权；9：公开非专属（默认人人可绑，与订阅无关）。
	// 8（专属未授权）、10（订阅型且无专属订阅）必须被排除——
	// 旧实现会因「持有个人订阅」把它们也放进来，那正是权限泄漏。
	require.ElementsMatch(t, []int64{7, 9}, ids,
		"个人订阅不得授予专属分组/订阅型分组的准入")
}

// 真正的泄漏面：restrict_public_groups 用户。管理员已刻意限制此人只能用分组 7，
// 旧实现下「持有个人订阅」会直接穿透该限制（公开分组全放开 + 专属/订阅型也放开）。
func TestPersonalSubscription_DoesNotPierceRestrictPublicGroups(t *testing.T) {
	now := time.Now()
	subs := &visibilitySubRepo{subscriptions: []UserSubscription{
		{UserID: 1, GroupID: 0, Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour)},
	}}
	groups := []Group{
		{ID: 7, Name: "granted"},
		{ID: 8, Name: "standard-exclusive", IsExclusive: true},
		{ID: 9, Name: "public-but-restricted"},
		{ID: 10, Name: "sub-type-exclusive", IsExclusive: true, SubscriptionType: "subscription"},
	}
	svc := &APIKeyService{
		userRepo:    &visibilityUserRepo{user: &User{ID: 1, AllowedGroups: []int64{7}, RestrictPublicGroups: true}},
		userSubRepo: subs,
		groupRepo:   &visibilityGroupRepo{groups: groups},
	}

	available, err := svc.GetAvailableGroups(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, available, 1, "受限用户的可用分组只能是被授权的 7")
	require.Equal(t, int64(7), available[0].ID)

	visible, restrict, err := svc.GetUserGroupVisibility(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, restrict)
	require.Equal(t, map[int64]struct{}{7: {}}, visible, "可见集合不得因个人订阅扩大")
}

// GetUserGroupVisibility：不得把 group_id=0 当哨兵键注入（那会被广场层读成「拥有分组 0」）。
func TestPersonalSubscription_NotInjectedAsVisibilitySentinel(t *testing.T) {
	svc, _ := newPersonalSubOnlyService(t)

	visible, _, err := svc.GetUserGroupVisibility(context.Background(), 1)
	require.NoError(t, err)
	require.NotContains(t, visible, int64(0), "个人订阅不得作为哨兵键进入可见分组集合")
	require.Equal(t, map[int64]struct{}{7: {}}, visible)
}

// canUserBindGroupInternal：显式锁定「个人订阅在准入层不可见」。
func TestCanUserBindGroupInternal_IgnoresPersonalSubscription(t *testing.T) {
	subscribed := map[int64]bool{} // 个人订阅的 GroupID=0 已被 GetAvailableGroups 跳过
	svc := &APIKeyService{}

	// 未受限用户：公开分组默认可绑（CanBindGroup 原生语义，与订阅无关）。
	open := &User{ID: 1, AllowedGroups: []int64{7}}
	require.True(t, svc.canUserBindGroupInternal(open, &Group{ID: 7}, subscribed))
	require.True(t, svc.canUserBindGroupInternal(open, &Group{ID: 9}, subscribed),
		"公开非专属分组本默认可绑；不是个人订阅给的")
	require.False(t, svc.canUserBindGroupInternal(open, &Group{ID: 8, IsExclusive: true}, subscribed),
		"专属分组不得因个人订阅放行")
	require.False(t, svc.canUserBindGroupInternal(open, &Group{ID: 10, SubscriptionType: "subscription"}, subscribed),
		"订阅型分组只认该分组的专属订阅")

	// 受限用户（restrict_public_groups）：公开分组也必须落在授权集内。
	// 旧实现下这一条被个人订阅整体穿透，是本次修复的核心场景。
	restricted := &User{ID: 1, AllowedGroups: []int64{7}, RestrictPublicGroups: true}
	require.True(t, svc.canUserBindGroupInternal(restricted, &Group{ID: 7}, subscribed))
	require.False(t, svc.canUserBindGroupInternal(restricted, &Group{ID: 9}, subscribed),
		"受限用户的公开分组不得因个人订阅放行")
	require.False(t, svc.canUserBindGroupInternal(restricted, &Group{ID: 8, IsExclusive: true}, subscribed))
	require.False(t, svc.canUserBindGroupInternal(restricted, &Group{ID: 10, SubscriptionType: "subscription"}, subscribed))

	// 反证机制有效性：只有「该分组的专属订阅」才能打开订阅型分组。
	require.True(t, svc.canUserBindGroupInternal(restricted, &Group{ID: 10, SubscriptionType: "subscription"}, map[int64]bool{10: true}),
		"专属订阅仍须生效（不能误删正向能力）")
}

// 分组决议：个人订阅不参与候选判定（未授权标准分组必须被剔除）。
// 这里同时锁住「决议层没有订阅探针这个参数」——防止日后重新塞回去。
func TestResolveEffectiveGroup_PersonalSubscriptionCannotExpandCandidates(t *testing.T) {
	granted := &Group{ID: 7, Name: "granted", Status: StatusActive}
	notGranted := &Group{ID: 8, Name: "not-granted", Status: StatusActive, IsExclusive: true}
	gid7, gid8 := int64(7), int64(8)

	apiKey := &APIKey{
		UserID:   1,
		GroupID:  &gid7,
		Group:    granted,
		GroupIDs: []int64{7, 8},
		Groups:   []*Group{granted, notGranted},
		User:     &User{ID: 1, AllowedGroups: []int64{7}},
	}
	decision := ResolveEffectiveGroup(apiKey, "any-model", nil)
	require.NotNil(t, decision)
	require.Len(t, decision.Candidates, 1, "未授权分组不得进入候选集")
	require.Equal(t, int64(7), decision.Candidates[0].ID)

	// 全部候选均未授权时，决议必须无结果（而不是被个人订阅救回来）。
	allDenied := &APIKey{
		UserID:   1,
		GroupID:  &gid8,
		Group:    notGranted,
		GroupIDs: []int64{8},
		Groups:   []*Group{notGranted},
		User:     &User{ID: 1},
	}
	d2 := ResolveEffectiveGroup(allDenied, "any-model", nil)
	require.NotNil(t, d2)
	require.Nil(t, d2.Group, "无授权候选时不得凭个人订阅造出候选")
	require.Equal(t, EffectiveGroupReasonNoCandidates, d2.Reason)
}
