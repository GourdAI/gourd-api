package service

import (
	"context"
	"sort"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// api_key_groups.go 实现「一个 API Key 绑定多个分组」的请求期「生效分组」决议。
//
// 设计契约（冻结）：
//   1. 鉴权：候选集合 = key 绑定分组 ∩（状态可用 ∩ 用户有权使用）；至少一个可用即放行。
//   2. 主分组：优先 api_keys.group_id（若仍在候选集内），否则取候选集第一个（确定性排序）。
//   3. 模型感知：候选 >1 时，按请求模型选出生效分组——优先「本分组内账号能服务该模型」的分组，
//      并列时按账号优先级/倍率决定；无法判定时回退主分组。
//   4. 计费倍率/限流/日志：全部取生效分组（就地覆写到 APIKey.GroupID/Group 实现零改动）。
//
// 本文件只提供纯函数式决议逻辑 + 少量仓储/服务协作，便于单测覆盖。

// APIKeyGroupBindingRepository 是关联表读写的窄接口（由 repository 层实现）。
// 用窄接口而非直接依赖具体仓储类型，保持依赖方向 service → repository 且便于测试替身。
type APIKeyGroupBindingRepository interface {
	ListGroupIDsByAPIKeyID(ctx context.Context, apiKeyID int64) ([]int64, error)
	ReplaceAPIKeyGroups(ctx context.Context, apiKeyID int64, groupIDs []int64, primaryGroupID *int64) error
}

// EffectiveGroupDecision 是请求期生效分组决议的结果。
type EffectiveGroupDecision struct {
	// Group 是最终生效的分组对象（已就地覆写到 APIKey）。
	Group *Group
	// GroupID 是生效分组 ID（便于日志与测试断言）。
	GroupID int64
	// Reason 说明决议来源，用于可观测性与排障。
	Reason EffectiveGroupReason
	// Candidates 是参与决议的候选分组（已过滤可用性与授权）。
	Candidates []*Group
}

// EffectiveGroupReason 枚举决议来源。
type EffectiveGroupReason int

const (
	// EffectiveGroupReasonNoCandidates 候选为空（鉴权层应已拦截，此处仅作守卫）。
	EffectiveGroupReasonNoCandidates EffectiveGroupReason = iota
	// EffectiveGroupReasonSingleCandidate 候选只有一个：直接采用（单分组 key 的既有行为）。
	EffectiveGroupReasonSingleCandidate
	// EffectiveGroupReasonPrimary 多个候选中主分组仍在集内：按契约优先主分组。
	EffectiveGroupReasonPrimary
	// EffectiveGroupReasonModelAware 多候选且模型可知：按「账号可服务该模型 + 账号优先级/倍率」选出。
	EffectiveGroupReasonModelAware
	// EffectiveGroupReasonFallbackFirst 无法判定时回退候选集第一个（确定性排序）。
	EffectiveGroupReasonFallbackFirst
)

// ResolveEffectiveGroup 按冻结契约决议生效分组。
//
// model 为空或候选唯一或模型无法判定时回退主分组语义；本函数不改动 apiKey，
// 由调用方（服务方法 / 中间件）使用 cloneAPIKeyWithGroup 同款手法覆写。
func ResolveEffectiveGroup(apiKey *APIKey, model string, groupModelAvailability func(groupID int64) (bool, int)) *EffectiveGroupDecision {
	if apiKey == nil {
		return &EffectiveGroupDecision{Reason: EffectiveGroupReasonNoCandidates}
	}

	candidates := availableAPIKeyGroups(apiKey)
	if len(candidates) == 0 {
		return &EffectiveGroupDecision{Reason: EffectiveGroupReasonNoCandidates}
	}

	if len(candidates) == 1 {
		return &EffectiveGroupDecision{
			Group:      candidates[0],
			GroupID:    candidates[0].ID,
			Reason:     EffectiveGroupReasonSingleCandidate,
			Candidates: candidates,
		}
	}

	primary := primaryAPIKeyGroup(apiKey, candidates)

	// 模型不可知（或未注入可用性探针）时，按契约回退主分组。
	if model == "" || groupModelAvailability == nil {
		if primary != nil {
			return &EffectiveGroupDecision{
				Group:      primary,
				GroupID:    primary.ID,
				Reason:     EffectiveGroupReasonPrimary,
				Candidates: candidates,
			}
		}
		return &EffectiveGroupDecision{
			Group:      candidates[0],
			GroupID:    candidates[0].ID,
			Reason:     EffectiveGroupReasonFallbackFirst,
			Candidates: candidates,
		}
	}

	// 模型可知：只保留「本分组内存在能服务该模型的账号」的候选。
	type scored struct {
		group    *Group
		priority int
	}
	serviceable := make([]scored, 0, len(candidates))
	for _, group := range candidates {
		ok, priority := groupModelAvailability(group.ID)
		if !ok {
			continue
		}
		serviceable = append(serviceable, scored{group: group, priority: priority})
	}

	if len(serviceable) == 0 {
		// 没有分组能服务该模型：回退主分组，让下游返回真实的「无可用账号」错误，
		// 而不是在这里误判成模型不可用。
		if primary != nil {
			return &EffectiveGroupDecision{
				Group:      primary,
				GroupID:    primary.ID,
				Reason:     EffectiveGroupReasonPrimary,
				Candidates: candidates,
			}
		}
		return &EffectiveGroupDecision{
			Group:      candidates[0],
			GroupID:    candidates[0].ID,
			Reason:     EffectiveGroupReasonFallbackFirst,
			Candidates: candidates,
		}
	}

	// 并列时按账号优先级（数值小者优先）决定；再并列时按主分组优先，最后按 ID 升序，保证确定性。
	sort.SliceStable(serviceable, func(i, j int) bool {
		if serviceable[i].priority != serviceable[j].priority {
			return serviceable[i].priority < serviceable[j].priority
		}
		if primary != nil {
			iPrimary := serviceable[i].group.ID == primary.ID
			jPrimary := serviceable[j].group.ID == primary.ID
			if iPrimary != jPrimary {
				return iPrimary
			}
		}
		return serviceable[i].group.ID < serviceable[j].group.ID
	})

	chosen := serviceable[0].group
	return &EffectiveGroupDecision{
		Group:      chosen,
		GroupID:    chosen.ID,
		Reason:     EffectiveGroupReasonModelAware,
		Candidates: candidates,
	}
}

// availableAPIKeyGroups 返回 key 的可用候选分组（保持 apiKey.GroupIDs 顺序）。
//
// 「可用」定义（与鉴权契约一致）：
//  1. 分组对象存在、状态可用（非 deleted、IsActive）；
//  2. 用户对该分组仍有绑定权限——标准分组复查 user.CanBindGroup（堵住「授权被撤销后
//     决议层仍选用该分组」的越权，P1①）；订阅型分组的有效性由运行期订阅校验负责
//     （与鉴权层 validateAPIKeyGroupAllowed 对订阅型候选无条件放行的口径一致）。
//
// 注意：apiKey.User 为 nil 时跳过授权复查（fail-open），以保留单分组 key 与测试替身的既有行为；
// 鉴权层已在此之前用同一 CanBindGroup 校验过候选集合，此处是请求期决议的第二道闸。
func availableAPIKeyGroups(apiKey *APIKey) []*Group {
	if apiKey == nil {
		return nil
	}
	usable := func(group *Group) bool {
		if !IsGroupUsableForResolution(group) {
			return false
		}
		if apiKey.User != nil && !group.IsSubscriptionType() {
			if !apiKey.User.CanBindGroup(group.ID, group.IsExclusive) {
				return false
			}
		}
		return true
	}
	if len(apiKey.Groups) > 0 {
		out := make([]*Group, 0, len(apiKey.Groups))
		for _, group := range apiKey.Groups {
			if usable(group) {
				out = append(out, group)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	// 退化路径：快照未物化候选对象时，用主分组单元素集合（单分组 key 行为不变）。
	if usable(apiKey.Group) {
		return []*Group{apiKey.Group}
	}
	return nil
}

// primaryAPIKeyGroup 在候选集中定位主分组：优先 api_keys.group_id，否则候选集第一个。
func primaryAPIKeyGroup(apiKey *APIKey, candidates []*Group) *Group {
	if apiKey == nil || len(candidates) == 0 {
		return nil
	}
	if apiKey.GroupID != nil {
		for _, group := range candidates {
			if group != nil && group.ID == *apiKey.GroupID {
				return group
			}
		}
	}
	return candidates[0]
}

// EffectiveAPIKeyGroup 返回请求级生效分组：优先 request ctx 上的 ctxkey.Group（认证/决议
// 中间件写入的即为生效分组），否则回退 apiKey.Group（未决议时保持历史语义）。
func EffectiveAPIKeyGroup(ctx context.Context, apiKey *APIKey) (*Group, bool) {
	if ctx != nil {
		if group, ok := ctx.Value(ctxkey.Group).(*Group); ok && group != nil && IsGroupContextValid(group) {
			return group, true
		}
	}
	if apiKey != nil && apiKey.Group != nil {
		return apiKey.Group, true
	}
	return nil, false
}

// WithEffectiveGroup 把生效分组写入 request ctx，供需要在 request ctx 上读取的链路使用
// （与 gin context 中的 APIKey 覆写并行存在，二者保持同一分组）。
func WithEffectiveGroup(ctx context.Context, group *Group) context.Context {
	if ctx == nil || !IsGroupContextValid(group) {
		return ctx
	}
	if existing, ok := ctx.Value(ctxkey.Group).(*Group); ok && existing != nil && existing.ID == group.ID {
		return ctx
	}
	return context.WithValue(ctx, ctxkey.Group, group)
}

// EffectiveAPIKeyWithGroup 用「生效分组」覆写 APIKey 的 GroupID/Group（保留候选集合）。
//
// 这是 handler 包 cloneAPIKeyWithGroup 的服务层等价实现：
//   - 返回浅拷贝，绝不修改传入对象（避免污染认证缓存物化出来的共享对象 / L1 缓存）；
//   - GroupID 与 Group 同步替换，使下游 ~120 处读取点零改动；
//   - GroupIDs/Groups 保持不变，保留候选集合信息。
//
// P1②（RPM 阈值串组）：User.UserGroupRPMOverride 是快照构建时按**主分组**预物化的
// (user, group) RPM 覆盖值。生效分组切换后该值不再适用，必须连同 User 一起克隆并清空，
// 迫使 checkRPM 按生效分组回查 DB（billing_cache_service.go 的 override==nil 分支），
// 否则会用主分组的阈值去卡生效分组的计数桶——既污染该桶，又可能在主分组 override=0
// （免检）时静默绕过生效分组的 group.RPMLimit。
func EffectiveAPIKeyWithGroup(apiKey *APIKey, group *Group) *APIKey {
	if apiKey == nil || group == nil {
		return apiKey
	}
	if apiKey.Group != nil && apiKey.Group.ID == group.ID {
		// 生效分组与当前分组一致：无需覆写，直接复用（热路径优化，避免多余分配）。
		return apiKey
	}
	cloned := *apiKey
	groupID := group.ID
	cloned.GroupID = &groupID
	cloned.Group = group
	if cloned.User != nil && cloned.User.UserGroupRPMOverride != nil {
		// 克隆 User 后清空按主分组预物化的 RPM override，避免共享指针被改、并强制按生效分组回查。
		userCopy := *cloned.User
		userCopy.UserGroupRPMOverride = nil
		cloned.User = &userCopy
	}
	return &cloned
}

// IsGroupUsableForResolution 判定分组对象是否可用于决议（非空、已水合、未删除且启用）。
// 注意：这是决议专用判定，不要与 ctx 上下文的 IsGroupContextValid 混淆。
func IsGroupUsableForResolution(group *Group) bool {
	if group == nil || group.ID <= 0 {
		return false
	}
	return group.IsActive()
}

// ResolveEffectiveGroupForRequest 是请求期生效分组决议的服务入口。
//
// groupModelPriority 为「某分组内可服务指定模型的最优账号优先级」探针：
// 返回 (ok, priority)，ok=false 表示该分组内没有能服务该模型的账号。
// 传 nil 时退化为「非模型感知」决议（按主分组优先）。
//
// 本方法只做决议，不修改 apiKey；调用方（routes 中间件）用 EffectiveAPIKeyWithGroup
// 就地覆写 gin context 里的 APIKey，使下游计费/限流/日志代码零改动。
func (s *APIKeyService) ResolveEffectiveGroupForRequest(apiKey *APIKey, model string, groupModelPriority func(groupID int64) (bool, int)) *EffectiveGroupDecision {
	return ResolveEffectiveGroup(apiKey, model, groupModelPriority)
}

// HasMultipleCandidateGroups 报告该 key 是否绑定了多个候选分组（>1）。
// 供中间件在热路径上快速短路（单分组/未绑定请求直接跳过决议）。
func HasMultipleCandidateGroups(apiKey *APIKey) bool {
	if apiKey == nil {
		return false
	}
	return len(apiKey.GroupIDs) > 1 || len(apiKey.Groups) > 1
}

// PricingCandidateGroups 返回「定价判定」应当考察的分组集合（并集口径的输入）。
//
// 为什么需要它：定价准入闸门与模型列表过滤器此前只读 apiKey.Group（请求期生效
// 分组）。生效分组在多分组 Key 上并不总是覆盖真正的计费上下文——决议可能因模型
// 不可读（GET /models、视频轮询）、探针全零或网关服务缺失而回退主分组，此时
// 「只在候选 B 配了价」的模型会被判为未定价：请求侧 404 误杀，列表侧则隐身。
// 而计费按实际选中的账号所在分组算钱，其定价来源可以是任一候选分组，因此准入
// 判定必须与调度口径同源：任一候选分组能解析出价格即视为已定价。
//
// 语义约束：
//   - 生效分组（apiKey.Group，可能已被 effectiveGroupMiddleware 覆写）一定出现在
//     结果中；候选集合缺失（快照未物化）时退化为单分组，与历史行为完全一致；
//   - 不在此处做 CanBindGroup/IsActive 复查——那是「分组能否服务本请求」的决议语
//     义，与本函数要回答的「这个模型算不算得出价」无关，混用会把已定价模型误判
//     为未定价（闸门只应看价格配置是否存在）。
func PricingCandidateGroups(apiKey *APIKey) []*Group {
	if apiKey == nil {
		return nil
	}
	if len(apiKey.Groups) == 0 {
		if apiKey.Group == nil {
			return nil
		}
		return []*Group{apiKey.Group}
	}
	// 生效分组不在候选集内（决议覆写到一个未物化的分组等边缘形态）时前置补入，
	// 保证「生效分组自己的价」永远参与判定，不会因候选集陈旧而漏判。
	if apiKey.Group != nil {
		for _, group := range apiKey.Groups {
			if group != nil && apiKey.Group.ID != 0 && group.ID == apiKey.Group.ID {
				return apiKey.Groups
			}
		}
		return append([]*Group{apiKey.Group}, apiKey.Groups...)
	}
	return apiKey.Groups
}
