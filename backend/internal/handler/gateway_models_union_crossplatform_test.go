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

// gateway_models_union_crossplatform_test.go 锁定「/v1/models 并集不按平台裁剪」口径。
//
// 回归背景：multiGroupModelUnion 此前只合并「与主分组 platform 相同」的分组，理由是
// 「避免客户端拿到无法用当前协议调用的模型」。该理由在本仓不成立——网关对各上游统一
// 做协议转换，绑定分组能服务的模型都能用同一把 key 调用成功（2026-09-24 端到端实测：
// 主分组 qoder + 候选 workbuddy 的 key 请求 deepseek-v4.1-flash → 200，usage_logs.group_id
// 正确落到 workbuddy 分组），于是形成「能调但不列」，靠 /v1/models 自动发现模型的客户端
// （Cursor / Cline 等）看不见实际可用的跨平台模型。

// newUnionLoosePricingGatewayService 构造「定价闸门关闭」的 GatewayService，
// 使这些用例只考察并集的分组覆盖范围，不被定价过滤干扰（定价并集语义另有
// TestGatewayModels_MultiGroupUnionFiltersUnpricedPerGroup 覆盖）。
func newUnionLoosePricingGatewayService(repo service.AccountRepository) *service.GatewayService {
	cfg := &config.Config{}
	cfg.Pricing.RequirePricedModels = false
	billing := service.NewBillingService(cfg, nil)
	resolver := service.NewModelPricingResolver(nil, billing)
	return service.NewGatewayService(
		repo, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, billing, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, resolver, nil, nil, nil,
	)
}

func callModelsHandler(t *testing.T, h *GatewayHandler, apiKey *service.APIKey) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), apiKey)

	h.Models(c)
	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	return modelIDsForTest(got.Data)
}

// 跨平台绑定必须进并集：主分组 openai、候选 anthropic 时，两个平台的模型都要列出。
//
// 这一用例是「决定性对照」——旧实现（按 platform 裁剪）只会返回 gpt-5.4，
// claude-sonnet-4-5 会被整体丢掉，因此若有人把平台过滤加回来，本测试必挂。
func TestGatewayModels_MultiGroupUnionAcrossPlatforms(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		openaiGroupID    = int64(71)
		anthropicGroupID = int64(72)
	)

	repo := &gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			openaiGroupID: {unionAccountMapping(1, service.PlatformOpenAI, map[string]string{
				"gpt-5.4": "gpt-5.4",
			})},
			anthropicGroupID: {unionAccountMapping(2, service.PlatformAnthropic, map[string]string{
				"claude-sonnet-4-5": "claude-sonnet-4-5",
			})},
		},
	}
	h := &GatewayHandler{gatewayService: newUnionLoosePricingGatewayService(repo)}

	groupOpenAI := &service.Group{ID: openaiGroupID, Name: "oai", Platform: service.PlatformOpenAI, Status: service.StatusActive}
	groupAnthropic := &service.Group{ID: anthropicGroupID, Name: "ant", Platform: service.PlatformAnthropic, Status: service.StatusActive}

	models := callModelsHandler(t, h, &service.APIKey{
		Group:    groupOpenAI, // 生效分组 = 主分组 openai（旧实现在此把 anthropic 候选裁掉）
		Groups:   []*service.Group{groupOpenAI, groupAnthropic},
		GroupIDs: []int64{openaiGroupID, anthropicGroupID},
	})

	require.Contains(t, models, "gpt-5.4", "主分组自己的模型必须在列")
	require.Contains(t, models, "claude-sonnet-4-5",
		"跨平台候选分组的模型必须进并集：网关统一协议入口下它确实可调用，"+
			"按主分组 platform 裁剪会重新制造「能调但不列」")
}

// 反向保护：未启用（inactive）分组不能把模型混进并集——它无法服务请求，
// 列出来等于给出必然失败的模型名。这条同时证明「去掉平台过滤」并不等于
// 「去掉所有过滤」，只去掉了 platform 那一刀。
func TestGatewayModels_MultiGroupUnionSkipsInactiveGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		liveGroupID = int64(73)
		deadGroupID = int64(74)
		soloGroupID = int64(75)
	)

	repo := &gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			liveGroupID: {unionAccountMapping(1, service.PlatformOpenAI, map[string]string{"gpt-5.4": "gpt-5.4"})},
			// 死分组提供另一个平台的模型：若 status 判据失守，它会出现在并集里。
			deadGroupID: {unionAccountMapping(2, service.PlatformAnthropic, map[string]string{"claude-opus-4-1": "claude-opus-4-1"})},
		},
	}
	h := &GatewayHandler{gatewayService: newUnionLoosePricingGatewayService(repo)}

	groupLive := &service.Group{ID: liveGroupID, Name: "live", Platform: service.PlatformOpenAI, Status: service.StatusActive}
	groupDead := &service.Group{ID: deadGroupID, Name: "dead", Platform: service.PlatformAnthropic, Status: service.StatusDisabled}

	models := callModelsHandler(t, h, &service.APIKey{
		Group:    groupLive,
		Groups:   []*service.Group{groupLive, groupDead},
		GroupIDs: []int64{liveGroupID, deadGroupID},
	})
	require.NotContains(t, models, "claude-opus-4-1", "未启用分组的模型不得进入并集")

	// 单分组 key 不得因本次改动进入 union 路径（行为与历史逐字一致）。
	repo2 := &gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			soloGroupID: {unionAccountMapping(3, service.PlatformOpenAI, map[string]string{"gpt-5.4": "gpt-5.4"})},
		},
	}
	h2 := &GatewayHandler{gatewayService: newUnionLoosePricingGatewayService(repo2)}
	solo := &service.Group{ID: soloGroupID, Name: "solo", Platform: service.PlatformOpenAI, Status: service.StatusActive}
	soloModels := callModelsHandler(t, h2, &service.APIKey{
		Group:    solo,
		Groups:   []*service.Group{solo},
		GroupIDs: []int64{soloGroupID},
	})
	require.Equal(t, []string{"gpt-5.4"}, soloModels, "单分组 key 的列表必须与并集逻辑完全无关")
}

// composite 分组（一个分组挂多个上游平台）参与并集时不得「隐形」。
//
// composite 的账号平台是若干真实平台，而非字面量 "composite"；若并集循环一律按
// group.Platform 去过滤账号，composite 分组会取到空集——反而比它单独绑定时更糟
// （单独走 PlatformComposite 专用分支能列出全平台模型）。
func TestGatewayModels_MultiGroupUnionIncludesCompositeGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		openaiGroupID    = int64(77)
		compositeGroupID = int64(78)
	)

	repo := &gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			openaiGroupID: {unionAccountMapping(1, service.PlatformOpenAI, map[string]string{
				"gpt-5.4": "gpt-5.4",
			})},
			// composite 分组下的真实平台账号：这里放一个 workbuddy 账号。
			compositeGroupID: {unionAccountMapping(2, service.PlatformWorkbuddy, map[string]string{
				"deepseek-v4.1-flash": "deepseek-v4.1-flash",
			})},
		},
	}
	h := &GatewayHandler{gatewayService: newUnionLoosePricingGatewayService(repo)}

	groupOpenAI := &service.Group{ID: openaiGroupID, Name: "oai", Platform: service.PlatformOpenAI, Status: service.StatusActive}
	groupComposite := &service.Group{ID: compositeGroupID, Name: "cmp", Platform: service.PlatformComposite, Status: service.StatusActive}

	models := callModelsHandler(t, h, &service.APIKey{
		Group:    groupOpenAI,
		Groups:   []*service.Group{groupOpenAI, groupComposite},
		GroupIDs: []int64{openaiGroupID, compositeGroupID},
	})

	require.Contains(t, models, "gpt-5.4", "主分组模型必须在列")
	require.Contains(t, models, "deepseek-v4.1-flash",
		"composite 分组内 workbuddy 账号的模型必须进并集：按 group.Platform 过滤账号会让 composite 分组隐形")
}

// 白名单口径：并集里每个分组只受**自己**的白名单约束，主分组白名单不得二次裁剪
// 其他分组的模型（旧实现在外层用 unionGroup.ModelAllowlist 又过滤了一遍）。
func TestGatewayModels_MultiGroupUnionNotTrimmedByPrimaryAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		groupAID = int64(79)
		groupBID = int64(80)
	)

	repo := &gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupAID: {unionAccountMapping(1, service.PlatformOpenAI, map[string]string{
				"gpt-5.4":      "gpt-5.4",
				"gpt-5.4-mini": "gpt-5.4-mini",
			})},
			groupBID: {unionAccountMapping(2, service.PlatformAnthropic, map[string]string{
				"claude-sonnet-4-5": "claude-sonnet-4-5",
			})},
		},
	}
	h := &GatewayHandler{gatewayService: newUnionLoosePricingGatewayService(repo)}

	// A（主分组）开白名单只放行 gpt-5.4；B 不开白名单。
	groupA := &service.Group{
		ID: groupAID, Name: "a", Platform: service.PlatformOpenAI, Status: service.StatusActive,
		ModelAllowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"gpt-5.4"}},
	}
	groupB := &service.Group{ID: groupBID, Name: "b", Platform: service.PlatformAnthropic, Status: service.StatusActive}

	models := callModelsHandler(t, h, &service.APIKey{
		Group:    groupA,
		Groups:   []*service.Group{groupA, groupB},
		GroupIDs: []int64{groupAID, groupBID},
	})

	require.Contains(t, models, "gpt-5.4", "A 组白名单内的模型保留")
	require.NotContains(t, models, "gpt-5.4-mini", "A 组白名单外的 A 组模型仍应被 A 自己的白名单剔除")
	require.Contains(t, models, "claude-sonnet-4-5",
		"B 组（未开白名单）的模型不得被 A 的白名单二次裁剪：多分组语义是各分组口径取并，"+
			"用主分组白名单统裁会重新制造「能调但不列」")
}
