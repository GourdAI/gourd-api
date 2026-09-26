//go:build unit

package service

// trae_upstream_error_test.go 回归：Trae 上游「HTTP 200 + 流内 event:error」必须分级落
// 账号状态。修复前面板一律显示正常、调度器持续选用被限额的账号，且该次请求按成功
// 0 token 出账。错误码口径与字典见 trae_upstream_error.go。

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// traeOpsEvents 取出 gin context 里累积的 Ops 上游错误事件。
func traeOpsEvents(t *testing.T, c *gin.Context) []*OpsUpstreamErrorEvent {
	t.Helper()
	raw, ok := c.Get(OpsUpstreamErrorsKey)
	if !ok || raw == nil {
		return nil
	}
	events, ok := raw.([]*OpsUpstreamErrorEvent)
	require.True(t, ok, "Ops 事件类型不符")
	return events
}

// traeErrRecordingRepo 记录 SetTempUnschedulable / SetError 调用。
type traeErrRecordingRepo struct {
	workbuddyTestAccountRepo
	mu           sync.Mutex
	tempCalls    int
	lastUntil    time.Time
	lastTempReas string
	errorCalls   int
	lastErrorMsg string
}

func (r *traeErrRecordingRepo) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tempCalls++
	r.lastUntil = until
	r.lastTempReas = reason
	return nil
}

func (r *traeErrRecordingRepo) SetError(_ context.Context, _ int64, errorMsg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errorCalls++
	r.lastErrorMsg = errorMsg
	return nil
}

func (r *traeErrRecordingRepo) snapshot() (int, time.Time, string, int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tempCalls, r.lastUntil, r.lastTempReas, r.errorCalls, r.lastErrorMsg
}

func newTraeErrTestGinContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	return c
}

func TestClassifyTraeBusinessError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code string
		want traeUpstreamErrorClass
	}{
		{"", traeErrorClassNone},
		{"0", traeErrorClassNone},
		{"  ", traeErrorClassNone},
		{"1005", traeErrorClassQuota},
		{"1001", traeErrorClassSession},
		{"401", traeErrorClassSession},
		{"4010", traeErrorClassRestrict},
		{"4015", traeErrorClassRestrict},
		{"4001", traeErrorClassUnknown}, // 参数无效：请求级语义，不得标坏账号
		{"9028", traeErrorClassUnknown},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, classifyTraeBusinessError(tc.code), "code=%q", tc.code)
	}
}

// 额度类（1005）→ 临时不可调度至次日 0 点（可自动恢复），不得永久置错误态。
func TestMarkTraeAccountQuotaSetsTempUnschedulable(t *testing.T) {
	t.Parallel()
	repo := &traeErrRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{}, repo)
	account := &Account{ID: 901, Platform: PlatformTrae, Name: "trae-a"}

	c := newTraeErrTestGinContext()
	svc.markTraeAccountFromBusinessError(context.Background(), c, account, "1005", "credits exhausted")

	tempCalls, until, reason, errorCalls, _ := repo.snapshot()
	require.Equal(t, 1, tempCalls)
	require.Equal(t, 0, errorCalls, "额度类不得置永久错误态")
	require.Contains(t, reason, "1005")
	require.Contains(t, reason, "credits exhausted")
	require.True(t, until.After(time.Now()), "恢复时间必须在未来")
	require.Equal(t, timezone.StartOfDay(time.Now()).AddDate(0, 0, 1).Format(time.RFC3339),
		until.Format(time.RFC3339), "额度类恢复到次日 0 点")
}

// 会话失效（1001）→ 置错误态（必须人工重新登录，不能靠自动恢复继续调度）。
func TestMarkTraeAccountSessionSetsError(t *testing.T) {
	t.Parallel()
	repo := &traeErrRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{}, repo)
	account := &Account{ID: 902, Platform: PlatformTrae}

	svc.markTraeAccountFromBusinessError(context.Background(), newTraeErrTestGinContext(), account, "1001", "token expired")

	tempCalls, _, _, errorCalls, msg := repo.snapshot()
	require.Equal(t, 0, tempCalls)
	require.Equal(t, 1, errorCalls)
	require.Contains(t, msg, "rejected the session")
	require.Contains(t, msg, "token expired")
}

// 风控类（4010/4015）→ 置错误态并保留码，供管理员判断是否等待自动解除。
func TestMarkTraeAccountRestrictedSetsError(t *testing.T) {
	t.Parallel()
	repo := &traeErrRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{}, repo)
	account := &Account{ID: 903, Platform: PlatformTrae}

	svc.markTraeAccountFromBusinessError(context.Background(), newTraeErrTestGinContext(), account, "4015", "account/ip risk")

	_, _, _, errorCalls, msg := repo.snapshot()
	require.Equal(t, 1, errorCalls)
	require.Contains(t, msg, "restricted")
	require.Contains(t, msg, "4015")
}

// 字典外码不落账号状态（避免把请求级问题误标成账号坏了），但仍留 Ops 痕迹。
func TestMarkTraeAccountUnknownCodeNoStateButOps(t *testing.T) {
	t.Parallel()
	repo := &traeErrRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{}, repo)
	account := &Account{ID: 904, Platform: PlatformTrae}

	c := newTraeErrTestGinContext()
	svc.markTraeAccountFromBusinessError(context.Background(), c, account, "4001", "model not available")

	tempCalls, _, _, errorCalls, _ := repo.snapshot()
	require.Equal(t, 0, tempCalls, "字典外码不得落状态")
	require.Equal(t, 0, errorCalls)
	require.Equal(t, traeErrorClassUnknown, classifyTraeBusinessError("4001"))

	// Ops 事件必须留下（否则面板与错误日志完全不可见），且状态码填 200
	// ——业务错误藏在 HTTP 200 流内，留 0 会让 skip_monitoring 规则永不命中。
	events := traeOpsEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, 200, events[0].UpstreamStatusCode)
	require.Equal(t, "stream_error", events[0].Kind)
	require.Contains(t, events[0].Message, "code=4001")
}

// 无码 / code=0 不是错误：既不落状态也不留痕。
func TestMarkTraeAccountNoCodeIsNoop(t *testing.T) {
	t.Parallel()
	repo := &traeErrRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{}, repo)
	c := newTraeErrTestGinContext()

	svc.markTraeAccountFromBusinessError(context.Background(), c, &Account{ID: 905, Platform: PlatformTrae}, "0", "success")
	svc.markTraeAccountFromBusinessError(context.Background(), c, &Account{ID: 905, Platform: PlatformTrae}, "", "")

	tempCalls, _, _, errorCalls, _ := repo.snapshot()
	require.Equal(t, 0, tempCalls)
	require.Equal(t, 0, errorCalls)
	require.Empty(t, traeOpsEvents(t, c))
}

// 上游重发同一错误帧时只处置一次（SetError 无 SQL 级幂等守卫，重复写会串行阻塞
// SSE 读流热路径）。
func TestMarkTraeAccountDedupsRepeatedFrames(t *testing.T) {
	t.Parallel()
	repo := &traeErrRecordingRepo{}
	svc := newWorkbuddyTestService(&workbuddyTestUpstream{}, repo)
	account := &Account{ID: 906, Platform: PlatformTrae}
	c := newTraeErrTestGinContext()

	svc.markTraeAccountFromBusinessError(context.Background(), c, account, "1001", "token expired")
	svc.markTraeAccountFromBusinessError(context.Background(), c, account, "1001", "token expired")
	svc.markTraeAccountFromBusinessError(context.Background(), c, account, "1001", "token expired")

	_, _, _, errorCalls, _ := repo.snapshot()
	require.Equal(t, 1, errorCalls, "同账号同码只处置一次")
	require.Len(t, traeOpsEvents(t, c), 1)

	// 不同码仍各自处置（不被一次性闸门全拦）。
	svc.markTraeAccountFromBusinessError(context.Background(), c, account, "1005", "credits exhausted")
	_, _, _, errorCalls, _ = repo.snapshot()
	require.Equal(t, 1, errorCalls, "不同码不得重复写 SetError")
}

// 去重粒度按 (账号, 码)：failover 下多账号必须各自能落状态。
func TestTraeMarkBizErrorOnceScopedByAccountAndCode(t *testing.T) {
	t.Parallel()
	c := newTraeErrTestGinContext()
	require.True(t, traeMarkBizErrorOnce(c, 1, "1005"))
	require.False(t, traeMarkBizErrorOnce(c, 1, "1005"))
	require.True(t, traeMarkBizErrorOnce(c, 2, "1005"), "不同账号必须各自放行")
	require.True(t, traeMarkBizErrorOnce(c, 1, "1001"), "同账号不同码必须放行")
	// c==nil（非请求路径）时不得拦。
	require.True(t, traeMarkBizErrorOnce(nil, 1, "1005"))
}

func TestTraeBusinessErrorDetailShape(t *testing.T) {
	t.Parallel()
	require.Equal(t, "boom", traeBusinessErrorDetail("1005", " boom "))
	require.Equal(t, "code 1005", traeBusinessErrorDetail("1005", "   "), "空消息回落码")
	// 中文按 rune 截断，不得切出半个字。
	long := strings.Repeat("额", 300)
	got := traeBusinessErrorDetail("1005", long)
	require.Len(t, []rune(got), 203, "200 rune + 省略号")
	require.True(t, strings.HasSuffix(got, "..."))
	require.Equal(t, "额额额", string([]rune(got)[:3]))
}
