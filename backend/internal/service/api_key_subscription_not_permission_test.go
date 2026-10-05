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
// 个人订阅（无套餐 / 有套餐但不带分组）只决定「按什么额度扣费」，绝不决定「能用哪些分组」。
// 一旦让它授予准入，任何持有个人额度的人都能绕开管理员的 user_allowed_groups
// 与 restrict_public_groups 限制，甚至绑定专属分组——那是权限扩张，不是产品语义。
// 本文件反向锁定这一点，防止将来被重新引入。
//
// 钱包化后订阅行已无 group_id，因此构造个人钱包时用 PlanID/TotalLimitUSD 表达，
// 不再传任何分组坐标（这本身就是被锁定的语义：准入层拿不到订阅的分组信息）。

func newPersonalSubOnlyService(t *testing.T) (*APIKeyService, *visibilitySubRepo) {
	t.Helper()
	now := time.Now()
	personal := UserSubscription{
		UserID:    1,
		Status:    SubscriptionStatusActive,
		ExpiresAt: now.Add(time.Hour),
		// 一份典型的「只给了有效期、也填了额度」的个人钱包。
		TotalLimitUSD: ptrFloat64(10),
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
		{UserID: 1, Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour), TotalLimitUSD: ptrFloat64(10)},
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

// GetUserGroupVisibility：不得把订阅当作分组坐标注入（旧模型下 group_id=0 会被
// 当成哨兵键，广场层会读成「拥有分组 0」）。
func TestPersonalSubscription_NotInjectedAsVisibilitySentinel(t *testing.T) {
	svc, _ := newPersonalSubOnlyService(t)

	visible, _, err := svc.GetUserGroupVisibility(context.Background(), 1)
	require.NoError(t, err)
	require.NotContains(t, visible, int64(0), "个人订阅不得作为哨兵键进入可见分组集合")
	require.Equal(t, map[int64]struct{}{7: {}}, visible)
}

// canUserBindGroup：显式锁定「准入层完全不看订阅」。
//
// 钱包化后该函数已没有 subscribedGroupIDs 参数，也没有任何订阅探针——
// 准入只看 user.CanBindGroup（公开分组 / user_allowed_groups 授权）。
// 这里把「个人订阅不加分」这一结论逐条钉在分组形状上。
func TestCanUserBindGroup_IgnoresPersonalSubscription(t *testing.T) {
	svc := &APIKeyService{}
	ctx := context.Background()

	// 未受限用户：公开分组默认可绑（CanBindGroup 原生语义，与订阅无关）。
	open := &User{ID: 1, AllowedGroups: []int64{7}}
	require.True(t, svc.canUserBindGroup(ctx, open, &Group{ID: 7}))
	require.True(t, svc.canUserBindGroup(ctx, open, &Group{ID: 9}),
		"公开非专属分组本默认可绑；不是个人订阅给的")
	require.False(t, svc.canUserBindGroup(ctx, open, &Group{ID: 8, IsExclusive: true}),
		"专属分组不得因个人订阅放行")
	require.False(t, svc.canUserBindGroup(ctx, open, &Group{ID: 10, IsExclusive: true, SubscriptionType: "subscription"}),
		"订阅型专属分组同样只认分组授权，订阅不加分")

	// 受限用户（restrict_public_groups）：公开分组也必须落在授权集内。
	// 旧实现下这一条被个人订阅整体穿透，是本次修复的核心场景。
	restricted := &User{ID: 1, AllowedGroups: []int64{7}, RestrictPublicGroups: true}
	require.True(t, svc.canUserBindGroup(ctx, restricted, &Group{ID: 7}))
	require.False(t, svc.canUserBindGroup(ctx, restricted, &Group{ID: 9}),
		"受限用户的公开分组不得因个人订阅放行")
	require.False(t, svc.canUserBindGroup(ctx, restricted, &Group{ID: 8, IsExclusive: true}))
	require.False(t, svc.canUserBindGroup(ctx, restricted, &Group{ID: 10, IsExclusive: true, SubscriptionType: "subscription"}))

	// 正向能力不得误删：被明确授权的订阅型专属分组仍然可绑（依据是授权，不是订阅）。
	grantedSubType := &User{ID: 1, AllowedGroups: []int64{10}, RestrictPublicGroups: true}
	require.True(t, svc.canUserBindGroup(ctx, grantedSubType, &Group{ID: 10, IsExclusive: true, SubscriptionType: "subscription"}),
		"分组授权仍须生效（不能误删正向能力）")

	// nil 入参不得 panic，也不得放行。
	require.False(t, svc.canUserBindGroup(ctx, nil, &Group{ID: 7}))
	require.False(t, svc.canUserBindGroup(ctx, open, nil))
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
