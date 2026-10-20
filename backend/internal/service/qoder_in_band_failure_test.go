//go:build unit

package service

// qoder_in_band_failure_test.go 回归：Qoder「HTTP 200 + 流内错误帧」不再被记成成功，
// 且排队类（10605）按上游给的秒数短冷却 + 换号。
//
// 坐实的失效链（2026-10-20 用户线上报错原文）：
//
//	data:{"headers":{},"body":"{\"code\":\"10605\",\"message\":\"AIIsQueued\",\"isQueued\":true,
//	      \"modelKey\":\"dfmodel\",\"queueCount\":95,\"queueType\":\"p3\",\"retryAfterSeconds\":30,
//	      \"serviceAvailable\":true,\"waitTime\":6}","statusCodeValue":403}
//
// 修复前：10605 不在码表 → class=unknown → 不落账号状态、不换号；且
// streamRawChatCompletions 只认 [DONE]/usage/finish_reason（归一化器恒补 [DONE]）
// → forwardErr=nil → handler 走成功分支 → 用户侧报错、运维面板记「成功」、
// 账号健康度还被正向加分。

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

const (
	// 用户线上真实内层错误体（OCR 噪声已按 JSON 结构还原）
	qoderQueuedInner = `{"code":"10605","message":"AIIsQueued","isQueued":true,"modelKey":"dfmodel",` +
		`"queueCount":95,"queueType":"p3","retryAfterSeconds":30,"serviceAvailable":true,"waitTime":6}`
	qoderContentFrameInner = `{"id":"chat-1","model":"auto","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`
)

// ── 1. 分类与解析：10605 必须命中排队类，而不是 unknown ─────────────────────────

func TestQoderQueuedCodeClassified(t *testing.T) {
	t.Parallel()
	require.Equal(t, qoderErrorClassQueued, classifyQoderBusinessError("10605"))
	require.True(t, qoderFailoverErrorClass(qoderErrorClassQueued), "排队类必须允许换号（用户拍板）")

	// 线上形态：信封码 403 + 内嵌 10605。resolve 必须给出内嵌码而不是 403。
	code, class := resolveQoderBusinessErrorCode("403", qoderQueuedInner)
	require.Equal(t, "10605", code, "内嵌业务码必须胜出，否则退化成 unknown 而不落状态")
	require.Equal(t, qoderErrorClassQueued, class)
}

// ── 2. 冷却时长：取上游 retryAfterSeconds，并夹逼到 [5s, 10min] ────────────────

func TestQoderQueuedCooldownBounds(t *testing.T) {
	t.Parallel()
	// 结构化值优先（线上形态：retryAfterSeconds 与 code **同层**，不在 message 文本里）。
	require.Equal(t, 30*time.Second, qoderQueuedCooldown(qoderIntPtrTest(30), "AIIsQueued"))
	require.Equal(t, qoderQueuedCooldownMax, qoderQueuedCooldown(qoderIntPtrTest(86400), "AIIsQueued"),
		"深队列大值必须夹到上限，不得照上游值拉到一天")
	// 上游给 0/1 秒：下限 5s——调度器换号零退避，秒级回灌只会继续顶高同一队列。
	require.Equal(t, qoderQueuedCooldownMin, qoderQueuedCooldown(qoderIntPtrTest(1), ""))
	require.Equal(t, qoderQueuedCooldownMin, qoderQueuedCooldown(qoderIntPtrTest(0), ""),
		"字段存在且为 0 不等于缺失；只有缺失才回落 30s")
	// 完全缺失：回落 30s（实证样本值）。
	require.Equal(t, 30*time.Second, qoderQueuedCooldown(nil, ""))
	require.Equal(t, 30*time.Second, qoderQueuedCooldown(nil, "quota queued, come back later"))
	// 只有文本携带时仍可用（兼容旧形态，不因缺结构化字段而丢信息）。
	require.Equal(t, 30*time.Second, qoderQueuedCooldown(nil, `{"retryAfterSeconds":30}`))
	require.Equal(t, qoderQueuedCooldownMax, qoderQueuedCooldown(nil, `{"retryAfterSeconds":99999}`))
	// 结构化值优先于文本：两者矛盾时取调用方直接递来的那个。
	require.Equal(t, 5*time.Second, qoderQueuedCooldown(qoderIntPtrTest(2), `{"retryAfterSeconds":86400}`))
}

func qoderIntPtrTest(v int) *int { return &v }

// ── 3. 端到端：首帧即 10605 → 换号 + 短冷却 + Retry-After 同源 ──────────────────

func TestQoderQueuedFirstFrameFailoversWithShortCooldown(t *testing.T) {
	// 线上真实形态：statusCodeValue=403 信封 + 内层 10605。
	server := qoderRawUpstream(t, qoderEnvelopeFrame(t, qoderQueuedInner, 403)+"data: [DONE]\n\n")
	repo := &qoderStateRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	c := newWorkbuddyTestGinContext()

	resp, err := svc.sendQoderUpstreamRequest(context.Background(), c,
		qoderFailoverAccount(9501, server.URL), qoderFailoverTestCCBody(true), true)

	require.Nil(t, resp, "可换号时不得把坏死流交给下游")
	var foErr *UpstreamFailoverError
	require.ErrorAs(t, err, &foErr, "10605 必须包成 UpstreamFailoverError")
	require.True(t, foErr.ShouldRetryNextAccount())
	require.Equal(t, http.StatusTooManyRequests, foErr.StatusCode, "排队在客户端语义上是「稍后重试」，不是 502")
	require.Equal(t, "30", foErr.ResponseHeaders.Get("Retry-After"), "Retry-After 必须取上游给的秒数")

	tempCalls, until, reason, _, _ := repo.snapshot()
	require.Equal(t, 1, tempCalls, "排队类必须落临时不可调度")
	delta := time.Until(until)
	require.Greater(t, delta, 20*time.Second, "冷却必须短于等于上游给的等待")
	require.Less(t, delta, 2*time.Minute, "排队绝不能被按日拉黑（号本身健康）")
	require.Contains(t, reason, "10605")
	require.Contains(t, reason, "queued")

	// 换号路径不是「客户端可见失败」：本次最终由下一个号服务。
	_, hasStreamErr := GetOpsStreamError(c)
	require.False(t, hasStreamErr, "已恢复的尝试不得记成客户端可见失败")
}

func TestQoderQueuedCooldownClampedInFailoverHeader(t *testing.T) {
	server := qoderRawUpstream(t, qoderEnvelopeFrame(t,
		`{"code":"10605","message":"AIIsQueued","retryAfterSeconds":86400}`, 200)+"data: [DONE]\n\n")
	repo := &qoderStateRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, repo)

	resp, err := svc.sendQoderUpstreamRequest(context.Background(), newWorkbuddyTestGinContext(),
		qoderFailoverAccount(9502, server.URL), qoderFailoverTestCCBody(true), true)
	require.Nil(t, resp)
	var foErr *UpstreamFailoverError
	require.ErrorAs(t, err, &foErr)
	got, convErr := strconv.Atoi(foErr.ResponseHeaders.Get("Retry-After"))
	require.NoError(t, convErr)
	require.Equal(t, int(qoderQueuedCooldownMax.Seconds()), got, "深队列大值必须夹到上限，避免客户端拿到 24h 等待")
}

// ── 4. 形态盲区：嵌套 error 体 / 仅 message ────────────────────────────────────

func TestQoderNestedAndBareErrorShapesRecognized(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		inner    string
		wantCode string
	}{
		{"nested_error", `{"error":{"code":"110","message":"Daily usage limit reached"}}`, "110"},
		{"bare_message", `{"message":"今日额度已用尽"}`, ""},
	} {
		data := qoderParseDataFrame(strings.TrimPrefix(qoderSSEEnvelope(tc.inner, 200), "data: "))
		require.NotNil(t, data, tc.name)
		require.NotNil(t, data.BusinessErr, "%s 形态必须识别为业务错误（修复前被当正常帧逐键透传）", tc.name)
		require.Equal(t, tc.wantCode, data.BusinessErr.Code, tc.name)
	}

	// 反向护栏：带内容字段的正常帧不得被误判为错误。
	for _, inner := range []string{
		qoderContentFrameInner,
		`{"id":"chat-1","message":"note","choices":[{"index":0,"delta":{"content":"x"}}]}`,
		`{"usage":{"prompt_tokens":1}}`,
	} {
		data := qoderParseDataFrame(strings.TrimPrefix(qoderSSEEnvelope(inner, 200), "data: "))
		require.Nil(t, data.BusinessErr, "正常帧不得判错: %s", inner)
	}
}

// ── 5. 核心可观测缺陷：带内错误一经交付必须记失败，不再显示成功 ────────────────

func TestQoderDeliveredInBandErrorCountsAsFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		frames      string
		wantStatus  int
		wantNoMark  bool
		wantMessage string
	}{
		{
			// 先正文后错误：首帧预读无法短路（换号会重放输出），但必须记失败。
			name: "mid_stream_unknown_code",
			frames: qoderEnvelopeFrame(t, qoderContentFrameInner, 200) +
				qoderEnvelopeFrame(t, `{"code":"113","message":"usage limit"}`, 200),
			wantStatus:  http.StatusBadGateway,
			wantMessage: "113",
		},
		{
			// 字典外 5 位码（unknown）：用户拍板「记失败但不换号」。
			name:       "first_frame_unknown_code",
			frames:     qoderEnvelopeFrame(t, `{"code":"99999","message":"weird upstream state"}`, 200),
			wantStatus: http.StatusBadGateway,
		},
		{
			name:   "normal_stream_not_failure",
			frames: qoderEnvelopeFrame(t, qoderContentFrameInner, 200),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := qoderRawUpstream(t, tc.frames+"data: [DONE]\n\n")
			repo := &qoderStateRecordingRepo{}
			svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
			c := newWorkbuddyTestGinContext()

			resp, err := svc.sendQoderUpstreamRequest(context.Background(), c,
				qoderFailoverAccount(9503, server.URL), qoderFailoverTestCCBody(true), true)
			require.NoError(t, err, "不可换号的带内错误仍按原样透传给客户端")
			require.NotNil(t, resp)
			body, readErr := io.ReadAll(resp.Body)
			require.NoError(t, readErr)
			_ = resp.Body.Close()
			require.Contains(t, string(body), "data: [DONE]", "归一化器必须补 [DONE]（也正是它掩盖了错误）")

			streamErr, ok := GetOpsStreamError(c)
			if tc.wantMessage == "" && tc.wantStatus == 0 {
				require.False(t, ok, "正常流不得记失败")
				return
			}
			require.True(t, ok, "带内错误交付后必须登记为客户端可见失败（修复前此处为空 → 面板显示成功）")
			require.True(t, streamErr.CountTowardsSLA)
			require.Equal(t, tc.wantStatus, streamErr.IntendedStatus)
			if tc.wantMessage != "" {
				require.Contains(t, streamErr.Message, tc.wantMessage)
			}
		})
	}
}

// 客户端已断开（入站 ctx 取消）：错误帧只是被排空到 EOF，没真正交付给用户，
// 不得记成本次失败（与 raw 路径 clientAborted 不计上游故障同口径）。
//
// 【直接测守卫函数】走整条流时上游请求会随入站 ctx 一起取消，可能根本读不到
// EOF，用例会在「未走到守卫」的情况下假通过；所以直接给守卫函数喂一个已置
// bizErr 的 reader，并用「live ctx」版本作对照（后者必须记失败）。
func TestQoderMarkDeliveredInBandErrorHonoursClientGone(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 9505, Platform: PlatformQoder, Name: "probe"}

	newErroredReader := func() *qoderSSEReader {
		r := newQoderSSEReaderWithErrorHook(
			strings.NewReader(qoderSSEEnvelope(`{"code":"113","message":"upstream busy"}`, 200)),
			"qfmodel", nil)
		_ = r.qoderProbeFirstFrame()
		require.NotNil(t, r.bizErr, "夹具必须先真的产出一个业务错误帧")
		return r
	}

	// 对照：客户端仍在，错误帧会交付 → 必须记失败。
	liveCtx, liveCancel := context.WithCancel(context.Background())
	defer liveCancel()
	liveC := newWorkbuddyTestGinContext()
	liveC.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(liveCtx)
	svc.qoderMarkDeliveredInBandError(liveC, account, newErroredReader())
	_, liveMarked := GetOpsStreamError(liveC)
	require.True(t, liveMarked, "客户端在线时带内错误必须记失败")

	// 守卫：入站 ctx 已取消 → 不得记失败。
	goneCtx, goneCancel := context.WithCancel(context.Background())
	goneC := newWorkbuddyTestGinContext()
	goneC.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(goneCtx)
	goneCancel()
	require.Error(t, goneC.Request.Context().Err())
	svc.qoderMarkDeliveredInBandError(goneC, account, newErroredReader())
	_, goneMarked := GetOpsStreamError(goneC)
	require.False(t, goneMarked, "客户端已断开的尝试不得记失败（会虚抬错误率）")
}

// ── 6. 交付观察器只记一次（上游重发错误帧 / 多次读尾部） ───────────────────────

func TestQoderDeliveryMarkerIsOnceOnly(t *testing.T) {
	server := qoderRawUpstream(t,
		qoderEnvelopeFrame(t, qoderContentFrameInner, 200)+
			qoderEnvelopeFrame(t, `{"code":"113","message":"dup"}`, 200)+
			qoderEnvelopeFrame(t, `{"code":"113","message":"dup"}`, 200))
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, &qoderStateRecordingRepo{})
	c := newWorkbuddyTestGinContext()

	resp, err := svc.sendQoderUpstreamRequest(context.Background(), c,
		qoderFailoverAccount(9504, server.URL), qoderFailoverTestCCBody(true), true)
	require.NoError(t, err)
	_, readErr := io.ReadAll(resp.Body)
	require.NoError(t, readErr)
	// 二次读取（已 EOF）不得重复补记。
	buf := make([]byte, 16)
	_, _ = resp.Body.Read(buf)
	_ = resp.Body.Close()

	events := GetOpsStreamErrors(c)
	require.Len(t, events, 1, "同一尝试的带内失败只登记一次（markOpsStreamError 首标记生效 + marked 守卫）")
}
