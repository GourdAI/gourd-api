//go:build unit

package service

// qoder_stream_truncation_test.go 回归：Qoder「上游正文中途静默断流」不得被归一化器
// 伪造的 data: [DONE] 洗成成功。
//
// 坐实的失效链（2026-10-24 线上样本：Qoder / qwen3.8-flash，入站 /v1/chat/completions，
// 上游 /algo/api/v2/service/pro/sse/agent_chat_generation，首字 453ms、总耗时 3m0s、
// 输入输出 0/0 token、运维「请求明细」显示「成功」）：
//
//	qoderSSEReader.finish 恒补一个 data: [DONE]
//	  → streamRawChatCompletions 的成功判据是「[DONE]/usage/finish_reason 任一出现」
//	    （openai_raw_stream_truncation.go 的 openAIRawStreamTerminalState）
//	  → 恒为 Terminated()=true ⇒ IsTruncated()=false ⇒ forwardErr=nil
//	  → handler 走成功分支：ReportOpenAIAccountScheduleResult(success=true)
//	    + usage_logs 落 0 token 行 ⇒ 面板记「成功」，账号健康度还被正向加分。
//
// 与 2026-10-20 那批修复的关系：那批只覆盖「上游发得出错误帧」的形态（带内错误由
// qoderSSEDeliveryReader 在交付点补记失败）。**上游根本不回错误帧、直接断流**是另一条
// 路径，当时没有任何信号可用——交付观察器只在 bizErr != nil 时才标记，而断流的
// bizErr 是 nil。本次修复的是后者。

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// qoderTruncationContentInner 只有正文、无 finish_reason / usage 的一帧（真实上游
// 生成中途的形态：finish_reason 恒为 null，usage 只在最后一帧才出现）。
const qoderTruncationContentInner = `{"id":"chat-t","model":"auto","choices":[{"index":0,"delta":{"role":"assistant","content":"partial answer"},"finish_reason":null}]}`

// qoderCompletedInner 带上 finish_reason 的收尾帧（上游正常讲完了）。
const qoderCompletedInner = `{"id":"chat-t","model":"auto","choices":[{"index":0,"delta":{"content":" done"},"finish_reason":"stop"}]}`

// qoderDriveTruncation 用与线上完全同构的两段管线驱动一段上游 SSE：
//
//	归一化器（newQoderSSEReaderWithErrorHook + 交付观察器）
//	→ streamRawChatCompletions 的终止/截断判据
//
// 返回 forward 结果与错误。第二个返回值为流通本身（供断言 [DONE] 是否被伪造）。
func qoderDriveTruncation(t *testing.T, upstreamSSE string) (*OpenAIForwardResult, error, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	server := qoderRawUpstream(t, upstreamSSE)
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, &qoderStateRecordingRepo{})
	account := qoderFailoverAccount(9601, server.URL)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(""))

	body := qoderFailoverTestCCBody(true)
	resp, sendErr := svc.sendQoderUpstreamRequest(context.Background(), c, account, body, true)
	require.NoError(t, sendErr, "上游未发错误帧，发送阶段不得报错")
	require.NotNil(t, resp)

	result, forwardErr := svc.streamRawChatCompletions(
		c, resp, account,
		"qwen3.8-flash", "qfmodel", "qfmodel",
		nil, nil, time.Now(), len(body),
	)
	return result, forwardErr, rec.Body.String()
}

// ── 核心：正文中途断流（无 finish_reason / 无 usage / 无上游 [DONE]）必须记失败 ──
func TestQoderTruncatedStreamIsNotRecordedAsSuccess(t *testing.T) {
	result, err, wire := qoderDriveTruncation(t, qoderEnvelopeFrame(t, qoderTruncationContentInner, 200))

	require.Error(t, err, "上游在正文中途静默断流：必须返回错误，不能以 forwardErr=nil 收尾")
	// 干净 EOF 无传输层错误时，raw 路径用 ErrOpenAIUpstreamStreamTruncated 作 cause，
	// 与上游连接被 reset（扫错器带回真实错误）两种形态共用同一分类。
	require.ErrorIs(t, err, ErrOpenAIUpstreamStreamTruncated, "干净 EOF 截断应归为该哨兵错误")
	code, _, ok := OpenAIUpstreamStreamReadErrorDetails(err)
	require.True(t, ok, "必须带上可下发客户端的稳定分类")
	require.Equal(t, OpenAIUpstreamStreamTruncatedCode, code)
	require.NotNil(t, result, "已写出正文时仍要带上已收到的 usage 正常计费")

	require.Contains(t, wire, "partial answer", "已产出的正文必须已经交付客户端")
	require.NotContains(t, wire, "data: [DONE]",
		"上游没有给出任何完成证据时，归一化器不得代为补 [DONE]（正是它把断流洗成成功）")
}

// ── 反向护栏：上游自己发了 [DONE]（无 finish_reason）→ 正常成功 ───────────────
func TestQoderUpstreamDoneWithoutFinishReasonStaysSuccess(t *testing.T) {
	result, err, wire := qoderDriveTruncation(t,
		qoderEnvelopeFrame(t, qoderTruncationContentInner, 200)+"data: [DONE]\n\n")

	require.NoError(t, err, "上游显式发了 [DONE]，即使帧里没有 finish_reason 也是正常完成")
	require.NotNil(t, result)
	require.Equal(t, 1, strings.Count(wire, "data: [DONE]"), "仍须恰好一个 [DONE]")
}

// ── 反向护栏：正文帧自带 finish_reason（上游省略了 [DONE]）→ 正常成功 ─────────
func TestQoderFinishReasonWithoutDoneSentinelStaysSuccess(t *testing.T) {
	result, err, wire := qoderDriveTruncation(t,
		qoderEnvelopeFrame(t, qoderTruncationContentInner, 200)+
			qoderEnvelopeFrame(t, qoderCompletedInner, 200))

	require.NoError(t, err, "finish_reason 已是完成证据，上游没补 [DONE] 不应判成截断")
	require.NotNil(t, result)
	require.Equal(t, 1, strings.Count(wire, "data: [DONE]"), "由归一化器补齐恰好一个 [DONE]")
}

// ── 反向护栏：usage 尾帧（不带 [DONE]/finish_reason）→ 正常成功 ───────────────
func TestQoderUsageTailFrameWithoutDoneStaysSuccess(t *testing.T) {
	usageFrame := `{"id":"chat-t","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`
	result, err, wire := qoderDriveTruncation(t,
		qoderEnvelopeFrame(t, qoderTruncationContentInner, 200)+
			qoderEnvelopeFrame(t, usageFrame, 200))

	require.NoError(t, err, "usage 尾帧是 OpenAI CC 协议的完成哨兵")
	require.NotNil(t, result)
	require.Equal(t, 1, strings.Count(wire, "data: [DONE]"))
}

// ── 反向护栏：带内业务错误帧仍由交付观察器按原路径记失败（不得被本次改动带坏） ──
func TestQoderInBandErrorStillMarkedAfterTruncationFix(t *testing.T) {
	server := qoderRawUpstream(t,
		qoderEnvelopeFrame(t, qoderTruncationContentInner, 200)+
			qoderEnvelopeFrame(t, `{"code":"113","message":"usage limit"}`, 200))
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, &qoderStateRecordingRepo{})
	c := newWorkbuddyTestGinContext()

	resp, err := svc.sendQoderUpstreamRequest(context.Background(), c,
		qoderFailoverAccount(9602, server.URL), qoderFailoverTestCCBody(true), true)
	require.NoError(t, err)
	require.NotNil(t, resp)
	_, readErr := io.ReadAll(resp.Body)
	require.NoError(t, readErr)
	_ = resp.Body.Close()

	streamErr, ok := GetOpsStreamError(c)
	require.True(t, ok, "先正文后错误的形态仍必须在交付点记失败")
	require.True(t, streamErr.CountTowardsSLA)
	require.Contains(t, streamErr.Message, "113")
}

// ── 非流式聚合路径：同一缺陷（finishReason 默认 "stop"）必须一并收口 ──────
func TestAggregateQoderSSEWithoutTerminalEvidenceIsTruncated(t *testing.T) {
	// 正文帧已有内容、但无 finish_reason / 无 usage / 无 [DONE]：上游只是回头走了。
	_, _, err := aggregateQoderSSE(
		strings.NewReader(qoderSSEEnvelope(qoderTruncationContentInner, 200)), "")
	require.ErrorIs(t, err, errQoderStreamTruncated,
		"非流式聚合不得用本地默认 finishReason=stop 把断流洗成完整响应")
	require.False(t, qoderIsEmptyStreamError(err), "截断不得被误归为空流（两者排障口径不同）")

	// 反向护栏：三条完成证据各自存在一个就得算成功（不误伤）。
	usageFrame := `{"id":"chat-t","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`
	for _, tc := range []struct {
		name     string
		upstream string
	}{
		{"upstream_done", qoderSSEEnvelope(qoderTruncationContentInner, 200) + "\n\n" + "data: [DONE]\n\n"},
		{"finish_reason", qoderSSEEnvelope(qoderCompletedInner, 200)},
		{"usage_tail", qoderSSEEnvelope(qoderTruncationContentInner, 200) + "\n\n" + qoderSSEEnvelope(usageFrame, 200)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := aggregateQoderSSE(strings.NewReader(tc.upstream), "")
			require.NoError(t, err, "有完成证据就是正常响应")
		})
	}
}

// ── 单元级：finish() 的完成证据判据 ──────────────────────────────────────────
func TestQoderReaderFinishRequiresCompletionEvidence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		upstream    string
		wantDone    bool
		wantErrMark bool
	}{
		{
			name:     "upstream_done",
			upstream: qoderSSEEnvelope(qoderTruncationContentInner, 200) + "\n\n" + "data: [DONE]\n\n",
			wantDone: true,
		},
		{
			name:     "finish_reason",
			upstream: qoderSSEEnvelope(qoderCompletedInner, 200),
			wantDone: true,
		},
		{
			name:     "bare_truncation",
			upstream: qoderSSEEnvelope(qoderTruncationContentInner, 200),
			wantDone: false,
		},
		{
			name:        "empty_stream",
			upstream:    ": keep-alive\n\n",
			wantDone:    false,
			wantErrMark: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := string(readAllQoder(t, newQoderSSEReader(strings.NewReader(tc.upstream), "")))
			require.Equal(t, tc.wantDone, strings.Contains(out, "data: [DONE]"), tc.name)
			require.Equal(t, tc.wantErrMark, strings.Contains(out, "empty upstream stream"), tc.name)
		})
	}
}
