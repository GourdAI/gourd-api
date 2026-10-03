package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 个人订阅准入：持有 group_id=0 的订阅即拥有全部标准分组（含专属分组）的绑定权限。
// 本文件不带 //go:build unit，因此在默认 `go test ./...` 与 `-tags unit` 两种口径下都会执行。

// ---- 自足替身（不依赖 unit-tagged 测试文件里的 stub）----

type admissionUserRepoStub struct {
	UserRepository
	user *User
}

func (r *admissionUserRepoStub) GetByID(context.Context, int64) (*User, error) { return r.user, nil }

type admissionGroupRepoStub struct {
	GroupRepository
	groups []Group
}

func (r *admissionGroupRepoStub) ListActive(context.Context) ([]Group, error) { return r.groups, nil }

type admissionSubRepoStub struct {
	UserSubscriptionRepository
	subs         []UserSubscription
	exists       bool
	existsGIDs   []int64
	gotCtxValues map[any]any
}

func (r *admissionSubRepoStub) ListActiveByUserID(_ context.Context, userID int64) ([]UserSubscription, error) {
	now := time.Now()
	active := make([]UserSubscription, 0, len(r.subs))
	for _, sub := range r.subs {
		if sub.UserID == userID && sub.Status == SubscriptionStatusActive && sub.ExpiresAt.After(now) {
			active = append(active, sub)
		}
	}
	return active, nil
}

func (r *admissionSubRepoStub) ExistsActiveByUserIDAndGroupID(ctx context.Context, _, groupID int64) (bool, error) {
	r.existsGIDs = append(r.existsGIDs, groupID)
	if r.gotCtxValues != nil {
		for k := range r.gotCtxValues {
			r.gotCtxValues[k] = ctx.Value(k)
		}
	}
	return r.exists, nil
}

// ---- canUserBindGroupInternal ----

func TestCanUserBindGroupInternal_PersonalGrantOpensExclusiveStandardGroup(t *testing.T) {
	group := &Group{ID: 9, Name: "专属组", IsExclusive: true, SubscriptionType: SubscriptionTypeStandard}
	restricted := &User{ID: 1}

	// 无个人订阅：专属标准分组不可绑定（既有语义不变）
	require.False(t, restricted.CanBindGroup(group.ID, group.IsExclusive))
	require.False(t, (&APIKeyService{}).canUserBindGroupInternal(restricted, group, map[int64]bool{}))

	// 有个人订阅：放行
	require.True(t, (&APIKeyService{}).canUserBindGroupInternal(restricted, group, map[int64]bool{}, true))
}

func TestCanUserBindGroupInternal_PersonalGrantOpensSubscriptionGroup(t *testing.T) {
	group := &Group{ID: 11, SubscriptionType: SubscriptionTypeSubscription}
	require.False(t, (&APIKeyService{}).canUserBindGroupInternal(&User{ID: 1}, group, map[int64]bool{}))
	require.True(t, (&APIKeyService{}).canUserBindGroupInternal(&User{ID: 1}, group, map[int64]bool{}, true))
}

// 不传探针即「无个人订阅」，保证既有调用点行为逐位不变。
func TestCanUserBindGroupInternal_NoVariadicMeansNoGrant(t *testing.T) {
	group := &Group{ID: 12, IsExclusive: true, SubscriptionType: SubscriptionTypeStandard}
	require.False(t, (&APIKeyService{}).canUserBindGroupInternal(&User{ID: 1}, group, map[int64]bool{}))
}

// ---- hasPersonalSubscription ----

func TestHasPersonalSubscription_FailClosed(t *testing.T) {
	// 未注入仓储：false（异常时不多给准入）
	require.False(t, (&APIKeyService{}).hasPersonalSubscription(context.Background(), 5))

	// userID 非法：不查库
	repo := &admissionSubRepoStub{exists: true}
	require.False(t, (&APIKeyService{userSubRepo: repo}).hasPersonalSubscription(context.Background(), 0))
	require.Empty(t, repo.existsGIDs)

	// 正常路径：按哨兵分组 0 查询
	repo2 := &admissionSubRepoStub{exists: true}
	require.True(t, (&APIKeyService{userSubRepo: repo2}).hasPersonalSubscription(context.Background(), 5))
	require.Equal(t, []int64{0}, repo2.existsGIDs)

	repo3 := &admissionSubRepoStub{exists: false}
	require.False(t, (&APIKeyService{userSubRepo: repo3}).hasPersonalSubscription(context.Background(), 5))
}

// ---- GetAvailableGroups / GetUserGroupVisibility ----

func TestGetAvailableGroups_PersonalSubscriptionGrantsAllGroups(t *testing.T) {
	now := time.Now()
	exclusive := Group{ID: 20, Status: StatusActive, IsExclusive: true, SubscriptionType: SubscriptionTypeStandard}
	subGroup := Group{ID: 21, Status: StatusActive, SubscriptionType: SubscriptionTypeSubscription}
	// 无个人订阅：受限用户的专属/订阅分组都不可用
	svcWithout := &APIKeyService{
		userRepo:    &admissionUserRepoStub{user: &User{ID: 1, RestrictPublicGroups: true}},
		userSubRepo: &admissionSubRepoStub{},
		groupRepo:   &admissionGroupRepoStub{groups: []Group{exclusive, subGroup}},
	}
	none, err := svcWithout.GetAvailableGroups(context.Background(), 1)
	require.NoError(t, err)
	require.Empty(t, none)

	svcWith := &APIKeyService{
		userRepo: &admissionUserRepoStub{user: &User{ID: 1, RestrictPublicGroups: true}},
		userSubRepo: &admissionSubRepoStub{subs: []UserSubscription{
			{UserID: 1, GroupID: 0, Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour)},
		}},
		groupRepo: &admissionGroupRepoStub{groups: []Group{exclusive, subGroup}},
	}
	available, err := svcWith.GetAvailableGroups(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, available, 2, "个人订阅应打开专属标准分组与订阅型分组")
	for i := range available {
		require.NotEqual(t, int64(0), available[i].ID, "个人订阅本身不是分组，不得作为可绑定项返回")
	}
}

func TestGetUserGroupVisibility_EncodesPersonalAsSentinelKey(t *testing.T) {
	now := time.Now()
	svc := &APIKeyService{
		userRepo: &admissionUserRepoStub{user: &User{ID: 1}},
		userSubRepo: &admissionSubRepoStub{subs: []UserSubscription{
			{UserID: 1, GroupID: 0, Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour)},
		}},
	}
	allowed, _, err := svc.GetUserGroupVisibility(context.Background(), 1)
	require.NoError(t, err)
	_, ok := allowed[0]
	require.True(t, ok, "个人订阅必须以 group_id=0 哨兵写入可见集合")
}

// ---- availableAPIKeyGroups 的惰性 grant 探针 ----

func TestAvailableAPIKeyGroups_PersonalGrantIsLazyAndCached(t *testing.T) {
	exclusive := &Group{ID: 30, Status: StatusActive, IsExclusive: true, SubscriptionType: SubscriptionTypeStandard}
	apiKey := &APIKey{
		UserID: 1,
		User:   &User{ID: 1},
		Groups: []*Group{exclusive, {ID: 31, Status: StatusActive, SubscriptionType: SubscriptionTypeStandard}},
	}

	// 无探针：未授权专属分组被过滤，公开分组仍可用（第二道闸语义不变）
	noGrant := availableAPIKeyGroups(apiKey)
	require.Len(t, noGrant, 1)
	require.Equal(t, int64(31), noGrant[0].ID)

	calls := 0
	grant := func() bool { calls++; return true }
	got := availableAPIKeyGroups(apiKey, grant)
	require.Len(t, got, 2, "持有个人订阅时专属分组与公开分组都可用")
	require.Equal(t, 1, calls, "探针每轮决议最多求值一次")

	// 公开分组本就无需授权，不应触发探针
	calls = 0
	open := &APIKey{UserID: 1, User: &User{ID: 1}, Groups: []*Group{{ID: 32, Status: StatusActive, SubscriptionType: SubscriptionTypeStandard}}}
	require.Len(t, availableAPIKeyGroups(open, grant), 1)
	require.Zero(t, calls, "无需授权判定时应完全跳过探针")
}

// grant 返回 false 时不得放宽（fail-closed）。
func TestAvailableAPIKeyGroups_GrantDenialKeepsFiltering(t *testing.T) {
	exclusive := &Group{ID: 33, Status: StatusActive, IsExclusive: true, SubscriptionType: SubscriptionTypeStandard}
	apiKey := &APIKey{UserID: 1, User: &User{ID: 1}, Groups: []*Group{exclusive}}
	require.Empty(t, availableAPIKeyGroups(apiKey, func() bool { return false }))
}

// PersonalSubscriptionGrant：缺依赖时返回 nil（决议层据此保持既有行为）。
func TestPersonalSubscriptionGrant_NilWhenUnwired(t *testing.T) {
	svc := &APIKeyService{}
	require.Nil(t, svc.PersonalSubscriptionGrant(context.Background(), &APIKey{UserID: 1}))
	require.Nil(t, svc.PersonalSubscriptionGrant(context.Background(), nil))

	repo := &admissionSubRepoStub{exists: true}
	svc2 := &APIKeyService{userSubRepo: repo}
	probe := svc2.PersonalSubscriptionGrant(context.Background(), &APIKey{UserID: 7})
	require.NotNil(t, probe)
	require.True(t, probe())
	require.Equal(t, []int64{0}, repo.existsGIDs, "必须按个人订阅哨兵分组查询")
}

// 探针必须继承调用方 ctx（不得用 context.Background() 脱离请求生命周期）。
func TestPersonalSubscriptionGrant_PropagatesCallerContext(t *testing.T) {
	type ctxKey string
	marker := ctxKey("req")
	repo := &admissionSubRepoStub{exists: true, gotCtxValues: map[any]any{marker: nil}}
	svc := &APIKeyService{userSubRepo: repo}

	ctx := context.WithValue(context.Background(), marker, 1)
	probe := svc.PersonalSubscriptionGrant(ctx, &APIKey{UserID: 9})
	require.NotNil(t, probe)
	require.True(t, probe())
	require.Equal(t, 1, repo.gotCtxValues[marker], "探针必须使用传入的请求 ctx")
}
