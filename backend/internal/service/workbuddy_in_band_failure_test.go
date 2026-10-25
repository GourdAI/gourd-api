//go:build unit

package service

// workbuddy_in_band_failure_test.go 回归：WorkBuddy「HTTP 200 + 流内 error 帧」必须记成
// 客户端可见失败（SLA），不再静默显示「成功」。
//
// 坐实的失效链（与 Qoder 2026-10-20 那次完全同构）：
//  1. streamRawChatCompletions 只认 [DONE]/usage/finish_reason，不认识「带内 error 帧」；
//     修复前 finsh 又无条件补 [DONE]（本轮已修），于是 forwardErr=nil；
//  2. workbuddyStreamErrorHandler 只调 appendOpsUpstreamError —— 它把事件挂到**已存在**的
//     错误行 JSON 上，本身不生成行（ops_error_logger 只在 wire>=400 或有人显式调
//     MarkOpsStreamFailure 时才落 ops_error_logs 行）；
//  3. chat 域码表恒 unknown ⇒ applyWorkbuddyErrorClass 对 unknown 直接 return，也不落账号状态。
//
// 三条叠加 ⇒ 账号被上游拒了，运维面板仍显示「成功」、调度器继续选同一枚号。
// 本轮按 Qoder 的 delivery-observer 口径补上（workbuddySSEDeliveryReader）。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// workbuddyInBandErrStream 先正文后错误帧（首帧预读无法短路的形态）。
const workbuddyInBandErrStream = `data: {"id":"wb-1","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n" +
	`data: {"error":{"message":"rate limited","code":"6004"}}` + "\n\n"

// workbuddyDriveInBandError 走真实发送管线（含交付观察器），返回 gin 上下文供断言 Ops。
func workbuddyDriveInBandError(t *testing.T, upstreamSSE string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamSSE))
	}))
	t.Cleanup(server.Close)

	svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, &qoderStateRecordingRepo{})
	account := &Account{
		ID:          9711,
		Platform:    PlatformWorkbuddy,
		Name:        "wb-probe",
		Credentials: map[string]any{"base_url": server.URL, "access_token": "wb-token"},
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	body := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"ping"}],"stream":true}`)
	resp, err := svc.sendWorkbuddyUpstreamRequest(context.Background(), c, account, body, true)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// 必须把流读完：交付观察器在 EOF 处才判定「错误帧确实已交付」。
	_, readErr := io.ReadAll(resp.Body)
	require.NoError(t, readErr)
	_ = resp.Body.Close()
	return c
}

// ── 核心：带内错误交付后必须登记为 SLA 失败 ──────────────────────────────────
func TestWorkbuddyDeliveredInBandErrorCountsAsFailure(t *testing.T) {
	c := workbuddyDriveInBandError(t, workbuddyInBandErrStream)

	streamErr, ok := GetOpsStreamError(c)
	require.True(t, ok,
		"带内错误交付后必须登记为客户端可见失败（修复前此处为空 → 面板显示成功）")
	require.True(t, streamErr.CountTowardsSLA, "必须计入 SLA 失败，否则错误率/成功率仍失真")
	require.Equal(t, "6004", streamErr.Code)
	require.Contains(t, streamErr.Message, "6004")
}

// 反向护栏：正常流（含 finish_reason 收尾）不得被记成失败。
func TestWorkbuddyNormalStreamNotMarkedAsFailure(t *testing.T) {
	normal := `data: {"id":"wb-1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"wb-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	c := workbuddyDriveInBandError(t, normal)
	_, ok := GetOpsStreamError(c)
	require.False(t, ok, "正常流不得虚抬错误率")
}

// 反向护栏：网关自己合成的空流兜底帧（本地解析缺陷）不算上游失败，
// 否则会把「上游其实正常、我们解析不出来」误报成账号问题。
func TestWorkbuddyLocalEmptyStreamFrameNotCountedAsUpstreamFailure(t *testing.T) {
	c := workbuddyDriveInBandError(t, ": keep-alive\n\n")
	_, ok := GetOpsStreamError(c)
	require.False(t, ok, "本地合成的空流帧属解析缺陷，不得记成上游失败")
}

// ── 交付点语义：客户端已断开时不记失败（错误帧没真正被读到） ────────────────
func TestWorkbuddyMarkDeliveredInBandErrorHonoursClientGone(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 9712, Platform: PlatformWorkbuddy, Name: "probe"}

	newErroredReader := func() *workbuddySSEReader {
		r := newWorkbuddySSEReaderWithHook(
			strings.NewReader(`data: {"error":{"message":"rate limited","code":"6004"}}`+"\n\n"), nil)
		_, _ = io.ReadAll(r)
		require.True(t, r.upstreamErrFrameDelivered(), "夹具必须先真的交付一个上游错误帧")
		return r
	}

	// 对照：客户端仍在 → 必须记失败。
	liveCtx, liveCancel := context.WithCancel(context.Background())
	defer liveCancel()
	liveC := newWorkbuddyTestGinContext()
	liveC.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(liveCtx)
	liveReader := newErroredReader()
	svc.workbuddyMarkDeliveredInBandError(liveC, account, liveReader.errFrameCode, liveReader.errFrameMessage)
	_, liveMarked := GetOpsStreamError(liveC)
	require.True(t, liveMarked, "客户端在线时带内错误必须记失败")

	// 守卫：入站 ctx 已取消 → 不得记失败。
	goneCtx, goneCancel := context.WithCancel(context.Background())
	goneC := newWorkbuddyTestGinContext()
	goneC.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(goneCtx)
	goneCancel()
	require.Error(t, goneC.Request.Context().Err())
	goneReader := newErroredReader()
	svc.workbuddyMarkDeliveredInBandError(goneC, account, goneReader.errFrameCode, goneReader.errFrameMessage)
	_, goneMarked := GetOpsStreamError(goneC)
	require.False(t, goneMarked, "客户端已断开不得记失败（会虚抬错误率）")
}
