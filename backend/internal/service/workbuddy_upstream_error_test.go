//go:build unit

package service

// workbuddy_upstream_error_test.go 回归：WorkBuddy 上游错误必须分级落账号状态。
// 修复前它是 Trae/Qoder/WorkBuddy 三平台里唯一没有任何事后冷却路径的：号子撞上
// 「今日额度已用尽」后除了等下一次手动探测刷新快照，没有任何东西让它退出调度。
// 错误码字典与纪律见 workbuddy_upstream_error.go（字典外码一律不落状态）。

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// workbuddyErrRecordingRepo 记录 SetTempUnschedulable / SetError 调用。
type workbuddyErrRecordingRepo struct {
	workbuddyTestAccountRepo
	mu         sync.Mutex
	tempCalls  int
	lastUntil  time.Time
	lastReason string
	errCalls   int
	lastErrMsg string
}

func (r *workbuddyErrRecordingRepo) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tempCalls++
	r.lastUntil = until
	r.lastReason = reason
	return nil
}

func (r *workbuddyErrRecordingRepo) SetError(_ context.Context, _ int64, errorMsg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errCalls++
	r.lastErrMsg = errorMsg
	return nil
}

func (r *workbuddyErrRecordingRepo) snapshot() (int, time.Time, string, int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tempCalls, r.lastUntil, r.lastReason, r.errCalls, r.lastErrMsg
}

func newWorkbuddyErrTestGinContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	return c
}

func workbuddyOpsEvents(t *testing.T, c *gin.Context) []*OpsUpstreamErrorEvent {
	t.Helper()
	raw, ok := c.Get(OpsUpstreamErrorsKey)
	if !ok || raw == nil {
		return nil
	}
	events, ok := raw.([]*OpsUpstreamErrorEvent)
	require.True(t, ok, "Ops 事件类型不符")
	return events
}

func TestClassifyWorkbuddyBillingError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code int
		want workbuddyUpstreamErrorClass
	}{
		{0, workbuddyErrorClassNone},
		{110, workbuddyErrorClassQuota},     // 实测：今日额度已用尽
		{401, workbuddyErrorClassSession},   // 实测样本存在（业务码形态）
		{10001, workbuddyErrorClassUnknown}, // 今天已签到：幂等业务态，不是账号坏了
		{14001, workbuddyErrorClassUnknown},
		{404, workbuddyErrorClassUnknown},
		{40001, workbuddyErrorClassUnknown}, // refresh token expired：token 域自行处理，不在此升级
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, classifyWorkbuddyBillingError(tc.code), "code=%d", tc.code)
	}
}

// chat 域码表**刻意留空**：6004 的字面语义是 rate limited（瞬时/请求级），没有任何
// 「额度耗尽」证据。字典外必须 unknown → 由调用方落 Ops 痕迹但不落账号状态。
// 往这张表里加码的唯一合法途径是拿到真实上游耗尽样本。
func TestClassifyWorkbuddyChatErrorIsUnknownForUncodedCodes(t *testing.T) {
	t.Parallel()
	require.Equal(t, workbuddyErrorClassNone, classifyWorkbuddyChatError(""))
	require.Equal(t, workbuddyErrorClassNone, classifyWorkbuddyChatError("0"))
	require.Equal(t, workbuddyErrorClassUnknown, classifyWorkbuddyChatError("6004"))
	require.Equal(t, workbuddyErrorClassUnknown, classifyWorkbuddyChatError("110"),
		"billing 的 110 不得跨域生效：chat 域没有该码的实测样本")
}

// A 侧：billing 探测 code 110 → 临时不可调度至次日 0 点（可自动恢复），不得永久置错。
func TestMarkWorkbuddyBillingQuotaSetsTempUnschedulable(t *testing.T) {
	t.Parallel()
	repo := &workbuddyErrRecordingRepo{}
	account := &Account{ID: 921, Platform: PlatformWorkbuddy, Name: "wb-a"}

	markWorkbuddyAccountFromBillingError(context.Background(), repo, account,
		&workbuddyBillingError{Status: 200, Code: 110, Msg: "今日额度已用尽"})

	tempCalls, until, reason, errorCalls, _ := repo.snapshot()
	require.Equal(t, 1, tempCalls)
	require.Equal(t, 0, errorCalls, "额度类不得置永久错误态")
	require.Contains(t, reason, "110")
	require.Contains(t, reason, "今日额度已用尽")
	require.True(t, until.After(time.Now()), "恢复时间必须在未来")
	require.Equal(t, timezone.StartOfDay(time.Now()).AddDate(0, 0, 1).Format(time.RFC3339),
		until.Format(time.RFC3339), "额度类恢复到次日 0 点（与 trae/qoder 同口径）")
}

// billing 域登录态码 401 → 置错误态（必须人工重新登录，不能靠自动恢复继续调度）。
func TestMarkWorkbuddyBillingSessionSetsError(t *testing.T) {
	t.Parallel()
	repo := &workbuddyErrRecordingRepo{}
	account := &Account{ID: 922, Platform: PlatformWorkbuddy}

	markWorkbuddyAccountFromBillingError(context.Background(), repo, account,
		&workbuddyBillingError{Status: 200, Code: 401, Msg: "unauthorized"})

	tempCalls, _, _, errorCalls, msg := repo.snapshot()
	require.Equal(t, 0, tempCalls)
	require.Equal(t, 1, errorCalls)
	require.Contains(t, msg, "rejected the session")
	require.Contains(t, msg, "unauthorized")
}

// 字典外码（10001 今天已签到）与传输错误都不得动账号状态 —— 把幂等业务态标成
// 账号坏了，比不拦更糟。
func TestMarkWorkbuddyBillingUnknownCodeDoesNotTouchAccount(t *testing.T) {
	t.Parallel()
	repo := &workbuddyErrRecordingRepo{}
	account := &Account{ID: 923, Platform: PlatformWorkbuddy}

	markWorkbuddyAccountFromBillingError(context.Background(), repo, account,
		&workbuddyBillingError{Status: 200, Code: 10001, Msg: "今天已签到"})
	markWorkbuddyAccountFromBillingError(context.Background(), repo, account,
		&workbuddyBillingError{Status: 404, Code: 0, Msg: "not found"})
	markWorkbuddyAccountFromBillingError(context.Background(), repo, account,
		context.DeadlineExceeded) // 非 billing 错误类型：完全不参与处置

	tempCalls, _, _, errorCalls, _ := repo.snapshot()
	require.Zero(t, tempCalls)
	require.Zero(t, errorCalls)
}

// B 侧：SSE 规范化读取器遇到**上游** error 帧时回调，且帧本身原样透传（对外协议不变）。
func TestWorkbuddySSEReaderInvokesErrorFrameHook(t *testing.T) {
	t.Parallel()
	var gotCode, gotMsg string
	hook := func(code, message string) { gotCode, gotMsg = code, message }

	input := "data: {\"error\":{\"message\":\"rate limited\",\"code\":\"6004\"}}\n\n" +
		"data: [DONE]\n\n"
	out, err := io.ReadAll(newWorkbuddySSEReaderWithHook(strings.NewReader(input), hook))
	require.NoError(t, err)

	require.Equal(t, "6004", gotCode)
	require.Equal(t, "rate limited", gotMsg)
	// 透传不被处置破坏：error 帧与 [DONE] 都必须在输出里。
	require.Contains(t, string(out), `"code":"6004"`)
	require.Contains(t, string(out), "data: [DONE]")
}

// 网关自合成的空流兜底帧（本地缺陷，code=upstream_parse）不得触发处置回调 ——
// 否则「上游没吐内容」会被误报成「账号额度耗尽」，把好号打下线。
func TestWorkbuddySSELocalEmptyStreamFrameDoesNotInvokeHook(t *testing.T) {
	t.Parallel()
	called := false
	hook := func(code, message string) { called = true }

	out, err := io.ReadAll(newWorkbuddySSEReaderWithHook(strings.NewReader(""), hook))
	require.NoError(t, err)
	require.False(t, called, "本地合成帧不得进入上游处置")
	require.Contains(t, string(out), "upstream_parse", "兜底帧本身仍要照常产出")
}

// B 侧接线：chat 流内 error 帧 → Ops 事件可见但不落账号状态（码表为空期）。
// 非流式聚合路径同样必须能处置上游 error 帧：客户端要非流式时，
// aggregateWorkbuddySSE 是消费上游 SSE 的唯一入口，不接就是半成品。
func TestAggregateWorkbuddySSEInvokesErrorFrameHook(t *testing.T) {
	t.Parallel()
	var codes []string
	hook := func(code, message string) { codes = append(codes, code) }

	input := "data: {\"error\":{\"message\":\"rate limited\",\"code\":\"6004\"}}\n\n" +
		"data: [DONE]\n\n"
	body, _, err := aggregateWorkbuddySSEWithHook(strings.NewReader(input), hook)
	require.NoError(t, err)
	require.Equal(t, []string{"6004"}, codes, "非流式路径也必须回调处置")
	// 响应体语义不变（本次只补处置与观测，不改对外行为）。
	require.NotEmpty(t, body)
}

// 公用解析器必须同时兼容 string 与数字两种 code 形态（实测两域各用一种）。
func TestWorkbuddyErrorFrameFieldsCodeForms(t *testing.T) {
	t.Parallel()
	code, msg, ok := workbuddyErrorFrameFields(map[string]any{
		"error": map[string]any{"code": "6004", "message": "rate limited"},
	})
	require.True(t, ok)
	require.Equal(t, "6004", code)
	require.Equal(t, "rate limited", msg)

	// 数字形态（billing 信封顶层 code 就是 int）
	code, _, ok = workbuddyErrorFrameFields(map[string]any{
		"error": map[string]any{"code": float64(110), "message": "quota"},
	})
	require.True(t, ok)
	require.Equal(t, "110", code)

	// error 为字符串形态：无码但保留消息，不得静默丢弃
	code, msg, ok = workbuddyErrorFrameFields(map[string]any{"error": "boom"})
	require.True(t, ok)
	require.Empty(t, code)
	require.Equal(t, "boom", msg)

	_, _, ok = workbuddyErrorFrameFields(map[string]any{"choices": []any{}})
	require.False(t, ok, "正常帧不得误判为 error 帧")
	_, _, ok = workbuddyErrorFrameFields(map[string]any{"error": map[string]any{}})
	require.False(t, ok, "空 error 对象无可处置信息")
}

func TestWorkbuddyStreamErrorHandlerRecordsOpsWithoutStateChange(t *testing.T) {
	t.Parallel()
	repo := &workbuddyErrRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{}, repo)
	account := &Account{ID: 924, Platform: PlatformWorkbuddy}
	c := newWorkbuddyErrTestGinContext()

	hook := svc.workbuddyStreamErrorHandler(context.Background(), c, account)
	require.NotNil(t, hook)
	out, err := io.ReadAll(newWorkbuddySSEReaderWithHook(
		strings.NewReader("data: {\"error\":{\"message\":\"rate limited\",\"code\":\"6004\"}}\n\ndata: [DONE]\n\n"), hook))
	require.NoError(t, err)
	require.NotEmpty(t, out)

	tempCalls, _, _, errorCalls, _ := repo.snapshot()
	require.Zero(t, tempCalls, "字典外码不得落状态（码表纪律）")
	require.Zero(t, errorCalls)

	events := workbuddyOpsEvents(t, c)
	require.Len(t, events, 1, "字典外码不落状态但必须留 Ops 痕迹")
	require.Equal(t, 200, events[0].UpstreamStatusCode, "填 0 会让 skip_monitoring 规则永久失效")
	require.Equal(t, "stream_error", events[0].Kind)
	require.Equal(t, "workbuddy_upstream_frame", events[0].Stage)
	require.Equal(t, string(workbuddyErrorClassUnknown), events[0].Reason)
	require.Contains(t, events[0].Message, "6004")
}

// 同一请求内重复错误帧只处置一次（去重），第二次连 Ops 痕迹也不重复写。
func TestWorkbuddyStreamErrorHandlerDedupsPerRequest(t *testing.T) {
	t.Parallel()
	repo := &workbuddyErrRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{}, repo)
	account := &Account{ID: 925, Platform: PlatformWorkbuddy}
	c := newWorkbuddyErrTestGinContext()

	hook := svc.workbuddyStreamErrorHandler(context.Background(), c, account)
	hook("6004", "rate limited")
	hook("6004", "rate limited")
	hook("6005", "another code") // 不同码是独立处置单元

	events := workbuddyOpsEvents(t, c)
	require.Len(t, events, 2, "去重按 (账号,码) 粒度，不同码各自一条")
}
