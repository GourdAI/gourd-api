//go:build unit

package service

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// qoder_models_test.go Qoder 模型目录拉取契约。
//
// 两条不可退让的纪律：
//  1. 对外模型 ID 必须取 key（display_name 发上游会被静默降档），所以「同一 key 跨场景
//     只留首次、以靠前场景元数据为准」+「顺序稳定」是防误报回归的核心；
//  2. 出站必须复用聊天域 COSY 签名头且 accept=application/json（聊天端点要求的
//     text/event-stream 对本端点不适用）。
//
// 共享基建（假上游 / 配置 / 仓库替身）复用 trae_models_test.go。

// ---------------------------------------------------------------------------
// parseQoderModelCatalog
// ---------------------------------------------------------------------------

func TestParseQoderModelCatalogMergesScenesAndKeepsFirstOccurrence(t *testing.T) {
	t.Parallel()

	body := `{
	  "chat": [
	    {"key":"auto","display_name":"Auto"},
	    {"key":"dmodel","display_name":"DeepSeek-V4-Pro","enable":true,"max_input_tokens":196608,"max_output_tokens":32768},
	    {"key":"gmodel","display_name":"GLM-5","is_vl":true}
	  ],
	  "developer": [
	    {"key":"dmodel","display_name":"DeepSeek-V4-Pro (developer view)","max_input_tokens":131072},
	    {"key":"kmodel_latest","display_name":"Kimi-K2-Latest","thinking_config":{"supported_reasoning_efforts":["low","high"]}}
	  ],
	  "assistant": [
	    {"key":"gmodel","display_name":"GLM-5 again"}
	  ]
	}`

	models, metadata, err := parseQoderModelCatalog([]byte(body))
	require.NoError(t, err)
	// 顺序：先已知场景（qoderModelListScenes 序）、场景内保持上游数组序。
	require.Equal(t, []string{"dmodel", "gmodel", "kmodel_latest"}, models)

	// 同 key 跨场景只留首次：以 chat（靠前场景）的元数据为准，developer 的值不得覆盖。
	require.Equal(t, "DeepSeek-V4-Pro", metadata["dmodel"].DisplayName)
	require.Equal(t, int64(196608), metadata["dmodel"].ContextWindow)
	require.Equal(t, int64(32768), metadata["dmodel"].MaxOutputTokens)
	require.Equal(t, "GLM-5", metadata["gmodel"].DisplayName)
	require.Len(t, metadata, 3)
}

func TestParseQoderModelCatalogSkipsUnusableEntries(t *testing.T) {
	t.Parallel()

	body := `{
	  "chat": [
	    {"key":"auto","display_name":"Auto Router"},
	    {"key":"","display_name":"No Key"},
	    {"key":"   ","display_name":"Blank Key"},
	    {"key":"off-model","display_name":"Turned Off","enable":false},
	    {"key":"live-model","display_name":"Live","enable":true},
	    {"key":"default-on-model","display_name":"Default On"}
	  ]
	}`
	models, metadata, err := parseQoderModelCatalog([]byte(body))
	require.NoError(t, err)
	require.Equal(t, []string{"live-model", "default-on-model"}, models)
	require.NotContains(t, metadata, "auto", "auto 是路由入口，不是可调模型")
	require.NotContains(t, metadata, "off-model")
	// enable 缺省视为启用（上游并非总是显式下发）。
	require.Contains(t, metadata, "default-on-model")
}

func TestParseQoderModelCatalogMapsCapabilityFields(t *testing.T) {
	t.Parallel()

	body := `{
	  "chat": [
	    {"key":"vl-model","display_name":"VL","is_vl":true,"max_input_tokens":256000,"max_output_tokens":65536,
	     "thinking_config":{"enabled":{"efforts":["low","high"]}}},
	    {"key":"opaque-thinking","display_name":"Opaque","max_input_tokens":128000,
	     "thinking_config":{"enable":true}},
	    {"key":"text-model","display_name":"Text Only","max_input_tokens":0}
	  ]
	}`
	models, metadata, err := parseQoderModelCatalog([]byte(body))
	require.NoError(t, err)
	require.Equal(t, []string{"vl-model", "opaque-thinking", "text-model"}, models)

	vl := metadata["vl-model"]
	require.Equal(t, []string{"text", "image"}, vl.InputModalities)
	require.Equal(t, int64(256000), vl.ContextWindow)
	require.Equal(t, int64(256000), vl.MaxContextWindow, "上游只给 max_input_tokens，两侧同值")
	require.Equal(t, int64(65536), vl.MaxOutputTokens)
	// 能拿到具体 effort 时才提升为 reasoning=true：否则
	// upstreamModelMetadataIsComplete 会把「reasoning=true 但无 levels」判为不完整，
	// 快照永远写不进去、每轮同步都会回一条 partial 告警。
	require.NotNil(t, vl.Reasoning, "thinking_config 带 effort 档位 → 判定支持推理")
	require.True(t, *vl.Reasoning)
	require.Equal(t, []string{"low", "high"}, vl.SupportedReasoningLevels)
	require.Equal(t, "low", vl.DefaultReasoningLevel)

	// thinking_config 存在但形状不认识（只有布尔开关、无档位）：宁可不标，
	// 也不能谎报 reasoning=true（会污染快照完整性）或谎报 false。
	opaque := metadata["opaque-thinking"]
	require.Nil(t, opaque.Reasoning, "无可用 effort 档位时不得声称支持推理")
	require.Empty(t, opaque.SupportedReasoningLevels)

	text := metadata["text-model"]
	require.Equal(t, []string{"text"}, text.InputModalities)
	require.Equal(t, int64(0), text.ContextWindow, "max_input_tokens<=0 不得造值")
	require.Equal(t, int64(0), text.MaxContextWindow)
	require.Nil(t, text.Reasoning, "无 thinking_config 不得声称支持推理")
}

// TestQoderThinkingEffortsAcceptsKnownShapes 锁定档位提取对上游多种字段形态的宽容度。
func TestQoderThinkingEffortsAcceptsKnownShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		body  string
		want  []string
		claim bool
	}{
		{"supported_reasoning_efforts 数组", `{"chat":[{"key":"k1","max_input_tokens":1,"thinking_config":{"supported_reasoning_efforts":["low","high","bogus"]}}]}`, []string{"low", "high"}, true},
		{"enabled.efforts 嵌套", `{"chat":[{"key":"k1","max_input_tokens":1,"thinking_config":{"enabled":{"efforts":["xhigh"]},"disabled":null}}]}`, []string{"xhigh"}, true},
		{"扁平数组", `{"chat":[{"key":"k1","max_input_tokens":1,"thinking_config":["off","minimal"]}]}`, []string{"none", "minimal"}, true},
		{"off/disabled 归一为 none", `{"chat":[{"key":"k1","max_input_tokens":1,"thinking_config":{"efforts":["off"]}}]}`, []string{"none"}, true},
		{"纯布尔无可档位", `{"chat":[{"key":"k1","max_input_tokens":1,"thinking_config":{"enable":true}}]}`, nil, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, metadata, err := parseQoderModelCatalog([]byte(tc.body))
			require.NoError(t, err)
			entry := metadata["k1"]
			require.Equal(t, tc.want, entry.SupportedReasoningLevels)
			if tc.claim {
				require.NotNil(t, entry.Reasoning)
				require.True(t, *entry.Reasoning)
			} else {
				require.Nil(t, entry.Reasoning)
			}
		})
	}
}

func TestParseQoderModelCatalogFallsBackDisplayNameToKey(t *testing.T) {
	t.Parallel()

	models, metadata, err := parseQoderModelCatalog([]byte(
		`{"quest":[{"key":"qmodel"},{"key":"qmodel2","display_name":"   "}]}`))
	require.NoError(t, err)
	require.Equal(t, []string{"qmodel", "qmodel2"}, models)
	require.Equal(t, "qmodel", metadata["qmodel"].DisplayName, "display_name 缺失回落 key")
	require.Equal(t, "qmodel2", metadata["qmodel2"].DisplayName, "display_name 空白亦回落 key")
	require.Equal(t, "qmodel", metadata["qmodel"].ID)
}

func TestParseQoderModelCatalogOrdersUnknownScenesDeterministically(t *testing.T) {
	t.Parallel()

	// 上游键集合会随版本增减：未知场景必须被纳入（否则漏模型），且顺序确定
	// （否则管理页会把「map 随机序」误报为「模型变更」）。
	body := `{
	  "qwake": [{"key":"w1","display_name":"W1"}],
	  "app":   [{"key":"a1","display_name":"A1"}],
	  "chat":  [{"key":"c1","display_name":"C1"}],
	  "zzz":   [{"key":"z1","display_name":"Z1"}],
	  "inline":[{"key":"i1","display_name":"I1"}],
	  "Beta":  [{"key":"b1","display_name":"B1"}]
	}`
	models, _, err := parseQoderModelCatalog([]byte(body))
	require.NoError(t, err)
	require.Equal(t, []string{
		// 已知场景按 qoderModelListScenes 声明序（chat 先于 inline）
		"c1", "i1",
		// 未知场景按字典序（Beta < app < qwake < zzz，大小写敏感排序）
		"b1", "a1", "w1", "z1",
	}, models)

	// 同一输入两次解析结果完全相等（含元数据表）。
	firstModels, firstMeta, err := parseQoderModelCatalog([]byte(body))
	require.NoError(t, err)
	secondModels, secondMeta, err := parseQoderModelCatalog([]byte(body))
	require.NoError(t, err)
	require.Equal(t, firstModels, secondModels)
	require.Equal(t, firstMeta, secondMeta)
}

func TestParseQoderModelCatalogRejectsUnusableResponses(t *testing.T) {
	t.Parallel()

	_, _, err := parseQoderModelCatalog([]byte(`{"chat":`))
	require.Error(t, err)
	require.ErrorContains(t, err, "parse qoder model catalog")

	// OpenAI 形态的响应（不是场景映射）：data 条目没有 key 字段，因此不能静默交出零模型。
	_, _, err = parseQoderModelCatalog([]byte(`{"data":[{"id":"gpt-5"}]}`))
	require.Error(t, err)
	require.ErrorContains(t, err, "no enabled models")

	_, _, err = parseQoderModelCatalog([]byte(`{}`))
	require.Error(t, err)
	require.ErrorContains(t, err, "no scene keys")

	_, _, err = parseQoderModelCatalog([]byte(`[]`))
	require.Error(t, err)
	require.ErrorContains(t, err, "parse qoder model catalog")

	// 有场景键但没有一个可用模型（全 auto / 全禁用）。
	_, _, err = parseQoderModelCatalog([]byte(`{"chat":[{"key":"auto"}],"quest":[{"key":"off","enable":false}]}`))
	require.Error(t, err)
	require.ErrorContains(t, err, "no enabled models")
}

// ---------------------------------------------------------------------------
// fetchQoderUpstreamModels
// ---------------------------------------------------------------------------

func qoderModelSyncTestAccount(baseURL string) *Account {
	return &Account{
		ID:       4402,
		Platform: PlatformQoder,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "dt-test-device-token",
			// uid 必须非空：否则 qoderHealIdentity 会去拉真实 userinfo。
			"uid":      "u-qoder-1",
			"realm":    "cn",
			"base_url": baseURL,
		},
	}
}

const qoderCatalogHappyPath = `{
  "chat": [
    {"key":"auto"},
    {"key":"dmodel","display_name":"DeepSeek-V4-Pro","enable":true,"max_input_tokens":196608,"max_output_tokens":32768},
    {"key":"gmodel","display_name":"GLM-5","is_vl":true,"thinking_config":{"supported_reasoning_efforts":["low"]}}
  ],
  "qwake": [{"key":"kmodel_latest","display_name":"Kimi-K2-Latest"}]
}`

func TestFetchQoderUpstreamModelsGetsCatalogWithSignedCOSYHeaders(t *testing.T) {
	server, capture, upstream := newModelCatalogTestServer(t, http.StatusOK, qoderCatalogHappyPath)

	svc := &AccountTestService{httpUpstream: upstream, cfg: modelSyncInsecureTestConfig()}
	models, metadata, err := svc.fetchQoderUpstreamModels(context.Background(), qoderModelSyncTestAccount(server.URL))
	require.NoError(t, err)
	require.Equal(t, []string{"dmodel", "gmodel", "kmodel_latest"}, models)
	require.Equal(t, "GLM-5", metadata["gmodel"].DisplayName)
	require.NotNil(t, metadata["gmodel"].Reasoning)
	require.Equal(t, []string{"text", "image"}, metadata["gmodel"].InputModalities)

	calls := capture.all()
	require.Len(t, calls, 1)
	call := calls[0]
	require.Equal(t, http.MethodGet, call.method)
	require.Equal(t, "/algo/api/v2/model/list", call.path)
	require.Equal(t, "Encode=1", call.query)
	require.Empty(t, call.body, "GET 无请求体（签名 body 取空串）")

	// 鉴权：与聊天同一套 COSY 签名头族。
	require.True(t, strings.HasPrefix(call.header.Get("authorization"), "Bearer COSY."),
		"authorization 应为 COSY 签名形态，实际 %q", call.header.Get("authorization"))
	require.NotEmpty(t, call.header.Get("cosy-key"))
	require.NotEmpty(t, call.header.Get("cosy-date"))
	require.NotEmpty(t, call.header.Get("cosy-machineid"))
	require.Equal(t, "auto", call.header.Get("x-model-key"), "目录请求按路由入口占位")
	// 本端点必须收 JSON，而不是聊天的 SSE。
	require.Equal(t, "application/json", call.header.Get("accept"))
	require.NotContains(t, call.header.Get("accept"), "text/event-stream")
	require.Equal(t, "identity", call.header.Get("accept-encoding"))
}

func TestFetchQoderUpstreamModelsSurfacesUpstreamFailures(t *testing.T) {
	t.Run("http_error_status", func(t *testing.T) {
		server, _, upstream := newModelCatalogTestServer(t, http.StatusUnauthorized, `{"msg":"105 Login expired"}`)
		svc := &AccountTestService{httpUpstream: upstream, cfg: modelSyncInsecureTestConfig()}

		_, _, err := svc.fetchQoderUpstreamModels(context.Background(), qoderModelSyncTestAccount(server.URL))
		syncErr := requireUpstreamModelSyncError(t, err)
		require.Equal(t, UpstreamModelSyncErrorUpstream, syncErr.Kind)
		require.Equal(t, http.StatusUnauthorized, syncErr.StatusCode)
		require.ErrorContains(t, err, "HTTP 401")
	})

	t.Run("unparsable_body", func(t *testing.T) {
		server, _, upstream := newModelCatalogTestServer(t, http.StatusOK, `plain-text-not-json`)
		svc := &AccountTestService{httpUpstream: upstream, cfg: modelSyncInsecureTestConfig()}

		_, _, err := svc.fetchQoderUpstreamModels(context.Background(), qoderModelSyncTestAccount(server.URL))
		requireUpstreamModelSyncError(t, err)
		require.ErrorContains(t, err, "not usable")
	})
}

func TestFetchQoderUpstreamModelsConfigurationErrors(t *testing.T) {
	t.Parallel()

	// 无 httpUpstream：配置错误。
	_, _, err := (&AccountTestService{cfg: modelSyncInsecureTestConfig()}).
		fetchQoderUpstreamModels(context.Background(), qoderModelSyncTestAccount("https://qoder.example.com"))
	syncErr := requireUpstreamModelSyncError(t, err)
	require.Equal(t, UpstreamModelSyncErrorConfiguration, syncErr.Kind)
	require.ErrorContains(t, err, "Upstream HTTP client is not configured")

	svc := &AccountTestService{httpUpstream: &qoderFakeUpstream{}, cfg: modelSyncInsecureTestConfig()}

	// 无 access_token 且无 PAT/refresh_token：配置错误，不触网。
	noToken := qoderModelSyncTestAccount("https://qoder.example.com")
	noToken.Credentials["access_token"] = ""
	_, _, err = svc.fetchQoderUpstreamModels(context.Background(), noToken)
	require.Equal(t, UpstreamModelSyncErrorConfiguration, requireUpstreamModelSyncError(t, err).Kind)
	require.ErrorContains(t, err, "No Qoder access token is available")

	// base_url 形态非法：配置错误。
	badBase := qoderModelSyncTestAccount("::not-a-url::")
	_, _, err = svc.fetchQoderUpstreamModels(context.Background(), badBase)
	require.Equal(t, UpstreamModelSyncErrorConfiguration, requireUpstreamModelSyncError(t, err).Kind)
	require.ErrorContains(t, err, "Invalid Qoder base URL")
}
