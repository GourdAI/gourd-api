package middleware

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// model_pricing_admission_union_test.go 锁定定价准入的「候选分组并集」口径。
//
// 修正前的缺陷：闸门只读生效分组 apiKey.Group。多分组 Key 的生效分组并非总是覆盖
// 真实计费上下文——决议会因模型不可读（GET /models、视频轮询）、可服务性探针全零、
// 网关服务缺失而**回退主分组**，此时「只在候选 B 配了价」的模型会被按主分组 A 判为
// 未定价并 404，而这个请求实际会被调度到 B 组账号、按 B 的价卡正常计费。
// 这与历史上「闸门读主分组、计费读生效分组」的 P0 是同型倒挂，只是触发条件从
// 「链序错」变成「决议回退」。
//
// 修正后口径：任一候选分组能解析出定价即放行（service.PricingCandidateGroups）。

// newUnionGateTestRouter 挂载与生产一致的链序（不含 effectiveGroup 覆写 hook，
// 用于模拟「决议回退主分组」——context 里的 APIKey 保持主分组语义）。
func newUnionGateTestRouter(t *testing.T, apiKey *service.APIKey) (*gin.Engine, *[]string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = true
	gate := service.NewModelPricingGate(
		service.NewModelPricingResolver(nil, service.NewBillingService(cfg, nil)), cfg)

	router := gin.New()
	var calls []string
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyAPIKey), apiKey)
		c.Next()
	})
	router.Use(GroupModelAllowlist())
	router.Use(ModelPricingAdmission(gate))
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		calls = append(calls, "handler")
		c.Status(http.StatusOK)
	})
	return router, &calls
}

// 生效分组回退主分组 A（无价），但候选 B 配了价：必须放行。
func TestModelPricingAdmission_PricedInAnyCandidateGroup(t *testing.T) {
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
	// 刻意不覆写：Group 仍是主分组 A，模拟决议回退（model 不可读 / 探针全零）。
	apiKey := &service.APIKey{Group: groupA, Groups: []*service.Group{groupA, groupB}, GroupIDs: []int64{1, 2}}

	router, calls := newUnionGateTestRouter(t, apiKey)
	w := doJSON(t, router, http.MethodPost, "/v1/chat/completions", `{"model":"hy4-preview","messages":[]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("候选 B 已配价即视为已定价，回退主分组时不得误杀，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("必须进入 handler，实际 %v", *calls)
	}
}

// 反向保护：并集不得退化成"全部放行"——所有候选都无价时仍必须 404。
func TestModelPricingAdmission_NoCandidatePricingStillBlocked(t *testing.T) {
	groupA := &service.Group{ID: 1, Name: "a", Platform: service.PlatformWorkbuddy}
	groupB := &service.Group{ID: 2, Name: "b", Platform: service.PlatformWorkbuddy}
	apiKey := &service.APIKey{Group: groupA, Groups: []*service.Group{groupA, groupB}, GroupIDs: []int64{1, 2}}

	router, calls := newUnionGateTestRouter(t, apiKey)
	w := doJSON(t, router, http.MethodPost, "/v1/chat/completions", `{"model":"hy4-preview","messages":[]}`)

	if w.Code != http.StatusNotFound {
		t.Fatalf("候选全无定价必须 404，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("被拒请求不得进入 handler，实际 %v", *calls)
	}
}

// 单分组 Key 行为零变化（向后兼容护栏）：无候选集时按主分组判定。
func TestModelPricingAdmission_SingleGroupUnchanged(t *testing.T) {
	input := 1e-6
	output := 2e-6
	priced := &service.Group{
		ID: 1, Name: "p", Platform: service.PlatformWorkbuddy,
		ModelPricing: []service.ChannelModelPricing{
			{Models: []string{"hy4-preview"}, InputPrice: &input, OutputPrice: &output},
		},
	}
	unpriced := &service.Group{ID: 2, Name: "u", Platform: service.PlatformWorkbuddy}

	router, calls := newUnionGateTestRouter(t, &service.APIKey{Group: priced})
	if w := doJSON(t, router, http.MethodPost, "/v1/chat/completions", `{"model":"hy4-preview","messages":[]}`); w.Code != http.StatusOK {
		t.Fatalf("单分组已定价必须放行，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("必须进入 handler，实际 %v", *calls)
	}

	router2, calls2 := newUnionGateTestRouter(t, &service.APIKey{Group: unpriced})
	if w := doJSON(t, router2, http.MethodPost, "/v1/chat/completions", `{"model":"hy4-preview","messages":[]}`); w.Code != http.StatusNotFound {
		t.Fatalf("单分组未定价必须 404，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(*calls2) != 0 {
		t.Fatalf("被拒请求不得进入 handler，实际 %v", *calls2)
	}
}
