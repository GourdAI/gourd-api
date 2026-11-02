//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// workbuddy_11140_test.go —— 11140「一码两义」分流与烧号放大防护。
//
// fixture 来自 2026-11-01 用户提供的**真实上游响应体**（WorkBuddy 国内站，App 5.9.2，
// HTTP 403 / code 11140）。这是本仓第一条落地的 chat 域 11140 一手样本，符合
// workbuddy_upstream_error.go 文件头「码表纪律」：加码的唯一合法途径是真实样本。
//
// 关键陷阱（三处，都容易读错）：
//  1. 顶层 "message":"Internal error" **是存在的** ⇒ extractUpstreamErrorMessage 不为空，
//     而是一句零信息量占位文案。旧实现据此写进 SetError 的理由就是
//     "Access forbidden (403): Internal error" —— 管理员看不出这是封号，更看不出能申诉。
//     分流必须读 data.displayMsg，而该路径 extractUpstreamErrorMessage 完全不覆盖。
//  2. code 11140 同时用于「账号被限制」与「内容未通过安全审核」，只有 displayMsg 能分开。
//  3. 11128（渠道闸）与 11140 的 details 里都写 request illegal，不能只看 code。

const workbuddySample11140AccountRestricted = `{"code":-32603,"message":"Internal error","data":{"statusCode":403,` +
	`"displayMsg":{"en":"Your account has been restricted due to unusual activity",` +
	`"zh":"账号存在异常行为，已被限制使用","zh-hant":"帳號存在異常行為，已被限制使用"},` +
	`"displayTips":{"en":"We detected unusual activity on this account. Submit feedback to appeal this restriction",` +
	`"zh":"系统检测到该账号存在异常使用行为，如需申诉请提交反馈"},` +
	`"retry_policy":{"retry":false},"actions":["EDIT_INPUT","SUBMIT_FEEDBACK"],` +
	`"details":"403 request illegal (01a1951a42fa78d8a2ced13ae94075f5/6daa6d11-742f-4bcf-9565-7f157cdcdc4e)",` +
	`"code":11140,"category":"internal"}}`

func workbuddySample11140ContentModeration(zh bool) string {
	if zh {
		return `{"code":-32603,"message":"Internal error","data":{"statusCode":403,` +
			`"displayMsg":{"zh":"内容未通过安全审核","en":"Your content failed the safety review"},` +
			`"code":11140,"category":"internal"}}`
	}
	return `{"code":-32603,"message":"Internal error","data":{"statusCode":403,` +
		`"displayMsg":{"en":"This content violates our usage policy"},` +
		`"code":11140,"category":"internal"}}`
}

func TestClassifyWorkbuddyForbiddenBody(t *testing.T) {
	t.Parallel()

	// 真实样本 ⇒ 账号限制。
	require.Equal(t, workbuddyForbiddenAccountRestricted,
		classifyWorkbuddyForbiddenBody([]byte(workbuddySample11140AccountRestricted)))

	// 同 code、不同文案 ⇒ 内容审核（中/英两种形态）。
	require.Equal(t, workbuddyForbiddenContentModeration,
		classifyWorkbuddyForbiddenBody([]byte(workbuddySample11140ContentModeration(true))))
	require.Equal(t, workbuddyForbiddenContentModeration,
		classifyWorkbuddyForbiddenBody([]byte(workbuddySample11140ContentModeration(false))))

	// 繁中文案单独命中（displayMsg 只有 zh-hant 时也要认）。
	// 【fixture 一律用 Go 双引号 + \u 转义】本轮两次踩坑后的结论：
	//   - 工具链写 CJK 会产生肉眼不可见的码点漂移（同一串繁体字经不同写入通道得到不同码点）；
	//   - gjson **不解码** JSON 里的 \uXXXX（实测拿到 54 字符字面串），反引号原始字符串也不解码；
	//   - 只有 Go **双引号**字符串会由编译器把 \u 转义解成真实码点，然手传给 gjson。
	// 内容："內容未通過安全審查"（审查用繁体「審查」，故意走最易漂移的那条）。
	require.Equal(t, workbuddyForbiddenContentModeration,
		classifyWorkbuddyForbiddenBody([]byte("{\"data\":{\"displayMsg\":{\"zh-hant\":\"\u5167\u5bb9\u672a\u901a\u904e\u5b89\u5168\u5be9\u67e5\"},\"code\":11140}}")))

	// 非 11140 ⇒ 不分流（11128 是渠道闸、形态不同，不能混为一谈）。
	require.Equal(t, workbuddyForbiddenUnknown,
		classifyWorkbuddyForbiddenBody([]byte(`{"data":{"displayMsg":{"zh":"账号存在异常行为"},"code":11128}}}`)))

	// 11140 但无可读文案 ⇒ 不猜。
	require.Equal(t, workbuddyForbiddenUnknown,
		classifyWorkbuddyForbiddenBody([]byte(`{"data":{"code":11140}}`)))
	// 顶层 code 形态也认（信封字段位置不唯一，实测两处都出现过）。
	require.Equal(t, workbuddyForbiddenAccountRestricted,
		classifyWorkbuddyForbiddenBody([]byte(`{"code":11140,"data":{"displayMsg":{"zh":"账号存在异常行为，已被限制使用"}}}`)))

	// 非 JSON / 空 ⇒ unknown，不 panic。
	require.Equal(t, workbuddyForbiddenUnknown, classifyWorkbuddyForbiddenBody(nil))
	require.Equal(t, workbuddyForbiddenUnknown, classifyWorkbuddyForbiddenBody([]byte(`not json`)))

	// 提取展示文案：优先中文。
	require.Equal(t, "账号存在异常行为，已被限制使用",
		workbuddyForbiddenDisplayMessage([]byte(workbuddySample11140AccountRestricted)))
	require.Equal(t, "", workbuddyForbiddenDisplayMessage(nil))
}

// newWorkbuddy403Service 组装一个只带 repo stub 的 RateLimitService。
// 刻意**不**注入 openAI403CounterCache：WorkBuddy 分支不依赖 Redis 计数器，
// 避免「无 Redis 部署 ⇒ 降级为立即永久禁用」这条既有拦路点。
func newWorkbuddy403Service(repo *rateLimitAccountRepoStub) (*RateLimitService, *Account) {
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	blocker := &runtimeBlockRecorder{}
	svc.SetAccountRuntimeBlocker(blocker)
	return svc, &Account{ID: 901, Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey}
}

func TestWorkbuddy403AccountRestrictedMarksAccountAndAdvisesAppeal(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	svc, account := newWorkbuddy403Service(repo)

	shouldDisable := svc.HandleUpstreamError(context.Background(), account,
		http.StatusForbidden, http.Header{}, []byte(workbuddySample11140AccountRestricted))

	require.True(t, shouldDisable, "账号被限制 = 该号确实不能继续服务，仍要停调")
	require.Equal(t, 1, repo.setErrorCalls, "账号级限制是事实记录，SetError 保留")
	require.Contains(t, repo.lastErrorMsg, "账号存在异常行为")
	// 管理员必须看得见「这是封号、要去申诉、改代码没用」——旧文案只有一句 Internal error。
	require.Contains(t, repo.lastErrorMsg, "appeal", "处置文案要给出申诉渠道")
	require.Contains(t, repo.lastErrorMsg, "11140")
	require.Equal(t, 0, repo.tempCalls, "限制类不走临时冷却")
}

func TestWorkbuddy403ContentModerationNeverTouchesAccountState(t *testing.T) {
	for _, body := range []string{workbuddySample11140ContentModeration(true), workbuddySample11140ContentModeration(false)} {
		repo := &rateLimitAccountRepoStub{}
		svc, account := newWorkbuddy403Service(repo)

		shouldDisable := svc.HandleUpstreamError(context.Background(), account,
			http.StatusForbidden, http.Header{}, []byte(body))

		require.False(t, shouldDisable, "内容被拒不是账号问题")
		require.Equal(t, 0, repo.setErrorCalls, "内容审核绝不得永久禁用账号")
		require.Equal(t, 0, repo.tempCalls, "内容审核也不得给账号记冷却")
	}
}

func TestWorkbuddy403UnclassifiedCoolsDownInsteadOfPermanentDisable(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	svc, account := newWorkbuddy403Service(repo)

	// 11140 但文案读不到 ⇒ 未知：临时冷却，不永久下线。
	svc.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{},
		[]byte(`{"data":{"code":11140,"category":"internal"}}`))

	require.Equal(t, 0, repo.setErrorCalls, "读不到证据不得把请求级问题当账号级结论")
	require.Equal(t, 1, repo.tempCalls)
	require.Contains(t, repo.lastTempReason, "unclassified")
}

// TestWorkbuddy403ContentModerationNotReplayedAcrossAccounts 第二道闸：内容审核型必须
// 被移出 failover 状态集。否则同一个坏 body 会在池内逐个账号重放（上限 maxAccountSwitches），
// 每个号各撞一次 11140 —— 这正是「批量异常流量」的形态，也是最可疑的连带风控诱因。
func TestWorkbuddy403ContentModerationNotReplayedAcrossAccounts(t *testing.T) {
	t.Parallel()
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 1, Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey}

	require.False(t,
		svc.shouldFailoverOpenAIUpstreamResponse(account, http.StatusForbidden, "Internal error",
			[]byte(workbuddySample11140ContentModeration(true))),
		"内容审核型 403 不得换号重放")

	// 反向：账号限制型仍应换号（该号已不可用，下一个号可能正常）。
	require.True(t,
		svc.shouldFailoverOpenAIUpstreamResponse(account, http.StatusForbidden, "Internal error",
			[]byte(workbuddySample11140AccountRestricted)))

	// 反向：非 WorkBuddy 平台不受本闸影响（口径不得外溢）。
	require.True(t,
		svc.shouldFailoverOpenAIUpstreamResponse(&Account{ID: 2, Platform: PlatformOpenAI},
			http.StatusForbidden, "x", []byte(workbuddySample11140ContentModeration(true))))
}

// TestWorkbuddy403UnrelatedBodyKeepsLegacyFailover 非 11140 的 WorkBuddy 403 不得被误拦：
// 本闸只认 code 11140 + 可读 displayMsg，其余一律保持原有 failover 行为。
func TestWorkbuddy403UnrelatedBodyKeepsLegacyFailover(t *testing.T) {
	t.Parallel()
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 3, Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey}

	require.True(t, svc.shouldFailoverOpenAIUpstreamResponse(account, http.StatusForbidden,
		"quota exceeded", []byte(`{"error":{"message":"quota exceeded"}}`)))
}
