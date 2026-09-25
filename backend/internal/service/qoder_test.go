//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// qoder_test.go 平台契约单测：NormalizeQoderCredentials 各形态（dt-/drt-/pt-/
// JSON 粘贴/驼峰键/缺 token 报错/realm 校验）与端点 region 解析。

func TestNormalizeQoderCredentialsDeviceToken(t *testing.T) {
	t.Parallel()
	creds := map[string]any{"access_token": "dt-abc", "uid": "u-1"}
	require.NoError(t, NormalizeQoderCredentials(creds))
	require.Equal(t, "dt-abc", creds["access_token"])
}

func TestNormalizeQoderCredentialsRefreshAndPersonalTokens(t *testing.T) {
	t.Parallel()
	// drt- 刷新令牌 / pt- PAT 单独存在即合法（发送时按需交换）。
	require.NoError(t, NormalizeQoderCredentials(map[string]any{"refresh_token": "drt-x"}))
	require.NoError(t, NormalizeQoderCredentials(map[string]any{"personal_token": "pt-y"}))
	// 驼峰键归一。
	creds := map[string]any{"accessToken": "dt-camel", "expiresAt": "123"}
	require.NoError(t, NormalizeQoderCredentials(creds))
	require.Equal(t, "dt-camel", creds["access_token"])
	require.Equal(t, "123", creds["expires_at"])
	require.NotContains(t, creds, "accessToken")
}

func TestNormalizeQoderCredentialsJSONPaste(t *testing.T) {
	t.Parallel()
	// JSON 整串粘贴进 access_token：内部键展开。
	creds := map[string]any{
		"access_token": `{"device_token":"dt-inner","uid":"u-inner","realm":"cn"}`,
	}
	require.NoError(t, NormalizeQoderCredentials(creds))
	require.Equal(t, "dt-inner", creds["access_token"], "嵌套 access_token 缺失时回落 device_token")
	require.Equal(t, "u-inner", creds["uid"])
	require.Equal(t, "cn", creds["realm"])
}

func TestNormalizeQoderCredentialsMissingToken(t *testing.T) {
	t.Parallel()
	err := NormalizeQoderCredentials(map[string]any{"uid": "u-1", "realm": "cn"})
	require.Error(t, err, "只有非 token 键必须报错")
	// 完全无关的凭据（其他平台）为 no-op。
	require.NoError(t, NormalizeQoderCredentials(map[string]any{"api_key": "sk-x"}))
	require.NoError(t, NormalizeQoderCredentials(nil))
}

func TestNormalizeQoderCredentialsInvalidRealm(t *testing.T) {
	t.Parallel()
	err := NormalizeQoderCredentials(map[string]any{"access_token": "dt-x", "realm": "mars"})
	require.Error(t, err)
	// 合法 realm 保留。
	creds := map[string]any{"access_token": "dt-x", "realm": "CN"}
	require.NoError(t, NormalizeQoderCredentials(creds))
	require.Equal(t, "cn", creds["realm"])
}

func TestResolveQoderEndpointsGlobal(t *testing.T) {
	t.Parallel()
	eps := resolveQoderEndpoints("global", "")
	require.Equal(t, "https://api1.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1", eps.ChatURL)
	require.Equal(t, "https://api2.qoder.sh/algo/api/v2/model/list?Encode=1", eps.ModelListURL)
	require.Equal(t, "https://center.qoder.sh/algo/api/v3/user/jobToken?Encode=1", eps.JobTokenURL)
	require.Equal(t, "https://openapi.qoder.sh/api/v1/userinfo", eps.UserinfoURL)
	require.Equal(t, "https://openapi.qoder.sh/api/v2/user/plan", eps.PlanURL)
	require.Equal(t, "https://openapi.qoder.sh/api/v2/quota/usage", eps.QuotaURL)
	require.Equal(t, "https://qoder.com/device/selectAccounts", eps.DeviceLoginBase)
	require.Equal(t, "https://openapi.qoder.sh/api/v1/deviceToken/poll", eps.PollEndpoint)
}

func TestResolveQoderEndpointsCN(t *testing.T) {
	t.Parallel()
	eps := resolveQoderEndpoints("cn", "")
	require.Equal(t, "https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1", eps.ChatURL)
	require.Equal(t, "https://gateway.qoder.com.cn/algo/api/v3/user/jobToken?Encode=1", eps.JobTokenURL)
	require.Equal(t, "https://openapi.qoder.com.cn/api/v1/userinfo", eps.UserinfoURL)
	require.Equal(t, "https://qoder.com.cn/device/selectAccounts", eps.DeviceLoginBase)
	require.Equal(t, "https://openapi.qoder.com.cn/api/v1/deviceToken/poll", eps.PollEndpoint)
}

func TestResolveQoderEndpointsBaseURLOverride(t *testing.T) {
	t.Parallel()
	eps := resolveQoderEndpoints("global", "https://relay.example.com/")
	require.Equal(t, "https://relay.example.com/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1", eps.ChatURL)
	require.Equal(t, "https://relay.example.com/algo/api/v3/user/jobToken?Encode=1", eps.JobTokenURL)
	// openapi 域不受中转覆盖。
	require.Equal(t, "https://openapi.qoder.sh/api/v1/userinfo", eps.UserinfoURL)
	// 无 scheme 自动补 https。
	eps2 := resolveQoderEndpoints("global", "relay.example.com")
	require.True(t, len(eps2.AlgoBase) > 0 && eps2.AlgoBase[:8] == "https://", "缺 scheme 自动补 https")
}

func TestDefaultQoderModelIDs(t *testing.T) {
	t.Parallel()
	ids := DefaultQoderModelIDs()
	require.NotEmpty(t, ids)
	require.Contains(t, ids, "auto")
	require.Contains(t, ids, "qmodel_38max")
	// 上游真值目录共 14 项（Qoder CN 客户端 model classes；2026-09-22 实测）。
	require.Len(t, ids, 14)
	// 新增项与旧错配 key 的修正断言（防回归）。
	require.Contains(t, ids, "q37fmodel", "Qwen3.7-Flash 存在于上游目录")
	require.Contains(t, ids, "gfmodel", "GLM-5.3-Flash 存在于上游目录")
	require.Contains(t, ids, "kmodel_latest", "Kimi-K3 存在于上游目录")
	require.NotContains(t, ids, "q36fmodel", "q36fmodel 非上游 key")
	// 目录展示名映射（上游真值）。
	require.Equal(t, "Auto", qoderModelDisplayName("auto"))
	require.Equal(t, "Qwen3.7-Flash", qoderModelDisplayName("q37fmodel"))
	require.Equal(t, "GLM-5.3-Flash", qoderModelDisplayName("gfmodel"))
	require.Equal(t, "Kimi-K3", qoderModelDisplayName("kmodel_latest"))
	require.Equal(t, "Kimi-K2.8-Preview", qoderModelDisplayName("kmodel"))
	require.Equal(t, "Qwen3.8-Flash", qoderModelDisplayName("qfmodel"))
	require.Equal(t, "DeepSeek-Flash", qoderModelDisplayName("dfmodel"))
	// 目录每个 ID 都有展示名（无遗漏）。
	for _, id := range ids {
		require.Equal(t, qoderModelCatalog[id], qoderModelDisplayName(id), "id %s 必须有展示名", id)
	}
	require.Equal(t, "unknown-key", qoderModelDisplayName("unknown-key"))
}

func TestIsQoderPlatform(t *testing.T) {
	t.Parallel()
	require.True(t, IsQoder("qoder"))
	require.False(t, IsQoder("workbuddy"))
	acc := &Account{ID: 1, Platform: PlatformQoder}
	require.True(t, acc.IsQoderPlatform())
	require.False(t, (&Account{ID: 2, Platform: PlatformWorkbuddy}).IsQoderPlatform())
	var nilAcc *Account
	require.False(t, nilAcc.IsQoderPlatform())
}

func TestGetQoderRealmAndBaseURL(t *testing.T) {
	t.Parallel()
	// 缺省 global（与 workbuddy 缺省 cn 相反）。
	acc := &Account{ID: 10, Platform: PlatformQoder, Credentials: map[string]any{}}
	require.Equal(t, "global", acc.GetQoderRealm())
	require.Equal(t, DefaultQoderGlobalBaseURL, acc.GetQoderBaseURL())
	// cn 显式声明。
	accCN := &Account{ID: 11, Platform: PlatformQoder, Credentials: map[string]any{"realm": "cn"}}
	require.Equal(t, "cn", accCN.GetQoderRealm())
	require.Equal(t, DefaultQoderCNBaseURL, accCN.GetQoderBaseURL())
	// base_url 覆盖。
	accOverride := &Account{ID: 12, Platform: PlatformQoder, Credentials: map[string]any{"base_url": "https://relay.example.com"}}
	require.Equal(t, "https://relay.example.com", accOverride.GetQoderBaseURL())
	// domain 含 qoder.com.cn 判 cn。
	accDomain := &Account{ID: 13, Platform: PlatformQoder, Credentials: map[string]any{"domain": "https://qoder.com.cn"}}
	require.Equal(t, "cn", accDomain.GetQoderRealm())
}

func TestGetQoderCredentials(t *testing.T) {
	t.Parallel()
	acc := &Account{ID: 20, Platform: PlatformQoder, Credentials: map[string]any{
		"access_token": "dt-a",
		"expires_at":   "1700000000",
		"uid":          "u-20",
		"nickname":     "nick",
	}}
	creds := acc.GetQoderCredentials()
	require.Equal(t, "dt-a", creds.AccessToken)
	require.Equal(t, int64(1700000000), creds.ExpiresAt)
	require.Equal(t, "u-20", creds.UID)
	require.Equal(t, "nick", creds.Nickname)
	// 非 qoder 平台返回零值。
	other := &Account{ID: 21, Platform: PlatformOpenAI}
	require.Empty(t, other.GetQoderCredentials().AccessToken)
}

// TestQoderFallbackPricingCoversCatalog 钉死 Qoder 14 个模型代号全部有
// fallback 价卡可循（2026-09-22 计费缺口修复）：缺任何一个，对应模型都会
// 零成本落账（pricing_missing_record_zero_cost），restrict_models 渠道下
// 更会被 pricing restriction 拒调。
func TestQoderFallbackPricingCoversCatalog(t *testing.T) {
	t.Parallel()
	svc := newTestBillingService()

	for _, id := range DefaultQoderModelIDs() {
		pricing := svc.getFallbackPricing(id)
		require.NotNilf(t, pricing, "Qoder 模型代号 %s 必须有 fallback 价卡", id)
		require.Greaterf(t, pricing.InputPricePerToken, 0.0, "%s input 价必须 > 0", id)
		require.Greaterf(t, pricing.OutputPricePerToken, 0.0, "%s output 价必须 > 0", id)
	}

	// 同款模型对齐口径（防价卡被改错档）：
	// dmodel/dfmodel 与 deepseek-* 同卡。
	pro := svc.getFallbackPricing("dmodel")
	require.InDelta(t, deepseekProOffPeakInputPrice, pro.InputPricePerToken, 1e-12)
	require.InDelta(t, deepseekProOffPeakOutputPrice, pro.OutputPricePerToken, 1e-12)
	flash := svc.getFallbackPricing("dfmodel")
	require.InDelta(t, deepseekFlashOffPeakInputPrice, flash.InputPricePerToken, 1e-12)
	require.InDelta(t, deepseekFlashOffPeakOutputPrice, flash.OutputPricePerToken, 1e-12)
	// gmodel/gfmodel/gm51model 与 glm-5.3 / glm-5.3-flash / glm-5.2 同卡。
	for code, ref := range map[string]string{
		"gmodel":    "glm-5.3",
		"gfmodel":   "glm-5.3-flash",
		"gm51model": "glm-5.2",
	} {
		got := svc.getFallbackPricing(code)
		want := svc.getFallbackPricing(ref)
		require.InDeltaf(t, want.InputPricePerToken, got.InputPricePerToken, 1e-12, "%s 应同 %s", code, ref)
		require.InDeltaf(t, want.OutputPricePerToken, got.OutputPricePerToken, 1e-12, "%s 应同 %s", code, ref)
	}
	// kmodel_latest 同 Kimi-K3，mmodel 同 MiniMax-M2.7。
	k3 := svc.getFallbackPricing("kmodel_latest")
	require.InDelta(t, 3e-6, k3.InputPricePerToken, 1e-12)
	require.InDelta(t, 15e-6, k3.OutputPricePerToken, 1e-12)
	mm := svc.getFallbackPricing("mmodel")
	require.InDelta(t, 0.30e-6, mm.InputPricePerToken, 1e-12)
}

// TestQoderFallbackPricingDoesNotLeakToOtherFamilies 反向保护：Qoder 代号
// 精确匹配不得外溢——相似但不在目录内的名字（含前缀/后缀变体）不得命中。
func TestQoderFallbackPricingDoesNotLeakToOtherFamilies(t *testing.T) {
	t.Parallel()
	svc := newTestBillingService()

	for _, model := range []string{
		"dmodel-x", "x-dmodel", "dmodel2",
		"qmodel-extra", "qmodel-38max", "qmodel38max",
		"kmodel-v2", "mmodel-pro", "gfmodel2", "auto-route",
		"gmodel-5.3", "dfmodel-flash",
	} {
		require.Nilf(t, svc.getFallbackPricing(model), "非目录代号 %q 不得命中 Qoder 价卡", model)
	}
}

// TestQoderFallbackPricingAutoUsesTopTier 钉死 auto（智能路由）保守计价口径：
// 按平台最高档（Kimi-K3 卡）计费，不得是 $0。
func TestQoderFallbackPricingAutoUsesTopTier(t *testing.T) {
	t.Parallel()
	svc := newTestBillingService()

	auto := svc.getFallbackPricing("auto")
	require.NotNil(t, auto)
	require.InDelta(t, 3e-6, auto.InputPricePerToken, 1e-12)
	require.InDelta(t, 15e-6, auto.OutputPricePerToken, 1e-12)
}

