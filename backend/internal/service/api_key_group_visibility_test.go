//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type visibilityUserRepo struct {
	UserRepository
	user *User
	err  error
}

func (r *visibilityUserRepo) GetByID(context.Context, int64) (*User, error) { return r.user, r.err }

type visibilitySubRepo struct {
	UserSubscriptionRepository
	subscriptions []UserSubscription
	err           error
	calls         int
}

func (r *visibilitySubRepo) ListActiveByUserID(_ context.Context, userID int64) ([]UserSubscription, error) {
	r.calls++
	// Match the repository's active-status and expiry predicates.
	active := make([]UserSubscription, 0)
	for _, sub := range r.subscriptions {
		if sub.UserID == userID && sub.IsActive() {
			active = append(active, sub)
		}
	}
	return active, r.err
}

type visibilityGroupRepo struct {
	GroupRepository
	groups []Group
}

func (r *visibilityGroupRepo) ListActive(context.Context) ([]Group, error) { return r.groups, nil }

// 契约第 6 节（2026-10-03 定案）：订阅不绑定分组、也不授予任何分组准入/可见性。
// 因此本用例从旧版的「生效订阅会把分组 42 带进可见集」翻转为反向锁定：
// 即使用户持有一份到期未过的 active 订阅，分组 42（专属 + 未被授权）
// 仍不得出现在可用列表或可见集合里，而且可见性计算根本不得去查订阅仓储。
func TestGetUserGroupVisibilityIgnoresActiveSubscriptions(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrestricted", true: "restricted"}[restricted], func(t *testing.T) {
			now := time.Now()
			plan42, plan43, plan44, plan45 := int64(42), int64(43), int64(44), int64(45)
			subs := &visibilitySubRepo{subscriptions: []UserSubscription{
				{UserID: 1, PlanID: &plan42, Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour)},
				{UserID: 1, PlanID: &plan43, Status: SubscriptionStatusActive, ExpiresAt: now.Add(-time.Hour)},
				{UserID: 1, PlanID: &plan44, Status: "expired", ExpiresAt: now.Add(time.Hour)},
				{UserID: 2, PlanID: &plan45, Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour)},
			}}
			svc := &APIKeyService{
				userRepo:    &visibilityUserRepo{user: &User{ID: 1, AllowedGroups: []int64{7}, RestrictPublicGroups: restricted}},
				userSubRepo: subs,
				groupRepo:   &visibilityGroupRepo{groups: []Group{{ID: 42, IsExclusive: true, SubscriptionType: "subscription"}}},
			}
			available, err := svc.GetAvailableGroups(context.Background(), 1)
			require.NoError(t, err)
			// 分组 42 是专属且未被授权的：它不能因为「持有订阅」而被列为可用。
			require.Empty(t, available, "生效订阅不得把未授权分组变成可用分组")

			visible, restrict, err := svc.GetUserGroupVisibility(context.Background(), 1)
			require.NoError(t, err)
			require.Equal(t, restricted, restrict)
			// 可见集合只来自 user_allowed_groups，与订阅无关。
			require.Equal(t, map[int64]struct{}{7: {}}, visible, "订阅不得扩大可见分组集合")
			// 最强锁定：可见性计算不应去碰订阅仓储（旧实现靠它把分组撑大）。
			require.Zero(t, subs.calls, "分组可见性不得查询订阅仓储")
		})
	}
}

func TestGetUserGroupVisibilityEmptyAndErrors(t *testing.T) {
	failure := errors.New("repository unavailable")
	for _, tc := range []struct {
		name            string
		userErr, subErr error
	}{
		{name: "empty"}, {name: "user failure", userErr: failure},
		// 反向锁定：订阅仓储故障不得影响分组可见性（它已不在这条链路上）。
		// 旧实现会把 subErr 上抛，那会把一个计费组件的抖变成“看不到分组”的故障。
		{name: "subscription failure is irrelevant", subErr: failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subs := &visibilitySubRepo{err: tc.subErr}
			svc := &APIKeyService{userRepo: &visibilityUserRepo{user: &User{ID: 1}, err: tc.userErr}, userSubRepo: subs}
			got, _, err := svc.GetUserGroupVisibility(context.Background(), 1)
			if tc.userErr != nil {
				require.ErrorIs(t, err, failure)
				require.Nil(t, got, "repository failures must not become anonymous visibility")
			} else {
				require.NoError(t, err)
				require.NotNil(t, got, "an empty logged-in user must not become anonymous")
				require.Empty(t, got)
			}
			require.Zero(t, subs.calls, "可见性计算不得查订阅仓储（包括用户读取失败之后的任何分支）")
		})
	}
}
