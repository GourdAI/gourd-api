//go:build unit

package service

// trae_stream_failover_test.go 回归：Trae「HTTP 200 + 流内 event:error」在**首帧即错**
// （积分耗尽 1005 的主要形态）时必须换号，而不是把上游原文透给客户端。
//
// 修复前的失效链（排查 2026-10-19 记录，别再引用 trae_sse.go 旧注释当依据）：
// 流式路径只注册落状态回调、不返回 UpstreamFailoverError；错误帧一经写出就把
// raw 路径的 clientOutputStarted 置真（缓冲闸门把 sawError 列为放行条件），而那里
// 的换号分支硬要求「未写出任何字节」⇒ 本请求必然透错且永不换号，只有下一个请求
// 才被 SetTempUnschedulable 绕开。现在由 traeProbeFirstFrame 在写出任何字节之前
// 抢占这个窗口。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// traeFailoverTestServer 起一个按原样回吐 SSE 文本的上游（HTTP 恒 200，错误藏在流内）。
func traeFailoverTestServer(t *testing.T, sseBody string) (*httptest.Server, *OpenAIGatewayService, *traeErrRecordingRepo) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody)
	}))
	t.Cleanup(server.Close)

	repo := &traeErrRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	return server, svc, repo
}

func traeFailoverTestCCBody(stream bool) []byte {
	if stream {
		return []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"ping"}],"stream":true}`)
	}
	return []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"ping"}],"stream":false}`)
}

// ── 核心正向：首帧即 1005 → 必须返回可换号的 failover 错误 ──────────────────

func TestTraeStreamCreditsExhaustedReturnsFailover(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stream     bool
		code       string
		wantStatus int
	}{
		{"streaming", true, "1005", http.StatusTooManyRequests},
		{"non_streaming", false, "1005", http.StatusTooManyRequests},
		{"session_expired_streaming", true, "1001", http.StatusUnauthorized},
		{"restricted_streaming", true, "4015", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, svc, repo := traeFailoverTestServer(t, sseStream(
				sseEvent("error", `{"code":`+tc.code+`,"message":"credits exhausted"}`),
			))
			account := traeCreditsAccount(9101, server.URL, nil)
			c := newWorkbuddyTestGinContext()

			resp, err := svc.sendTraeUpstreamRequest(context.Background(), c, account,
				traeFailoverTestCCBody(tc.stream), tc.stream)

			// 1) 必须是 failover 错误，否则 handler 的换号循环不会 continue。
			require.Nil(t, resp, "短路后不得把坏死流交给下游")
			var foErr *UpstreamFailoverError
			require.ErrorAs(t, err, &foErr, "首帧即业务错误必须包成 UpstreamFailoverError（否则本请求不换号）")
			require.True(t, foErr.ShouldRetryNextAccount(), "必须允许切换到下一个账号")
			require.False(t, foErr.RetryableOnSameAccount,
				"1005/1001 对这张号是确定态，同账号重试只会多花一次上游往返")
			require.Equal(t, tc.wantStatus, foErr.StatusCode)

			// 2) 客户端一个字节都没收到 —— 这是「还能透明换号」的硬前提。
			// 注意 gin 在未写出时 Size() 返回 -1（不是 0），因此用 Written() 作为判据。
			require.False(t, c.Writer.Written(), "换号前不得向客户端写出任何字节")

			// 3) 账号状态照旧落库（换号与拉黑是两件事，不能因换号而漏掉落状态）。
			tempCalls, until, reason, errorCalls, _ := repo.snapshot()
			switch tc.code {
			case "1005":
				require.Equal(t, 1, tempCalls, "额度类必须落临时不可调度")
				require.True(t, until.After(time.Now()))
				require.Contains(t, reason, "1005")
			default:
				require.Equal(t, 1, errorCalls, "会话/风控类必须置错误态")
			}

			// 4) 错误体保留业务码，供错误透传规则匹配与排障。
			require.Contains(t, string(foErr.ResponseBody), tc.code)
			require.Contains(t, string(foErr.ResponseBody), "credits exhausted")
		})
	}
}

// Retry-After 只在额度类给出（会话/风控类靠人工处理，给等待时长会误导客户端反复重试）。
func TestTraeStreamFailoverRetryAfterOnlyForQuota(t *testing.T) {
	cases := []struct {
		code      string
		wantValue bool
	}{
		{"1005", true},
		{"1001", false},
		{"4015", false},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			server, svc, _ := traeFailoverTestServer(t, sseStream(
				sseEvent("error", `{"code":`+tc.code+`,"message":"nope"}`),
			))
			account := traeCreditsAccount(9102, server.URL, nil)
			_, err := svc.sendTraeUpstreamRequest(context.Background(), newWorkbuddyTestGinContext(),
				account, traeFailoverTestCCBody(true), true)

			var foErr *UpstreamFailoverError
			require.ErrorAs(t, err, &foErr)
			value := strings.TrimSpace(foErr.ResponseHeaders.Get("Retry-After"))
			if !tc.wantValue {
				require.Empty(t, value, "非额度类不得给出等待时长")
				return
			}
			seconds, parseErr := strconv.Atoi(value)
			require.NoError(t, parseErr, "额度类必须给出可解析的 Retry-After")
			require.Greater(t, seconds, 0)
			// 与 traeQuotaRecoveryUntil（次日 0 点）同口径，绝不可能超过一天；
			// 超过 handler 的 isSafeRetryAfter 上限会被整条丢弃。
			require.LessOrEqual(t, seconds, 24*3600)
		})
	}
}

// ── 反向护栏 1：不该换号的一律维持「错误帧照常透给客户端」 ──────────────────

// 4001 一类字典外码：所有 Trae 号共用同一出站 function，换号只会把同一个错再打一遍。
// 必须既不换号、也不吞掉上游原文（管理员/调用方还得靠它定位）。
func TestTraeStreamUnknownCodeStillEmitsErrorFrame(t *testing.T) {
	server, svc, repo := traeFailoverTestServer(t, sseStream(
		sseEvent("error", `{"code":4001,"message":"We're sorry, the param is invalid."}`),
	))
	account := traeCreditsAccount(9103, server.URL, nil)
	c := newWorkbuddyTestGinContext()

	resp, err := svc.sendTraeUpstreamRequest(context.Background(), c, account,
		traeFailoverTestCCBody(true), true)
	require.NoError(t, err, "字典外错误码不得包成换号错误")
	require.NotNil(t, resp)
	defer func() { _ = resp.Body.Close() }()

	out, readErr := io.ReadAll(resp.Body)
	require.NoError(t, readErr)
	require.Contains(t, string(out), "param is invalid", "错误帧必须原样下发")
	require.Contains(t, string(out), "4001")
	require.NotContains(t, string(out), "[DONE]", "错误流不得伪装成正常结束")

	// 4001 归 unknown：只留 Ops 痕迹，不落账号状态（避免把请求级问题误标成账号坏了）。
	tempCalls, _, _, errorCalls, _ := repo.snapshot()
	require.Zero(t, tempCalls)
	require.Zero(t, errorCalls)
}

// 已有正文后才出错：语义字节已产出，重放会造成重复输出，绝不能换号。
func TestTraeStreamMidStreamErrorIsNotFailovered(t *testing.T) {
	server, svc, repo := traeFailoverTestServer(t, sseStream(
		sseEvent("output", `{"response":"partial answer"}`),
		sseEvent("error", `{"code":1005,"message":"credits exhausted"}`),
	))
	account := traeCreditsAccount(9104, server.URL, nil)
	c := newWorkbuddyTestGinContext()

	resp, err := svc.sendTraeUpstreamRequest(context.Background(), c, account,
		traeFailoverTestCCBody(true), true)
	require.NoError(t, err, "已产出正文后不得换号（会重复输出）")
	require.NotNil(t, resp)
	defer func() { _ = resp.Body.Close() }()

	out, readErr := io.ReadAll(resp.Body)
	require.NoError(t, readErr)
	require.Contains(t, string(out), "partial answer", "已产出的正文一字节都不能丢")
	require.Contains(t, string(out), "1005")

	// 落状态仍然要发生，否则该号下一轮继续接单。
	tempCalls, _, _, _, _ := repo.snapshot()
	require.Equal(t, 1, tempCalls)
}

// ── 反向护栏 2：预读首帧不得吃掉任何正常字节 ────────────────────────────────

func TestTraeStreamProbePreservesNormalFrames(t *testing.T) {
	server, svc, _ := traeFailoverTestServer(t, sseStream(
		sseEvent("output", `{"response":"hi"}`),
		sseEvent("output", `{"response":" there"}`),
		sseEvent("done", `{"finish_reason":"stop"}`),
	))
	account := traeCreditsAccount(9105, server.URL, nil)

	for _, stream := range []bool{true, false} {
		name := "streaming"
		if !stream {
			name = "non_streaming"
		}
		t.Run(name, func(t *testing.T) {
			c := newWorkbuddyTestGinContext()
			resp, err := svc.sendTraeUpstreamRequest(context.Background(), c, account,
				traeFailoverTestCCBody(stream), stream)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()

			out, readErr := io.ReadAll(resp.Body)
			require.NoError(t, readErr)
			require.Contains(t, string(out), "hi", "预读的首帧必须留在缓冲区里可被下游消费")
			require.Contains(t, string(out), " there")
			if stream {
				require.Equal(t, 1, strings.Count(string(out), "[DONE]"), "不得重复补 [DONE]")
			}
		})
	}
}

// ── 判定单元：可换号集合严格等于「账号级失败」 ──────────────────────────────

func TestTraeFailoverableBusinessError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code string
		want bool
	}{
		{"1005", true}, // 额度：这张号用完，别的号还有
		{"1001", true}, // 会话：这张号的票坏了
		{"401", true},
		{"4010", true}, // 风控：这张号被限
		{"4015", true},
		{"4001", false}, // 请求级：换号必然复现
		{"9028", false},
		{"", false},
		{"0", false},
	}
	for _, tc := range cases {
		err := &traeStreamError{Code: tc.code, Message: "m"}
		require.Equal(t, tc.want, traeFailoverableBusinessError(err), "code=%q", tc.code)
	}
	// 非 Trae 错误对象不得被误判为可换号。
	require.False(t, traeFailoverableBusinessError(io.ErrUnexpectedEOF))
	require.False(t, traeFailoverableBusinessError(nil))
}

// probe 的幂等守卫：第二次调用不得重复读流（否则会把已交出的帧当新数据再读一遍）。
func TestTraeProbeFirstFrameIsIdempotent(t *testing.T) {
	t.Parallel()
	reader := traeNewSSEReader(strings.NewReader(sseStream(
		sseEvent("error", `{"code":1005,"message":"credits exhausted"}`),
	)), traeStreamMeta{})
	require.Error(t, reader.traeProbeFirstFrame())
	require.NoError(t, reader.traeProbeFirstFrame(), "已短路后再 probe 必须直接放行（幂等）")

	// 【不丢字节】短路时错误帧只是「没写出」，仍在 reader 缓冲区里：调用方一旦没能
	// 把它包成换号错误（判据分叉的意外），错误帧会照常下发而不是退化成空回复。
	out := readAllTrae(t, reader)
	require.Contains(t, out, "credits exhausted", "短路不得吞掉错误帧本体")
	require.NotContains(t, out, "[DONE]")

	reader = traeNewSSEReader(strings.NewReader(sseStream(
		sseEvent("output", `{"response":"ok"}`),
	)), traeStreamMeta{})
	require.NoError(t, reader.traeProbeFirstFrame())
	require.NoError(t, reader.traeProbeFirstFrame())
	out = readAllTrae(t, reader)
	require.Contains(t, out, "ok")
}
