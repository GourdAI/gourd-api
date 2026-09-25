//go:build unit

package service

// qoder_upstream_error_test.go 回归测试：「Qoder 上游限额（110 今日额度已用尽等）
// 但管理面板仍显示正常」根因修复。
//
// 背景（2026-09-22 排查坐实）：Qoder 业务错误走 HTTP 200 + SSE 流内错误帧；请求
// 转发路径此前只透传 CC error 帧、不落账号状态——账号被限额后面板维持「正常」，
// 调度器持续选用。修复 = 按官方错误码字典分级落状态（额度类→临时不可调度至
// 次日 0 点；105/104/108/109→错误态），流式（reader 回调）与非流式（聚合并错误）
// 两条路径全覆盖，账号测试路径共用同一口径。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// --- 分类器纯函数矩阵 ---

func TestClassifyQoderBusinessErrorMatrix(t *testing.T) {
	t.Parallel()
	// 额度/配额类：自动恢复（次日 0 点）。
	for _, code := range []string{"110", "111", "112", "115", "116", "117", "118", "119"} {
		require.Equal(t, qoderErrorClassQuota, classifyQoderBusinessError(code), "code %s 应为额度类", code)
	}
	// 登录态类：需重新登录。
	require.Equal(t, qoderErrorClassSession, classifyQoderBusinessError("105"))
	// 账户/许可类：需管理员处理。
	for _, code := range []string{"104", "108", "109"} {
		require.Equal(t, qoderErrorClassRestrict, classifyQoderBusinessError(code), "code %s 应为账户类", code)
	}
	// 空码 / code "0"：非业务错误。
	require.Equal(t, qoderErrorClassNone, classifyQoderBusinessError(""))
	require.Equal(t, qoderErrorClassNone, classifyQoderBusinessError("0"))
	require.Equal(t, qoderErrorClassNone, classifyQoderBusinessError("  0  "))
	// 字典外错误码（含信封状态码形态）：不落状态。
	// 103/107 属请求级/环境级语义（重复请求 / 网络异常），与 101/102/113/114 同口径，
	// 必须逐码回归以防将来被误加进额度/账户表。
	for _, code := range []string{"999", "101", "102", "103", "107", "113", "114", "403", "418"} {
		require.Equal(t, qoderErrorClassUnknown, classifyQoderBusinessError(code), "code %s 应为字典外", code)
	}
}

// TestQoderInnerBusinessCodeAcceptsNumericCode 回归：上游内层 code 存在字符串型
// （{"code":"110"}）与数字型（{"code":110}）两种 JSON 形态。帧解析层此前只做
// .(string) 断言，数字型错误帧会被当成正常内容帧透传——既不转 CC error 帧、
// 也不触发落状态回调，账号被限额却仍显示「正常」（原始故障形态之一）。
func TestQoderInnerBusinessCodeAcceptsNumericCode(t *testing.T) {
	t.Parallel()
	require.Equal(t, "110", qoderInnerBusinessCode(map[string]any{"code": "110"}), "字符串型")
	require.Equal(t, "110", qoderInnerBusinessCode(map[string]any{"code": float64(110)}), "数字型")
	require.Equal(t, "105", qoderInnerBusinessCode(map[string]any{"code": json.Number("105")}), "json.Number 型")
	// code "0" / 空 / 缺失 / 其它类型：非业务错误。
	require.Equal(t, "", qoderInnerBusinessCode(map[string]any{"code": "0"}))
	require.Equal(t, "", qoderInnerBusinessCode(map[string]any{"code": float64(0)}))
	require.Equal(t, "", qoderInnerBusinessCode(map[string]any{"code": "  "}))
	require.Equal(t, "", qoderInnerBusinessCode(map[string]any{}))
	require.Equal(t, "", qoderInnerBusinessCode(map[string]any{"code": true}))
	require.Equal(t, "", qoderInnerBusinessCode(nil))
}

// TestQoderParseDataFrameNumericBusinessCode 帧解析层：数字型 code 必须被识别为
// 业务错误（而非静默当作内容帧）。
func TestQoderParseDataFrameNumericBusinessCode(t *testing.T) {
	t.Parallel()
	data := qoderParseDataFrame(qoderSSEEnvelope(`{"code":110,"message":"Daily usage limit reached"}`, 200)[len("data: "):])
	require.NotNil(t, data)
	require.NotNil(t, data.BusinessErr, "数字型 code 必须被识别为业务错误")
	require.Equal(t, "110", data.BusinessErr.Code)
	require.Equal(t, "Daily usage limit reached", data.BusinessErr.Message)
}

// TestAggregateQoderSSENumericBusinessCode 非流式聚合层：数字型 code 同样中断聚合
// 并返回结构化信封错误（与流式同口径）。
func TestAggregateQoderSSENumericBusinessCode(t *testing.T) {
	t.Parallel()
	upstream := qoderSSEEnvelope(`{"code":110,"message":"Daily usage limit reached"}`, 200) + "\n\n" + "data: [DONE]\n\n"
	_, _, err := aggregateQoderSSE(strings.NewReader(upstream), "")
	require.Error(t, err, "数字型 code 必须中断聚合")
	require.True(t, isQoderUpstreamEnvelopeError(err))
	var env *errQoderUpstreamEnvelope
	require.True(t, errors.As(err, &env))
	require.Equal(t, "110", env.Code)
}

func TestQoderQuotaRecoveryUntilIsNextMidnight(t *testing.T) {
	t.Parallel()
	now := time.Now()
	until := qoderQuotaRecoveryUntil(now)
	require.True(t, until.After(now), "恢复时间必须在未来")
	// 次日 0 点：与次日的 StartOfDay 相等（全局时区口径）。
	require.True(t, until.Equal(timezone.StartOfDay(now).AddDate(0, 0, 1)))
	// 恰为当日 00:00:00。
	require.Equal(t, 0, until.Hour())
	require.Equal(t, 0, until.Minute())
	require.Equal(t, 0, until.Second())
}

// --- 落状态处置（recording repo） ---

// qoderStateRecordingRepo 记录 SetTempUnschedulable / SetError 调用。
type qoderStateRecordingRepo struct {
	workbuddyTestAccountRepo
	mu           sync.Mutex
	tempCalls    int
	lastUntil    time.Time
	lastTempReas string
	errorCalls   int
	lastErrorMsg string
}

func (r *qoderStateRecordingRepo) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tempCalls++
	r.lastUntil = until
	r.lastTempReas = reason
	return nil
}

func (r *qoderStateRecordingRepo) SetError(_ context.Context, _ int64, errorMsg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errorCalls++
	r.lastErrorMsg = errorMsg
	return nil
}

func (r *qoderStateRecordingRepo) snapshot() (int, time.Time, string, int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tempCalls, r.lastUntil, r.lastTempReas, r.errorCalls, r.lastErrorMsg
}

func TestApplyQoderBusinessErrorAccountStateQuota(t *testing.T) {
	t.Parallel()
	repo := &qoderStateRecordingRepo{}
	account := &Account{ID: 921, Platform: PlatformQoder}
	now := time.Now()

	class := applyQoderBusinessErrorAccountState(context.Background(), repo, account, "110", "You have used up today's request quota.")
	require.Equal(t, qoderErrorClassQuota, class)

	tempCalls, until, reason, errorCalls, _ := repo.snapshot()
	require.Equal(t, 1, tempCalls, "额度类必须落临时不可调度")
	require.Equal(t, 0, errorCalls, "额度类不得落错误态")
	require.True(t, until.After(now), "恢复时间在未来")
	require.True(t, until.Equal(timezone.StartOfDay(now).AddDate(0, 0, 1)), "恢复时间=次日 0 点")
	require.Contains(t, reason, "110")
	require.Contains(t, reason, "quota exhausted")
}

func TestApplyQoderBusinessErrorAccountStateSessionAndRestricted(t *testing.T) {
	t.Parallel()
	// 105 登录过期：错误态 + 会话文案（与既有测试路径口径一致）。
	repo := &qoderStateRecordingRepo{}
	account := &Account{ID: 922, Platform: PlatformQoder}
	class := applyQoderBusinessErrorAccountState(context.Background(), repo, account, "105", "Login expired")
	require.Equal(t, qoderErrorClassSession, class)
	tempCalls, _, _, errorCalls, msg := repo.snapshot()
	require.Equal(t, 0, tempCalls)
	require.Equal(t, 1, errorCalls, "105 必须落错误态")
	require.Contains(t, msg, "Login expired")
	require.Contains(t, msg, "rejected the session")

	// 109 应用被禁用：错误态。
	repo2 := &qoderStateRecordingRepo{}
	class2 := applyQoderBusinessErrorAccountState(context.Background(), repo2, &Account{ID: 923, Platform: PlatformQoder}, "109", "Application disabled")
	require.Equal(t, qoderErrorClassRestrict, class2)
	_, _, _, errorCalls2, msg2 := repo2.snapshot()
	require.Equal(t, 1, errorCalls2)
	require.Contains(t, msg2, "109")
}

func TestApplyQoderBusinessErrorAccountStateUnknownNoop(t *testing.T) {
	t.Parallel()
	repo := &qoderStateRecordingRepo{}
	account := &Account{ID: 924, Platform: PlatformQoder}
	// 字典外/空码：零落状态（仅返回分类）。
	require.Equal(t, qoderErrorClassUnknown, applyQoderBusinessErrorAccountState(context.Background(), repo, account, "999", "whatever"))
	require.Equal(t, qoderErrorClassNone, applyQoderBusinessErrorAccountState(context.Background(), repo, account, "", ""))
	tempCalls, _, _, errorCalls, _ := repo.snapshot()
	require.Equal(t, 0, tempCalls)
	require.Equal(t, 0, errorCalls)

	// nil repo / nil account：仍返回分类供调用方观测，但不落状态（零副作用）。
	require.Equal(t, qoderErrorClassQuota, applyQoderBusinessErrorAccountState(context.Background(), nil, account, "110", "quota"))
	require.Equal(t, qoderErrorClassQuota, applyQoderBusinessErrorAccountState(context.Background(), repo, nil, "110", "quota"))
	tempCalls2, _, _, errorCalls2, _ := repo.snapshot()
	require.Equal(t, 0, tempCalls2)
	require.Equal(t, 0, errorCalls2)
}

// --- 内嵌码解析（信封级错误的嵌套形态） ---

func TestResolveQoderBusinessErrorCodeEmbedded(t *testing.T) {
	t.Parallel()
	// 直接携带的字典码优先。
	code, class := resolveQoderBusinessErrorCode("110", "whatever")
	require.Equal(t, "110", code)
	require.Equal(t, qoderErrorClassQuota, class)

	// 信封级错误：非字典码（如信封状态码）但消息体内嵌 {"code":"110"}。
	code2, class2 := resolveQoderBusinessErrorCode("403", `{"code":"110","message":"Daily usage limit reached"}`)
	require.Equal(t, "110", code2, "内嵌业务码必须被提取")
	require.Equal(t, qoderErrorClassQuota, class2)

	// 数字型 code 同样提取。
	code3, class3 := resolveQoderBusinessErrorCode("418", `{"code":105}`)
	require.Equal(t, "105", code3)
	require.Equal(t, qoderErrorClassSession, class3)

	// 普通文本消息不误判。
	code4, class4 := resolveQoderBusinessErrorCode("999", "some quota exceeded text")
	require.Equal(t, "999", code4)
	require.Equal(t, qoderErrorClassUnknown, class4)

	// 非 JSON 文本 / 空消息：返回原码与原分类。
	code5, class5 := resolveQoderBusinessErrorCode("", "plain text")
	require.Equal(t, "", code5)
	require.Equal(t, qoderErrorClassNone, class5)
}

func TestApplyQoderBusinessErrorEmbeddedCodeMarksTempUnschedulable(t *testing.T) {
	t.Parallel()
	repo := &qoderStateRecordingRepo{}
	account := &Account{ID: 925, Platform: PlatformQoder}
	// 信封级错误形态：外层码是信封状态码（非字典），业务码在内嵌 JSON 中。
	class := applyQoderBusinessErrorAccountState(context.Background(), repo, account,
		"418", `{"code":"110","message":"Daily usage limit reached"}`)
	require.Equal(t, qoderErrorClassQuota, class, "内嵌 110 必须被识别为额度类")
	tempCalls, _, reason, errorCalls, _ := repo.snapshot()
	require.Equal(t, 1, tempCalls, "内嵌 110 必须落临时不可调度")
	require.Equal(t, 0, errorCalls)
	require.Contains(t, reason, "110")
}

// --- 白盒：SSE reader 回调 ---

func TestQoderSSEReaderErrorHookFiresOnBusinessError(t *testing.T) {
	t.Parallel()
	upstream := qoderSSEEnvelope(`{"code":"110","message":"Daily usage limit reached"}`, 200) + "\n\n" +
		"data: [DONE]\n\n"
	var gotCodes []string
	var gotMsgs []string
	var mu sync.Mutex
	reader := newQoderSSEReaderWithErrorHook(strings.NewReader(upstream), "", func(code, message string) {
		mu.Lock()
		defer mu.Unlock()
		gotCodes = append(gotCodes, code)
		gotMsgs = append(gotMsgs, message)
	})
	out := string(readAllQoder(t, reader))
	require.Contains(t, out, `"code":"110"`, "CC error 帧照常透传")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"110"}, gotCodes, "回调必须收到业务错误码")
	require.Equal(t, []string{"Daily usage limit reached"}, gotMsgs)
}

func TestQoderSSEReaderNilHookCompatible(t *testing.T) {
	t.Parallel()
	// nil 回调（旧构造签名）行为不变：不 panic、帧照常透传。
	upstream := qoderSSEEnvelope(`{"code":"110","message":"x"}`, 200) + "\n\n" + "data: [DONE]\n\n"
	out := string(readAllQoder(t, newQoderSSEReader(strings.NewReader(upstream), "")))
	require.Contains(t, out, `"code":"110"`)
}

// --- 端到端：出站发送路径（httptest 假上游） ---

// qoderStateErrorUpstream 起一个假 Qoder 上游：POST chat 回错误帧信封流。
func qoderStateErrorUpstream(t *testing.T, code, message string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "agent_chat_generation") {
			w.Header().Set("Content-Type", "text/event-stream")
			inner := fmt.Sprintf(`{"code":%q,"message":%q}`, code, message)
			env := map[string]any{"headers": map[string]any{}, "body": inner, "statusCodeValue": 200}
			raw, _ := json.Marshal(env)
			_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestSendQoderUpstreamStreamingQuotaErrorMarksAccount 核心回归（用户场景）：
// 流式请求收到 HTTP 200 + 流内 110 错误帧 → 账号被标记临时不可调度（次日 0 点），
// 面板不再误报「正常」。
func TestSendQoderUpstreamStreamingQuotaErrorMarksAccount(t *testing.T) {
	t.Parallel()
	server := qoderStateErrorUpstream(t, "110", "You have used up today's request quota. Please try again tomorrow.")
	repo := &qoderStateRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := &Account{
		ID:       931,
		Platform: PlatformQoder,
		Credentials: map[string]any{
			"access_token": "dt-quota",
			"uid":          "uid-q1",
			"base_url":     server.URL,
		},
	}

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendQoderUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"auto","messages":[{"role":"user","content":"ping"}]}`), true)
	require.NoError(t, err)
	defer resp.Body.Close()
	// 消费规范化流：触发 reader 回调（真实网关中由下游读流驱动）。
	body, _ := io.ReadAll(resp.Body)

	tempCalls, until, reason, errorCalls, _ := repo.snapshot()
	require.Equal(t, 1, tempCalls, "流内 110 错误帧必须落临时不可调度")
	require.Equal(t, 0, errorCalls, "额度类不得落错误态")
	require.True(t, until.Equal(timezone.StartOfDay(time.Now()).AddDate(0, 0, 1)), "恢复时间=次日 0 点")
	require.Contains(t, reason, "110")

	// 客户端仍收到兼容的 CC error 帧（落状态不得改变对外协议），且恰好一个 [DONE]。
	require.Contains(t, string(body), `"error"`, "必须透传 CC error 帧")
	require.Contains(t, string(body), `"code":"110"`)
	require.Equal(t, 1, strings.Count(string(body), "data: [DONE]"), "恰好一个 [DONE]")

	// Ops 上游错误事件必须落库（此前无任何断言守护，整段 append 可被删而测试全过）。
	opsEvents := requireQoderOpsEvents(t, c, 1)
	require.Equal(t, http.StatusOK, opsEvents[0].UpstreamStatusCode,
		"真实传输码必须是 200（非 0 才能让 skip_monitoring 透传规则命中）")
	require.Equal(t, "stream_error", opsEvents[0].Kind, "流内错误帧用 stream_error 口径")
	require.Equal(t, string(qoderErrorClassQuota), opsEvents[0].Reason)
	require.Equal(t, int64(931), opsEvents[0].AccountID)
	require.Contains(t, opsEvents[0].Message, "code=110")
	require.Contains(t, opsEvents[0].Detail, "quota", "Detail 需携带上游原文供关键字匹配")
}

// requireQoderOpsEvents 读取并断言 gin context 中的 Ops 上游错误事件数量。
func requireQoderOpsEvents(t *testing.T, c *gin.Context, want int) []*OpsUpstreamErrorEvent {
	t.Helper()
	raw, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok, "必须记录 Ops 上游错误事件")
	events, ok := raw.([]*OpsUpstreamErrorEvent)
	require.True(t, ok, "Ops 事件类型不符")
	require.Len(t, events, want)
	return events
}

// TestSendQoderUpstreamStreamingSessionErrorMarksAccountEvent 流式 105 → 错误态。
func TestSendQoderUpstreamStreamingSessionErrorMarksAccount(t *testing.T) {
	t.Parallel()
	server := qoderStateErrorUpstream(t, "105", "Login expired")
	repo := &qoderStateRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := &Account{
		ID:       932,
		Platform: PlatformQoder,
		Credentials: map[string]any{
			"access_token": "dt-session",
			"uid":          "uid-q2",
			"base_url":     server.URL,
		},
	}

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendQoderUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"auto","messages":[{"role":"user","content":"ping"}]}`), true)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	tempCalls, _, _, errorCalls, msg := repo.snapshot()
	require.Equal(t, 0, tempCalls)
	require.Equal(t, 1, errorCalls, "流内 105 必须落错误态")
	require.Contains(t, msg, "Login expired")
}

// TestSendQoderUpstreamNonStreamingQuotaErrorMarksAccount 非流式聚合路径同样落状态。
func TestSendQoderUpstreamNonStreamingQuotaErrorMarksAccount(t *testing.T) {
	t.Parallel()
	server := qoderStateErrorUpstream(t, "110", "Daily usage limit reached")
	repo := &qoderStateRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := &Account{
		ID:       933,
		Platform: PlatformQoder,
		Credentials: map[string]any{
			"access_token": "dt-nonstream",
			"uid":          "uid-q3",
			"base_url":     server.URL,
		},
	}

	c := newWorkbuddyTestGinContext()
	_, err := svc.sendQoderUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"auto","messages":[{"role":"user","content":"ping"}]}`), false)
	require.Error(t, err, "非流式聚合遇业务错误返回结构化错误")
	require.True(t, isQoderUpstreamEnvelopeError(err))

	tempCalls, until, reason, errorCalls, _ := repo.snapshot()
	require.Equal(t, 1, tempCalls, "非流式路径 110 必须落临时不可调度")
	require.Equal(t, 0, errorCalls)
	require.True(t, until.Equal(timezone.StartOfDay(time.Now()).AddDate(0, 0, 1)))
	require.Contains(t, reason, "110")

	opsEvents := requireQoderOpsEvents(t, c, 1)
	require.Equal(t, http.StatusOK, opsEvents[0].UpstreamStatusCode)
	require.Equal(t, "stream_error", opsEvents[0].Kind)
	require.Contains(t, opsEvents[0].Message, "code=110")
}

// TestProcessQoderTestStreamQuotaErrorMarksTempUnschedulable 测试路径口径统一：
// 110 → 临时不可调度（此前无条件 SetError）；105 → 错误态（回归保护）。
func TestProcessQoderTestStreamQuotaErrorMarksTempUnschedulable(t *testing.T) {
	t.Parallel()
	repo := &qoderStateRecordingRepo{}
	svc := &AccountTestService{accountRepo: repo}
	account := &Account{ID: 934, Platform: PlatformQoder, Credentials: map[string]any{"access_token": "dt-x"}}

	c := newWorkbuddyTestGinContext()
	body := strings.NewReader(`data: {"error":{"code":"110","message":"Daily usage limit reached"}}` + "\n\n")
	err := svc.processQoderTestStream(c, account, body)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Daily usage limit reached")

	tempCalls, _, reason, errorCalls, _ := repo.snapshot()
	require.Equal(t, 1, tempCalls, "测试路径 110 走临时不可调度（口径与请求路径统一）")
	require.Equal(t, 0, errorCalls)
	require.Contains(t, reason, "110")
}

// --- 本次 code review 补强：数字型 code 端到端 / 重复帧去重 / unknown 码留痕 ---

// qoderNumericCodeUpstream 假上游：发**数字型** code 的错误帧（{"code":110} 而非
// {"code":"110"}），errorFrames 控制重发次数（验证去重）。
func qoderNumericCodeUpstream(t *testing.T, code int, message string, errorFrames int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "agent_chat_generation") {
			w.Header().Set("Content-Type", "text/event-stream")
			inner := fmt.Sprintf(`{"code":%d,"message":%q}`, code, message)
			env := map[string]any{"headers": map[string]any{}, "body": inner, "statusCodeValue": 200}
			raw, _ := json.Marshal(env)
			for i := 0; i < max(errorFrames, 1); i++ {
				_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestSendQoderUpstreamNumericCodeMarksAccount 回归（review P0-1）：数字型内层 code
// 必须走完整链路落状态。修复前帧解析层只做 .(string) 断言，数字型错误帧被当
// 正常内容帧透传 → hook 不触发 → 账号仍显示「正常」。
func TestSendQoderUpstreamNumericCodeMarksAccount(t *testing.T) {
	t.Parallel()
	server := qoderNumericCodeUpstream(t, 110, "Daily usage limit reached", 1)
	repo := &qoderStateRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := &Account{
		ID:       935,
		Platform: PlatformQoder,
		Credentials: map[string]any{
			"access_token": "dt-numeric",
			"uid":          "uid-q5",
			"base_url":     server.URL,
		},
	}

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendQoderUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"auto","messages":[{"role":"user","content":"ping"}]}`), true)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	require.Contains(t, string(body), `"code":"110"`, "数字型 code 必须被转成 CC error 帧")
	tempCalls, _, reason, errorCalls, _ := repo.snapshot()
	require.Equal(t, 1, tempCalls, "数字型 110 必须落临时不可调度")
	require.Equal(t, 0, errorCalls)
	require.Contains(t, reason, "110")
	requireQoderOpsEvents(t, c, 1)
}

// TestSendQoderUpstreamRepeatedErrorFramesMarkOnce 回归（review P0-3）：上游重发错误帧
// 时，同一请求内只处置一次。SetError（105/104/108/109）没有 SetTempUnschedulable
// 那样的 SQL 级幂等守卫，每帧都做一次 UPDATE + outbox + GetByID + Redis 写，
// 全部串行阻塞在 SSE 读流热路径上；Ops 事件重复 append 还会挤掉同请求内
// 其它账号的 failover 记录（入库上限从新往回保留）。
func TestSendQoderUpstreamRepeatedErrorFramesMarkOnce(t *testing.T) {
	t.Parallel()
	server := qoderNumericCodeUpstream(t, 105, "Login expired", 3)
	repo := &qoderStateRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := &Account{
		ID:       936,
		Platform: PlatformQoder,
		Credentials: map[string]any{
			"access_token": "dt-dup",
			"uid":          "uid-q6",
			"base_url":     server.URL,
		},
	}

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendQoderUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"auto","messages":[{"role":"user","content":"ping"}]}`), true)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// 帧透传行为不变（修复前就每帧输出一条 error 帧），仅**处置**去重。
	require.Equal(t, 3, strings.Count(string(body), `"code":"105"`), "错误帧仍逐帧透传（对外协议不变）")
	_, _, _, errorCalls, msg := repo.snapshot()
	require.Equal(t, 1, errorCalls, "重发错误帧只写一次库")
	require.Contains(t, msg, "Login expired")
	requireQoderOpsEvents(t, c, 1)
}

// TestMarkQoderAccountUnknownCodeStillRecordsOps 回归（review P1-6）：字典外码不落
// 账号状态（避免误标），但必须留 Ops 痕迹——否则信封级错误
// （statusCodeValue=403/418/429/500）在面板与错误日志里完全不可见，
// 与 HTTP>=400 路径（无条件写 Ops）口径分叉。
func TestMarkQoderAccountUnknownCodeStillRecordsOps(t *testing.T) {
	t.Parallel()
	repo := &qoderStateRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{}, repo)
	account := &Account{ID: 937, Platform: PlatformQoder}

	c := newWorkbuddyTestGinContext()
	svc.markQoderAccountFromBusinessError(context.Background(), c, account, "418", "quota exceeded")

	tempCalls, _, _, errorCalls, _ := repo.snapshot()
	require.Equal(t, 0, tempCalls, "字典外码不得落状态")
	require.Equal(t, 0, errorCalls)

	opsEvents := requireQoderOpsEvents(t, c, 1)
	require.Equal(t, http.StatusOK, opsEvents[0].UpstreamStatusCode, "非 0 状态码才能命中透传规则")
	require.Equal(t, string(qoderErrorClassUnknown), opsEvents[0].Reason)
	require.Contains(t, opsEvents[0].Message, "code=418")

	// 同一码重复上报不重复留痕。
	svc.markQoderAccountFromBusinessError(context.Background(), c, account, "418", "quota exceeded")
	requireQoderOpsEvents(t, c, 1)

	// 不同码仍各自留痕（不能一刷到底）。
	svc.markQoderAccountFromBusinessError(context.Background(), c, account, "429", "rate limited")
	requireQoderOpsEvents(t, c, 2)
}

// TestQoderMarkBizErrorOnceDedupsPerAccountAndCode 去重粒度：按 (账号, 有效码)
// 而非整个请求——failover 场景下不同账号必须各自能落状态与留痕。
func TestQoderMarkBizErrorOnceDedupsPerAccountAndCode(t *testing.T) {
	t.Parallel()
	c := newWorkbuddyTestGinContext()
	require.True(t, qoderMarkBizErrorOnce(c, 1, "110"), "首次必须放行")
	require.False(t, qoderMarkBizErrorOnce(c, 1, "110"), "同账号同码重复必须拦下")
	require.True(t, qoderMarkBizErrorOnce(c, 1, "105"), "同账号不同码放行")
	require.True(t, qoderMarkBizErrorOnce(c, 2, "110"), "不同账号放行（failover 不能互相抦掉）")
	// c == nil 时不 panic、恒放行（无去重载体可存）。
	require.True(t, qoderMarkBizErrorOnce(nil, 1, "110"))
}

// TestApplyQoderBusinessErrorClassUsesDetachedContextWithTimeout 回归（review P0-2）：
// 入站 ctx 被取消（管理员关掉测试页面 / 客户端断开）后仍须能落标记，
// 且必须带上超时（否则 DB 卡顿会挂住 SSE 读流热路径）。
func TestApplyQoderBusinessErrorClassUsesDetachedContextWithTimeout(t *testing.T) {
	t.Parallel()
	repo := &qoderCtxRecordingRepo{}
	account := &Account{ID: 938, Platform: PlatformQoder}

	parent, cancel := context.WithCancel(context.Background())
	cancel() // 模拟客户端已断开

	class := applyQoderBusinessErrorAccountState(parent, repo, account, "110", "quota")
	require.Equal(t, qoderErrorClassQuota, class)

	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Equal(t, 1, repo.tempCalls, "入站 ctx 已取消仍须落标记")
	require.NoError(t, repo.lastCtxErr, "落状态的 ctx 不得继承已取消信号")
	require.True(t, repo.lastCtxHadDeadline, "必须带 deadline，否则 DB 卡顿会挂住 SSE 转发")
	deadline := repo.lastCtxDeadline
	require.True(t, deadline.After(time.Now()), "deadline 应在未来")
	require.True(t, time.Until(deadline) <= openAIAccountStateUpdateTimeout,
		"超时口径应与仓库既有账号状态写一致（%s）", openAIAccountStateUpdateTimeout)
}

// qoderCtxRecordingRepo 除计数外，额外捕获落状态时收到的 ctx 状态。
type qoderCtxRecordingRepo struct {
	workbuddyTestAccountRepo
	mu                 sync.Mutex
	tempCalls          int
	lastCtxErr         error
	lastCtxDeadline    time.Time
	lastCtxHadDeadline bool
}

func (r *qoderCtxRecordingRepo) SetTempUnschedulable(ctx context.Context, _ int64, _ time.Time, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tempCalls++
	r.lastCtxErr = ctx.Err()
	r.lastCtxDeadline, r.lastCtxHadDeadline = ctx.Deadline()
	return nil
}

func (r *qoderCtxRecordingRepo) SetError(ctx context.Context, _ int64, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastCtxErr = ctx.Err()
	r.lastCtxDeadline, r.lastCtxHadDeadline = ctx.Deadline()
	return nil
}
