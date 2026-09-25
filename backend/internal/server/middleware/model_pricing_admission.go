package middleware

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// model_pricing_admission.go 承载「未定价模型禁止调用」准入中间件。
//
// 挂载位置（关键）：compositeTarget 之后、handler 之前。原因有两条，都来自与
// 「计费到底按哪个模型/哪个分组算钱」对齐的需要：
//
//  1. 生效分组：effectiveGroupMiddleware 会把 gin context 里的 APIKey 就地覆写为
//     请求期决议出的生效分组。定价闸门必须读覆写后的分组，否则多分组 Key 会按
//     主分组判定——而计费按生效分组判定（openai_gateway_usage.go 的
//     resolveOpenAIChannelPricing 用的正是 apiKey.Group），两者倒挂会把「只有
//     另一个分组配了价、且只有那个分组能服务该模型」的请求误杀成 404。
//  2. 合成路由改写：compositeTargetPlatformMiddleware 会把请求体里的公开模型名
//     改写为上游真实模型名。composite 分组下真正被计费的是改写后的模型，因此
//     闸门也必须看改写后的名字（Gemini 原生形态不改写请求体，改从 context 的
//     合成路由决策里取）。
//
// 分组判定集：用「候选分组并集」（service.PricingCandidateGroups）而非单一生效
// 分组。多分组 Key 的生效分组可能回退到主分组（模型不可读、探针全零、网关服务
// 缺失），此时若只按主分组判价，会把「只在候选 B 配了价、且只有 B 能服务该模型」
// 的请求误杀成 404 —— 与历史上「闸门读主分组但计费读生效分组」的同型倒挂。模型
// 列表出口（gateway_models_pricing_filter / WS 逐帧校验）同步用并集，保证「看得见
// 就一定调得通、调得通就一定看得见」。
//
// 白名单准入（GroupModelAllowlist）仍在改写之前校验客户端书写的公开模型名——
// 那是管理员配置白名单时看到的名字，语义不变。两者分工：白名单管「这个分组
// 准不准用这个名字」，定价闸门管「这次调用算不算得出钱」。
//
// 拒绝时返回 404（按入口协议格式），并标记运维业务限流原因
// local_model_configuration 与 ingress 拒绝原因 model_unpriced。
// 开关 pricing.require_priced_models（默认开），置 false 完整回退旧行为。
func ModelPricingAdmission(gate *service.ModelPricingGate) gin.HandlerFunc {
	return func(c *gin.Context) {
		if gate == nil || !gate.Enabled() {
			c.Next()
			return
		}
		apiKey, ok := GetAPIKeyFromContext(c)
		if !ok || apiKey == nil || c.Request == nil {
			c.Next()
			return
		}
		if isResponsesWebSocketRoute(c) {
			// Responses WS 长连接由 handler 校验首帧与每个 response.create。
			c.Next()
			return
		}

		models := pricingAdmissionModels(c)
		if len(models) == 0 {
			c.Next()
			return
		}

		for _, candidate := range models {
			// 并集口径：多分组 Key 下生效分组可能回退到主分组（模型不可读、探针全零
			// 等），此时只看 apiKey.Group 会把「只在候选 B 配了价」的模型误杀成 404。
			// 与调度/计费同源：任一候选分组算得出价即放行。
			if gate.HasPricingForGroups(c.Request.Context(), candidate, service.PricingCandidateGroups(apiKey)) {
				continue
			}
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalModelConfiguration)
			MarkIngressRejected(c, IngressRejectModelUnpriced)
			pricingAdmissionErrorWriter(c)(c, http.StatusNotFound, ModelUnpricedMessage(candidate))
			c.Abort()
			return
		}
		c.Next()
	}
}

// ModelUnpricedMessage 是模型因未配置定价被拒时的统一文案。HTTP 入口中间件与
// Responses WebSocket 逐帧校验共用，保证客户端在各协议入口看到同一句话。
func ModelUnpricedMessage(model string) string {
	return `Model "` + model + `" is not available for this group: no pricing is configured`
}

// pricingAdmissionModels 收集本次请求需要判定的模型名。
//
// 合成路由命中时，以解析出的上游真实模型为唯一判定对象（它才是被计费的对象），
// 不再叠加公开别名：别名是给客户端看的入口名，管理员通常只给上游真实模型配价，
// 把别名一并判定会把「别名无价但上游有价」的请求误杀成 404。合成路由未命中时
// 请求体/路径里的模型名原样透传并被计费，故按那些名字判定。
//
// 请求体候选集含重复键与大小写变体：下游存在 gjson（首个、大小写敏感）、
// encoding/json 绑定（末值、大小写不敏感）与 multipart 表单三类解析器，任一
// 解析器可能绑定到的值都要判，任一未定价即拒绝。
func pricingAdmissionModels(c *gin.Context) []string {
	models := make([]string, 0, 4)
	// 请求体候选集刻意包含重复键与大小写变体（对齐下游三类解析器），但定价判定
	// 对大小写不敏感（计费链入口已 ToLower），同名变体重复判定是纯浪费。按小写
	// 折叠去重：首次出现的原形保留（拒绝文案要回显客户端实际书写的名字）。
	seen := make(map[string]struct{}, 4)
	appendModel := func(model string) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		models = append(models, model)
	}

	// 合成路由决策（composite 分组）：命中即以上游真实模型为准，并排除别名。
	// Gemini 原生形态（compositeGeminiTarget）不改写请求体、模型在路径参数里，
	// 因此必须从 context 的决策里取，不能依赖读请求体。
	if upstream, ok := service.ResolvedUpstreamModelFromContext(c.Request.Context()); ok {
		appendModel(upstream)
		return models
	}

	if model := groupModelAllowlistModelFromParams(c); model != "" {
		appendModel(model)
	} else {
		switch c.Request.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
			if err != nil {
				// 读体失败（如超限）交给下游既有处理：白名单中间件与 handler
				// 会各自按协议返回 400/413，这里不重复写响应。
				return models
			}
			// 必须回填：compositeTarget 只在 composite 分组下读过请求体，其余
			// 分组走到这里时请求体尚未被预读，消费后不回填会让 handler 读到空体。
			requestmodel.ResetRequestBody(c.Request, body)
			for _, candidate := range requestmodel.FromBodyCandidates(c.FullPath(), c.GetHeader("Content-Type"), body) {
				appendModel(candidate)
			}
		}
		if len(models) == 0 {
			// Grok Realtime 升级请求把模型固定在查询参数里。
			appendModel(c.Query("model"))
		}
	}
	return models
}

// pricingAdmissionErrorWriter 按入口协议选择错误格式，与白名单准入完全一致
// （Gemini 原生用 Google 格式，Messages 入口用 Anthropic 格式，其余用 OpenAI 格式）。
func pricingAdmissionErrorWriter(c *gin.Context) GatewayErrorWriter {
	return groupModelAllowlistErrorWriter(c)
}
