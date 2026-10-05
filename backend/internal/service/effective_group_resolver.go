package service

import (
	"context"
	"strings"
	"time"
)

// effective_group_resolver.go 实现「按请求模型决议生效分组」所依赖的账号可服务性探针。
//
// 探针语义（冻结契约第 3 条）：
//   - 分组内存在**可调度**且能服务该模型的账号 → ok=true；
//   - priority 返回该分组内「可服务该模型的最优（最小）账号优先级」，并列时按契约用于
//     跨分组比较（priority 小者优先）；
//   - 无任何可服务账号 → ok=false（该分组不参与本次决议）。
//
// 【口径必须与选号同源】2026-10-05 事故：管理员把多分组 Key 主分组（Trae 平台）下的
// 账号全部禁用后，请求仍被决议到该主分组并返回 503「no available accounts」，而不是
// 切到同样能提供该模型的候选分组。根因是探针只判「平台匹配 + IsModelSupported」，
// 完全不看账号能否接单：
//   - listSchedulableAccounts 读的是调度快照 bucket，管理员禁用账号只刷新单账号 meta
//     投影（repository/scheduler_cache.go SetAccount → writeAccountIDs），**bucket 成员
//     不会被移除**，因此缓存列表里照样有那个被禁用的号；
//   - Trae 上游目录本身含 glm-5.3（service/trae.go DefaultTraeModelIDs 首项即 glm-5.3，
//     2026-09-29 实测 solo_agent 通道可通），所以「模型支持」这一判据也拦不住它；
//   - 该号 Priority 缺省 0（= 最高优先级），跨分组比较时按契约「并列取主分组」
//     （api_key_groups.go 排序分支）→ 生效分组被锁死在主分组。
//
// 修法：探针补上真实选号链路使用的同一组闸门（gateway_scheduling.go
// isAccountSchedulableForModelSelection → Account.IsSchedulableForModelWithContext，
// 含 IsSchedulable 的状态/手动停调/过载/限流/临时不可调度/配额判定 + per-model 冷却），
// 以及 OpenAI 兼容引擎在候选过滤阶段就会命中的积分耗尽硬闸门
// （openai_account_scheduler.go 与 legacy 同口径的 shouldAutoPauseAccountByCredits）。
// 判定为「不可服务」时 ResolveEffectiveGroup 会自然把该分组排除出 serviceable 集合，
// 从而决议到其它能服务的候选分组。
//
// 取值范围与调度一致：priority 来自账号的 Priority 字段（缺省 0 = 最高优先级）。
// 该探针只读取调度快照/仓储（带缓存），不做任何写操作，也不修改请求上下文。

// GroupModelAccountProbe 描述一个分组对某模型的可服务性。
type GroupModelAccountProbe struct {
	Servable bool
	Priority int
}

// ProbeGroupModelServability 探测分组是否能服务指定模型，返回最优账号优先级。
//
// platform 为分组自身平台；composite 分组按「任意平台可服务」处理（其真实目标平台
// 由 compositeTargetPlatformMiddleware 在更早的中间件里决议，这里只负责找出
// 「本分组内是否存在能服务该模型的账号」）。
func (s *GatewayService) ProbeGroupModelServability(ctx context.Context, group *Group, model string) GroupModelAccountProbe {
	if s == nil || group == nil || group.ID <= 0 {
		return GroupModelAccountProbe{}
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return GroupModelAccountProbe{}
	}

	// 平台过滤：非 composite 分组按自身平台过滤账号；composite 分组不做平台过滤
	// （由账号自身的 model_mapping 判定可服务性）。
	isComposite := group.Platform == PlatformComposite
	platform := group.Platform

	var accounts []Account
	var err error
	if isComposite {
		accounts, _, err = s.listSchedulableAccounts(ctx, &group.ID, "", false)
	} else if platform == "" {
		accounts, _, err = s.listSchedulableAccounts(ctx, &group.ID, "", true)
	} else {
		accounts, _, err = s.listSchedulableAccounts(ctx, &group.ID, platform, true)
	}
	if err != nil || len(accounts) == 0 {
		return GroupModelAccountProbe{}
	}

	best := 0
	servable := false
	for i := range accounts {
		account := &accounts[i]
		if !isComposite && platform != "" && account.Platform != platform {
			continue
		}
		// 可调度性与 per-(账号,模型) 冷却：与选号链路同源，否则「号已禁用/已停调」
		// 会被判成分组仍可服务，请求被锁进该分组后直接 503 而不跨组（见文件头事故记录）。
		if !account.IsSchedulableForModelWithContext(ctx, model) {
			continue
		}
		// 积分耗尽硬闸门：OpenAI 兼容引擎（Trae/Qoder/WorkBuddy 走这条路）在候选过滤
		// 阶段就会因它拒号；探针不判则同一账号在两侧得到相反答案，照样锁死分组。
		if paused, _ := shouldAutoPauseAccountByCredits(account, time.Now()); paused {
			continue
		}
		if !account.IsModelSupported(model) {
			continue
		}
		if !servable || account.Priority < best {
			best = account.Priority
			servable = true
		}
	}
	if !servable {
		return GroupModelAccountProbe{}
	}
	return GroupModelAccountProbe{Servable: true, Priority: best}
}

// GroupModelProbeFunc 返回可直接喂给 ResolveEffectiveGroup 的探针闭包。
// 调用方（routes 中间件）用它把「分组 → 可服务性」绑定到本次请求的 ctx 上。
func (s *GatewayService) GroupModelProbeFunc(ctx context.Context, model string) func(groupID int64) (bool, int) {
	if s == nil {
		return nil
	}
	return func(groupID int64) (bool, int) {
		probe := s.ProbeGroupModelServability(ctx, &Group{ID: groupID, Platform: s.groupPlatformFromContext(ctx, groupID)}, model)
		if !probe.Servable {
			return false, 0
		}
		return true, probe.Priority
	}
}

// groupPlatformFromContext 尽量从调度上下文/快照取分组平台，取不到时返回空字符串
// （空平台表示不做平台过滤，由账号自身的 model_mapping 判定可服务性）。
func (s *GatewayService) groupPlatformFromContext(ctx context.Context, groupID int64) string {
	if group := s.groupFromContext(ctx, groupID); group != nil {
		return group.Platform
	}
	if s.groupRepo != nil {
		group, err := s.groupRepo.GetByIDLite(ctx, groupID)
		if err == nil && group != nil {
			return group.Platform
		}
	}
	return ""
}

// ProbeGroupModelServabilityForGroups 是面向决议器的便捷封装：对一组候选分组逐个探测，
// 返回 ResolveEffectiveGroup 需要的探针函数。候选分组平台从认证快照携带的 Group 对象读取，
// 避免额外 DB 往返。
func ProbeGroupModelServabilityForGroups(
	ctx context.Context,
	gatewayService *GatewayService,
	candidates []*Group,
	model string,
) func(groupID int64) (bool, int) {
	if gatewayService == nil || len(candidates) == 0 || strings.TrimSpace(model) == "" {
		return nil
	}
	byID := make(map[int64]*Group, len(candidates))
	for _, group := range candidates {
		if group != nil {
			byID[group.ID] = group
		}
	}
	return func(groupID int64) (bool, int) {
		group, ok := byID[groupID]
		if !ok {
			return false, 0
		}
		probe := gatewayService.ProbeGroupModelServability(ctx, group, model)
		return probe.Servable, probe.Priority
	}
}
