package routes

import (
	"context"
	"net/http"
	"strings"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// effective_group_middleware.go 实现「请求期生效分组决议」中间件。
//
// 与 compositeTargetPlatformMiddleware 同构：
//   - 位置：紧邻 compositeTarget 之前注册（模型白名单准入之后），保证
//     ① 决议只发生在模型已可读的时刻；② 后续所有平台分支读到的是生效分组。
//   - 手法：读 body 取模型 → 决议生效分组 → 就地覆写 gin context 中的 APIKey
//     （与 handler 包 cloneAPIKeyWithGroup 同款：浅拷贝替换 GroupID/Group，
//     GroupIDs/Groups 候选集合保持不变）。
//
// 零改动保证：下游 ~120 处 apiKey.GroupID / apiKey.Group 读取点无需修改即可
// 拿到生效分组；计费倍率、组级 RPM、usage_logs.group_id 自动跟随生效分组。
//
// 契约：
//   - 单分组 / 未绑定分组：中间件立即放行（热路径，零开销）；
//   - 多分组：model 可读时按模型决议；GET/无 body 端点（/v1/models、/v1/usage 等）
//     回退主分组，不影响既有行为；
//   - 决议结果同时写入 request ctx 的 ctxkey.Group（与 setGroupContext 语义一致），
//     供需要在 request ctx 上读分组的链路使用。
func effectiveGroupMiddleware(gatewayService *service.GatewayService, subscriptionService *service.SubscriptionService) gin.HandlerFunc {
	// 把订阅重载抽成函数类型：*service.SubscriptionService 为 nil 时 loader 也为 nil，
	// 避开 typed-nil 接口陷阱，同时让 reloadSubscriptionForEffectiveGroup 可被单测替身驱动。
	var loader subscriptionLoader
	if subscriptionService != nil {
		loader = subscriptionService.GetActiveSubscription
	}
	return func(c *gin.Context) {
		apiKey, ok := middleware.GetAPIKeyFromContext(c)
		if !ok || apiKey == nil {
			c.Next()
			return
		}
		// 快速短路：仅多分组 key 需要决议。
		if !service.HasMultipleCandidateGroups(apiKey) {
			c.Next()
			return
		}

		model := effectiveGroupRequestModel(c)
		var probe func(groupID int64) (bool, int)
		if model != "" && gatewayService != nil && c.Request != nil {
			probe = service.ProbeGroupModelServabilityForGroups(
				c.Request.Context(),
				gatewayService,
				candidateGroups(apiKey),
				model,
			)
		}

		decision := service.ResolveEffectiveGroup(apiKey, model, probe)
		if decision == nil || decision.Group == nil {
			c.Next()
			return
		}

		effective := service.EffectiveAPIKeyWithGroup(apiKey, decision.Group)
		if effective != apiKey {
			// 就地覆写：后续中间件与 handler 读到的即为生效分组。
			c.Set(string(middleware.ContextKeyAPIKey), effective)
		}
		if decision.Group.ID > 0 && c.Request != nil {
			c.Request = c.Request.WithContext(service.WithEffectiveGroup(c.Request.Context(), decision.Group))
		}
		// P1③：生效分组为订阅型且与 auth 阶段按主分组加载的订阅不同组时，按生效分组
		// 重新加载订阅并覆写 ctx，保证 usage_logs.subscription_id 归属到真正提供服务的分组。
		// 重载失败（生效分组无有效订阅）时**不覆写**：保留主分组订阅，由计费层
		// checkSubscriptionEligibility 按生效分组查出无订阅而拒绝——绝不沿用主分组订阅放行，
		// 也不降级为余额模式（避免引入新的放行路径）。
		reloadSubscriptionForEffectiveGroup(c, decision.Group, apiKey.UserID, loader)
		c.Next()
	}
}

// subscriptionLoader 是「按 (user, group) 取有效订阅」的窄依赖，
// *service.SubscriptionService.GetActiveSubscription 直接满足该签名。
type subscriptionLoader func(ctx context.Context, userID, groupID int64) (*service.UserSubscription, error)

// reloadSubscriptionForEffectiveGroup 在生效分组为订阅型且与 ctx 中已加载订阅（按主分组
// 加载）不属于同一分组时，按生效分组重新加载订阅并覆写 ctx。
//
// 只在重载成功时覆写；失败/无订阅时保持原样，交由计费层按生效分组拒绝（见调用处注释）。
func reloadSubscriptionForEffectiveGroup(
	c *gin.Context,
	group *service.Group,
	userID int64,
	loader subscriptionLoader,
) {
	if loader == nil || group == nil || !group.IsSubscriptionType() {
		return
	}
	if current, ok := middleware.GetSubscriptionFromContext(c); ok && current != nil && current.GroupID == group.ID {
		return // ctx 中已是生效分组的订阅，无需重载。
	}
	sub, err := loader(c.Request.Context(), userID, group.ID)
	if err != nil || sub == nil {
		return // 不覆写：保留主分组订阅，计费层会按生效分组拒绝。
	}
	c.Set(string(middleware.ContextKeySubscription), sub)
}

// candidateGroups 返回 key 的候选分组对象集合（快照已物化；缺失时退化为主分组）。
func candidateGroups(apiKey *service.APIKey) []*service.Group {
	if apiKey == nil {
		return nil
	}
	if len(apiKey.Groups) > 0 {
		return apiKey.Groups
	}
	if apiKey.Group != nil {
		return []*service.Group{apiKey.Group}
	}
	return nil
}

// effectiveGroupRequestModel 从当前请求解析模型名，复用与 composite 决议完全一致的
// 解析路径（路径参数 / 查询参数 / 请求体，含 Live 的 session.model）。
//
// 只读不改写 body：model 已可读时优先复用中间件链上游已缓存的 request body；
// 无 body 的 GET 端点（/v1/models、/v1/usage、视频查询等）返回空串，决议回退主分组。
func effectiveGroupRequestModel(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
		// GET 端点不携带模型（视频轮询等按主分组语义处理）。
		return ""
	}
	routePath := c.FullPath()

	// 先看路径参数（Gemini 的 /models/{model}:{action} 与 /models/:model）。
	if model := strings.TrimSpace(c.Param("model")); model != "" {
		return model
	}
	if modelAction := strings.TrimSpace(c.Param("modelAction")); modelAction != "" {
		trimmed := strings.TrimPrefix(modelAction, "/")
		if idx := strings.LastIndex(trimmed, ":"); idx >= 0 {
			return strings.TrimSpace(trimmed[:idx])
		}
		return trimmed
	}

	body, ok := peekRequestBody(c.Request)
	if !ok || len(body) == 0 {
		return ""
	}
	return strings.TrimSpace(requestmodel.FromBodyForRoute(routePath, c.GetHeader("Content-Type"), body))
}

// peekRequestBody 尽量复用请求上已缓存的 body，避免重复读取 IO。
// 读取后必须原样复位（与 compositeTargetPlatformMiddleware 的 data retention 约定一致）。
func peekRequestBody(req *http.Request) ([]byte, bool) {
	if req == nil {
		return nil, false
	}
	body, err := pkghttputil.ReadRequestBodyWithPrealloc(req)
	if err != nil {
		return nil, false
	}
	requestmodel.ResetRequestBody(req, body)
	return body, true
}
