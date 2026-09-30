//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// trae_models_test.go 锁住 Trae 模型目录拉取的两层契约：
//
//  1. parseTraeModelCatalog —— 「哪些条目可以被开放给用户」的四条过滤纪律
//     （config_switch=false / is_invisible_to_user=true / display_name 空 /
//     config_name 空），以及两个布尔字段「缺省即视为可见/启用」的宽容解析；
//  2. fetchTraeUpstreamModels —— 端点形态（POST + 固定请求体）与头族
//     （沿用聊天域指纹，但 Accept 必须被覆写回 application/json）。
//
// 本文件同时提供 modelCatalogCapture / newModelCatalogTestServer /
// modelSyncInsecureTestConfig 三个基建，供 qoder_models_test.go 与
// upstream_models_native_test.go 复用（同包）。
//
// 基建选型说明：同包的 upstream_models_test.go / openai_oauth_passthrough_test.go
// 因未写 build 标签而在两种构型下都参与编译，因此其中的
// upstreamModelMetadataRepoStub / httpUpstreamRecorder 可直接复用；但前者只记「最后
// 一次」写入（无法断言“未发生写入”/“恰好发生 N 次”），后者只留内存里的 req（无法
// 断言真实的出站 wire 头族）。本文件的目录拉取测试需要两者，所以改用同为 unit
// 标签的 qoderFakeUpstream（qoder_campaign_service_test.go，带 client 转发）。
// 另外：Go 对无标签文件在任何构型下都编译，但仓内约定要求测试文件首行带
// //go:build unit，本文件严格遵循。

// ---------------------------------------------------------------------------
// 共享测试基建（本包内模型目录相关测试通用）
// ---------------------------------------------------------------------------

// modelCatalogCall 是一次「经真实 wire 抵达假上游」的请求快照。
type modelCatalogCall struct {
	method string
	path   string
	query  string
	header http.Header
	body   string
}

// modelCatalogCapture 记录假上游收到的全部请求（并发安全）。
type modelCatalogCapture struct {
	mu       sync.Mutex
	requests []modelCatalogCall
}

func (c *modelCatalogCapture) all() []modelCatalogCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]modelCatalogCall(nil), c.requests...)
}

func (c *modelCatalogCapture) last(t *testing.T) modelCatalogCall {
	t.Helper()
	all := c.all()
	require.NotEmpty(t, all, "假上游未收到任何请求")
	return all[len(all)-1]
}

// newModelCatalogTestServer 起一个固定应答的假上游，并返回一个真正向它转发请求的
// HTTPUpstream 替身（复用 qoderFakeUpstream：记录方法/路径/头/请求体后交给
// server.Client() 发出），从而断言的是经过 HTTP/1.1 wire 往返后的真实头族。
func newModelCatalogTestServer(t *testing.T, status int, body string) (*httptest.Server, *modelCatalogCapture, *qoderFakeUpstream) {
	t.Helper()
	capture := &modelCatalogCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		capture.mu.Lock()
		capture.requests = append(capture.requests, modelCatalogCall{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			header: r.Header.Clone(), body: string(raw),
		})
		capture.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server, capture, &qoderFakeUpstream{client: server.Client()}
}

// modelSyncInsecureTestConfig 放行 http://（httptest 是明文回环）且不启用白名单，
// 使 validateUpstreamBaseURL 只做格式校验。
func modelSyncInsecureTestConfig() *config.Config {
	return &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
		},
	}
}

// modelCatalogTestRepo 是带多轮 UpdateExtra 记账的账号仓库替身。
// 与 upstream_models_test.go 里的 upstreamModelMetadataRepoStub （单槽记录）不同，
// 本表保留每一次写入，用于断言「本轮根本没落库」与「恰好落库一次」。
type modelCatalogTestRepo struct {
	mockAccountRepoForGemini
	mu         sync.Mutex
	extraCalls []qoderExtraCall
	extraErr   error
}

func (r *modelCatalogTestRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	cp := make(map[string]any, len(updates))
	for k, v := range updates {
		cp[k] = v
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.extraCalls = append(r.extraCalls, qoderExtraCall{accountID: id, updates: cp})
	return r.extraErr
}

func (r *modelCatalogTestRepo) calls() []qoderExtraCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]qoderExtraCall(nil), r.extraCalls...)
}

// lastExtraSnapshot 返回最近一次写入的指定 key（无记录返回 nil）。
func (r *modelCatalogTestRepo) lastExtraSnapshot(key string) any {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.extraCalls) - 1; i >= 0; i-- {
		if v, ok := r.extraCalls[i].updates[key]; ok {
			return v
		}
	}
	return nil
}

func requireUpstreamModelSyncError(t *testing.T, err error) *UpstreamModelSyncError {
	t.Helper()
	require.Error(t, err)
	var syncErr *UpstreamModelSyncError
	require.True(t, errors.As(err, &syncErr), "错误应为 *UpstreamModelSyncError，实际 %T: %v", err, err)
	return syncErr
}

// ---------------------------------------------------------------------------
// parseTraeModelCatalog
// ---------------------------------------------------------------------------

const traeCatalogHappyPath = `{
  "code": 0,
  "config_info_list": [
    {"config_name":"doubao-seed-2.1-pro","config_switch":true,"is_invisible_to_user":false,
     "display_config":{"display_name":"Doubao Seed 2.1 Pro"},"context_window_tokens":{"dev":256000}},
    {"config_name":"gpt-5.6-sol","config_switch":true,"is_invisible_to_user":false,
     "display_config":{"display_name":"GPT-5.6 Sol"},"context_window_tokens":{"dev":400000}},
    {"config_name":"minimax-m3","config_switch":true,"is_invisible_to_user":false,
     "display_config":{"display_name":"MiniMax M3"}}
  ]
}`

func TestParseTraeModelCatalogKeepsUpstreamOrderAndMetadata(t *testing.T) {
	t.Parallel()

	models, metadata, err := parseTraeModelCatalog([]byte(traeCatalogHappyPath))
	require.NoError(t, err)
	// 顺序 = 上游数组序（本层不做字典序重排，重排交给 buildCatalogFromNativeModels）。
	require.Equal(t, []string{"doubao-seed-2.1-pro", "gpt-5.6-sol", "minimax-m3"}, models)

	require.Equal(t, UpstreamModelMetadata{
		ID:               "doubao-seed-2.1-pro",
		DisplayName:      "Doubao Seed 2.1 Pro",
		ContextWindow:    256000,
		MaxContextWindow: 256000,
	}, metadata["doubao-seed-2.1-pro"])

	gpt := metadata["gpt-5.6-sol"]
	require.Equal(t, "GPT-5.6 Sol", gpt.DisplayName)
	require.Equal(t, int64(400000), gpt.ContextWindow)
	require.Equal(t, int64(400000), gpt.MaxContextWindow)

	// context_window_tokens 缺失时不得凭空造值。
	require.Equal(t, int64(0), metadata["minimax-m3"].ContextWindow)
	require.Equal(t, int64(0), metadata["minimax-m3"].MaxContextWindow)
	// 该端点不下发推理/模态信息，绝不允许被解析成「有能力」。
	require.Nil(t, metadata["minimax-m3"].Reasoning)
	require.Empty(t, metadata["minimax-m3"].InputModalities)
}

func TestParseTraeModelCatalogFiltersUnusableEntries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		entry       string
		filteredIDs []string
	}{
		{
			name:        "config_switch_false",
			entry:       `{"config_name":"dead-model","config_switch":false,"display_config":{"display_name":"Dead"}}`,
			filteredIDs: []string{"dead-model"},
		},
		{
			name:        "invisible_to_user",
			entry:       `{"config_name":"subagent-model","is_invisible_to_user":true,"display_config":{"display_name":"Browser Use Subagent"}}`,
			filteredIDs: []string{"subagent-model"},
		},
		{
			name:        "display_name_blank",
			entry:       `{"config_name":"placeholder-model","display_config":{"display_name":"   "}}`,
			filteredIDs: []string{"placeholder-model"},
		},
		{
			name:        "display_name_absent",
			entry:       `{"config_name":"no-display-name"}`,
			filteredIDs: []string{"no-display-name"},
		},
		{
			name:        "config_name_blank",
			entry:       `{"config_name":"   ","display_config":{"display_name":"No ID"}}`,
			filteredIDs: nil,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			body := `{"config_info_list":[` + tc.entry +
				`,{"config_name":"healthy-model","display_config":{"display_name":"Healthy"}}]}`
			models, metadata, err := parseTraeModelCatalog([]byte(body))
			require.NoError(t, err)
			require.Equal(t, []string{"healthy-model"}, models)
			for _, filtered := range tc.filteredIDs {
				require.NotContains(t, metadata, filtered)
			}
			require.Len(t, metadata, 1)
		})
	}
}

func TestParseTraeModelCatalogTreatsMissingBooleansAsVisibleAndEnabled(t *testing.T) {
	t.Parallel()

	// 上游并不总是显式下发两个布尔字段：缺省必须按「启用 + 可见」处理。
	models, metadata, err := parseTraeModelCatalog([]byte(
		`{"config_info_list":[{"config_name":"sparse-model","display_config":{"display_name":"Sparse"}}]}`))
	require.NoError(t, err)
	require.Equal(t, []string{"sparse-model"}, models)
	require.Equal(t, "Sparse", metadata["sparse-model"].DisplayName)

	// 显式 true / false 的正确组合同样放行。
	models, _, err = parseTraeModelCatalog([]byte(
		`{"config_info_list":[{"config_name":"explicit-model","config_switch":true,"is_invisible_to_user":false,"display_config":{"display_name":"Explicit"}}]}`))
	require.NoError(t, err)
	require.Equal(t, []string{"explicit-model"}, models)

	// 空 config_name 只影响自身，不牵连同批其他条目。
	models, _, err = parseTraeModelCatalog([]byte(
		`{"config_info_list":[{"config_name":"","display_config":{"display_name":"Ghost"}},{"config_name":" survivor ","display_config":{"display_name":"Survivor"}}]}`))
	require.NoError(t, err)
	require.Equal(t, []string{"survivor"}, models, "config_name 两端空白应被裁剪")
}

// 信封容错：本端点无一手抓包证据，而 Trae 在本仓已证实的响应全部走火山
// 信封 {code,msg,Result:{...}}（见 traeExtractLoginHost），第三方实现另见裸对象
// 与 data 包裹，网关还有外层再包一层 Result 的形态。四种必须等价，
// 否则真值形态一变就是「JSON 200 但报空列表」的 502（旧实现只接住前两种）。
func TestParseTraeModelCatalogAcceptsAllKnownEnvelopes(t *testing.T) {
	t.Parallel()

	entries := `[{"config_name":"m-a","display_config":{"display_name":"Model A"}},{"config_name":"m-b","display_config":{"display_name":"Model B"}}]`

	naked, nakedMeta, err := parseTraeModelCatalog([]byte(`{"code":0,"config_info_list":` + entries + `}`))
	require.NoError(t, err)
	require.Equal(t, []string{"m-a", "m-b"}, naked)
	require.Len(t, nakedMeta, 2)

	for _, body := range []string{
		`{"code":0,"msg":"success","data":{"config_info_list":` + entries + `}}`,
		`{"code":0,"msg":"success","Result":{"config_info_list":` + entries + `}}`,
		`{"code":0,"msg":"success","result":{"config_info_list":` + entries + `}}`,
		// 网关双层包裹
		`{"Result":{"Result":{"config_info_list":` + entries + `}}}`,
		// Result 内层混有无关字段不影响定位
		`{"code":0,"Result":{"usage":"x","config_info_list":` + entries + `}}`,
	} {
		models, metadata, err := parseTraeModelCatalog([]byte(body))
		require.NoError(t, err, body)
		require.Equal(t, naked, models, body)
		require.Equal(t, nakedMeta, metadata, body)
	}
}

// camelCase 下发形态（上游同族接口存在两种命名记录）必须同样可用。
func TestParseTraeModelCatalogAcceptsCamelCaseFields(t *testing.T) {
	t.Parallel()

	body := `{"Result":{"configInfoList":[{"configName":"camel-model","configSwitch":true,"isInvisibleToUser":false,"displayConfig":{"displayName":"Camel One"},"contextWindowTokens":{"dev":128000}}]}}`
	models, metadata, err := parseTraeModelCatalog([]byte(body))
	require.NoError(t, err)
	require.Equal(t, []string{"camel-model"}, models)
	require.Equal(t, "Camel One", metadata["camel-model"].DisplayName)
	require.Equal(t, int64(128000), metadata["camel-model"].ContextWindow)
}

// config_switch 的值语义是「开关打开」——这里钉住方向，防止后续改名/重构时
// 把「false=已下线」反写成「true=已下线」（本轮 review 就抓到过一次这样的反转）。
func TestParseTraeModelCatalogConfigSwitchDirection(t *testing.T) {
	t.Parallel()

	one := func(body string) []string {
		t.Helper()
		models, _, err := parseTraeModelCatalog([]byte(body))
		require.NoError(t, err)
		return models
	}
	require.Equal(t, []string{"keep"}, one(`{"config_info_list":[{"config_name":"keep","config_switch":true,"display_config":{"display_name":"Keep"}},{"config_name":"drop","config_switch":false,"display_config":{"display_name":"Drop"}}]}`), "config_switch=false 应被过滤")
	require.Equal(t, []string{"keep"}, one(`{"config_info_list":[{"config_name":"keep","display_config":{"display_name":"Keep"}},{"config_name":"drop","is_invisible_to_user":true,"display_config":{"display_name":"Drop"}}]}`), "is_invisible_to_user=true 应被过滤")
}

func TestParseTraeModelCatalogDedupesRepeatedConfigName(t *testing.T) {
	t.Parallel()

	body := `{"config_info_list":[
		{"config_name":"dup","display_config":{"display_name":"First"}},
		{"config_name":"dup","display_config":{"display_name":"Second"}},
		{"config_name":"other","display_config":{"display_name":"Other"}}
	]}`
	models, metadata, err := parseTraeModelCatalog([]byte(body))
	require.NoError(t, err)
	require.Equal(t, []string{"dup", "other"}, models)
	require.Equal(t, "First", metadata["dup"].DisplayName, "首次出现者胜出")
}

func TestParseTraeModelCatalogRejectsUnusableResponses(t *testing.T) {
	t.Parallel()

	_, _, err := parseTraeModelCatalog([]byte(`not json at all`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse trae model catalog")

	// 信封不认识与列表确实为空必须分开报（两者处置方式不同：前者是上游改版、
	// 后者是该账号确实无可用模型），且两者都不能静默返回空列表
	// （空表会被上层误判为「同步成功但零模型」）。
	for _, tc := range []struct {
		body   string
		expect string
	}{
		{`{"code":0,"msg":"ok"}`, "config_info_list not found"},
		{`{"code":0,"msg":"ok","data":{"foo":1}}`, "config_info_list not found"},
		{`{"config_info_list":[]}`, "config_info_list is empty"},
		{`{"Result":{"config_info_list":[]}}`, "config_info_list is empty"},
		{`{"data":{"config_info_list":[]}}`, "config_info_list is empty"},
		{`{"data":null}`, "config_info_list not found"},
	} {
		_, _, err := parseTraeModelCatalog([]byte(tc.body))
		require.Error(t, err, tc.body)
		require.Contains(t, err.Error(), tc.expect, tc.body)
	}

	// 「不认识信封」的报文必须带上真实顶层键名与 body 摘要，否则线上只能看到
	// 一句无信息量的「是空的」。同时验凭据不会随报文泄露。
	_, _, err = parseTraeModelCatalog([]byte(`{"Code":0,"Message":"nope","Result":{"ModelList":[]}}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "top-level keys:")
	require.Contains(t, err.Error(), "Code")
	require.Contains(t, err.Error(), "Message")

	sensitive := `{"code":0,"msg":"failed","access_token":"super-secret-token-value","Result":{"config_info_list":null}}`
	_, _, err = parseTraeModelCatalog([]byte(sensitive))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "super-secret-token-value", "错误报文不得回显凭据（logredact 应已脱敏）")
}

// ---------------------------------------------------------------------------
// fetchTraeUpstreamModels
// ---------------------------------------------------------------------------

func traeModelSyncTestAccount(baseURL string) *Account {
	return &Account{
		ID:       4401,
		Platform: PlatformTrae,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			// 带 scheme 前缀的粘贴形态：必须被归一为 "Cloud-IDE-JWT <裸 JWT>"。
			"access_token": "Cloud-IDE-JWT eyJhbGciOiJBTEST.payload.sig",
			"uid":          "7001234567890123456",
			"realm":        "cn",
			"base_url":     baseURL,
		},
	}
}

func TestFetchTraeUpstreamModelsPostsCatalogEndpointWithJSONAccept(t *testing.T) {
	server, capture, upstream := newModelCatalogTestServer(t, http.StatusOK, traeCatalogHappyPath)

	svc := &AccountTestService{httpUpstream: upstream, cfg: modelSyncInsecureTestConfig()}
	models, metadata, err := svc.fetchTraeUpstreamModels(context.Background(), traeModelSyncTestAccount(server.URL))
	require.NoError(t, err)
	require.Equal(t, []string{"doubao-seed-2.1-pro", "gpt-5.6-sol", "minimax-m3"}, models)
	require.Equal(t, "GPT-5.6 Sol", metadata["gpt-5.6-sol"].DisplayName)

	calls := capture.all()
	require.Len(t, calls, 1, "token 未过期时不该发出第二次请求")
	call := calls[0]
	require.Equal(t, http.MethodPost, call.method)
	require.Equal(t, traeModelListPath, call.path)

	// 请求体契约：function=solo_agent（必须与聊天侧 traeChatFunction 同表，否则拉到的
	// 模型调不通），null 占位字段必须保留（上游按缺键/空值区分处理）。
	var sent map[string]any
	require.NoError(t, json.Unmarshal([]byte(call.body), &sent))
	require.Equal(t, traeChatFunction, sent["function"],
		"目录 function 必须等于聊天 function：上游按 function 分片下发可调表，两侧不同表会把必 4001 的模型开放给下游")
	require.Equal(t, false, sent["need_prompt"])
	require.Equal(t, true, sent["poly_prompt"])
	require.Contains(t, sent, "config_names")
	require.Nil(t, sent["config_names"])
	require.Contains(t, sent, "current_config_info")
	require.Nil(t, sent["current_config_info"])
	require.Contains(t, sent, "mode_type")
	require.Contains(t, sent, "agent_type")

	// 头族：聊天域 IDE 指纹 + 目录接口必需的 Accept 覆写。
	require.Equal(t, "Cloud-IDE-JWT eyJhbGciOiJBTEST.payload.sig", call.header.Get("Authorization"))
	require.Equal(t, "eyJhbGciOiJBTEST.payload.sig", call.header.Get("X-Ide-Token"))
	require.Equal(t, "eyJhbGciOiJBTEST.payload.sig", call.header.Get("X-Cloudide-Token"))
	require.Equal(t, "7001234567890123456", call.header.Get("X-Uid"))
	require.NotEmpty(t, call.header.Get("X-Ide-Version-Code"))
	require.Equal(t, defaultTraeIDEVersionCode, call.header.Get("X-Ide-Version-Code"))
	// 这是本端点与聊天域唯一的差异：目录返回 JSON，不是 SSE。
	require.Equal(t, "application/json", call.header.Get("Accept"))
	require.NotContains(t, call.header.Get("Accept"), "text/event-stream")
}

func TestFetchTraeUpstreamModelsSurfacesUpstreamFailures(t *testing.T) {
	t.Run("all_entries_filtered_is_error", func(t *testing.T) {
		body := `{"config_info_list":[
			{"config_name":"off-1","config_switch":false,"display_config":{"display_name":"Off 1"}},
			{"config_name":"hidden-1","is_invisible_to_user":true,"display_config":{"display_name":"Hidden 1"}}
		]}`
		server, _, upstream := newModelCatalogTestServer(t, http.StatusOK, body)
		svc := &AccountTestService{httpUpstream: upstream, cfg: modelSyncInsecureTestConfig()}

		models, metadata, err := svc.fetchTraeUpstreamModels(context.Background(), traeModelSyncTestAccount(server.URL))
		syncErr := requireUpstreamModelSyncError(t, err)
		require.Equal(t, UpstreamModelSyncErrorUpstream, syncErr.Kind)
		require.ErrorContains(t, err, "no schedulable models")
		require.Empty(t, models)
		require.Nil(t, metadata)
	})

	t.Run("unparsable_body", func(t *testing.T) {
		server, _, upstream := newModelCatalogTestServer(t, http.StatusOK, `<html>502 Bad Gateway</html>`)
		svc := &AccountTestService{httpUpstream: upstream, cfg: modelSyncInsecureTestConfig()}

		_, _, err := svc.fetchTraeUpstreamModels(context.Background(), traeModelSyncTestAccount(server.URL))
		requireUpstreamModelSyncError(t, err)
		require.ErrorContains(t, err, "not usable")
	})

	t.Run("http_error_status", func(t *testing.T) {
		server, capture, upstream := newModelCatalogTestServer(t, http.StatusInternalServerError, `{"msg":"internal"}`)
		svc := &AccountTestService{httpUpstream: upstream, cfg: modelSyncInsecureTestConfig()}

		_, _, err := svc.fetchTraeUpstreamModels(context.Background(), traeModelSyncTestAccount(server.URL))
		syncErr := requireUpstreamModelSyncError(t, err)
		require.Equal(t, UpstreamModelSyncErrorUpstream, syncErr.Kind)
		require.Equal(t, http.StatusInternalServerError, syncErr.StatusCode)
		require.ErrorContains(t, err, "HTTP 500")
		require.Len(t, capture.all(), 1)
	})
}

func TestFetchTraeUpstreamModelsConfigurationErrors(t *testing.T) {
	t.Parallel()

	// 无 httpUpstream：配置错误，不触网。
	_, _, err := (&AccountTestService{cfg: modelSyncInsecureTestConfig()}).
		fetchTraeUpstreamModels(context.Background(), traeModelSyncTestAccount("https://trae.example.com"))
	syncErr := requireUpstreamModelSyncError(t, err)
	require.Equal(t, UpstreamModelSyncErrorConfiguration, syncErr.Kind)
	require.ErrorContains(t, err, "Upstream HTTP client is not configured")

	svc := &AccountTestService{httpUpstream: &qoderFakeUpstream{}, cfg: modelSyncInsecureTestConfig()}

	// 完全没有 access_token（也没有 refresh_token）：配置错误。
	noToken := traeModelSyncTestAccount("https://trae.example.com")
	noToken.Credentials["access_token"] = ""
	_, _, err = svc.fetchTraeUpstreamModels(context.Background(), noToken)
	requireUpstreamModelSyncError(t, err)
	require.ErrorContains(t, err, "No Trae access token is available")

	// base_url 形态非法：配置错误。
	_, _, err = svc.fetchTraeUpstreamModels(context.Background(), traeModelSyncTestAccount("not-a-url"))
	requireUpstreamModelSyncError(t, err)
	require.ErrorContains(t, err, "Invalid Trae base URL")
}
