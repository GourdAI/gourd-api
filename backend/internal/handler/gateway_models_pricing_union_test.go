package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newPricingUnionGatewayServiceForTest 构造一个「定价闸门开启」的 GatewayService，
// 用于覆盖 /v1/models 的多分组 union 分支。
//
// 位置参数（NewGatewayService 共 28 个）：1=accountRepo、9=cfg、12=billingService、
// 25=resolver。其余全部传 nil：union 分支只读账号 model_mapping 与定价闸门。
func newPricingUnionGatewayServiceForTest(repo service.AccountRepository) *service.GatewayService {
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = true
	billing := service.NewBillingService(cfg, nil)
	resolver := service.NewModelPricingResolver(nil, billing)
	return service.NewGatewayService(
		repo, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, billing, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, resolver, nil, nil, nil,
	)
}

// unionAccountMapping 构造一个只带 model_mapping 的账号。
func unionAccountMapping(id int64, platform string, mapping map[string]string) service.Account {
	cred := make(map[string]any, len(mapping))
	for k, v := range mapping {
		cred[k] = v
	}
	return service.Account{
		ID:       id,
		Platform: platform,
		Status:   service.StatusActive,
		Credentials: map[string]any{
			"model_mapping": cred,
		},
	}
}

// TestGatewayModels_MultiGroupUnionFiltersUnpricedPerGroup 回归：P1② union 分支漏过滤。
//
// 多分组 Key（A + B，平台一致）走 union 合并分支。要点有二：
//  1. 未定价模型（hy4-preview）在两个分组都无价，必须从列表剔除——此前 union
//     分支在白名单未开启时完全不过滤，直接把未定价模型推给客户端。
//  2. 「只在 B 组配了价」的模型（b-only-model）必须保留。定价过滤必须按各分组
//     自己的上下文做；若在外层用 unionGroup（=eligible[0]，即 A）统一过滤，
//     该模型会被误删。
func TestGatewayModels_MultiGroupUnionFiltersUnpricedPerGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		groupAID = int64(61)
		groupBID = int64(62)
	)

	repo := &gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupAID: {unionAccountMapping(1, service.PlatformOpenAI, map[string]string{
				"gpt-5.4":      "gpt-5.4",
				"hy4-preview":  "hy4-preview",
				"a-only-model": "a-only-model",
			})},
			groupBID: {unionAccountMapping(2, service.PlatformOpenAI, map[string]string{
				"gpt-5.4":      "gpt-5.4",
				"hy4-preview":  "hy4-preview",
				"b-only-model": "b-only-model",
			})},
		},
	}
	h := &GatewayHandler{gatewayService: newPricingUnionGatewayServiceForTest(repo)}

	input := 1e-6
	output := 2e-6
	// A 组给 a-only-model 配价；B 组给 b-only-model 配价。两者都未给 hy4-preview 配价。
	groupA := &service.Group{
		ID: groupAID, Name: "a", Platform: service.PlatformOpenAI, Status: service.StatusActive,
		ModelPricing: []service.ChannelModelPricing{
			{Models: []string{"a-only-model"}, InputPrice: &input, OutputPrice: &output},
		},
	}
	groupB := &service.Group{
		ID: groupBID, Name: "b", Platform: service.PlatformOpenAI, Status: service.StatusActive,
		ModelPricing: []service.ChannelModelPricing{
			{Models: []string{"b-only-model"}, InputPrice: &input, OutputPrice: &output},
		},
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group:    groupA,
		Groups:   []*service.Group{groupA, groupB},
		GroupIDs: []int64{groupAID, groupBID},
	})

	h.Models(c)
	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	models := modelIDsForTest(got.Data)

	require.NotContains(t, models, "hy4-preview",
		"两个分组都未配价的模型不得出现在 /v1/models（P1② union 分支漏过滤）")
	require.Contains(t, models, "a-only-model",
		"A 组已配价的模型必须保留")
	require.Contains(t, models, "b-only-model",
		"B 组已配价的模型必须保留：定价过滤须按各分组上下文做，"+
			"若用 unionGroup(eligible[0]=A) 统一过滤会把它误删")
	require.Contains(t, models, "gpt-5.4",
		"内置兜底价卡覆盖的模型必须保留")
}

// TestGatewayModels_MultiGroupUnionFilterDisabledKeepsAll 反向保护：开关关闭时
// union 分支必须原样返回（可回滚性），否则灰度回退会静默改变客户端可见模型集。
func TestGatewayModels_MultiGroupUnionFilterDisabledKeepsAll(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		groupAID = int64(63)
		groupBID = int64(64)
	)

	repo := &gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupAID: {unionAccountMapping(1, service.PlatformOpenAI, map[string]string{"hy4-preview": "hy4-preview"})},
			groupBID: {unionAccountMapping(2, service.PlatformOpenAI, map[string]string{"hy5-preview": "hy5-preview"})},
		},
	}

	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = false // 关闭
	billing := service.NewBillingService(cfg, nil)
	resolver := service.NewModelPricingResolver(nil, billing)
	h := &GatewayHandler{
		gatewayService: service.NewGatewayService(
			repo, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, billing, nil, nil,
			nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, resolver, nil, nil, nil,
		),
	}

	groupA := &service.Group{ID: groupAID, Name: "a", Platform: service.PlatformOpenAI, Status: service.StatusActive}
	groupB := &service.Group{ID: groupBID, Name: "b", Platform: service.PlatformOpenAI, Status: service.StatusActive}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group:    groupA,
		Groups:   []*service.Group{groupA, groupB},
		GroupIDs: []int64{groupAID, groupBID},
	})

	h.Models(c)
	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	models := modelIDsForTest(got.Data)

	require.Contains(t, models, "hy4-preview", "开关关闭时未定价模型必须原样保留")
	require.Contains(t, models, "hy5-preview", "开关关闭时未定价模型必须原样保留")
}
