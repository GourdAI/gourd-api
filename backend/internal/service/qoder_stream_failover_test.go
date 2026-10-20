//go:build unit

package service

// qoder_stream_failover_test.go 回归：Qoder「HTTP 200 + 流内业务错误帧」在**首帧即错**
// （今日额度已用尽 110 的主要形态）时必须换号，而不是把上游原文透给客户端。
//
// 修复前的失效链（与 Trae 同构，2026-10-20 排查）：流式路径只注册落状态回调、
// 不返回 UpstreamFailoverError；错误帧一经写出就把 raw 路径的 clientOutputStarted
// 置真，而那里的换号分支硬要求「未写出任何字节」⇒ 本请求必然透错且永不换号，只有
// 下一个请求才被 SetTempUnschedulable 绕开（观察现象即「有时报错、有时正常」）。
// 现在由 qoderProbeFirstFrame 在写出任何字节之前抢占这个窗口。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// qoderFailoverTestCCBody 最小出站 CC body。
func qoderFailoverTestCCBody(stream bool) []byte {
	if stream {
		return []byte(`{"model":"auto","messages":[{"role":"user","content":"ping"}],"stream":true}`)
	}
	return []byte(`{"model":"auto","messages":[{"role":"user","content":"ping"}],"stream":false}`)
}

// qoderRawUpstream 起一个把 sseBody 原样回吐的假 Qoder 上游（HTTP 恒 200）。
func qoderRawUpstream(t *testing.T, sseBody string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "agent_chat_generation") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sseBody)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	return server
}

// qoderEnvelopeFrame 组装一帧 Qoder 信封（statusCodeValue 默认 200，内层 body 为字符串）。
func qoderEnvelopeFrame(t *testing.T, inner string, statusCodeValue int) string {
	t.Helper()
	env := map[string]any{"headers": map[string]any{}, "body": inner}
	if statusCodeValue != 0 {
		env["statusCodeValue"] = statusCodeValue
	}
	raw, err := json.Marshal(env)
	require.NoError(t, err)
	return "data: " + string(raw) + "\n\n"
}

// qoderFailoverAccount 构造一个可直接驱动 sendQoderUpstreamRequest 的 Qoder 账号。
func qoderFailoverAccount(id int64, serverURL string) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformQoder,
		Credentials: map[string]any{
			"access_token": "dt-failover",
			"uid":          "uid-failover",
			"base_url":     serverURL,
		},
	}
}

// ── 核心正向：首帧即额度/会话/资格错误 → 必须返回可换号的 failover 错误 ──────────

func TestQoderStreamAccountErrorReturnsFailover(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stream     bool
		code       string
		wantStatus int
		wantTemp   bool
	}{
		{"quota_streaming", true, "110", http.StatusTooManyRequests, true},
		{"quota_non_streaming", false, "110", http.StatusTooManyRequests, true},
		{"quota_monthly_streaming", true, "115", http.StatusTooManyRequests, true},
		{"session_streaming", true, "105", http.StatusUnauthorized, false},
		{"restricted_streaming", true, "104", http.StatusForbidden, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := qoderRawUpstream(t, qoderEnvelopeFrame(t,
				`{"code":`+strconv.Quote(tc.code)+`,"message":"quota exhausted"}`, 200)+
				"data: [DONE]\n\n")
			repo := &qoderStateRecordingRepo{}
			svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
			account := qoderFailoverAccount(9401, server.URL)
			c := newWorkbuddyTestGinContext()

			resp, err := svc.sendQoderUpstreamRequest(context.Background(), c, account,
				qoderFailoverTestCCBody(tc.stream), tc.stream)

			// 1) 必须是 failover 错误，否则 handler 的换号循环不会 continue。
			require.Nil(t, resp, "短路后不得把坏死流交给下游")
			var foErr *UpstreamFailoverError
			require.ErrorAs(t, err, &foErr, "首帧即业务错误必须包成 UpstreamFailoverError（否则本请求不换号）")
			require.True(t, foErr.ShouldRetryNextAccount(), "必须允许切换到下一个账号")
			require.False(t, foErr.RetryableOnSameAccount,
				"110/105 对这张号是确定态，同账号重试只会多花一次上游往返")
			require.Equal(t, tc.wantStatus, foErr.StatusCode)
			require.Equal(t, "inference", string(foErr.Stage),
				"不得标 AccountAuth：否则 handler 会把文案改写成与 Qoder 无关的 Grok 口径")

			// 2) 客户端一个字节都没收到 —— 这是「还能透明换号」的硬前提。
			// 注意 gin 在未写出时 Size() 返回 -1（不是 0），因此用 Written() 作为判据。
			require.False(t, c.Writer.Written(), "换号前不得向客户端写出任何字节")

			// 3) 账号状态照旧落库（换号与拉黑是两件事，不能因换号而漏掉落状态）。
			tempCalls, until, reason, errorCalls, _ := repo.snapshot()
			if tc.wantTemp {
				require.Equal(t, 1, tempCalls, "额度类必须落临时不可调度")
				require.True(t, until.After(time.Now()))
				require.Contains(t, reason, tc.code)
			} else {
				require.Equal(t, 1, errorCalls, "会话/资格类必须置错误态")
			}

			// 4) 错误体保留业务码与上游原文，供错误透传规则匹配与排障。
			require.Contains(t, string(foErr.ResponseBody), tc.code)
			require.Contains(t, string(foErr.ResponseBody), "quota exhausted")
		})
	}
}

// Retry-After 只在额度类给出（会话/资格类靠人工处理，给等待时长会误导客户端反复重试）。
func TestQoderStreamFailoverRetryAfterOnlyForQuota(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code      string
		wantValue bool
	}{
		{"110", true},
		{"105", false},
		{"104", false},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			server := qoderRawUpstream(t, qoderEnvelopeFrame(t,
				`{"code":`+strconv.Quote(tc.code)+`,"message":"nope"}`, 200)+"data: [DONE]\n\n")
			svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, &qoderStateRecordingRepo{})
			_, err := svc.sendQoderUpstreamRequest(context.Background(), newWorkbuddyTestGinContext(),
				qoderFailoverAccount(9402, server.URL), qoderFailoverTestCCBody(true), true)

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
			// 与 qoderQuotaRecoveryUntil（次日 0 点）同口径，绝不可能超过一天；
			// 超过 handler 的 isSafeRetryAfter 上限会被整条丢弃。
			require.LessOrEqual(t, seconds, 24*3600)
		})
	}
}

// 信封级错误（statusCodeValue=403）时业务码**内嵌在 body 文本里**，帧自带的 code 是
// 那个 HTTP 码。必须走 resolveQoderBusinessErrorCode 挖出内嵌码才判得对：直接
// classify("403") 会归 unknown 而放弃换号，恰好漏掉这一形态。
func TestQoderStreamEnvelopeEmbeddedCodeStillFailovers(t *testing.T) {
	t.Parallel()
	server := qoderRawUpstream(t, qoderEnvelopeFrame(t,
		`{"code":"110","message":"Daily usage limit reached"}`, 403)+"data: [DONE]\n\n")
	repo := &qoderStateRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	c := newWorkbuddyTestGinContext()

	_, err := svc.sendQoderUpstreamRequest(context.Background(), c,
		qoderFailoverAccount(9403, server.URL), qoderFailoverTestCCBody(true), true)

	var foErr *UpstreamFailoverError
	require.ErrorAs(t, err, &foErr, "内嵌 110 必须被识别为额度类并换号")
	require.Equal(t, http.StatusTooManyRequests, foErr.StatusCode)
	require.Contains(t, string(foErr.ResponseBody), "110", "对外错误体要用内嵌业务码而非信封 HTTP 码")

	tempCalls, _, reason, _, _ := repo.snapshot()
	require.Equal(t, 1, tempCalls, "落状态与换号判定必须同源（同一口径，不得两处各自为政）")
	require.Contains(t, reason, "110")
}

// 非流式路径必须同时满足两种判据：handler 靠 UpstreamFailoverError 换号，既有调用方
// 靠 isQoderUpstreamEnvelopeError 区分「上游业务错」与「本地读错」。只返裸 failover
// 错误会让后者当场失效（错误分类与可观测不应因换号而退化）。
func TestQoderNonStreamFailoverKeepsEnvelopeIdentity(t *testing.T) {
	t.Parallel()
	server := qoderRawUpstream(t, qoderEnvelopeFrame(t,
		`{"code":"111","message":"Quota exhausted"}`, 200)+"data: [DONE]\n\n")
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, &qoderStateRecordingRepo{})

	_, err := svc.sendQoderUpstreamRequest(context.Background(), newWorkbuddyTestGinContext(),
		qoderFailoverAccount(9404, server.URL), qoderFailoverTestCCBody(false), false)

	var foErr *UpstreamFailoverError
	require.ErrorAs(t, err, &foErr, "非流式同样必须可换号")
	require.True(t, isQoderUpstreamEnvelopeError(err), "信封错误身份必须保留")
	// Error() 文本保留 code 与上游原文（裸 failover 错误只会给出「upstream error: 429 (failover)」）。
	require.Contains(t, err.Error(), "111")
	require.Contains(t, err.Error(), "Quota exhausted")
}

// ── 反向护栏 1：不该换号的一律维持「错误帧照常透给客户端」 ──────────────────

// 101 签名 / 102 时间戳 / 103 重复请求一类字典外码：请求级/环境级语义，所有 Qoder 号
// 都会以同一形态被拒，换号只会把同一个错再打一遍。必须既不换号、也不吞上游原文。
func TestQoderStreamUnknownCodeStillEmitsErrorFrame(t *testing.T) {
	t.Parallel()
	server := qoderRawUpstream(t, qoderEnvelopeFrame(t,
		`{"code":"101","message":"signature mismatch"}`, 200)+"data: [DONE]\n\n")
	repo := &qoderStateRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	c := newWorkbuddyTestGinContext()

	resp, err := svc.sendQoderUpstreamRequest(context.Background(), c,
		qoderFailoverAccount(9405, server.URL), qoderFailoverTestCCBody(true), true)
	require.NoError(t, err, "字典外错误码不得包成换号错误")
	require.NotNil(t, resp)
	defer func() { _ = resp.Body.Close() }()

	out, readErr := io.ReadAll(resp.Body)
	require.NoError(t, readErr)
	require.Contains(t, string(out), "signature mismatch", "错误帧必须原样下发")
	require.Contains(t, string(out), "101")

	// 字典外码归 unknown：只留 Ops 痕迹，不落账号状态（避免把请求级问题误标成账号坏了）。
	tempCalls, _, _, errorCalls, _ := repo.snapshot()
	require.Zero(t, tempCalls)
	require.Zero(t, errorCalls)
	requireQoderOpsEvents(t, c, 1)
}

// 已有正文后才出错：语义字节已产出，重放会造成重复输出，绝不能换号。
func TestQoderStreamMidStreamErrorIsNotFailovered(t *testing.T) {
	t.Parallel()
	server := qoderRawUpstream(t,
		qoderEnvelopeFrame(t, `{"id":"c1","choices":[{"index":0,"delta":{"content":"partial"}}]}`, 200)+
			qoderEnvelopeFrame(t, `{"code":"110","message":"quota exhausted"}`, 200)+
			"data: [DONE]\n\n")
	repo := &qoderStateRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	c := newWorkbuddyTestGinContext()

	resp, err := svc.sendQoderUpstreamRequest(context.Background(), c,
		qoderFailoverAccount(9406, server.URL), qoderFailoverTestCCBody(true), true)
	require.NoError(t, err, "已产出正文后不得换号（会重复输出）")
	require.NotNil(t, resp)
	defer func() { _ = resp.Body.Close() }()

	out, readErr := io.ReadAll(resp.Body)
	require.NoError(t, readErr)
	require.Contains(t, string(out), "partial", "已产出的正文一字节都不能丢")
	require.Contains(t, string(out), "110")

	// 落状态仍然要发生，否则该号下一轮继续接单。
	tempCalls, _, _, _, _ := repo.snapshot()
	require.Equal(t, 1, tempCalls)
}

// ── 反向护栏 2：预读首帧不得吃掉任何正常字节 ────────────────────────────────

func TestQoderStreamProbePreservesNormalFrames(t *testing.T) {
	t.Parallel()
	server := qoderRawUpstream(t,
		qoderEnvelopeFrame(t, `{"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}]}`, 200)+
			qoderEnvelopeFrame(t, `{"id":"c1","choices":[{"index":0,"delta":{"content":" there"}}]}`, 200)+
			qoderEnvelopeFrame(t, `{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, 200)+
			"data: [DONE]\n\n")

	for _, stream := range []bool{true, false} {
		name := "streaming"
		if !stream {
			name = "non_streaming"
		}
		t.Run(name, func(t *testing.T) {
			svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, &qoderStateRecordingRepo{})
			resp, err := svc.sendQoderUpstreamRequest(context.Background(), newWorkbuddyTestGinContext(),
				qoderFailoverAccount(9407, server.URL), qoderFailoverTestCCBody(stream), stream)
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

func TestQoderFailoverableBusinessError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code string
		want bool
	}{
		{"110", true}, // 额度：这张号用完，别的号还有
		{"111", true},
		{"112", true},
		{"115", true},
		{"116", true},
		{"117", true},
		{"118", true},
		{"119", true},
		{"105", true}, // 会话：这张号的票坏了
		{"104", true}, // 资格：这张号没权益
		{"108", true},
		{"109", true},
		{"101", false}, // 签名：请求级，换号必然复现
		{"102", false},
		{"103", false},
		{"107", false},
		{"113", false},
		{"114", false},
		{"418", false},
		{"", false},
		{"0", false},
	}
	for _, tc := range cases {
		err := &errQoderUpstreamEnvelope{StatusCode: 200, Code: tc.code, Message: "m"}
		require.Equal(t, tc.want, qoderFailoverableBusinessError(err), "code=%q", tc.code)
	}
	// 非信封错误对象不得被误判为可换号。
	require.False(t, qoderFailoverableBusinessError(io.ErrUnexpectedEOF))
	require.False(t, qoderFailoverableBusinessError(nil))
	// newQoderStreamFailoverError 与判据同源：不可换号时必须返回 nil（调用方维持旧行为）。
	require.Nil(t, newQoderStreamFailoverError(&errQoderUpstreamEnvelope{Code: "101"}, time.Now()))
	require.NotNil(t, newQoderStreamFailoverError(&errQoderUpstreamEnvelope{Code: "110"}, time.Now()))
}

// probe 的幂等守卫：第二次调用不得重复读流（否则会把已交出的帧当新数据再读一遍）。
func TestQoderProbeFirstFrameIsIdempotent(t *testing.T) {
	t.Parallel()
	reader := newQoderSSEReaderWithErrorHook(
		strings.NewReader(qoderEnvelopeFrame(t, `{"code":"110","message":"quota"}`, 200)),
		"", func(string, string, *int) {})
	require.Error(t, reader.qoderProbeFirstFrame())
	require.NoError(t, reader.qoderProbeFirstFrame(), "已短路后再 probe 必须直接放行（幂等）")

	// 【不丢字节】短路时错误帧只是「没写出」，仍在 reader 缓冲区里：调用方一旦没能
	// 把它包成换号错误（判据分叉的意外），错误帧会照常下发而不是退化成空回复。
	out := string(readAllQoder(t, reader))
	require.Contains(t, out, "quota", "短路不得吞掉错误帧本体")

	// 正常首帧：probe 返回 nil 且字节完好。
	reader = newQoderSSEReaderWithErrorHook(
		strings.NewReader(qoderEnvelopeFrame(t, `{"id":"c1","choices":[{"index":0,"delta":{"content":"ok"}}]}`, 200)),
		"", nil)
	require.NoError(t, reader.qoderProbeFirstFrame())
	require.NoError(t, reader.qoderProbeFirstFrame())
	require.Contains(t, string(readAllQoder(t, reader)), "ok")
}

// contentFrames 判据：错误帧不得算作正文，否则「首帧即错」会被误判成「已有正文」
// 而放弃换号（validFrames 把错误帧也计数在内，这里钉住两者的区别）。
func TestQoderErrorFrameDoesNotCountAsContent(t *testing.T) {
	t.Parallel()
	reader := newQoderSSEReaderWithErrorHook(
		strings.NewReader(qoderEnvelopeFrame(t, `{"code":"110","message":"quota"}`, 200)),
		"", func(string, string, *int) {})
	require.Error(t, reader.qoderProbeFirstFrame())
	require.Zero(t, reader.contentFrames, "错误帧不得计入正文帧数")
	require.Equal(t, 1, reader.validFrames, "validFrames 仍把错误帧计入（对外协议不变）")
}
