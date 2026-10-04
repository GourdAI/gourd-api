//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 自定义错误码不得屏蔽 (账号, 单模型) 级不可用冷却。
//
// ShouldHandleErrorCode 的语义是「哪些状态码可以把这个账号**整体**打下线」
// （account.go:1255）。此前 HandleUpstreamModelNotFound 首行也过这道闸门，于是
// 管理员勾了自定义错误码但没列入 404 时，上游明确回了「这个模型我没有」却
// 不写 per-model 冷却，同一个号会在每个请求里被反复选中、再撞一遍 404
//（用户可见的「限流了还被调用、反复报 404」）。
//
// 与 TestRateLimitService_HandleUpstreamError_CustomPolicyExclusionSkipsAllState
// 的分工必须清楚：那条锁的是「body 不含 model 的裸 404（endpoint 配错）」，
// 属于账号整体问题，仍应被自定义错误码闸门拦住；本文件锁的是「模型级确定性
// 不可用」，不在管理员的排除意图之内。两者共用 isUpstreamModelNotFoundError
// 的 "body 必须含 model" 判据作为分界线。

func customErrorCodeAccountExcluding(statusCode int) *Account {
	return &Account{
		ID:          101,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			// 只列 503：404 不在表内 ⇒ ShouldHandleErrorCode(404) == false。
			"custom_error_codes_enabled": true,
			"custom_error_codes":         []any{float64(http.StatusServiceUnavailable)},
		},
	}
}

func TestHandleUpstreamModelNotFound_WritesCooldownDespiteCustomErrorCodeExclusion(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &RateLimitService{accountRepo: repo}
	account := customErrorCodeAccountExcluding(http.StatusServiceUnavailable)
	require.False(t, account.ShouldHandleErrorCode(http.StatusNotFound),
		"前置条件：本用例必须跑在「自定义错误码不含 404」的账号上")

	handled := svc.HandleUpstreamModelNotFound(
		context.Background(),
		account,
		"gpt-5.4",
		http.StatusNotFound,
		[]byte(`{"error":{"message":"model not found: gpt-5.4"}}`),
	)

	require.True(t, handled, "模型不可用必须换算成换号信号，否则该号会被反复重选")
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, "gpt-5.4", repo.modelRateLimitCalls[0].scope)
	require.Equal(t, upstreamModelNotFoundReason, repo.modelRateLimitCalls[0].reason)
	// 只下线这一个模型：不得升级为账号级临时不可调度（那是管理员排除的语义）。
	require.Zero(t, repo.tempCalls, "per-model 冷却不得顺带把账号整体打下线")
}

func TestHandleUpstreamModelNotFound_BareEndpointNotFoundStillRespectsExclusion(t *testing.T) {
	// 分界线反向锁：base_url 配错回应的 `404 page not found` 不含 "model"，
	// 不是模型级信号。这条路径由 HandleUpstreamError 的通用分支处理，仍受
	// 自定义错误码闸门管辖，本修复不得把它误升级成冷却。
	repo := &modelNotFoundAccountRepoStub{}
	svc := &RateLimitService{accountRepo: repo}
	account := customErrorCodeAccountExcluding(http.StatusServiceUnavailable)

	handled := svc.HandleUpstreamModelNotFound(
		context.Background(),
		account,
		"gpt-5.4",
		http.StatusNotFound,
		[]byte(`404 page not found`),
	)

	require.False(t, handled)
	require.Empty(t, repo.modelRateLimitCalls)
	require.Zero(t, repo.tempCalls)
}

func TestHandleUpstreamModelNotFound_CoolsMappedModelKey(t *testing.T) {
	// 与上一轮的键口径修复配套：冷却写在映射名上，读取侧现在双键都查。
	repo := &modelNotFoundAccountRepoStub{}
	svc := &RateLimitService{accountRepo: repo}
	account := customErrorCodeAccountExcluding(http.StatusServiceUnavailable)
	account.Credentials["model_mapping"] = map[string]any{"claude-sonnet-4-5": "claude-sonnet-4-5-20250915"}

	handled := svc.HandleUpstreamModelNotFound(
		context.Background(),
		account,
		"claude-sonnet-4-5",
		http.StatusNotFound,
		[]byte(`{"error":{"message":"model not found"}}`),
	)

	require.True(t, handled)
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, "claude-sonnet-4-5-20250915", repo.modelRateLimitCalls[0].scope)

	// stub 仓库不会真的回写账号，按既有范式把冷却结果灌回 Extra 再验读取侧。
	account.Extra = map[string]any{
		modelRateLimitsKey: map[string]any{
			repo.modelRateLimitCalls[0].scope: map[string]any{
				"rate_limit_reset_at": repo.modelRateLimitCalls[0].resetAt.UTC().Format(time.RFC3339),
			},
		},
	}

	// 调度器必须能读到这条冷却（写在映射名上，但原始名请求现在也命中，读取侧双键）。
	require.False(t, account.IsSchedulableForModelWithContext(context.Background(), "claude-sonnet-4-5"),
		"写入映射键后，原始名请求也必须被同一个冷却挡住")
	require.True(t, account.IsSchedulableForModelWithContext(context.Background(), "gpt-5.6-sol"),
		"冷却只针对该模型，账号对其他模型照常可用")
}

func TestOpenAIModelNotFoundFailover_BypassesHandlerCustomErrorCodeGate(t *testing.T) {
	// handler 外层闸门（handleErrorResponse / handleCompatErrorResponse）在
	// ShouldHandleErrorCode 未过时直接回 500 且不换号。本函数是那条分支里的旁路：
	// 模型确定性不可用时照样写冷却并返回换号信号。
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := customErrorCodeAccountExcluding(http.StatusServiceUnavailable)
	require.False(t, account.ShouldHandleErrorCode(http.StatusNotFound))

	resp := &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"x-request-id": []string{"req-1"}},
	}
	err := svc.openAIModelNotFoundFailover(
		context.Background(),
		nil,
		account,
		resp,
		[]byte(`{"error":{"message":"model not found: gpt-5.4"}}`),
		[]string{"gpt-5.4"},
		nil,
	)

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "必须返回换号信号，否则同一个号在每个请求里被反复选中")
	require.Equal(t, http.StatusNotFound, failoverErr.StatusCode)
	require.False(t, failoverErr.RetryableOnSameAccount, "同账号重试同一模型必然再撞 404")
	require.Len(t, repo.modelRateLimitCalls, 1)
}

func TestOpenAIModelNotFoundFailover_IgnoresUnrelatedStatusCodes(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{rateLimitService: &RateLimitService{accountRepo: repo}}
	account := customErrorCodeAccountExcluding(http.StatusServiceUnavailable)

	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"500 上游故障", http.StatusInternalServerError, `{"error":{"message":"server error"}}`},
		{"429 限流", http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`},
		{"404 端点错配", http.StatusNotFound, `404 page not found`},
		{"400 普通参数错误", http.StatusBadRequest, `{"error":{"message":"invalid api key"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{}}
			err := svc.openAIModelNotFoundFailover(context.Background(), nil, account, resp, []byte(tc.body), []string{"gpt-5.4"}, nil)
			require.NoError(t, err, "旁路只认两种确定性的模型不可用形态，其余一律不拦")
		})
	}
	require.Empty(t, repo.modelRateLimitCalls)
}

func TestOpenAIModelNotFoundFailover_SkipsWhenModelNameUnknown(t *testing.T) {
	// 没有模型名就写不出 (账号,模型) 键；此时保持原行为（走通用 500），
	// 不得退化成账号级处罚。
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{rateLimitService: &RateLimitService{accountRepo: repo}}
	account := customErrorCodeAccountExcluding(http.StatusServiceUnavailable)
	resp := &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}}

	err := svc.openAIModelNotFoundFailover(context.Background(), nil, account, resp,
		[]byte(`{"error":{"message":"model not found"}}`), nil, nil)

	require.NoError(t, err)
	require.Zero(t, repo.tempCalls)
	require.Empty(t, repo.modelRateLimitCalls)
}

// 模型名兜底解析：调用方没传 requestedModel 时（Responses 端点的兼容转发路径），
// 必须能从 requestBody 取回模型名并换算为调度名，否则冷却键为空 → 旁路白做。
// 这是 handleErrorResponse 正常路径一直有的能力，旁路必须同口径。
func TestOpenAIModelNotFoundFailover_FallsBackToRequestBodyModel(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{rateLimitService: &RateLimitService{accountRepo: repo}}
	account := customErrorCodeAccountExcluding(http.StatusServiceUnavailable)
	account.Credentials["model_mapping"] = map[string]any{"claude-sonnet-4-5": "claude-sonnet-4-5-20250915"}
	resp := &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}}

	err := svc.openAIModelNotFoundFailover(context.Background(), nil, account, resp,
		[]byte(`{"error":{"message":"model not found"}}`),
		nil,
		[]byte(`{"model":"claude-sonnet-4-5","input":[{"role":"user","content":"hi"}]}`),
	)

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "requestedModel 缺失时应回退到 body 里的模型名")
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, "claude-sonnet-4-5-20250915", repo.modelRateLimitCalls[0].scope,
		"兜底路径同样要经过调度模型名换算，与正常路径同口径")
}

func TestOpenAIModelNotFoundFailover_NilSafety(t *testing.T) {
	account := customErrorCodeAccountExcluding(http.StatusServiceUnavailable)
	resp := &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}}
	body := []byte(`{"error":{"message":"model not found"}}`)

	var nilSvc *OpenAIGatewayService
	require.NoError(t, nilSvc.openAIModelNotFoundFailover(context.Background(), nil, account, resp, body, []string{"m"}, nil))
	require.NoError(t, (&OpenAIGatewayService{}).openAIModelNotFoundFailover(context.Background(), nil, account, resp, body, []string{"m"}, nil))
	require.NoError(t, (&OpenAIGatewayService{rateLimitService: &RateLimitService{}}).
		openAIModelNotFoundFailover(context.Background(), nil, nil, resp, body, []string{"m"}, nil))
	require.NoError(t, (&OpenAIGatewayService{rateLimitService: &RateLimitService{accountRepo: &modelNotFoundAccountRepoStub{}}}).
		openAIModelNotFoundFailover(context.Background(), nil, account, nil, body, []string{"m"}, nil))
}

// 关键词表口径：只有报文真的说 "not found" / "unknown model" 才算确定性模型不可用。
// 「The model x does not exist」这类写法规则不命中（宁可不写冷却也不误伤），
// 本用例把这个保守边界钉住，防止后来者为了「多兼容几种报文」把判据改宽。
func TestOpenAIModelNotFoundFailover_RequiresNotFoundKeyword(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{rateLimitService: &RateLimitService{accountRepo: repo}}
	account := customErrorCodeAccountExcluding(http.StatusServiceUnavailable)

	resp := &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}}
	err := svc.openAIModelNotFoundFailover(context.Background(), nil, account, resp,
		[]byte(`{"error":{"message":"The model gpt-5.4 does not exist"}}`), []string{"gpt-5.4"}, nil)
	require.NoError(t, err, "不含 not-found 关键词时报文不得触发旁路冷却")
	require.Empty(t, repo.modelRateLimitCalls)
}

// 【P1-D】HandleUpstreamError 自己的自定义错误码闸门（:359）也必须给 per-model
// 冷却让路。
//
// 上一轮只拆了 HandleUpstreamModelNotFound 首行与两处 OpenAI handler 的外层闸门，
// 但 HandleUpstreamError 在 :359 就早退，永远走不到 :373 的调用点。这条入口服务
// Anthropic / generic 网关（gateway_upstream_response、anthropic_passthrough、
// forward_as_chat_completions/responses、count_tokens、antigravity_retry）；不补上，
// 这些平台的「同一个号反复撞 404」依旧存在。
func TestHandleUpstreamError_WritesModelCooldownDespiteCustomErrorCodeGate(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &RateLimitService{accountRepo: repo}
	account := customErrorCodeAccountExcluding(http.StatusServiceUnavailable)
	require.False(t, account.ShouldHandleErrorCode(http.StatusNotFound),
		"前置条件：自定义错误码不含 404")

	handled := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"message":"model not found: gpt-5.4"}}`),
		"gpt-5.4",
	)

	require.True(t, handled, "模型确定性不可用时必须换号，否则同一个号会被反复重选")
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, "gpt-5.4", repo.modelRateLimitCalls[0].scope)
	require.Equal(t, upstreamModelNotFoundReason, repo.modelRateLimitCalls[0].reason)
	// 旁路不得顺带把账号整体打下线（那是管理员排除的语义）。
	require.Zero(t, repo.tempCalls, "per-model 冷却不得升级为账号级处罚")
}

// 池模式早退（:349）的条件是 `IsPoolMode() && !customErrorCodesEnabled`，本用例把
// 「不修」的那一半钉住：未开自定义错误码的池模式账号仍保持其既定语义（上游错误
// 不标记本地账号状态，而是在同账号重试，见 account.go IsPoolMode 注释），不得因为
// 本修复而开始写 per-model 冷却。
func TestHandleUpstreamError_PoolModeWithoutCustomCodesStillSkipsModelCooldown(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &RateLimitService{accountRepo: repo}
	// 必须是**没配临时不可调度规则**的池模式账号：上一版本用例复用了
	// openAIModelNotFoundTempAccount()，它带的 404+"not found" 规则会在 :350 优先
	// 命中并返回 true（池模式分支持意保留管理员显式规则），测不到 :354 的早退本身。
	account := &Account{
		ID:          101,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"pool_mode": true},
	}
	require.True(t, account.IsPoolMode())
	require.False(t, account.IsCustomErrorCodesEnabled(), "前置条件：未启用自定义错误码才会进 :349 早退")

	handled := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"message":"model not found: gpt-5.4"}}`),
		"gpt-5.4",
	)

	require.False(t, handled, "纯池模式（未开自定义码）走 :349 早退，本修复不改变它的语义")
	require.Empty(t, repo.modelRateLimitCalls)
	require.Zero(t, repo.tempCalls)
}

// 另一半：池模式 + 已启用自定义错误码时，:349 不拦（既有代码就是这样设计的，
// 因为管理员显式开了精细控制），请求会继续往下走。未修复前它在 :359 被拦掉而
// 不写冷却；现在应与普通账号一致写 per-model 冷却（否则同一个号照样反复撞 404）。
func TestHandleUpstreamError_PoolModeWithCustomCodesStillWritesModelCooldown(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &RateLimitService{accountRepo: repo}
	account := customErrorCodeAccountExcluding(http.StatusServiceUnavailable)
	account.Credentials["pool_mode"] = true
	require.True(t, account.IsPoolMode())
	require.True(t, account.IsCustomErrorCodesEnabled(), "前置条件：开了自定义码才会绕过 :349")
	require.False(t, account.ShouldHandleErrorCode(http.StatusNotFound))

	handled := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"message":"model not found: gpt-5.4"}}`),
		"gpt-5.4",
	)

	require.True(t, handled, "池模式一旦开了自定义码就不再享受 :349 早退，应与普通账号一致")
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Zero(t, repo.tempCalls, "仍不得升级为账号级处罚")
}
