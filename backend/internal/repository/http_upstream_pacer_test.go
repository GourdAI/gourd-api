package repository

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// 本文件补齐「http_upstream 的 Do / DoWithTLS 实际调用 paceAccount」这一行为缺口：
// 此前 account_pacer_test.go 只验证 pacer 自身语义，没有任何测试证明请求路径真的接上了它，
// 一旦有人误删 Do 中的 paceAccount 调用，全部现有测试仍会通过。

// newHTTPUpstreamPacerTestService 构造仅启用账号节奏控制的上游服务。
//
// 不走 NewHTTPUpstream：cfg 保持 nil 即可满足 Do 的调用链——
// pacing 发生在任何 cfg 解引用之前，且 shouldValidateResolvedIP / getIsolationMode /
// maxUpstreamClients / clientIdleTTL / defaultPoolSettings 对 nil cfg 均有安全回退。
// clients map 必须初始化：客户端缓存会在 Do 的慢路径写入。
func newHTTPUpstreamPacerTestService(minInterval, jitter time.Duration) *httpUpstreamService {
	return &httpUpstreamService{
		clients: make(map[string]*upstreamClientEntry),
		pacer:   newAccountPacer(minInterval, jitter),
	}
}

// newHTTPUpstreamPacerTestServer 启动一个最小的本地上游，避免测试依赖外网。
func newHTTPUpstreamPacerTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestHTTPUpstreamPacerDoAppliesAccountPacing 证明 Do 在出站前应用了账号级节奏控制。
//
// 先用 paceAccount 预约一个槽位（无 I/O 开销，保证测量起点干净），随后同一账号的 Do
// 必须等到该槽位到期才能发出；若 Do 内部不再调用 paceAccount，耗时会立刻回到毫秒级。
func TestHTTPUpstreamPacerDoAppliesAccountPacing(t *testing.T) {
	const interval = 100 * time.Millisecond
	svc := newHTTPUpstreamPacerTestService(interval, 0)
	srv := newHTTPUpstreamPacerTestServer(t)
	ctx := t.Context()

	require.NoError(t, svc.paceAccount(ctx, 71), "pre-reserving a slot must succeed")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	start := time.Now()
	resp, err := svc.Do(req, "", 71, 1)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	// 下限取 interval 的 80%，容忍调度与计时器精度误差（timer 不会提前触发，只会偏晚）。
	require.GreaterOrEqual(t, elapsed, interval*4/5,
		"Do must honor the pacing slot reserved for the same account, elapsed=%v", elapsed)
}

// TestHTTPUpstreamPacerDoWithTLSUsesSharedPacing DoWithTLS 的 plain HTTP 分支复用 Do，
// 必须与 Do 共享同一套账号节奏状态（同一账号跨两种调用形态仍受最小间隔约束）。
func TestHTTPUpstreamPacerDoWithTLSUsesSharedPacing(t *testing.T) {
	const interval = 100 * time.Millisecond
	svc := newHTTPUpstreamPacerTestService(interval, 0)
	srv := newHTTPUpstreamPacerTestServer(t)
	ctx := t.Context()

	require.NoError(t, svc.paceAccount(ctx, 72))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	start := time.Now()
	resp, err := svc.DoWithTLS(req, "", 72, 1, &tlsfingerprint.Profile{Name: "plain-http"})
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	require.GreaterOrEqual(t, elapsed, interval*4/5,
		"DoWithTLS must honor the pacing slot reserved for the same account, elapsed=%v", elapsed)
}

// TestHTTPUpstreamPacerPaceAccountSpacing paceAccount 自身语义：同账号连续两次调用按最小间隔排开。
func TestHTTPUpstreamPacerPaceAccountSpacing(t *testing.T) {
	const interval = 100 * time.Millisecond
	svc := newHTTPUpstreamPacerTestService(interval, 0)
	ctx := t.Context()

	require.NoError(t, svc.paceAccount(ctx, 73), "first call must pass immediately")

	start := time.Now()
	require.NoError(t, svc.paceAccount(ctx, 73))
	require.GreaterOrEqual(t, time.Since(start), interval*4/5,
		"second call for the same account must wait for the minimum interval")
}

// TestHTTPUpstreamPacerPaceAccountPassesThrough 禁用、无账号上下文与 nil 接收者均零等待放行。
func TestHTTPUpstreamPacerPaceAccountPassesThrough(t *testing.T) {
	ctx := t.Context()

	// minInterval=0 → pacer 为 nil，完全禁用（默认行为，零开销）。
	disabled := newHTTPUpstreamPacerTestService(0, 0)
	require.Nil(t, disabled.pacer, "zero interval must disable the pacer")
	start := time.Now()
	require.NoError(t, disabled.paceAccount(ctx, 74))
	require.Less(t, time.Since(start), 50*time.Millisecond, "disabled pacer must not block")

	// 无账号上下文（0 或负数）直接放行。
	enabled := newHTTPUpstreamPacerTestService(time.Second, 0)
	require.NoError(t, enabled.paceAccount(ctx, 0))
	require.NoError(t, enabled.paceAccount(ctx, -1))

	// nil 接收者安全（避免装配缺失时 panic）。
	var nilSvc *httpUpstreamService
	require.NoError(t, nilSvc.paceAccount(ctx, 74))
}
