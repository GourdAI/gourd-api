//go:build unit

package service

// qoder_model_resource_limit_test.go 回归：Qoder 10605 的「模型级资源限流」形态
// （subCode=RESOURCE_LIMIT / serviceAvailable=false）必须走 (账号, 模型) 级冷却 +
// 不换号，而不是拥塞排队的「账号级 30s 冷却 + 换号」。
//
// 背景（2026-10-26 线上报障）：qwen3.8-flash(qfmodel) 全量请求撞
// 403 信封 + 内嵌 10605 RESOURCE_LIMIT。该形态空的是**模型**资源池，与账号无关：
// 换任何 Qoder 号发同一 modelKey 都撞同一个错（实测每请求 10 次换号全中），
// 而 30s 账号冷却到期后号又被选中再撞 → 用户视角「每次调用都报错」。

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// qoderResourceLimitInner 线上真实形态（2026-10-26 截图原文）的最内层：
// subCode/serviceAvailable/modelKey 与 isQueued 同层。
const qoderResourceLimitInner = `{"isQueued":true,"modelKey":"qfmodel","queueCount":0,` +
	`"queueType":"p3","retryAfterSeconds":30,"serviceAvailable":false,` +
	`"subCode":"RESOURCE_LIMIT","waitTime":30}`

// qoderResourceLimitBody 流内路径实际拿到的**中层**串：外层 403 信封的 body，
// 含 code=10605 与 subCode，而 modelKey/serviceAvailable 嵌在 message 字段里。
var qoderResourceLimitBody = `{"code":"10605","message":` + strconv.Quote(qoderResourceLimitInner) +
	`,"subCode":"RESOURCE_LIMIT"}`

// qoderCongestedInner 拥塞排队形态（2026-10-20 实证样本）：serviceAvailable=true、无 subCode。
const qoderCongestedInner = `{"code":"10605","message":"AIIsQueued","isQueued":true,"modelKey":"dfmodel",` +
	`"queueCount":95,"queueType":"p3","retryAfterSeconds":30,` +
	`"serviceAvailable":true,"waitTime":6}`

func TestQoderQueuedIsModelResourceLimit_Forms(t *testing.T) {
	require.True(t, qoderQueuedIsModelResourceLimit(qoderResourceLimitInner),
		"subCode=RESOURCE_LIMIT 必须判为模型级资源限流")
	require.True(t, qoderQueuedIsModelResourceLimit(qoderResourceLimitBody),
		"三层嵌套的中层串（modelKey 在内层）同样必须识别")
	require.True(t, qoderQueuedIsModelResourceLimit(
		`{"isQueued":true,"modelKey":"qmodel_latest","queueCount":18,"queueType":"slow",`+
			`"serviceAvailable":false,"waitTime":8968}`),
		"官方论坛样本：无 subCode 但 serviceAvailable=false 同属资源限流形态")
	require.False(t, qoderQueuedIsModelResourceLimit(qoderCongestedInner),
		"拥塞排队（serviceAvailable=true）不得被判为资源限流，否则丢失换号能力")
	require.False(t, qoderQueuedIsModelResourceLimit("AIIsQueued"),
		"非 JSON 文本回落拥塞口径（修复前行为）")
	require.False(t, qoderQueuedIsModelResourceLimit(""),
		"空文本回落拥塞口径")
	require.Equal(t, "qfmodel", qoderQueuedModelKey(qoderResourceLimitInner))
	require.Equal(t, "qfmodel", qoderQueuedModelKey(qoderResourceLimitBody),
		"中层串必须向下钻取内层 modelKey")
	require.Equal(t, "", qoderQueuedModelKey("not json"))
}

func TestQoderApplyBusinessError_ResourceLimitWritesModelCooldownNotAccount(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	account := &Account{ID: 3, Platform: PlatformQoder}

	class := applyQoderBusinessErrorAccountState(context.Background(), repo, account,
		"403", qoderResourceLimitBody)
	require.Equal(t, qoderErrorClassQueued, class, "信封 403 + 内嵌 10605 必须解析为排队类")

	require.Len(t, repo.modelRateLimitCalls, 1, "资源限流必须写模型级冷却")
	call := repo.modelRateLimitCalls[0]
	require.Equal(t, int64(3), call.accountID)
	require.Equal(t, "qfmodel", call.scope, "冷却键必须是上游 modelKey")
	require.Equal(t, qoderModelResourceLimitReason, call.reason)
	require.WithinDuration(t, time.Now().Add(qoderModelResourceLimitCooldown), call.resetAt, 5*time.Second)
	require.Zero(t, repo.tempCalls, "资源限流不得写账号级临时停调（号对其它模型仍健康）")
}

func TestQoderApplyBusinessError_CongestionKeepsAccountCooldown(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	account := &Account{ID: 3, Platform: PlatformQoder}

	applyQoderBusinessErrorAccountState(context.Background(), repo, account,
		"10605", qoderCongestedInner)

	require.Zero(t, repo.modelRateLimitCalls, "拥塞排队不得写模型级冷却")
	require.Equal(t, 1, repo.tempCalls, "拥塞排队保持账号级短冷却（修复前行为）")
}

func TestQoderApplyBusinessError_ResourceLimitWithoutModelKeyFallsBack(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	account := &Account{ID: 3, Platform: PlatformQoder}

	// 有 RESOURCE_LIMIT 子码但缺 modelKey：无法定位冷却键，回落账号级短冷却，
	// 宁可不精确也不把冷却写到猜出来的键上。
	applyQoderBusinessErrorAccountState(context.Background(), repo, account,
		"10605", `{"isQueued":true,"queueCount":0,"serviceAvailable":false,"subCode":"RESOURCE_LIMIT"}`)

	require.Zero(t, repo.modelRateLimitCalls)
	require.Equal(t, 1, repo.tempCalls)
}

func TestQoderFailover_ResourceLimitStopsAccountSwitching(t *testing.T) {
	env := &errQoderUpstreamEnvelope{
		StatusCode: 403,
		Code:       "403",
		Message:    qoderResourceLimitBody,
	}
	fo := newQoderStreamFailoverError(env, time.Now())
	require.NotNil(t, fo, "10605 必须仍可换号错误化（Stop 语义由 NextAccountAction 承载）")
	require.Equal(t, http.StatusTooManyRequests, fo.StatusCode)
	require.False(t, fo.ShouldRetryNextAccount(),
		"模型级资源限流换号无效，必须 Stop（否则每请求白撞 10 次）")
	require.Equal(t, "300", fo.ResponseHeaders.Get("Retry-After"),
		"Retry-After 必须与模型级冷却同源（5min），不能透上游的 30s")
}

func TestQoderFailover_CongestionStillSwitchesAccount(t *testing.T) {
	env := &errQoderUpstreamEnvelope{
		StatusCode:        200,
		Code:              "10605",
		Message:           qoderCongestedInner,
		RetryAfterSeconds: qoderIntPtrTest(30),
	}
	fo := newQoderStreamFailoverError(env, time.Now())
	require.NotNil(t, fo)
	require.True(t, fo.ShouldRetryNextAccount(), "拥塞排队保持换号（修复前行为）")
	require.Equal(t, "30", fo.ResponseHeaders.Get("Retry-After"))
}

func TestQoderModelRateLimitKeys_QoderAliasSymmetry(t *testing.T) {
	ctx := context.Background()
	account := &Account{ID: 3, Platform: PlatformQoder}
	resetAt := time.Now().Add(qoderModelResourceLimitCooldown)

	// 写侧口径：冷却写在上游 modelKey（qfmodel）上。
	setAccountModelRateLimitSnapshot(account, "qfmodel", resetAt, qoderModelResourceLimitReason, time.Now())

	require.True(t, account.isModelRateLimitedWithContext(ctx, "qfmodel"),
		"直接用官方 key 的请求必须被冷却拦下")
	require.True(t, account.isModelRateLimitedWithContext(ctx, "qwen3.8-flash"),
		"展示名入站必须经平台别名命中同一冷却（读写键对称）")
	require.False(t, account.isModelRateLimitedWithContext(ctx, "glm-5.3"),
		"冷却只作用于该模型，同账号其它模型仍可调度")
}
