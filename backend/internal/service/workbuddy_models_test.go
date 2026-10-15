//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// workbuddy_models_test.go 锁住 WorkBuddy /v3/config 目录拉取的三层契约：
//
//  1. parseWorkbuddyModelCatalog —— 信封容错（顶层裸对象 / data 包裹 / agent 多一层
//     / agents 为数组或映射）与「技术性不可调」过滤；
//  2. workbuddyEntryMetadata —— 上游直出字段到对外元数据的映射，尤其是
//     「reasoning=true 必须有 levels」这条完整性判据的两个分支；
//  3. fetchWorkbuddyUpstreamModels —— 端点形态（GET /v3/config）、鉴权头族、
//     双 UA 回落、以及「过滤后为空」必须报错而非静默成功。
//
// 夹具形态取自本机 WorkBuddy 桌面端真实缓存 ~/.workbuddy/cache/acc-product-config-v3.json
// 与 2026-10-13 三域匿名实测响应（{"code":0,"msg":"OK","data":{"agent":{"agents":...},
// "models":...}}），不是凭空构造。

// workbuddyCatalogCnShape 是桌面端缓存的落盘形态：顶层裸对象 + agents 数组，
// cli 条目挂选择器实际展示的 21 项清单，其余 agent 挂内部辅助模型。
const workbuddyCatalogCnShape = `{
  "enterpriseId": "fyl2hqjv2nsw",
  "agents": [
    {"name": "cli", "models": ["auto","hy3","hy3-x","glm-5.3","glm-5.3-flash","glm-5.3-flashx","kimi-k3-2","minimax-m3-pay","deepseek-v4-pro","custom:luxen|luxen-1.0","lite"]},
    {"name": "summaryGenerator", "models": ["custom:luxen|luxen-1.0"]}
  ],
  "models": [
    {"id":"glm-5.3-flashx","name":"GLM-5.3-FlashX","descriptionEn":"Native multimodal model.",
     "contextWindow":{"defaultLength":300000,"supportedLengths":[300000,600000,1000000]},
     "maxAllowedSize":1000000,"maxInputTokens":1000000,"maxOutputTokens":131072,
     "reasoning":{"canDisableThinking":false,"defaultEffort":"high","supportedEfforts":["low","high","max"]},
     "supportsImages":true,"supportsReasoning":true,"supportsToolCall":true,"credits":"x0.14","vendor":"f"},
    {"id":"auto","name":"Auto","isDefault":true,"maxInputTokens":256000,"maxOutputTokens":32000,
     "reasoning":{"effort":"high","summary":"auto"},"supportsImages":true,"supportsReasoning":true},
    {"id":"kimi-k3-2","name":"Kimi-K3","credits":"x1.62","supportsReasoning":false},
    {"id":"hy3","name":"Hy3","contextWindow":{"defaultLength":200000},"supportsImages":false,"supportsReasoning":true,
     "reasoning":{"defaultEffort":"high","supportedEfforts":["low","high"]}},
    {"id":"glm-5.3","name":"GLM-5.3","supportsReasoning":true,
     "reasoning":{"supportedEfforts":["low","extra-high"]}},
    {"id":"lite","name":"Lite"},
    {"id":"custom:luxen|luxen-1.0","name":"luxen-1.0","modelType":"enterprise"}
  ]
}`

// workbuddyCatalogEnvelopeShape 是 2026-10-13 实测的带票信封形态（data.agent.agents
// 比缓存多一层 agent 包装）——正是这条差异逼出「递归找 agents」而非写死层级。
const workbuddyCatalogEnvelopeShape = `{
  "code": 0, "msg": "OK", "requestId": "571e8ac7-ea53-4464-a32d-10af64c33c85",
  "data": {
    "agent": {"agents": [{"name":"cli","models":["glm-5.2","deepseek-v4.1-flash"]}]},
    "models": [{"id":"glm-5.2","name":"GLM-5.2","contextWindow":{"defaultLength":200000},
                "supportsReasoning":true,"reasoning":{"supportedEfforts":["low","high"],"defaultEffort":"high"}}]
  }
}`

// ---------------------------------------------------------------------------
// A. 解析：信封容错
// ---------------------------------------------------------------------------

func TestParseWorkbuddyModelCatalogReadsCLIAgentList(t *testing.T) {
	ids, metadata, err := parseWorkbuddyModelCatalog([]byte(workbuddyCatalogCnShape))
	require.NoError(t, err)

	// auto 保留（上游选择器展示的路由入口，且静态表早已对外宣告）；
	// custom:* 与 lite 被技术性过滤掉。
	require.Equal(t, []string{"auto", "hy3", "hy3-x", "glm-5.3", "glm-5.3-flash", "glm-5.3-flashx", "kimi-k3-2", "minimax-m3-pay", "deepseek-v4-pro"}, ids)
	require.NotContains(t, ids, "lite")
	require.NotContains(t, ids, "custom:luxen|luxen-1.0")

	// 有元数据表的条目直出上游能力；没有的条目仍要有占位元数据（否则前端展示不出名字）。
	require.Equal(t, "GLM-5.3-FlashX", metadata["glm-5.3-flashx"].DisplayName)
	require.Equal(t, "minimax-m3-pay", metadata["minimax-m3-pay"].DisplayName, "上游 models 表缺该条目时回落到 ID 本身")
}

func TestParseWorkbuddyModelCatalogReadsDataEnvelopeShape(t *testing.T) {
	ids, metadata, err := parseWorkbuddyModelCatalog([]byte(workbuddyCatalogEnvelopeShape))
	require.NoError(t, err)
	require.Equal(t, []string{"glm-5.2", "deepseek-v4.1-flash"}, ids, "实测带票信封的 agents 在 data.agent 里，写死层级会漏")
	require.Equal(t, "GLM-5.2", metadata["glm-5.2"].DisplayName)
}

func TestParseWorkbuddyModelCatalogAcceptsAgentsAsMapping(t *testing.T) {
	body := `{"agents":{"cli":{"models":["glm-5.3","lite"]},"experts":{"models":["kimi-k3-2"]}}}`
	ids, _, err := parseWorkbuddyModelCatalog([]byte(body))
	require.NoError(t, err)
	require.Equal(t, []string{"glm-5.3"}, ids, "映射形态同样可读，且只认 cli 那份")
}

func TestParseWorkbuddyModelCatalogFallsBackToLongestNonCLIAgent(t *testing.T) {
	body := `{"agents":[{"name":"summaryGenerator","models":["glm-5.2"]},{"name":"Explore","models":["kimi-k3-2","hy3"]}]}`
	ids, _, err := parseWorkbuddyModelCatalog([]byte(body))
	require.NoError(t, err)
	require.Equal(t, []string{"kimi-k3-2", "hy3"}, ids, "上游改名去掉 cli 条目时，取最长的一份而不是直接失败")
}

func TestParseWorkbuddyModelCatalogRejectsUnusablePayloads(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"not json", `not-json`, "parse workbuddy model catalog"},
		{"null", `null`, "response is null"},
		{"business error code", `{"code":12403,"msg":"invalid user agent"}`, "code=12403"},
		{"no catalog at all", `{"code":0,"msg":"OK","data":{"models":null,"agent":{"agents":null}}}`, "no agent model list found"},
		{"all filtered as non-chat", `{"agents":[{"name":"cli","models":["lite","custom:luxen|luxen-1.0","codewise-jump","hunyuan-image-alpha"]}]}`, "filtered as non-chat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ids, metadata, err := parseWorkbuddyModelCatalog([]byte(tc.body))
			require.Error(t, err)
			require.Empty(t, ids)
			require.Nil(t, metadata)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// 反向护栏：上游一旦把内部条目的名字换掉，不能因为「名字没见过」就误杀真模型。
func TestParseWorkbuddyModelCatalogKeepsUnknownRealModels(t *testing.T) {
	body := `{"agents":[{"name":"cli","models":["brand-new-model-2027","deepseek-v9"]}]}`
	ids, _, err := parseWorkbuddyModelCatalog([]byte(body))
	require.NoError(t, err)
	require.Equal(t, []string{"brand-new-model-2027", "deepseek-v9"}, ids, "新上线模型必须原样同步进来，这正是本次报障的诉求")
}

// ---------------------------------------------------------------------------
// B. 元数据映射
// ---------------------------------------------------------------------------

func TestWorkbuddyEntryMetadataMapsCapabilityFields(t *testing.T) {
	_, metadata, err := parseWorkbuddyModelCatalog([]byte(workbuddyCatalogCnShape))
	require.NoError(t, err)

	flashx := metadata["glm-5.3-flashx"]
	require.Equal(t, "Native multimodal model.", flashx.Description)
	require.Equal(t, int64(1000000), flashx.ContextWindow, "defaultLength 之外还要吃 maxInputTokens 里的更大容量")
	require.Equal(t, int64(1000000), flashx.MaxContextWindow, "supportedLengths / maxAllowedSize 取最大")
	require.Equal(t, int64(131072), flashx.MaxOutputTokens)
	require.Equal(t, []string{"text", "image"}, flashx.InputModalities)
	require.NotNil(t, flashx.Reasoning)
	require.True(t, *flashx.Reasoning)
	require.Equal(t, []string{"low", "high", "max"}, flashx.SupportedReasoningLevels)
	require.Equal(t, "high", flashx.DefaultReasoningLevel)
	// 这条是最关键的回归防线：reasoning=true 且 levels 齐备才判「能力完整」，
	// 否则同步每轮回 metadata_partial、快照永远写不进库。
	require.True(t, upstreamModelMetadataIsComplete(flashx))
}

func TestWorkbuddyEntryMetadataNormalizesReasoningLevels(t *testing.T) {
	_, metadata, err := parseWorkbuddyModelCatalog([]byte(workbuddyCatalogCnShape))
	require.NoError(t, err)

	// 上游下发 "extra-high"，对外必须是归一后的 "xhigh"。
	glm53 := metadata["glm-5.3"]
	require.Equal(t, []string{"low", "xhigh"}, glm53.SupportedReasoningLevels)
	require.Equal(t, "low", glm53.DefaultReasoningLevel, "defaultEffort 缺省时回落首个合法档位，而不是留空")

	// 只有单值 effort（auto 条目）也要产出一档，否则会被判不完整。
	auto := metadata["auto"]
	require.NotNil(t, auto.Reasoning)
	require.True(t, *auto.Reasoning)
	require.Equal(t, []string{"high"}, auto.SupportedReasoningLevels)
	require.True(t, upstreamModelMetadataIsComplete(auto), "auto 的 effort 单值形态必须也能落进能力快照")

	// supportsImages=false 是定论（不是「没下发」），必须压回纯文本模态。
	hy3 := metadata["hy3"]
	require.Equal(t, []string{"text"}, hy3.InputModalities)
	require.Equal(t, int64(200000), hy3.ContextWindow)
}

func TestWorkbuddyEntryMetadataHonoursExplicitNonReasoning(t *testing.T) {
	_, metadata, err := parseWorkbuddyModelCatalog([]byte(workbuddyCatalogCnShape))
	require.NoError(t, err)

	kimi := metadata["kimi-k3-2"]
	require.NotNil(t, kimi.Reasoning, "上游给了 supportsReasoning=false 的定论要写下去，不能留 nil")
	require.False(t, *kimi.Reasoning)
	require.Empty(t, kimi.SupportedReasoningLevels)
}

func TestWorkbuddyCatalogIsChatModelRules(t *testing.T) {
	blocked := []string{"", "  ", "lite", "default-model", "fast-model", "balanced-model", "primary-model",
		"deep-model", "codewise-completions", "completion-gf", "custom:luxen|luxen-1.0",
		"hunyuan-image-alpha", "hunyuan-image-alpha-edit", "deepseek-v3-0324-taco-completion"}
	for _, id := range blocked {
		require.False(t, workbuddyCatalogIsChatModel(id), "%q 必须被过滤", id)
	}
	// 付费变体不在过滤之列：我们是网关、定价自配，照抄参考实现的「只投免费」商务裁剪
	// 会把上游真实可调的模型藏起来（本次报障正是要看到全部新模型）。
	kept := []string{"auto", "hy3-x", "glm-5.3-flashx", "hy4-preview-x", "kimi-k3-2", "minimax-m3-pay", "space-bunny", "step-5-preview"}
	for _, id := range kept {
		require.True(t, workbuddyCatalogIsChatModel(id), "%q 必须保留", id)
	}
}

// ---------------------------------------------------------------------------
// C. 拉取链路（端点形态 / 鉴权头 / 双 UA 回落）
// ---------------------------------------------------------------------------

func workbuddyModelSyncAccount(baseURL string) *Account {
	return &Account{
		ID:       4455,
		Platform: PlatformWorkbuddy,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"access_token": "wb-access-token",
			"uid":          "u-42",
			"realm":        "cn",
			"base_url":     baseURL,
		},
	}
}

func TestFetchWorkbuddyUpstreamModelsRequestsProductConfig(t *testing.T) {
	server, capture, upstream := newModelCatalogTestServer(t, http.StatusOK, workbuddyCatalogCnShape)
	svc := &AccountTestService{httpUpstream: upstream, cfg: modelSyncInsecureTestConfig()}

	ids, _, err := svc.fetchWorkbuddyUpstreamModels(context.Background(), workbuddyModelSyncAccount(server.URL))
	require.NoError(t, err)
	require.Contains(t, ids, "glm-5.3-flashx")

	call := capture.last(t)
	require.Equal(t, http.MethodGet, call.method)
	require.Equal(t, workbuddyModelListPath, call.path)
	require.Empty(t, call.header.Get("X-Refresh-Token"), "安全红线：目录请求绝不下发 refresh token")
	require.Equal(t, "application/json, text/plain, */*", call.header.Get("Accept"))
	require.Equal(t, "Bearer wb-access-token", call.header.Get("Authorization"))
	require.Equal(t, "u-42", call.header.Get("X-User-Id"))
	require.Equal(t, "WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1", call.header.Get("User-Agent"),
		"UA 决定上游下发哪张目录表，必须是桌面端完整形态而不是 billing 单段")
}

func TestFetchWorkbuddyUpstreamModelsFallsBackToSecondUAWhenCatalogEmpty(t *testing.T) {
	// 第一次（桌面端 UA）回 200 但没有目录（匿名实测就是这个形态：code=0 且 models/agents 均 null），
	// 必须换 billing UA 再试，而不是当成同步成功返回空表。
	var mu sync.Mutex
	attempts := 0
	capture := &modelCatalogCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		current := attempts
		mu.Unlock()
		capture.mu.Lock()
		capture.requests = append(capture.requests, modelCatalogCall{method: r.Method, path: r.URL.Path, header: r.Header.Clone()})
		capture.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if current == 1 {
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"agent":{"agents":null},"models":null}}`)
			return
		}
		_, _ = io.WriteString(w, workbuddyCatalogEnvelopeShape)
	}))
	t.Cleanup(server.Close)

	svc := &AccountTestService{httpUpstream: &qoderFakeUpstream{client: server.Client()}, cfg: modelSyncInsecureTestConfig()}
	ids, _, err := svc.fetchWorkbuddyUpstreamModels(context.Background(), workbuddyModelSyncAccount(server.URL))
	require.NoError(t, err)
	require.Equal(t, []string{"glm-5.2", "deepseek-v4.1-flash"}, ids)
	require.Len(t, capture.all(), 2, "空目录不是成功，必须再试一次 UA")
	require.Equal(t, "WorkBuddy/5.5.4", capture.all()[1].header.Get("User-Agent"))
}

func TestFetchWorkbuddyUpstreamModelsSurfacesUnauthorized(t *testing.T) {
	server, capture, upstream := newModelCatalogTestServer(t, http.StatusUnauthorized, `{"code":401,"msg":"token expired"}`)
	svc := &AccountTestService{httpUpstream: upstream, cfg: modelSyncInsecureTestConfig()}

	// 账号没有 refresh_token：换票救不了，必须把 401 原样报给管理员（而不是谎报「没有模型」）。
	ids, _, err := svc.fetchWorkbuddyUpstreamModels(context.Background(), workbuddyModelSyncAccount(server.URL))
	require.Error(t, err)
	require.Empty(t, ids)
	syncErr := requireUpstreamModelSyncError(t, err)
	require.Equal(t, UpstreamModelSyncErrorUpstream, syncErr.Kind)
	require.Equal(t, http.StatusUnauthorized, syncErr.StatusCode)
	require.Len(t, capture.all(), 1, "无 refresh_token 时不该空转第二轮 UA")
}

func TestFetchWorkbuddyUpstreamModelsRequiresAccessToken(t *testing.T) {
	svc := &AccountTestService{httpUpstream: &qoderFakeUpstream{}, cfg: modelSyncInsecureTestConfig()}
	account := &Account{ID: 4456, Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{}}
	_, _, err := svc.fetchWorkbuddyUpstreamModels(context.Background(), account)
	require.Error(t, err)
	require.Equal(t, UpstreamModelSyncErrorConfiguration, requireUpstreamModelSyncError(t, err).Kind)
}

func TestFetchWorkbuddyUpstreamModelsWithoutHTTPUpstream(t *testing.T) {
	svc := &AccountTestService{cfg: modelSyncInsecureTestConfig()}
	_, _, err := svc.fetchWorkbuddyUpstreamModels(context.Background(), workbuddyModelSyncAccount("https://copilot.tencent.com"))
	require.Error(t, err)
	require.Equal(t, UpstreamModelSyncErrorConfiguration, requireUpstreamModelSyncError(t, err).Kind)
}

// ---------------------------------------------------------------------------
// D. 分派接线：workbuddy 必须走原生路径，不再回落通用 /v1/models
// ---------------------------------------------------------------------------

func TestNativeModelCatalogFetchersIncludeWorkbuddy(t *testing.T) {
	server, capture, upstream := newModelCatalogTestServer(t, http.StatusOK, workbuddyCatalogEnvelopeShape)
	svc := &AccountTestService{httpUpstream: upstream, cfg: modelSyncInsecureTestConfig()}

	account := workbuddyModelSyncAccount(server.URL)
	ids, metadata, handled, err := svc.fetchNativeModelCatalog(context.Background(), account)
	require.NoError(t, err)
	require.True(t, handled, "workbuddy 必须被原生路径接管，否则又会落回那句 unsupported platform")
	require.Equal(t, []string{"glm-5.2", "deepseek-v4.1-flash"}, ids)
	require.NotNil(t, metadata["glm-5.2"])
	require.Equal(t, workbuddyModelListPath, capture.last(t).path)
}

func TestSyncUpstreamModelCatalogDispatchesWorkbuddyToNativePath(t *testing.T) {
	server, capture, upstream := newModelCatalogTestServer(t, http.StatusOK, workbuddyCatalogEnvelopeShape)
	svc := &AccountTestService{httpUpstream: upstream, cfg: modelSyncInsecureTestConfig()}

	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), workbuddyModelSyncAccount(server.URL))
	require.NoError(t, err)
	require.Equal(t, []string{"deepseek-v4.1-flash", "glm-5.2"}, catalog.Models, "同步出口按字典序稳定排序")
	require.Equal(t, workbuddyModelListPath, capture.last(t).path)
}
