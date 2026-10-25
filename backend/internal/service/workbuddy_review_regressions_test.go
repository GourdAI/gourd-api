//go:build unit

package service

// workbuddy_review_regressions_test.go 本机 code review 自查发现并修复的四处缺陷的回归护栏。
//
// 四个缺陷连带影响同一条自愈链，任一残留都会让「WorkBuddy 全部 401」以变体复发：
//
//	C 401 判据与凭据读取口径不一致（camelCase 别名账号仍被永久下线）
//	D refresh 响应缺 expiresIn 时保留**旧 token** 的过期时刻（刚刷完即被认为不需刷）
//	A 非流式聚合路径缺「讲完了」判定（断流/带内错误被粉饰成 HTTP 200 + finish_reason=stop）
//
// 每个修复都配了反向护栏：把修复按注释里的写法破坏掉，对应用例必须 FAIL。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ── C：401 分流判据必须与 GetWorkbuddyCredentials 同口径 ───────────────────
//
// camelCase 是 WorkBuddy 的**合法**凭据形态（直接粘贴 workbuddy2api 的 auth 文件字段，
// 见 workbuddy.go 的 read 函数）。修复前 isWorkbuddyRefreshableCredential 直接读
// GetCredential("refresh_token")，探针实测：
//
//	camelCase → GetWorkbuddyCredentials().RefreshToken="rt" 但 predicate=false
//	snake     → predicate=true
//
// 即这类账号撞 401 仍走永久 SetError，与本轮原始故障完全同形。
func TestWorkbuddy401PredicateHonoursCamelCaseCredentials(t *testing.T) {
	t.Parallel()
	camel := &Account{
		ID: 9911, Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"accessToken": "at", "refreshToken": "rt"},
	}
	require.NotEmpty(t, camel.GetWorkbuddyCredentials().RefreshToken,
		"夹具前提：读取侧本就能认出 camelCase 的 refreshToken")
	require.True(t, isWorkbuddyRefreshableCredential(camel),
		"camelCase 持 refresh_token 的 WorkBuddy 号必须走「临时停调」而非永久下线")
	// 与后台刷新器同口径：两处判据不得漂移，否则「能被后台刷新」的号却「401 后死亡」。
	require.True(t, NewWorkBuddyTokenRefresher(nil, nil, nil).CanRefresh(camel),
		"401 判据必须与后台刷新器的 CanRefresh 一致")
}

// 反向护栏：既无 camelCase 也无 snake_case 的 refresh_token 时仍应永久禁用
// （那一支是正确的，不能为了修 camelCase 而放宽成「所有 WorkBuddy 号都不下线」）。
func TestWorkbuddy401PredicateStillRejectsWithoutRefreshToken(t *testing.T) {
	t.Parallel()
	noRT := &Account{
		ID: 9912, Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "at"},
	}
	require.False(t, isWorkbuddyRefreshableCredential(noRT), "无 refresh_token 确实不可自愈")
}

// 端到端：camelCase 账号撞 401 后不得触发永久 SetError。
func TestWorkbuddy401CamelCaseCoolsDownInsteadOfPermanentDisable(t *testing.T) {
	t.Parallel()
	repo := &workbuddy401Repo{}
	svc := newWorkbuddy401TestService(repo)
	account := &Account{
		ID: 9913, Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey,
		Status: "active", Schedulable: true,
		Credentials: map[string]any{"accessToken": "at", "refreshToken": "rt"},
	}

	disabled := svc.HandleUpstreamError(context.Background(), account,
		http.StatusUnauthorized, http.Header{},
		[]byte(`<html><head><title>401 Authorization Required</title></head></html>`), "glm-5.3")

	require.Zero(t, repo.setErrorCalls, "camelCase 账号同样不得永久下线（修复前此处=1）")
	require.Equal(t, 1, repo.tempCalls, "应走临时停调给自愈留窗口")
	require.True(t, disabled, "本请求仍视为失败（需要换号重试）")
}

// ── D：refresh 写回必须用新 token 的过期时刻 ────────────────────────────────

// mustReadServiceSource 读取本包内某个源文件的完整内容（用于源码级不变量断言）。
//
// 为何用源码断言而不是行为断言：三处刷新实现各自持有独立的 HTTPUpstream 与
// validate 分支（网关 / 积分签到 / 账号测试），在同一冯具里同时驱动会失真。
// 这里只钉「写回逻辑必须带 JWT exp 兜底」这一条不变量，防止某处静默回退。
func mustReadServiceSource(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	require.NoError(t, err, "读取源文件 %s 失败（测试工作目录应为 internal/service）", name)
	return string(raw)
}

// refreshWorkbuddyTokenExpiryFallbackServer 返回一个只处理 refresh 端点的假上游，
// 响应体刻意不带 expiresIn（真实上游可能省略）。
func refreshWorkbuddyTokenExpiryFallbackServer(t *testing.T, newToken string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != workbuddyRefreshPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"accessToken":"`+newToken+`"}}`)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestWorkbuddyRefreshWritesNewTokenExpiryFromJWT 请求路径刷新：响应缺 expiresIn 时，
// 必须用**新 token** 的 exp 兜底，而不是把旧 token 的 expires_at 原样留下。
//
// 修复前探针实测：旧值 +40 天 / 新 token 真实 exp +2h → storedExpiresAt=旧值，
// 于是 workbuddyTokenNeedsRefresh 返回 false ——「刚刷完又认为不需刷」，
// 而新 token 在两小时后就会过期，届时正是下一轮全量 401 的起点。
func TestWorkbuddyRefreshWritesNewTokenExpiryFromJWT(t *testing.T) {
	t.Parallel()
	newExp := time.Now().Add(2 * time.Hour).Unix()
	newToken := workbuddyTestJWT(t, newExp, "uid-9914")
	server := refreshWorkbuddyTokenExpiryFallbackServer(t, newToken)

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := &Account{
		ID: 9914, Platform: PlatformWorkbuddy,
		Credentials: map[string]any{
			"base_url":      server.URL,
			"access_token":  "tok-old",
			"refresh_token": "rt-old",
			"expires_at":    time.Now().Add(40 * 24 * time.Hour).Unix(),
		},
	}

	require.NoError(t, svc.refreshWorkbuddyToken(context.Background(), account))
	// 【必须断言**写入值**而不是读取值】读取路径（GetWorkbuddyCredentials）自带 JWT exp 兜底，
	// 会把写入侧的缺陷掩盖掉：写入侧若保留旧 token 的 +40 天，读取侧看到正数就不兜底，
	// 仍然返回错误值；而若写入侧删掉该键，读取侧又恰好能兜底出正确值。
	// 所以断言的对象必须是落库的 map 本身。
	stored, ok := account.Credentials["expires_at"]
	require.True(t, ok, "刷新后必须写入 expires_at（不能只依赖读取侧兜底）")
	require.EqualValues(t, newExp, stored,
		"必须写成新 token 的 exp；保留旧 token 的过期时刻会让预刷新判据整段算错")
	// 读取侧口径也必须与写入一致（防止两处各自算一遍而漂移）。
	require.EqualValues(t, newExp, account.GetWorkbuddyCredentials().ExpiresAt)
}

// 反向护栏：expiresIn 存在时它仍是权威值，不得被 JWT 推导覆盖。
func TestWorkbuddyRefreshPrefersUpstreamExpiresIn(t *testing.T) {
	t.Parallel()
	// JWT 的 exp 刻意给得很远（+30 天），而 expiresIn 只说 3600 秒：应取 3600 秒那个。
	newToken := workbuddyTestJWT(t, time.Now().Add(30*24*time.Hour).Unix(), "uid-9915")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != workbuddyRefreshPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"accessToken":"`+newToken+`","expiresIn":3600}}`)
	}))
	t.Cleanup(server.Close)

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := &Account{
		ID: 9915, Platform: PlatformWorkbuddy,
		Credentials: map[string]any{
			"base_url":      server.URL,
			"access_token":  "tok-old",
			"refresh_token": "rt-old",
		},
	}
	require.NoError(t, svc.refreshWorkbuddyToken(context.Background(), account))
	expiresAt := account.GetWorkbuddyCredentials().ExpiresAt
	require.InDelta(t, time.Now().Add(time.Hour).Unix(), expiresAt, 60,
		"expiresIn 是上游明文权威值，必须优先于 JWT 推导")
}

// 三处刷新实现（网关 / 积分签到 / 账号测试）各有一份写回逻辑，必须同口径。
// 这里只校验「都不再无条件保留旧 expires_at」这条不变量：用源码级断言防止有人
// 在某一处回退成旧写法（三份实现无法在同一夹具里同时驱动，硬造会失真）。
func TestWorkbuddyRefreshWritersAllCarryJWTExpiryFallback(t *testing.T) {
	t.Parallel()
	for _, file := range []string{
		"workbuddy_client.go",
		"workbuddy_credits_service.go",
		"workbuddy_gateway.go",
	} {
		src := mustReadServiceSource(t, file)
		require.Contains(t, src, `else if exp := workbuddyJWTExpiry(env.Data.AccessToken); exp > 0 {`,
			"%s 的 refresh 写回必须带 JWT exp 兜底（与刷新器/其它刷新路径同源）", file)
		require.Contains(t, src, `delete(next, "expires_at")`,
			"%s 在无法得知新 token 过期时刻时必须清掉旧值（旧值描述的是旧 token）", file)
	}
}

// ── A：非流式聚合路径不得把断流/带内错误粉饰成成功 ─────────────────────────

// TestAggregateWorkbuddySSENoTerminalEvidenceIsTruncation 上游只给正文、就 EOF：
// 修复前 finishReason 的默认值 "stop" 会合成一个看似完整的 chat.completion。
func TestAggregateWorkbuddySSENoTerminalEvidenceIsTruncation(t *testing.T) {
	t.Parallel()
	input := `data: {"id":"x","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n"
	out, _, err := aggregateWorkbuddySSE(strings.NewReader(input))
	require.ErrorIs(t, err, errWorkbuddyStreamTruncated,
		"无 [DONE] / 无 finish_reason / 无 usage = 中途断流，不得聚合成完整响应")
	require.Nil(t, out, "修复前这里会给出 finish_reason=stop 的假完整响应")
	require.False(t, workbuddyIsEmptyStreamError(err), "截断不是空流，排障口径必须可区分")
}

// 反向护栏：三种完成证据任一存在都属正常收尾，不得误判成截断。
func TestAggregateWorkbuddySSETerminalEvidenceIsSuccess(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"upstream done": `data: {"id":"x","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
			`data: [DONE]` + "\n\n",
		"finish_reason": `data: {"id":"x","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n",
		"usage trailer": `data: {"id":"x","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
			`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}` + "\n\n",
	}
	for name, input := range cases {
		out, _, err := aggregateWorkbuddySSE(strings.NewReader(input))
		require.NoError(t, err, "%s 是完成证据，不得判为截断", name)
		require.Contains(t, string(out), "chat.completion")
	}
}

// 端到端：非流式请求遇到断流上游时必须整个请求失败，而不是回一份 HTTP 200 的
// 「成功」响应（那正是运营面板把失败显示成成功的数据来源）。
func TestWorkbuddyNonStreamTruncationFailsRequest(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 只有一帧正文，无 finish_reason / 无 usage / 无 [DONE]
		_, _ = io.WriteString(w, `data: {"id":"wb-1","choices":[{"index":0,"delta":{"content":"partial answer"},"finish_reason":null}]}`+"\n\n")
	}))
	t.Cleanup(server.Close)

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &qoderStateRecordingRepo{})
	account := &Account{
		ID: 9916, Platform: PlatformWorkbuddy,
		Credentials: map[string]any{"base_url": server.URL, "access_token": "wb-token"},
	}
	c := newWorkbuddyTestGinContext()
	body := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"ping"}],"stream":false}`)

	resp, err := svc.sendWorkbuddyUpstreamRequest(context.Background(), c, account, body, false)
	require.ErrorIs(t, err, errWorkbuddyStreamTruncated,
		"断流必须让整个非流式请求失败（修复前返回 HTTP 200 + finish_reason=stop）")
	require.Nil(t, resp)
}

// TestWorkbuddyNonStreamInBandErrorCountsAsSLAFailure 非流式路径里，上游 200 +
// 只回错误帧（且带了 [DONE]）的形态：响应体按既有协议仍是空 content 的 200，
// 但**必须计入 SLA 失败**。
//
// 本机探针实测（修复前）：status=200 object=chat.completion content="" opsMarked(SLA)=false
// —— 请求被记成成功，而客户端拿到的是空回复。流式路径由交付观察器补了这一道，
// 非流式没有交付点，所以漏了。
func TestWorkbuddyNonStreamInBandErrorCountsAsSLAFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"error":{"message":"rate limited","code":"6004"}}`+"\n\n"+"data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &qoderStateRecordingRepo{})
	account := &Account{
		ID: 9918, Platform: PlatformWorkbuddy,
		Credentials: map[string]any{"base_url": server.URL, "access_token": "wb-token"},
	}
	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendWorkbuddyUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"ping"}],"stream":false}`), false)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode, "响应形态不变：仍为 200（改协议属单独决策）")

	streamErr, marked := GetOpsStreamError(c)
	require.True(t, marked, "上游 200 + 错误帧必须计入 SLA 失败（修复前为 false）")
	require.True(t, streamErr.CountTowardsSLA)
	require.Equal(t, "6004", streamErr.Code)
}

// 反向护栏：非流式正常响应不得被司上失败。
func TestWorkbuddyNonStreamNormalResponseNotMarkedFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, workbuddyTestSSEBody)
	}))
	t.Cleanup(server.Close)

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := &Account{
		ID: 9919, Platform: PlatformWorkbuddy,
		Credentials: map[string]any{"base_url": server.URL, "access_token": "wb-token"},
	}
	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendWorkbuddyUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"glm-5.3"}`), false)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, marked := GetOpsStreamError(c)
	require.False(t, marked, "正常响应不得虚抬错误率")
}

// 反向护栏：非流式正常流不得被误判。
func TestWorkbuddyNonStreamNormalStreamSucceeds(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, workbuddyTestSSEBody)
	}))
	t.Cleanup(server.Close)

	upstream := &workbuddyTestUpstream{client: server.Client()}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := &Account{
		ID: 9917, Platform: PlatformWorkbuddy,
		Credentials: map[string]any{"base_url": server.URL, "access_token": "wb-token"},
	}
	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendWorkbuddyUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"glm-5.3"}`), false)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(resp.Body)
	require.NoError(t, readErr)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(raw), "chat.completion")
}
