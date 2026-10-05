package routes

import (
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
//
// 订阅不再绑定分组，也不再参与「生效分组决议」：原「按生效分组重载订阅」探针
// （reloadSubscriptionForEffectiveGroup）已删除。subscriptionService 参数仅为不改动
// 调用方（routes/gateway.go）接线而保留，函数体内不再引用它。
func effectiveGroupMiddleware(gatewayService *service.GatewayService, _ *service.SubscriptionService) gin.HandlerFunc {
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
		// 生效分组变更不再触发订阅读取：订阅是用户维度的额度钱包，与分组无关；
		// auth 中间件已按 user_id 一次性加载该用户全部生效钱包，切组不改变钱包归属。
		c.Next()
	}
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
