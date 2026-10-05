package service

// 生效分组「账号可服务性探针」的口径回归测试。
//
// 事故背景（2026-10-05 排查）：多分组 Key 的主分组是 Trae 平台分组，另有候选分组含
// glm-5.3 账号。管理员把主分组下的 Trae 账号全部禁用后，请求仍被决议到主分组并返回
// 503「Service temporarily unavailable」（no available accounts），而不是切到候选分组。
//
// 根因是探针口径与选号口径不同源：探针只判「平台匹配 + IsModelSupported」，不看账号
// 能否接单；而
//   - 调度快照 bucket 的成员不会因管理员禁用账号而移除（禁用只刷新单账号 meta 投影），
//     被禁用的号照样出现在 listSchedulableAccounts 结果里；
//   - Trae 上游目录本身含 glm-5.3（见 trae.go DefaultTraeModelIDs 首项），且空
//     model_mapping 一律放行所有模型，IsModelSupported 这一判据也拦不住；
//   - 号被禁后 Priority 仍是 0（最高优先级），跨分组比较「并列取主分组」→ 锁死主分组。
//
// 本文件锁定修复后的行为：探针必须复用选号链路的同一组闸门
// （IsSchedulableForModelWithContext + 积分耗尽硬闸门），并守住「健康号不误杀」的边界。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// probeBucketCacheStub 按 bucket.GroupID 分派账号列表，让探针能区分「主分组」与
// 「候选分组」各自的账号池（openAISnapshotCacheStub 忽略 bucket，做不到这点）。
type probeBucketCacheStub struct {
	SchedulerCache
	byGroup map[int64][]*Account
}

func (s *probeBucketCacheStub) GetSnapshot(_ context.Context, bucket SchedulerBucket) ([]*Account, bool, error) {
	accounts := s.byGroup[bucket.GroupID]
	if len(accounts) == 0 {
		return nil, false, nil
	}
	out := make([]*Account, 0, len(accounts))
	for _, account := range accounts {
		if account == nil {
			continue
		}
		cloned := *account
		out = append(out, &cloned)
	}
	return out, true, nil
}

// newProbeGatewayService 构造只带调度快照的 GatewayService：rateLimitService 为 nil
// 时 filterAccountsBySchedulingThreshold 直接放行（isAccountBlockedBySchedulingThreshold
// 的 nil 守卫），因此被测的就是探针本身。
func newProbeGatewayService(byGroup map[int64][]*Account) *GatewayService {
	return &GatewayService{
		schedulerSnapshot: NewSchedulerSnapshotService(&probeBucketCacheStub{byGroup: byGroup}, nil, nil, nil, nil),
	}
}

func probeTraeAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformTrae,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Priority:    0,
		Concurrency: 4,
	}
}

func probeGroup(id int64, platform string) *Group {
	return &Group{
		ID:       id,
		Platform: platform,
		Status:   StatusActive,
		Hydrated: true,
	}
}

const probeModel = "glm-5.3"

// TestProbeGroupModelServability_ExcludesDisabledAccount 锁定核心缺陷：分组内唯一的
// 账号被禁用后，该分组必须判为「不可服务」，否则生效分组会继续锁死在它上面。
func TestProbeGroupModelServability_ExcludesDisabledAccount(t *testing.T) {
	ctx := context.Background()
	disabled := probeTraeAccount(43001)
	disabled.Schedulable = false // 管理面板「参与调度」开关的落库形态
	require.False(t, disabled.IsSchedulable(), "前提：手动停调的账号不可调度")

	svc := newProbeGatewayService(map[int64][]*Account{7: {disabled}})

	probe := svc.ProbeGroupModelServability(ctx, probeGroup(7, PlatformTrae), probeModel)
	require.False(t, probe.Servable, "账号已全部禁用，分组必须判为不可服务（修复前为 true）")
	require.Zero(t, probe.Priority)
}

// TestProbeGroupModelServability_ExcludesErrorStatusAccount 覆盖另一种禁用形态：
// 编辑弹窗把 status 改成非 active（error/disabled）。
func TestProbeGroupModelServability_ExcludesErrorStatusAccount(t *testing.T) {
	ctx := context.Background()
	errored := probeTraeAccount(43002)
	errored.Status = StatusError

	svc := newProbeGatewayService(map[int64][]*Account{7: {errored}})

	require.False(t, svc.ProbeGroupModelServability(ctx, probeGroup(7, PlatformTrae), probeModel).Servable,
		"status 非 active 的账号不得让分组冒充可服务")
}

// TestProbeGroupModelServability_ExcludesTempUnschedulable 覆盖运行时自动停调
// （积分耗尽/上游错误写入的 TempUnschedulableUntil）。
func TestProbeGroupModelServability_ExcludesTempUnschedulable(t *testing.T) {
	ctx := context.Background()
	cooling := probeTraeAccount(43003)
	until := time.Now().Add(30 * time.Minute)
	cooling.TempUnschedulableUntil = &until

	svc := newProbeGatewayService(map[int64][]*Account{7: {cooling}})

	require.False(t, svc.ProbeGroupModelServability(ctx, probeGroup(7, PlatformTrae), probeModel).Servable,
		"处于临时不可调度窗口的账号不得让分组冒充可服务")
}

// TestProbeGroupModelServability_ExcludesModelRateLimited 覆盖 per-(账号,模型) 冷却：
// 该模型正在冷却，换一个模型仍可服务（不得放大成账号级判定）。
func TestProbeGroupModelServability_ExcludesModelRateLimited(t *testing.T) {
	ctx := context.Background()
	cooling := probeTraeAccount(43004)
	setAccountModelRateLimitSnapshot(cooling, probeModel, time.Now().Add(30*time.Minute), `{"status_code":404}`, time.Now())

	svc := newProbeGatewayService(map[int64][]*Account{7: {cooling}})
	group := probeGroup(7, PlatformTrae)

	require.False(t, svc.ProbeGroupModelServability(ctx, group, probeModel).Servable,
		"该模型处于冷却时必须判不可服务")
	require.True(t, svc.ProbeGroupModelServability(ctx, group, "kimi-k2.8-preview").Servable,
		"冷却只作用于该模型，其他模型仍可服务")
}

// TestProbeGroupModelServability_HealthyAccountStillServable 反向护栏：健康账号必须
// 照旧可服务。探针收紧的最大风险是把正常分组误判成不可用（导致全候选被排除、请求
// 反而退化成「无可用账号」），这条断言就是拦这个的。
func TestProbeGroupModelServability_HealthyAccountStillServable(t *testing.T) {
	ctx := context.Background()
	healthy := probeTraeAccount(43005)

	svc := newProbeGatewayService(map[int64][]*Account{7: {healthy}})

	probe := svc.ProbeGroupModelServability(ctx, probeGroup(7, PlatformTrae), probeModel)
	require.True(t, probe.Servable, "健康账号的分组必须仍可服务（不得误杀）")
	require.Equal(t, healthy.Priority, probe.Priority)
}

// TestProbeGroupModelServability_MixedPoolUsesBestHealthyAccount 分组内既有禁用号也有
// 健康号时：只要还剩一个能接单的号，分组就仍可服务，且 priority 取健康号的最优值。
// 断言禁用号 priority=0 不得把 best 拉成 0（否则跨分组比较会被已停调的号影响）。
func TestProbeGroupModelServability_MixedPoolUsesBestHealthyAccount(t *testing.T) {
	ctx := context.Background()
	disabledTop := probeTraeAccount(43006)
	disabledTop.Schedulable = false
	disabledTop.Priority = 0
	healthyLow := probeTraeAccount(43007)
	healthyLow.Priority = 5

	svc := newProbeGatewayService(map[int64][]*Account{7: {disabledTop, healthyLow}})

	probe := svc.ProbeGroupModelServability(ctx, probeGroup(7, PlatformTrae), probeModel)
	require.True(t, probe.Servable)
	require.Equal(t, 5, probe.Priority, "已停调账号的 priority 不得参与最优优先级计算")
}

// TestResolveEffectiveGroup_SkipsGroupWithDisabledAccounts 端到端锁定用户诉求：
// 主分组（Trae）账号全被禁用、候选分组仍能服务同一模型时，生效分组必须切到候选分组。
func TestResolveEffectiveGroup_SkipsGroupWithDisabledAccounts(t *testing.T) {
	ctx := context.Background()
	const (
		primaryTraeGroupID = int64(7)
		secondaryGroupID   = int64(9)
	)

	disabledTrae := probeTraeAccount(43008)
	disabledTrae.Schedulable = false
	glmPrimary := &Account{ID: 43009, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 3}

	svc := newProbeGatewayService(map[int64][]*Account{
		primaryTraeGroupID: {disabledTrae},
		secondaryGroupID:   {glmPrimary},
	})

	primaryGroup := probeGroup(primaryTraeGroupID, PlatformTrae)
	secondaryGroup := probeGroup(secondaryGroupID, PlatformOpenAI)

	primaryID := primaryTraeGroupID
	apiKey := &APIKey{
		GroupID:  &primaryID,
		Group:    primaryGroup,
		GroupIDs: []int64{primaryTraeGroupID, secondaryGroupID},
		Groups:   []*Group{primaryGroup, secondaryGroup},
	}

	decision := ResolveEffectiveGroup(apiKey, probeModel,
		ProbeGroupModelServabilityForGroups(ctx, svc, []*Group{primaryGroup, secondaryGroup}, probeModel))

	require.Equal(t, secondaryGroupID, decision.GroupID,
		"主分组账号全禁用时必须切到仍可服务的候选分组（修复前锁死在主分组并 503）")
	require.Equal(t, EffectiveGroupReasonModelAware, decision.Reason)
}

// TestResolveEffectiveGroup_KeepsPrimaryWhenBothServable 反向护栏：两个分组都有健康
// 账号且优先级并列时，按冻结契约仍取主分组（修复不得改变正常路径的选择结果）。
func TestResolveEffectiveGroup_KeepsPrimaryWhenBothServable(t *testing.T) {
	ctx := context.Background()
	const (
		primaryGroupID   = int64(7)
		secondaryGroupID = int64(9)
	)

	traeHealthy := probeTraeAccount(43010)
	glmHealthy := &Account{ID: 43011, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 0}

	svc := newProbeGatewayService(map[int64][]*Account{
		primaryGroupID:   {traeHealthy},
		secondaryGroupID: {glmHealthy},
	})

	primaryGroup := probeGroup(primaryGroupID, PlatformTrae)
	secondaryGroup := probeGroup(secondaryGroupID, PlatformOpenAI)

	primaryID := primaryGroupID
	apiKey := &APIKey{
		GroupID:  &primaryID,
		Group:    primaryGroup,
		GroupIDs: []int64{primaryGroupID, secondaryGroupID},
		Groups:   []*Group{primaryGroup, secondaryGroup},
	}

	decision := ResolveEffectiveGroup(apiKey, probeModel,
		ProbeGroupModelServabilityForGroups(ctx, svc, []*Group{primaryGroup, secondaryGroup}, probeModel))

	require.Equal(t, primaryGroupID, decision.GroupID, "priority 并列时主分组优先的契约不得被破坏")
}
