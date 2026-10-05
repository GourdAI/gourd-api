//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/suite"
)

// UserSubscriptionRepoSuite 覆盖「个人额度钱包」仓储契约。
//
// 2026-10-03 订阅钱包改造后的口径（见 .gwork/SUBSCRIPTION_WALLET_SPEC.md）：
//   - 订阅不绑定分组，全部按 user_id 寻址；group_id 列已删除。
//   - 额度是单一总额池（total_limit_usd / total_usage_usd），不存在日/周/月窗口，
//     因此不存在任何「窗口重置」语义。
//   - 一个用户可持多份订阅；消耗顺序为 expires_at 升序（先到期先消耗）。
type UserSubscriptionRepoSuite struct {
	suite.Suite
	ctx    context.Context
	client *dbent.Client
	repo   *userSubscriptionRepository
}

func (s *UserSubscriptionRepoSuite) SetupTest() {
	s.ctx = context.Background()
	tx := testEntTx(s.T())
	s.client = tx.Client()
	s.repo = NewUserSubscriptionRepository(s.client).(*userSubscriptionRepository)
}

func TestUserSubscriptionRepoSuite(t *testing.T) {
	suite.Run(t, new(UserSubscriptionRepoSuite))
}

func (s *UserSubscriptionRepoSuite) mustCreateUser(email string, role string) *service.User {
	s.T().Helper()

	if role == "" {
		role = service.RoleUser
	}

	u, err := s.client.User.Create().
		SetEmail(email).
		SetPasswordHash("test-password-hash").
		SetStatus(service.StatusActive).
		SetRole(role).
		Save(s.ctx)
	s.Require().NoError(err, "create user")
	return userEntityToService(u)
}

// mustCreateGroup 仅用于满足 subscription_plans.group_id 的非空约束；
// 订阅本身不再引用分组。
func (s *UserSubscriptionRepoSuite) mustCreateGroup(name string) *service.Group {
	s.T().Helper()

	g, err := s.client.Group.Create().
		SetName(name).
		SetStatus(service.StatusActive).
		Save(s.ctx)
	s.Require().NoError(err, "create group")
	return groupEntityToService(g)
}

// mustCreatePlan 创建一份套餐，用于验证 plan_id 溯源（可空）。
func (s *UserSubscriptionRepoSuite) mustCreatePlan(name string) *dbent.SubscriptionPlan {
	s.T().Helper()

	group := s.mustCreateGroup("plan-group-" + name)
	plan, err := s.client.SubscriptionPlan.Create().
		SetGroupID(group.ID).
		SetName(name).
		SetPrice(9.9).
		SetTotalLimitUsd(100.0).
		Save(s.ctx)
	s.Require().NoError(err, "create subscription plan")
	return plan
}

// mustCreateSubscription 创建一份订阅钱包；plan_id 可空（管理员手工发放）。
func (s *UserSubscriptionRepoSuite) mustCreateSubscription(userID int64, mutate func(*dbent.UserSubscriptionCreate)) *dbent.UserSubscription {
	s.T().Helper()

	now := time.Now()
	create := s.client.UserSubscription.Create().
		SetUserID(userID).
		SetStartsAt(now.Add(-1 * time.Hour)).
		SetExpiresAt(now.Add(24 * time.Hour)).
		SetStatus(service.SubscriptionStatusActive).
		SetAssignedAt(now).
		SetNotes("")

	if mutate != nil {
		mutate(create)
	}

	sub, err := create.Save(s.ctx)
	s.Require().NoError(err, "create user subscription")
	return sub
}

func float64Value(v float64) *float64 {
	return &v
}

// --- Create / GetByID / Update / Delete ---

func (s *UserSubscriptionRepoSuite) TestCreate() {
	user := s.mustCreateUser("sub-create@test.com", service.RoleUser)
	plan := s.mustCreatePlan("p-create")

	sub := &service.UserSubscription{
		UserID:        user.ID,
		PlanID:        &plan.ID,
		Status:        service.SubscriptionStatusActive,
		ExpiresAt:     time.Now().Add(24 * time.Hour),
		TotalLimitUSD: float64Value(120.0),
		TotalUsageUSD: 5.0,
	}

	err := s.repo.Create(s.ctx, sub)
	s.Require().NoError(err, "Create")
	s.Require().NotZero(sub.ID, "expected ID to be set")

	got, err := s.repo.GetByID(s.ctx, sub.ID)
	s.Require().NoError(err, "GetByID")
	s.Require().Equal(sub.UserID, got.UserID)
	s.Require().NotNil(got.PlanID, "expected plan_id persisted")
	s.Require().Equal(plan.ID, *got.PlanID)
	s.Require().NotNil(got.TotalLimitUSD)
	s.Require().InDelta(120.0, *got.TotalLimitUSD, 1e-6)
	s.Require().InDelta(5.0, got.TotalUsageUSD, 1e-6)
}

// TestCreate_UnlimitedWallet 验证不限额钱包：total_limit_usd 为 NULL。
func (s *UserSubscriptionRepoSuite) TestCreate_UnlimitedWallet() {
	user := s.mustCreateUser("sub-create-unlimited@test.com", service.RoleUser)

	sub := &service.UserSubscription{
		UserID:    user.ID,
		Status:    service.SubscriptionStatusActive,
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}
	s.Require().NoError(s.repo.Create(s.ctx, sub), "Create")

	got, err := s.repo.GetByID(s.ctx, sub.ID)
	s.Require().NoError(err, "GetByID")
	s.Require().Nil(got.PlanID, "手工发放的订阅没有来源套餐")
	s.Require().Nil(got.TotalLimitUSD, "nil 额度 = 不限额")
	s.Require().InDelta(0.0, got.TotalUsageUSD, 1e-6)
	s.Require().True(got.IsUnlimited(), "不限额判定应成立")
}

func (s *UserSubscriptionRepoSuite) TestGetByID_WithPreloads() {
	user := s.mustCreateUser("preload@test.com", service.RoleUser)
	plan := s.mustCreatePlan("p-preload")
	admin := s.mustCreateUser("admin@test.com", service.RoleAdmin)

	sub := s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetPlanID(plan.ID)
		c.SetAssignedBy(admin.ID)
	})

	got, err := s.repo.GetByID(s.ctx, sub.ID)
	s.Require().NoError(err, "GetByID")
	s.Require().NotNil(got.User, "expected User preload")
	s.Require().NotNil(got.Plan, "expected Plan preload")
	s.Require().NotNil(got.AssignedByUser, "expected AssignedByUser preload")
	s.Require().Equal(user.ID, got.User.ID)
	s.Require().Equal(plan.ID, got.Plan.ID)
	s.Require().Equal(plan.Name, got.Plan.Name)
	s.Require().Equal(admin.ID, got.AssignedByUser.ID)
}

func (s *UserSubscriptionRepoSuite) TestGetByID_NotFound() {
	_, err := s.repo.GetByID(s.ctx, 999999)
	s.Require().Error(err, "expected error for non-existent ID")
}

func (s *UserSubscriptionRepoSuite) TestUpdate() {
	user := s.mustCreateUser("update@test.com", service.RoleUser)
	created := s.mustCreateSubscription(user.ID, nil)

	sub, err := s.repo.GetByID(s.ctx, created.ID)
	s.Require().NoError(err, "GetByID")

	sub.Notes = "updated notes"
	s.Require().NoError(s.repo.Update(s.ctx, sub), "Update")

	got, err := s.repo.GetByID(s.ctx, sub.ID)
	s.Require().NoError(err, "GetByID after update")
	s.Require().Equal("updated notes", got.Notes)
}

// TestUpdate_PreservesLimitColumn 锁定一条产品约束：整行 Update（唯一生产调用方是
// 过期订阅续期）不得写回额度列，否则会用事务开始时的旧快照吞掉管理员并发改额度。
func (s *UserSubscriptionRepoSuite) TestUpdate_PreservesLimitColumn() {
	user := s.mustCreateUser("update-limit@test.com", service.RoleUser)
	created := s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetTotalLimitUsd(100.0)
	})

	sub, err := s.repo.GetByID(s.ctx, created.ID)
	s.Require().NoError(err, "GetByID")

	// 模拟并发：库里的额度已被管理员改动，而 sub 是旧快照。
	s.Require().NoError(s.repo.UpdateAssignedLimit(s.ctx, created.ID, float64Value(250.0)), "UpdateAssignedLimit")

	sub.Notes = "renewed"
	s.Require().NoError(s.repo.Update(s.ctx, sub), "Update (renew)")

	got, err := s.repo.GetByID(s.ctx, created.ID)
	s.Require().NoError(err, "GetByID after update")
	s.Require().Equal("renewed", got.Notes)
	s.Require().NotNil(got.TotalLimitUSD)
	s.Require().InDelta(250.0, *got.TotalLimitUSD, 1e-6, "Update 不得用旧快照覆盖额度列")
}

func (s *UserSubscriptionRepoSuite) TestDelete() {
	user := s.mustCreateUser("delete@test.com", service.RoleUser)
	sub := s.mustCreateSubscription(user.ID, nil)

	err := s.repo.Delete(s.ctx, sub.ID)
	s.Require().NoError(err, "Delete")

	_, err = s.repo.GetByID(s.ctx, sub.ID)
	s.Require().Error(err, "expected error after delete")
}

func (s *UserSubscriptionRepoSuite) TestGetByIDIncludeDeleted_PreservesPersistedStatus() {
	user := s.mustCreateUser("include-deleted@test.com", service.RoleUser)
	sub := s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetStatus(service.SubscriptionStatusActive)
	})

	s.Require().NoError(s.repo.Delete(s.ctx, sub.ID), "Delete")

	got, err := s.repo.GetByIDIncludeDeleted(s.ctx, sub.ID)
	s.Require().NoError(err, "GetByIDIncludeDeleted")
	s.Require().Equal(service.SubscriptionStatusActive, got.Status)
	s.Require().NotNil(got.DeletedAt)
	s.Require().NotNil(got.User)
}

func (s *UserSubscriptionRepoSuite) TestRestore() {
	user := s.mustCreateUser("restore@test.com", service.RoleUser)
	sub := s.mustCreateSubscription(user.ID, nil)

	s.Require().NoError(s.repo.Delete(s.ctx, sub.ID), "Delete")

	restored, err := s.repo.Restore(s.ctx, sub.ID, service.SubscriptionStatusExpired)
	s.Require().NoError(err, "Restore")
	s.Require().Equal(service.SubscriptionStatusExpired, restored.Status)
	s.Require().Nil(restored.DeletedAt)

	got, err := s.repo.GetByID(s.ctx, sub.ID)
	s.Require().NoError(err, "GetByID after restore")
	s.Require().Nil(got.DeletedAt)
	s.Require().Equal(service.SubscriptionStatusExpired, got.Status)
}

func (s *UserSubscriptionRepoSuite) TestDelete_Idempotent() {
	s.Require().NoError(s.repo.Delete(s.ctx, 42424242), "Delete should be idempotent")
}

// --- ListByUserID / ListActiveByUserID / ExistsActiveByUserID ---

func (s *UserSubscriptionRepoSuite) TestListByUserID() {
	user := s.mustCreateUser("listby@test.com", service.RoleUser)
	other := s.mustCreateUser("listby-other@test.com", service.RoleUser)
	plan := s.mustCreatePlan("p-listby")

	s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetPlanID(plan.ID)
	})
	s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetStatus(service.SubscriptionStatusExpired)
		c.SetExpiresAt(time.Now().Add(-24 * time.Hour))
	})
	// 不相关用户的订阅不应串台
	s.mustCreateSubscription(other.ID, nil)

	subs, err := s.repo.ListByUserID(s.ctx, user.ID)
	s.Require().NoError(err, "ListByUserID")
	s.Require().Len(subs, 2, "ListByUserID 应包含过期订阅（完整钱包历史）")
	for _, sub := range subs {
		s.Require().Equal(user.ID, sub.UserID)
	}
}

// TestListActiveByUserID 断言消耗顺序：按 expires_at 升序 = 先到期先消耗。
func (s *UserSubscriptionRepoSuite) TestListActiveByUserID() {
	user := s.mustCreateUser("listactive@test.com", service.RoleUser)
	now := time.Now()

	// 乱序写入，验证仓储层排序而非依赖插入顺序。
	s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetExpiresAt(now.Add(72 * time.Hour))
		c.SetTotalLimitUsd(300.0)
	})
	s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetExpiresAt(now.Add(2 * time.Hour))
		c.SetTotalLimitUsd(100.0)
	})
	s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetExpiresAt(now.Add(24 * time.Hour))
		c.SetTotalLimitUsd(200.0)
	})
	// 已过期：即便 status 仍是 active 也不得进入消耗队列
	s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetExpiresAt(now.Add(-1 * time.Hour))
	})
	// 显式 expired
	s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetStatus(service.SubscriptionStatusExpired)
		c.SetExpiresAt(now.Add(24 * time.Hour))
	})

	subs, err := s.repo.ListActiveByUserID(s.ctx, user.ID)
	s.Require().NoError(err, "ListActiveByUserID")
	s.Require().Len(subs, 3)

	for i := 1; i < len(subs); i++ {
		s.Require().True(subs[i-1].ExpiresAt.Before(subs[i].ExpiresAt) || subs[i-1].ExpiresAt.Equal(subs[i].ExpiresAt),
			"ListActiveByUserID 必须按 expires_at 升序（先到期先消耗）")
	}
	s.Require().True(subs[0].ExpiresAt.After(now.Add(1*time.Hour)), "最先消耗的应是最快到期的订阅")
	for _, sub := range subs {
		s.Require().Equal(service.SubscriptionStatusActive, sub.Status)
		s.Require().True(sub.IsActive())
	}
}

func (s *UserSubscriptionRepoSuite) TestExistsActiveByUserID() {
	user := s.mustCreateUser("exists@test.com", service.RoleUser)
	empty := s.mustCreateUser("exists-empty@test.com", service.RoleUser)

	s.Require().NoError(s.repo.Create(s.ctx, &service.UserSubscription{
		UserID:        user.ID,
		Status:        service.SubscriptionStatusActive,
		ExpiresAt:     time.Now().Add(24 * time.Hour),
		TotalLimitUSD: float64Value(50.0),
	}), "Create")

	exists, err := s.repo.ExistsActiveByUserID(s.ctx, user.ID)
	s.Require().NoError(err, "ExistsActiveByUserID")
	s.Require().True(exists, "持有生效订阅应命中")

	none, err := s.repo.ExistsActiveByUserID(s.ctx, empty.ID)
	s.Require().NoError(err, "ExistsActiveByUserID (no subscription)")
	s.Require().False(none, "无任何订阅不应命中")
}

// TestExistsActiveByUserID_ExpiredNotMatched 锁定「生效」口径：
// status=active 但 expires_at 已过 → 不算命中（不能退化成裸 Exist）。
func (s *UserSubscriptionRepoSuite) TestExistsActiveByUserID_ExpiredNotMatched() {
	user := s.mustCreateUser("exists-expired@test.com", service.RoleUser)

	s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetStatus(service.SubscriptionStatusActive)
		c.SetExpiresAt(time.Now().Add(-1 * time.Hour))
	})

	exists, err := s.repo.ExistsActiveByUserID(s.ctx, user.ID)
	s.Require().NoError(err, "ExistsActiveByUserID")
	s.Require().False(exists, "过期订阅不得读成命中")
}

// TestExistsActiveByUserID_IgnoresSoftDeletedRows 锁定「撤销 = 软删除」口径。
func (s *UserSubscriptionRepoSuite) TestExistsActiveByUserID_IgnoresSoftDeletedRows() {
	user := s.mustCreateUser("exists-active@test.com", service.RoleUser)
	sub := s.mustCreateSubscription(user.ID, nil)

	exists, err := s.repo.ExistsActiveByUserID(s.ctx, user.ID)
	s.Require().NoError(err, "ExistsActiveByUserID")
	s.Require().True(exists)

	s.Require().NoError(s.repo.Delete(s.ctx, sub.ID), "Delete")

	exists, err = s.repo.ExistsActiveByUserID(s.ctx, user.ID)
	s.Require().NoError(err, "ExistsActiveByUserID after delete")
	s.Require().False(exists, "已撤销（软删除）的订阅不得读成命中")
}

// --- List with filters ---

func (s *UserSubscriptionRepoSuite) TestList_NoFilters() {
	user := s.mustCreateUser("list@test.com", service.RoleUser)
	s.mustCreateSubscription(user.ID, nil)

	subs, page, err := s.repo.List(s.ctx, pagination.PaginationParams{Page: 1, PageSize: 10}, nil, nil, "", "", "")
	s.Require().NoError(err, "List")
	s.Require().NotEmpty(subs)
	s.Require().NotNil(page)
}

func (s *UserSubscriptionRepoSuite) TestList_FilterByUserID() {
	user1 := s.mustCreateUser("filter1@test.com", service.RoleUser)
	user2 := s.mustCreateUser("filter2@test.com", service.RoleUser)

	s.mustCreateSubscription(user1.ID, nil)
	s.mustCreateSubscription(user2.ID, nil)

	subs, _, err := s.repo.List(s.ctx, pagination.PaginationParams{Page: 1, PageSize: 10}, &user1.ID, nil, "", "", "")
	s.Require().NoError(err)
	s.Require().Len(subs, 1)
	s.Require().Equal(user1.ID, subs[0].UserID)
}

// TestList_FilterByPlanID 验证管理员列表按来源套餐筛选（替代旧的按分组筛选）。
func (s *UserSubscriptionRepoSuite) TestList_FilterByPlanID() {
	user := s.mustCreateUser("planfilter@test.com", service.RoleUser)
	p1 := s.mustCreatePlan("p-f1")
	p2 := s.mustCreatePlan("p-f2")

	s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetPlanID(p1.ID)
	})
	s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetPlanID(p2.ID)
	})
	// 手工发放（无套餐）的行不应被套餐筛选命中
	s.mustCreateSubscription(user.ID, nil)

	subs, _, err := s.repo.List(s.ctx, pagination.PaginationParams{Page: 1, PageSize: 10}, nil, &p1.ID, "", "", "")
	s.Require().NoError(err)
	s.Require().Len(subs, 1)
	s.Require().NotNil(subs[0].PlanID)
	s.Require().Equal(p1.ID, *subs[0].PlanID)
	s.Require().NotNil(subs[0].Plan, "expected Plan preload")
	s.Require().Equal(p1.Name, subs[0].Plan.Name)
}

func (s *UserSubscriptionRepoSuite) TestList_FilterByStatus() {
	user1 := s.mustCreateUser("statfilter1@test.com", service.RoleUser)
	user2 := s.mustCreateUser("statfilter2@test.com", service.RoleUser)

	s.mustCreateSubscription(user1.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetStatus(service.SubscriptionStatusActive)
		c.SetExpiresAt(time.Now().Add(24 * time.Hour))
	})
	s.mustCreateSubscription(user2.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetStatus(service.SubscriptionStatusExpired)
		c.SetExpiresAt(time.Now().Add(-24 * time.Hour))
	})

	subs, _, err := s.repo.List(s.ctx, pagination.PaginationParams{Page: 1, PageSize: 10}, nil, nil, service.SubscriptionStatusExpired, "", "")
	s.Require().NoError(err)
	s.Require().NotEmpty(subs)
	for _, sub := range subs {
		s.Require().Equal(service.SubscriptionStatusExpired, sub.Status)
	}
}

func (s *UserSubscriptionRepoSuite) TestList_IncludesRevokedWhenStatusEmpty() {
	user1 := s.mustCreateUser("allstatus1@test.com", service.RoleUser)
	user2 := s.mustCreateUser("allstatus2@test.com", service.RoleUser)
	user3 := s.mustCreateUser("allstatus3@test.com", service.RoleUser)

	s.mustCreateSubscription(user1.ID, nil)
	s.mustCreateSubscription(user2.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetStatus(service.SubscriptionStatusExpired)
		c.SetExpiresAt(time.Now().Add(-24 * time.Hour))
	})
	revoked := s.mustCreateSubscription(user3.ID, nil)
	s.Require().NoError(s.repo.Delete(s.ctx, revoked.ID))

	subs, _, err := s.repo.List(s.ctx, pagination.PaginationParams{Page: 1, PageSize: 100}, &user3.ID, nil, "", "", "")
	s.Require().NoError(err)
	s.Require().Len(subs, 1)

	var gotRevoked *service.UserSubscription
	for i := range subs {
		if subs[i].ID == revoked.ID {
			gotRevoked = &subs[i]
			break
		}
	}
	s.Require().NotNil(gotRevoked, "all status should include soft-deleted subscription")
	s.Require().Equal(service.SubscriptionStatusRevoked, gotRevoked.Status)
	s.Require().NotNil(gotRevoked.DeletedAt)
	s.Require().NotNil(gotRevoked.User, "撤销历史也需要用户信息（关系回填）")
}

func (s *UserSubscriptionRepoSuite) TestList_FilterByRevokedStatus() {
	user1 := s.mustCreateUser("revokedfilter1@test.com", service.RoleUser)
	user2 := s.mustCreateUser("revokedfilter2@test.com", service.RoleUser)

	active := s.mustCreateSubscription(user1.ID, nil)
	revoked := s.mustCreateSubscription(user2.ID, nil)
	s.Require().NoError(s.repo.Delete(s.ctx, revoked.ID))

	subs, pag, err := s.repo.List(s.ctx, pagination.PaginationParams{Page: 1, PageSize: 10}, nil, nil, service.SubscriptionStatusRevoked, "", "")
	s.Require().NoError(err)
	s.Require().Equal(int64(1), pag.Total)
	s.Require().Len(subs, 1)
	s.Require().Equal(revoked.ID, subs[0].ID)
	s.Require().NotEqual(active.ID, subs[0].ID)
	s.Require().Equal(service.SubscriptionStatusRevoked, subs[0].Status)
	s.Require().NotNil(subs[0].DeletedAt)
}

// --- 额度列与用量列（单一总额池）---

// TestUpdateAssignedLimit 覆盖三种额度写入语义：设值 / nil 保持原值 / <=0 改不限额。
func (s *UserSubscriptionRepoSuite) TestUpdateAssignedLimit() {
	user := s.mustCreateUser("assign-limit@test.com", service.RoleUser)
	sub := s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetTotalLimitUsd(100.0)
		c.SetTotalUsageUsd(30.0)
	})

	// 1) 设新额度：只动额度列，用量列保持不变
	s.Require().NoError(s.repo.UpdateAssignedLimit(s.ctx, sub.ID, float64Value(200.0)), "UpdateAssignedLimit")
	got, err := s.repo.GetByID(s.ctx, sub.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.TotalLimitUSD)
	s.Require().InDelta(200.0, *got.TotalLimitUSD, 1e-6)
	s.Require().InDelta(30.0, got.TotalUsageUSD, 1e-6, "改额度不得清零用量")
	s.Require().InDelta(170.0, *got.RemainingUSD(), 1e-6)

	// 2) nil = 保持原值不变（不是「清空」）
	s.Require().NoError(s.repo.UpdateAssignedLimit(s.ctx, sub.ID, nil), "UpdateAssignedLimit(nil)")
	got, err = s.repo.GetByID(s.ctx, sub.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.TotalLimitUSD)
	s.Require().InDelta(200.0, *got.TotalLimitUSD, 1e-6, "nil 入参应保持原额度")

	// 3) <=0 = 改为不限额（额度列写 NULL）
	s.Require().NoError(s.repo.UpdateAssignedLimit(s.ctx, sub.ID, float64Value(0)), "UpdateAssignedLimit(0)")
	got, err = s.repo.GetByID(s.ctx, sub.ID)
	s.Require().NoError(err)
	s.Require().Nil(got.TotalLimitUSD, "0 额度应转为不限额")
	s.Require().True(got.IsUnlimited())
	s.Require().False(got.HasEffectiveLimit())
	s.Require().InDelta(30.0, got.TotalUsageUSD, 1e-6)
}

func (s *UserSubscriptionRepoSuite) TestUpdateAssignedLimit_NotFound() {
	err := s.repo.UpdateAssignedLimit(s.ctx, 999999, float64Value(10.0))
	s.Require().Error(err, "expected error for non-existent subscription")
	s.Require().ErrorIs(err, service.ErrSubscriptionNotFound)
}

// TestResetUsage 覆盖管理员手动「重置用量」：只清用量，不动额度/状态/有效期。
func (s *UserSubscriptionRepoSuite) TestResetUsage() {
	user := s.mustCreateUser("reset-usage@test.com", service.RoleUser)
	expiresAt := time.Now().Add(48 * time.Hour)
	sub := s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetTotalLimitUsd(100.0)
		c.SetTotalUsageUsd(80.0)
		c.SetExpiresAt(expiresAt)
	})

	s.Require().NoError(s.repo.ResetUsage(s.ctx, sub.ID), "ResetUsage")

	got, err := s.repo.GetByID(s.ctx, sub.ID)
	s.Require().NoError(err)
	s.Require().InDelta(0.0, got.TotalUsageUSD, 1e-6, "用量应清零")
	s.Require().NotNil(got.TotalLimitUSD)
	s.Require().InDelta(100.0, *got.TotalLimitUSD, 1e-6, "重置用量不得改动额度")
	s.Require().Equal(service.SubscriptionStatusActive, got.Status, "重置用量不得改动状态")
	s.Require().WithinDuration(expiresAt, got.ExpiresAt, time.Second)
	s.Require().InDelta(100.0, *got.RemainingUSD(), 1e-6, "重置后可用额度应恢复满额")
}

func (s *UserSubscriptionRepoSuite) TestResetUsage_NotFound() {
	err := s.repo.ResetUsage(s.ctx, 999999)
	s.Require().Error(err, "expected error for non-existent subscription")
	s.Require().ErrorIs(err, service.ErrSubscriptionNotFound)
}

// --- Usage tracking ---

func (s *UserSubscriptionRepoSuite) TestIncrementUsage() {
	user := s.mustCreateUser("usage@test.com", service.RoleUser)
	sub := s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetTotalLimitUsd(10.0)
	})

	err := s.repo.IncrementUsage(s.ctx, sub.ID, 1.25)
	s.Require().NoError(err, "IncrementUsage")

	got, err := s.repo.GetByID(s.ctx, sub.ID)
	s.Require().NoError(err)
	s.Require().InDelta(1.25, got.TotalUsageUSD, 1e-6)
	s.Require().InDelta(8.75, *got.RemainingUSD(), 1e-6)
}

func (s *UserSubscriptionRepoSuite) TestIncrementUsage_Accumulates() {
	user := s.mustCreateUser("accum@test.com", service.RoleUser)
	sub := s.mustCreateSubscription(user.ID, nil)

	s.Require().NoError(s.repo.IncrementUsage(s.ctx, sub.ID, 1.0))
	s.Require().NoError(s.repo.IncrementUsage(s.ctx, sub.ID, 2.5))

	got, err := s.repo.GetByID(s.ctx, sub.ID)
	s.Require().NoError(err)
	s.Require().InDelta(3.5, got.TotalUsageUSD, 1e-6)
}

// TestIncrementUsage_OverLimitStillRecorded 锁定记账与准入的分工：
// 限额判定在请求前由计费缓存完成，IncrementUsage 只负责如实累加，
// 因此允许把 total_usage_usd 累加到超过 total_limit_usd。
func (s *UserSubscriptionRepoSuite) TestIncrementUsage_OverLimitStillRecorded() {
	user := s.mustCreateUser("over-limit@test.com", service.RoleUser)
	sub := s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetTotalLimitUsd(5.0)
	})

	s.Require().NoError(s.repo.IncrementUsage(s.ctx, sub.ID, 8.0), "IncrementUsage")

	got, err := s.repo.GetByID(s.ctx, sub.ID)
	s.Require().NoError(err)
	s.Require().InDelta(8.0, got.TotalUsageUSD, 1e-6)
	s.Require().False(got.CheckLimit(0), "超额后不可再用")
	s.Require().Nil(got.RemainingUSD(), "超额的钱包没有剩余额度")
}

// TestExtendExpiry_KeepsUsage 锁定「总额池一次性」：续费只延长有效期，绝不重置用量。
func (s *UserSubscriptionRepoSuite) TestExtendExpiry_KeepsUsage() {
	user := s.mustCreateUser("renew-keeps-usage@test.com", service.RoleUser)
	sub := s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetTotalLimitUsd(100.0)
		c.SetTotalUsageUsd(40.0)
	})

	newExpiry := time.Now().AddDate(0, 0, 90)
	s.Require().NoError(s.repo.ExtendExpiry(s.ctx, sub.ID, newExpiry), "ExtendExpiry (renew)")

	got, err := s.repo.GetByID(s.ctx, sub.ID)
	s.Require().NoError(err)
	s.Require().InDelta(40.0, got.TotalUsageUSD, 1e-6, "续费不得清零已用额度")
	s.Require().NotNil(got.TotalLimitUSD)
	s.Require().InDelta(100.0, *got.TotalLimitUSD, 1e-6, "续费不得改动额度")
	s.Require().WithinDuration(newExpiry, got.ExpiresAt, time.Second)
}

// --- UpdateStatus / ExtendExpiry / UpdateNotes ---

func (s *UserSubscriptionRepoSuite) TestUpdateStatus() {
	user := s.mustCreateUser("status@test.com", service.RoleUser)
	sub := s.mustCreateSubscription(user.ID, nil)

	err := s.repo.UpdateStatus(s.ctx, sub.ID, service.SubscriptionStatusExpired)
	s.Require().NoError(err, "UpdateStatus")

	got, err := s.repo.GetByID(s.ctx, sub.ID)
	s.Require().NoError(err)
	s.Require().Equal(service.SubscriptionStatusExpired, got.Status)
}

func (s *UserSubscriptionRepoSuite) TestExtendExpiry() {
	user := s.mustCreateUser("extend@test.com", service.RoleUser)
	sub := s.mustCreateSubscription(user.ID, nil)

	newExpiry := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	err := s.repo.ExtendExpiry(s.ctx, sub.ID, newExpiry)
	s.Require().NoError(err, "ExtendExpiry")

	got, err := s.repo.GetByID(s.ctx, sub.ID)
	s.Require().NoError(err)
	s.Require().WithinDuration(newExpiry, got.ExpiresAt, time.Microsecond)
}

func (s *UserSubscriptionRepoSuite) TestUpdateNotes() {
	user := s.mustCreateUser("notes@test.com", service.RoleUser)
	sub := s.mustCreateSubscription(user.ID, nil)

	err := s.repo.UpdateNotes(s.ctx, sub.ID, "VIP user")
	s.Require().NoError(err, "UpdateNotes")

	got, err := s.repo.GetByID(s.ctx, sub.ID)
	s.Require().NoError(err)
	s.Require().Equal("VIP user", got.Notes)
}

// --- GetByIDForUpdate ---

func (s *UserSubscriptionRepoSuite) TestGetByIDForUpdate() {
	user := s.mustCreateUser("forupdate@test.com", service.RoleUser)
	sub := s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetTotalLimitUsd(10.0)
	})

	got, err := s.repo.GetByIDForUpdate(s.ctx, sub.ID)
	s.Require().NoError(err, "GetByIDForUpdate")
	s.Require().Equal(sub.ID, got.ID)
	s.Require().NotNil(got.TotalLimitUSD)
	s.Require().InDelta(10.0, *got.TotalLimitUSD, 1e-6)

	_, err = s.repo.GetByIDForUpdate(s.ctx, 999999)
	s.Require().ErrorIs(err, service.ErrSubscriptionNotFound)
}

// --- ListExpired / BatchUpdateExpiredStatus ---

func (s *UserSubscriptionRepoSuite) TestListExpired() {
	user := s.mustCreateUser("listexp@test.com", service.RoleUser)

	s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetExpiresAt(time.Now().Add(24 * time.Hour))
	})
	s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetExpiresAt(time.Now().Add(-24 * time.Hour))
	})

	expired, err := s.repo.ListExpired(s.ctx)
	s.Require().NoError(err, "ListExpired")
	s.Require().NotEmpty(expired)
	for _, sub := range expired {
		s.Require().True(sub.IsExpired(), "ListExpired 只应返回已过期的 active 行")
	}
}

func (s *UserSubscriptionRepoSuite) TestBatchUpdateExpiredStatus() {
	user := s.mustCreateUser("batch@test.com", service.RoleUser)

	active := s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetExpiresAt(time.Now().Add(24 * time.Hour))
	})
	expiredActive := s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetExpiresAt(time.Now().Add(-24 * time.Hour))
	})

	affected, err := s.repo.BatchUpdateExpiredStatus(s.ctx)
	s.Require().NoError(err, "BatchUpdateExpiredStatus")
	s.Require().GreaterOrEqual(affected, int64(1))

	gotActive, _ := s.repo.GetByID(s.ctx, active.ID)
	s.Require().Equal(service.SubscriptionStatusActive, gotActive.Status)

	gotExpired, _ := s.repo.GetByID(s.ctx, expiredActive.ID)
	s.Require().Equal(service.SubscriptionStatusExpired, gotExpired.Status)
}

// --- 组合场景：多份钱包 + 先到期先消耗 ---

// TestWalletStack_ConsumptionOrderAndAggregate 验证钱包叠加的产品语义：
// 同一用户多份订阅共存，消耗顺序按到期时间，可用总额 = Σlimit − Σusage
// （这正是计费缓存把多份订阅聚合成一个键的等价性依据）。
func (s *UserSubscriptionRepoSuite) TestWalletStack_ConsumptionOrderAndAggregate() {
	user := s.mustCreateUser("wallet-stack@test.com", service.RoleUser)
	now := time.Now()

	first := s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetExpiresAt(now.Add(2 * time.Hour))
		c.SetTotalLimitUsd(10.0)
	})
	second := s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetExpiresAt(now.Add(24 * time.Hour))
		c.SetTotalLimitUsd(50.0)
	})

	s.Require().NoError(s.repo.IncrementUsage(s.ctx, first.ID, 4.0), "IncrementUsage first")
	s.Require().NoError(s.repo.IncrementUsage(s.ctx, second.ID, 6.0), "IncrementUsage second")

	subs, err := s.repo.ListActiveByUserID(s.ctx, user.ID)
	s.Require().NoError(err, "ListActiveByUserID")
	s.Require().Len(subs, 2, "重复购买/多份钱包应共存，不做分组合并")
	s.Require().Equal(first.ID, subs[0].ID, "最先消耗的应是最快到期的钱包")
	s.Require().Equal(second.ID, subs[1].ID)

	var totalLimit, totalUsage float64
	for i := range subs {
		if subs[i].TotalLimitUSD != nil {
			totalLimit += *subs[i].TotalLimitUSD
		}
		totalUsage += subs[i].TotalUsageUSD
	}
	s.Require().InDelta(60.0, totalLimit, 1e-6)
	s.Require().InDelta(10.0, totalUsage, 1e-6)
	s.Require().InDelta(50.0, totalLimit-totalUsage, 1e-6, "聚合可花金额 = Σlimit - Σusage")

	// 单笔费用可跨钱包拆分（分配算法在 service 层，这里提供有序输入）。
	// ListActiveByUserID 返回的正是按 expires_at 升序的消耗队列。
	ptrs := make([]*service.UserSubscription, 0, len(subs))
	for i := range subs {
		ptrs = append(ptrs, &subs[i])
	}
	allocs, ok := service.AllocateSubscriptionUsage(ptrs, 12.0)
	s.Require().True(ok, "12.0 应由两份钱包共同承担")
	s.Require().Len(allocs, 2)
	s.Require().InDelta(6.0, allocs[0].Amount, 1e-6, "先到期的钱包应被先打空")
	s.Require().Equal(first.ID, allocs[0].SubscriptionID)
	var sum float64
	for _, a := range allocs {
		sum += a.Amount
	}
	s.Require().InDelta(12.0, sum, 1e-6)
}

// --- 软删除过滤测试 ---

func (s *UserSubscriptionRepoSuite) TestIncrementUsage_SoftDeletedSubscription() {
	user := s.mustCreateUser("softdeleted@test.com", service.RoleUser)
	sub := s.mustCreateSubscription(user.ID, nil)

	s.Require().NoError(s.repo.Delete(s.ctx, sub.ID), "soft delete subscription")

	err := s.repo.IncrementUsage(s.ctx, sub.ID, 1.0)
	s.Require().Error(err, "should fail for soft-deleted subscription")
	s.Require().ErrorIs(err, service.ErrSubscriptionNotFound)
}

func (s *UserSubscriptionRepoSuite) TestIncrementUsage_NotFound() {
	err := s.repo.IncrementUsage(s.ctx, 999999, 1.0)
	s.Require().Error(err, "should fail for non-existent subscription")
	s.Require().ErrorIs(err, service.ErrSubscriptionNotFound)
}

// --- nil 入参测试 ---

func (s *UserSubscriptionRepoSuite) TestCreate_NilInput() {
	err := s.repo.Create(s.ctx, nil)
	s.Require().Error(err, "Create should fail with nil input")
	s.Require().ErrorIs(err, service.ErrSubscriptionNilInput)
}

func (s *UserSubscriptionRepoSuite) TestUpdate_NilInput() {
	err := s.repo.Update(s.ctx, nil)
	s.Require().Error(err, "Update should fail with nil input")
	s.Require().ErrorIs(err, service.ErrSubscriptionNilInput)
}

// --- 并发用量更新测试 ---

func (s *UserSubscriptionRepoSuite) TestIncrementUsage_Concurrent() {
	user := s.mustCreateUser("concurrent@test.com", service.RoleUser)
	sub := s.mustCreateSubscription(user.ID, func(c *dbent.UserSubscriptionCreate) {
		c.SetTotalLimitUsd(100.0)
	})

	const numGoroutines = 10
	const incrementPerGoroutine = 1.5

	// 启动多个 goroutine 并发调用 IncrementUsage
	errCh := make(chan error, numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			errCh <- s.repo.IncrementUsage(s.ctx, sub.ID, incrementPerGoroutine)
		}()
	}

	// 等待所有 goroutine 完成
	for i := 0; i < numGoroutines; i++ {
		err := <-errCh
		s.Require().NoError(err, "IncrementUsage should succeed")
	}

	// 验证总额池用量累加正确（原子累加，不存在丢更新）
	got, err := s.repo.GetByID(s.ctx, sub.ID)
	s.Require().NoError(err)
	expectedUsage := float64(numGoroutines) * incrementPerGoroutine
	s.Require().InDelta(expectedUsage, got.TotalUsageUSD, 1e-6, "total_usage_usd 应被原子累加")
	s.Require().InDelta(100.0-expectedUsage, *got.RemainingUSD(), 1e-6)
}

func (s *UserSubscriptionRepoSuite) TestTxContext_RollbackIsolation() {
	baseClient := testEntClient(s.T())
	tx, err := baseClient.Tx(context.Background())
	s.Require().NoError(err, "begin tx")
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	txCtx := dbent.NewTxContext(context.Background(), tx)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	userEnt, err := tx.Client().User.Create().
		SetEmail("tx-user-" + suffix + "@example.com").
		SetPasswordHash("test").
		Save(txCtx)
	s.Require().NoError(err, "create user in tx")

	repo := NewUserSubscriptionRepository(baseClient)
	sub := &service.UserSubscription{
		UserID:        userEnt.ID,
		ExpiresAt:     time.Now().AddDate(0, 0, 30),
		Status:        service.SubscriptionStatusActive,
		AssignedAt:    time.Now(),
		TotalLimitUSD: float64Value(30.0),
		Notes:         "tx",
	}
	s.Require().NoError(repo.Create(txCtx, sub), "create subscription in tx")
	s.Require().NoError(repo.UpdateNotes(txCtx, sub.ID, "tx-note"), "update subscription in tx")

	s.Require().NoError(tx.Rollback(), "rollback tx")
	tx = nil

	_, err = repo.GetByID(context.Background(), sub.ID)
	s.Require().ErrorIs(err, service.ErrSubscriptionNotFound)
}
