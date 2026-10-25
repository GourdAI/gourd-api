//go:build unit

package service

// cross_platform_stream_truncation_test.go 回归：Trae / WorkBuddy 与 Qoder 完全同构的
// 「上游正文中途静默断流被记成成功」缺陷。
//
// 三家共用同一个上层 streamRawChatCompletions，其成功判据是
// openAIRawStreamTerminalState（[DONE]/usage/finish_reason 任一出现即 Terminated）。
// 修复前各家的归一化器都会在没有完成证据时**代上游宣告完成**：
//   - Qoder：finish() 恒补 data: [DONE]（本文件不重复覆盖，见 qoder_stream_truncation_test.go）
//   - WorkBuddy：finish() 恒补 data: [DONE]
//   - Trae：finish() 只要 sawOutput 就主动构造 finish_reason=stop 帧 + [DONE]
//
// 于是上游断流时 forwardErr=nil → handler 走成功分支
// （ReportOpenAIAccountScheduleResult success=true + usage_logs 落 0 token 行），
// 运维「请求明细」显示「成功」。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// workbuddyTruncatedSSE 只有正文、无 finish_reason / 无 usage / 无 [DONE] 的上游流。
const workbuddyTruncatedSSE = `data: {"id":"wb-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"partial answer"},"finish_reason":null}]}` + "\n\n"

// traeTruncatedSSE 只有 output 帧、无 done / 无 usage 的上游流（Trae 从不发 [DONE]）。
const traeTruncatedSSE = "event: output\ndata: {\"response\":\"partial answer\"}\n\n"

// workbuddyDriveTruncation 用与线上同构的管线驱动 WorkBuddy 流：
// 归一化器 → streamRawChatCompletions 的终止/截断判据。
func workbuddyDriveTruncation(t *testing.T, upstreamSSE string) (*OpenAIForwardResult, error, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamSSE))
	}))
	t.Cleanup(server.Close)

	repo := &qoderStateRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := &Account{
		ID:          9701,
		Platform:    PlatformWorkbuddy,
		Credentials: map[string]any{"base_url": server.URL, "access_token": "wb-token"},
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	body := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"ping"}],"stream":true}`)
	resp, sendErr := svc.sendWorkbuddyUpstreamRequest(context.Background(), c, account, body, true)
	require.NoError(t, sendErr)
	require.NotNil(t, resp)

	result, forwardErr := svc.streamRawChatCompletions(
		c, resp, account, "glm-5.3", "glm-5.3", "glm-5.3", nil, nil, time.Now(), len(body))
	return result, forwardErr, rec.Body.String()
}

// ── WorkBuddy：正文中途断流必须记失败，不得伪造 [DONE] ──────────────────────
func TestWorkbuddyTruncatedStreamIsNotRecordedAsSuccess(t *testing.T) {
	_, err, wire := workbuddyDriveTruncation(t, workbuddyTruncatedSSE)
	require.Error(t, err, "上游中途静默断流必须返回错误，不能以 forwardErr=nil 收尾（面板会记成功）")
	require.NotContains(t, wire, "data: [DONE]", "上游没有完成证据，归一化器不得代为补 [DONE]")
	require.Contains(t, wire, "partial answer", "已产出的正文仍须交付")
}

// 反向护栏：上游自己发了 [DONE] → 正常成功。
func TestWorkbuddyUpstreamDoneStaysSuccess(t *testing.T) {
	_, err, wire := workbuddyDriveTruncation(t, workbuddyTruncatedSSE+"data: [DONE]\n\n")
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(wire, "data: [DONE]"))
}

// 反向护栏：正文帧带 finish_reason（上游省略 [DONE]）→ 正常成功。
func TestWorkbuddyFinishReasonWithoutDoneStaysSuccess(t *testing.T) {
	finished := `data: {"id":"wb-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	_, err, wire := workbuddyDriveTruncation(t, workbuddyTruncatedSSE+finished)
	require.NoError(t, err, "finish_reason 已是完成证据")
	require.Equal(t, 1, strings.Count(wire, "data: [DONE]"))
}

// ── Trae：只有 output 帧就 EOF = 断流，不得代盖 finish_reason=stop ───────────
func TestTraeTruncatedStreamIsNotRecordedAsSuccess(t *testing.T) {
	out := readAllTrae(t, traeNewSSEReader(strings.NewReader(traeTruncatedSSE), traeStreamMeta{}))
	require.NotContains(t, out, "data: [DONE]", "无完成证据时不得补 [DONE]（上层靠它判截断）")
	require.NotContains(t, out, `"finish_reason":"stop"`, "不得代上游伪造完成章")
	require.Contains(t, out, "partial answer")
}

// 反向护栏：Trae 的 usage 帧（真实上游的计费读数）也算完成证据。
func TestTraeUsageEventCountsAsCompletion(t *testing.T) {
	stream := sseStream(
		sseEvent("output", `{"response":"answer"}`),
		sseEvent("token_usage", `{"prompt_tokens":4,"completion_tokens":6}`),
	)
	out := readAllTrae(t, traeNewSSEReader(strings.NewReader(stream), traeStreamMeta{}))
	require.Equal(t, 1, countDone(out), "usage 尾帧是 CC 协议完成哨兵")
}
