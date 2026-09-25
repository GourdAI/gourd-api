package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// newPricingGateTestRouter 构造测试路由，按生产链序挂载：
//
//	apiKeyAuth → GroupModelAllowlist → effectiveGroup → compositeTarget → ModelPricingAdmission
//
// 使用真实 NewBillingService 内置价卡（与生产口径一致）。
// effectiveGroup/compositeTarget 两个 hook 默认为 nil（单分组、非合成路由场景
// 用不到）；需要时传入以模拟「生效分组覆写」与「合成路由改写」。
func newPricingGateTestRouter(t *testing.T, requirePriced bool, group *service.Group) (*gin.Engine, *[]string) {
	t.Helper()
	return newPricingGateTestRouterWithHooks(t, requirePriced, group, nil, nil)
}

func newPricingGateTestRouterWithHooks(
	t *testing.T,
	requirePriced bool,
	group *service.Group,
	effectiveGroupHook, compositeTargetHook gin.HandlerFunc,
) (*gin.Engine, *[]string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = requirePriced
	billing := service.NewBillingService(cfg, nil)
	resolver := service.NewModelPricingResolver(nil, billing)
	gate := service.NewModelPricingGate(resolver, cfg)

	router := gin.New()
	var calls []string
	router.Use(func(c *gin.Context) {
		if group != nil {
			c.Set(string(ContextKeyAPIKey), &service.APIKey{Group: group})
		}
		c.Next()
	})
	router.Use(GroupModelAllowlist())
	if effectiveGroupHook != nil {
		router.Use(effectiveGroupHook)
	}
	if compositeTargetHook != nil {
		router.Use(compositeTargetHook)
	}
	router.Use(ModelPricingAdmission(gate))
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		calls = append(calls, "handler")
		c.Status(http.StatusOK)
	})
	return router, &calls
}

// TestModelPricingAdmission_BlocksUnpricedModel 核心验收：
// 未定价模型在入口被 404 拒绝，且请求不进入 handler（不花上游额度）。
func TestModelPricingAdmission_BlocksUnpricedModel(t *testing.T) {
	group := &service.Group{Platform: service.PlatformWorkbuddy}
	router, calls := newPricingGateTestRouter(t, true, group)

	w := doJSON(t, router, http.MethodPost, "/v1/chat/completions", `{"model":"hy4-preview","messages":[]}`)

	if w.Code != http.StatusNotFound {
		t.Fatalf("未定价模型必须 404，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("被拒请求不得进入 handler，实际调用 %v", *calls)
	}
	if !strings.Contains(w.Body.String(), "no pricing is configured") {
		t.Fatalf("拒绝文案应说明未配置定价，实际: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "model_not_found") {
		t.Fatalf("拒绝响应应带 model_not_found code，实际: %s", w.Body.String())
	}
}

// TestModelPricingAdmission_AllowsPricedModel 反向保护：已定价模型不受影响。
func TestModelPricingAdmission_AllowsPricedModel(t *testing.T) {
	group := &service.Group{Platform: service.PlatformDeepseek}
	router, calls := newPricingGateTestRouter(t, true, group)

	w := doJSON(t, router, http.MethodPost, "/v1/chat/completions", `{"model":"deepseek-v4-flash","messages":[]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("已定价模型必须放行，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("已定价模型必须进入 handler，实际调用 %v", *calls)
	}
}

// TestModelPricingAdmission_DisabledAllowsUnpriced 开关关闭时回退旧行为（不拦截）。
func TestModelPricingAdmission_DisabledAllowsUnpriced(t *testing.T) {
	group := &service.Group{Platform: service.PlatformWorkbuddy}
	router, calls := newPricingGateTestRouter(t, false, group)

	w := doJSON(t, router, http.MethodPost, "/v1/chat/completions", `{"model":"hy4-preview","messages":[]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("开关关闭时必须放行，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("开关关闭时必须进入 handler，实际调用 %v", *calls)
	}
}

// TestModelPricingAdmission_GroupPricingUnblocks 管理员配价后必须立即可用
// （否则会出现「配了价仍被拦」的死锁，是本次改动最容易踩的回归点）。
func TestModelPricingAdmission_GroupPricingUnblocks(t *testing.T) {
	input := 1e-6
	output := 2e-6
	group := &service.Group{
		ID:       42,
		Platform: service.PlatformWorkbuddy,
		ModelPricing: []service.ChannelModelPricing{
			{Models: []string{"hy4-preview"}, InputPrice: &input, OutputPrice: &output},
		},
	}
	router, calls := newPricingGateTestRouter(t, true, group)

	w := doJSON(t, router, http.MethodPost, "/v1/chat/completions", `{"model":"hy4-preview","messages":[]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("分组已配价的模型必须放行，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("分组已配价的模型必须进入 handler，实际调用 %v", *calls)
	}
}

// TestModelPricingAdmission_ModelFreeRequestPasses 不带模型的请求（如 /models）交给 handler。
func TestModelPricingAdmission_ModelFreeRequestPasses(t *testing.T) {
	group := &service.Group{Platform: service.PlatformWorkbuddy}
	router, calls := newPricingGateTestRouter(t, true, group)

	w := doJSON(t, router, http.MethodPost, "/v1/chat/completions", `{}`)

	if w.Code != http.StatusOK {
		t.Fatalf("无模型请求必须放行，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("无模型请求必须进入 handler，实际调用 %v", *calls)
	}
}

// TestModelPricingAdmission_EffectiveGroupNotPrimaryGroup 回归：P0 误杀。
//
// 多分组 Key 下，只有分组 B 配了价且只有 B 能服务该模型；effectiveGroup 会把
// context 里的 APIKey 覆写为 B。定价闸门必须读覆写后的生效分组，不能读主分组 A，
// 否则 B 配了价也照样被 404 打死（计费按生效分组算，两侧口径倒挂）。
func TestModelPricingAdmission_EffectiveGroupNotPrimaryGroup(t *testing.T) {
	input := 1e-6
	output := 2e-6
	groupA := &service.Group{ID: 1, Name: "a", Platform: service.PlatformWorkbuddy}
	groupB := &service.Group{
		ID:       2,
		Name:     "b",
		Platform: service.PlatformWorkbuddy,
		ModelPricing: []service.ChannelModelPricing{
			{Models: []string{"hy4-preview"}, InputPrice: &input, OutputPrice: &output},
		},
	}

	// 主分组 = A（无价），候选 = [A, B]。
	apiKey := &service.APIKey{Group: groupA, Groups: []*service.Group{groupA, groupB}, GroupIDs: []int64{1, 2}}

	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = true
	gate := service.NewModelPricingGate(service.NewModelPricingResolver(nil, service.NewBillingService(cfg, nil)), cfg)

	var calls []string
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyAPIKey), apiKey)
		c.Next()
	})
	router.Use(GroupModelAllowlist())
	// 模拟 effectiveGroupMiddleware：决议出生效分组 B 并就地覆写 context。
	router.Use(func(c *gin.Context) {
		ak, _ := GetAPIKeyFromContext(c)
		c.Set(string(ContextKeyAPIKey), service.EffectiveAPIKeyWithGroup(ak, groupB))
		c.Next()
	})
	router.Use(ModelPricingAdmission(gate))
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		calls = append(calls, "handler")
		c.Status(http.StatusOK)
	})

	w := doJSON(t, router, http.MethodPost, "/v1/chat/completions", `{"model":"hy4-preview","messages":[]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("生效分组 B 已配价，必须放行（不得按主分组 A 误杀），实际 %d: %s", w.Code, w.Body.String())
	}
	if len(calls) != 1 {
		t.Fatalf("必须进入 handler，实际调用 %v", calls)
	}
}

// TestModelPricingAdmission_CompositeUsesUpstreamModel 回归：P1① 口径倒挂。
//
// composite 分组下客户端书写的是公开别名，compositeTarget 会把它改写为上游真实
// 模型名——计费按真实模型算，定价闸门也必须按真实模型判。
func TestModelPricingAdmission_CompositeUsesUpstreamModel(t *testing.T) {
	compositeGroup := &service.Group{ID: 9, Name: "combo", Platform: service.PlatformComposite}
	apiKey := &service.APIKey{Group: compositeGroup}

	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = true
	gate := service.NewModelPricingGate(service.NewModelPricingResolver(nil, service.NewBillingService(cfg, nil)), cfg)

	// 别名未配价、上游模型有价（内置兜底卡）。
	const publicAlias = "my-cheap-alias"
	const upstreamModel = "claude-sonnet-4"
	if gate.HasPricing(t.Context(), publicAlias, compositeGroup) {
		t.Fatalf("测试前提不成立：别名 %q 应当未配价", publicAlias)
	}
	if !gate.HasPricing(t.Context(), upstreamModel, compositeGroup) {
		t.Fatalf("测试前提不成立：上游模型 %q 应当已配价", upstreamModel)
	}

	var calls []string
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyAPIKey), apiKey)
		c.Next()
	})
	router.Use(GroupModelAllowlist())
	// 模拟 compositeTargetPlatformMiddleware：解析出上游模型并写进 context，
	// 同时改写请求体（与生产一致）。
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(service.WithCompositeRouteDecision(c.Request.Context(), service.CompositeRouteDecision{
			Matched:        true,
			PublicModel:    publicAlias,
			UpstreamModel:  upstreamModel,
			TargetPlatform: service.PlatformAnthropic,
		}))
		c.Next()
	})
	router.Use(ModelPricingAdmission(gate))
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		calls = append(calls, "handler")
		c.Status(http.StatusOK)
	})

	w := doJSON(t, router, http.MethodPost, "/v1/chat/completions",
		`{"model":"`+publicAlias+`","messages":[]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("上游真实模型已配价，必须放行（不得按未配价的公开别名误杀），实际 %d: %s", w.Code, w.Body.String())
	}
	if len(calls) != 1 {
		t.Fatalf("必须进入 handler，实际调用 %v", calls)
	}
}

// TestModelPricingAdmission_CompositeUnpricedUpstreamStillBlocked 反向保护：
// 合成路由解析出的上游模型本身未配价时，仍必须拦下——不能因为「改写后放行」
// 而让 composite 变成绕过定价闸门的后门。
func TestModelPricingAdmission_CompositeUnpricedUpstreamStillBlocked(t *testing.T) {
	compositeGroup := &service.Group{ID: 9, Name: "combo", Platform: service.PlatformComposite}
	apiKey := &service.APIKey{Group: compositeGroup}

	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = true
	gate := service.NewModelPricingGate(service.NewModelPricingResolver(nil, service.NewBillingService(cfg, nil)), cfg)

	var calls []string
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyAPIKey), apiKey)
		c.Next()
	})
	router.Use(GroupModelAllowlist())
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(service.WithCompositeRouteDecision(c.Request.Context(), service.CompositeRouteDecision{
			Matched:       true,
			PublicModel:   "public-alias",
			UpstreamModel: "hy4-preview",
		}))
		c.Next()
	})
	router.Use(ModelPricingAdmission(gate))
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		calls = append(calls, "handler")
		c.Status(http.StatusOK)
	})

	w := doJSON(t, router, http.MethodPost, "/v1/chat/completions", `{"model":"public-alias","messages":[]}`)

	if w.Code != http.StatusNotFound {
		t.Fatalf("上游模型未配价必须 404，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(calls) != 0 {
		t.Fatalf("被拒请求不得进入 handler，实际调用 %v", calls)
	}
}

// TestModelPricingAdmission_WhiteListStillEnforced 拆分中间件后，原白名单拒绝语义不变。
func TestModelPricingAdmission_WhiteListStillEnforced(t *testing.T) {
	group := &service.Group{
		Platform: service.PlatformDeepseek,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"deepseek-v4-flash"},
		},
	}
	router, calls := newPricingGateTestRouter(t, true, group)

	w := doJSON(t, router, http.MethodPost, "/v1/chat/completions", `{"model":"deepseek-v4-pro","messages":[]}`)

	if w.Code != http.StatusNotFound {
		t.Fatalf("白名单外的模型必须 404，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not available for this group") {
		t.Fatalf("白名单拒绝文案不应被定价文案替换，实际: %s", w.Body.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("被拒请求不得进入 handler，实际调用 %v", *calls)
	}
}

// TestModelPricingAdmission_ParamsModelChecked 路径参数模型（Gemini 原生形态）同样受检。
func TestModelPricingAdmission_ParamsModelChecked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = true
	billing := service.NewBillingService(cfg, nil)
	gate := service.NewModelPricingGate(service.NewModelPricingResolver(nil, billing), cfg)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformGemini}})
		c.Next()
	})
	router.Use(ModelPricingAdmission(gate))
	router.POST("/v1beta/models/*modelAction", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/hy4-preview:generateContent", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("路径参数里的未定价模型必须 404，实际 %d: %s", w.Code, w.Body.String())
	}
}
