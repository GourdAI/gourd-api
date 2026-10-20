package repository

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖「进程级出站闸门」（防封加固 A1）的两层事实：
//  1. upstreamGlobalGate 自身语义（间隔排开、并发上限、释放幂等、ctx 取消）；
//  2. Do / DoWithTLS **真的接上了它** —— 与 account_pacer 当年的教训一致：
//     只测组件自身，接线被删掉时全部测试照样绿。
//
// 与 anti-api 的口径差异（写死在实现注释里，这里做断言固化）：
// anti-api 是本地单人代理，可硬编码 Semaphore(1)+全局 1000ms；本仓是多租户网关，
// 因此两层都做成默认禁用（0=禁用），测试必须同时钉住「禁用时零开销」这条反向护栏。

// newGateTestService 构造只启用进程级闸门的上游服务。
// cfg 保持 nil：闸门与节奏发生在任何 cfg 解引用之前，且下游各 getter 对 nil cfg 有安全回退。
func newGateTestService(minInterval, jitter time.Duration, maxConcurrent int) *httpUpstreamService {
	return &httpUpstreamService{
		clients: make(map[string]*upstreamClientEntry),
		gate:    newUpstreamGlobalGate(minInterval, jitter, maxConcurrent),
	}
}

func newGateTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- 1. 组件自身语义 ---

// TestUpstreamGlobalGateDisabledByDefault 三项参数全 0 时必须返回 nil（默认零行为变更）。
func TestUpstreamGlobalGateDisabledByDefault(t *testing.T) {
	require.Nil(t, newUpstreamGlobalGate(0, 0, 0), "all-zero config must disable the gate")

	svc := newGateTestService(0, 0, 0)
	require.Nil(t, svc.gate)

	release, err := svc.acquireUpstreamGate(nil)
	require.NoError(t, err)
	require.NotNil(t, release, "release must never be nil so callers can call it unconditionally")
	start := time.Now()
	release()
	require.Less(t, time.Since(start), 50*time.Millisecond, "disabled gate must not block")

	// nil 接收者安全（装配缺失时不得 panic）。
	var nilSvc *httpUpstreamService
	release2, err := nilSvc.acquireUpstreamGate(nil)
	require.NoError(t, err)
	require.NotNil(t, release2)
	release2()
}

// TestUpstreamGlobalGateIntervalSpacing 间隔语义：连续两次占用按最小间隔排开。
func TestUpstreamGlobalGateIntervalSpacing(t *testing.T) {
	const interval = 80 * time.Millisecond
	g := newUpstreamGlobalGate(interval, 0, 0)
	ctx := t.Context()

	first, err := g.acquire(ctx)
	require.NoError(t, err)
	first()

	start := time.Now()
	second, err := g.acquire(ctx)
	elapsed := time.Since(start)
	require.NoError(t, err)
	second()

	require.GreaterOrEqual(t, elapsed, interval*3/4,
		"second acquire must wait for the global interval, elapsed=%v", elapsed)
}

// TestUpstreamGlobalGateConcurrentCap 并发上限：槽位未释放时，下一个占用必须等待。
//
// 这条同时钉住「长流必须持续占槽」的设计要点：释放只由调用方在响应体关闭时触发，
// 闸门自己不知道请求何时结束（Do 返回响应头 ≠ 流结束）。
func TestUpstreamGlobalGateConcurrentCap(t *testing.T) {
	g := newUpstreamGlobalGate(0, 0, 1)
	ctx := t.Context()

	first, err := g.acquire(ctx)
	require.NoError(t, err)

	blocked := make(chan struct{})
	go func() {
		second, err := g.acquire(ctx)
		if err == nil {
			second()
		}
		close(blocked)
	}()

	select {
	case <-blocked:
		t.Fatal("second acquire must block while the only slot is held")
	case <-time.After(80 * time.Millisecond):
	}

	// 未释放前不得放行，但也不能永久卡死：释放后必须立刻通过。
	first()
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("acquire must proceed after the slot is released")
	}
}

// TestUpstreamGlobalGateReleaseIsIdempotent 重复释放不得多还槽位，也不得因多还而永久阻塞。
//
// 反向护栏：去掉 onceRelease 后，第二次 release() 会在空 channel 上收而永久阻塞（本用例超时失败）；
// 且后续占用会拿到不存在的槽位。
func TestUpstreamGlobalGateReleaseIsIdempotent(t *testing.T) {
	g := newUpstreamGlobalGate(0, 0, 1)
	ctx := t.Context()

	release, err := g.acquire(ctx)
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		release()
		release()
		release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("duplicate release must be a no-op (once-guarded)")
	}

	// 三次释放只应还一个槽位：占用后仍应有一个可用槽，再占用必须等待。
	first, err := g.acquire(ctx)
	require.NoError(t, err)

	blocked := make(chan struct{})
	go func() {
		second, err := g.acquire(ctx)
		if err == nil {
			second()
		}
		close(blocked)
	}()
	select {
	case <-blocked:
		first()
		t.Fatal("over-release detected: extra slot became available after duplicate release")
	case <-time.After(120 * time.Millisecond):
	}
	first()
	<-blocked
}

// TestUpstreamGlobalGateHonorsContextCancel ctx 取消/超时必须让排队提前返回，
// 且已占用的并发槽位被归还（否则客户端断开会逐步耗尽闸门直至死锁）。
func TestUpstreamGlobalGateHonorsContextCancel(t *testing.T) {
	g := newUpstreamGlobalGate(0, 0, 1)
	ctx := t.Context()

	holding, err := g.acquire(ctx)
	require.NoError(t, err)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	release, err := g.acquire(cancelled)
	require.Error(t, err, "cancelled context must not acquire")
	require.NotNil(t, release, "release must still be non-nil on the error path")
	release() // 不得 panic，也不得影响真实持有者

	// 真实持有者释放后，闸门必须完全可用（证明取消路径没有吃掉槽位）。
	holding()
	next, err := g.acquire(ctx)
	require.NoError(t, err)
	next()
}

// TestUpstreamGlobalGateIntervalWaitHonorsTimeout 等待间隔期间超时同样要返回错误。
func TestUpstreamGlobalGateIntervalWaitHonorsTimeout(t *testing.T) {
	g := newUpstreamGlobalGate(time.Minute, 0, 0)
	ctx := t.Context()

	first, err := g.acquire(ctx)
	require.NoError(t, err)
	first()

	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	_, err = g.acquire(timeoutCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// --- 2. 接线：Do / DoWithTLS 是否真的经过闸门 ---

// TestHTTPUpstreamGateDoAppliesInterval 证明 Do 在出站前占用全局间隔槽位。
//
// 先经 acquireUpstreamGate 预约（无 I/O，起点干净），随后同一服务的 Do 必须等间隔到期。
// 若有人删掉 Do 里的接线，耗时会立刻回到毫秒级 → 本用例失败。
func TestHTTPUpstreamGateDoAppliesInterval(t *testing.T) {
	const interval = 100 * time.Millisecond
	svc := newGateTestService(interval, 0, 0)
	srv := newGateTestServer(t)
	ctx := t.Context()

	release, err := svc.acquireUpstreamGate(nil)
	require.NoError(t, err)
	release()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	start := time.Now()
	resp, err := svc.Do(req, "", 0, 1)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	require.GreaterOrEqual(t, elapsed, interval*4/5,
		"Do must honor the global gate interval, elapsed=%v", elapsed)
}

// TestHTTPUpstreamGateBodyCloseReleasesSlot 证明并发槽位是**响应体关闭**才释放，
// 而不是 Do 返回（拿到响应头）就释放 —— 这是流式（SSE）场景的关键口径。
func TestHTTPUpstreamGateBodyCloseReleasesSlot(t *testing.T) {
	inHandler := make(chan struct{})
	finish := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Gate-Hold") != "" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			close(inHandler)
			<-finish
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	svc := newGateTestService(0, 0, 1)
	ctx := t.Context()

	holdReq, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	holdReq.Header.Set("X-Gate-Hold", "1")
	holdResp, err := svc.Do(holdReq, "", 0, 1)
	require.NoError(t, err)
	<-inHandler
	require.Equal(t, http.StatusOK, holdResp.StatusCode)

	// 流还挂着（响应体未关闭）→ 第二个出站必须被闸门拦住。
	secondDone := make(chan *http.Response, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if err != nil {
			secondDone <- nil
			return
		}
		resp, err := svc.Do(req, "", 0, 1)
		if err != nil {
			secondDone <- nil
			return
		}
		secondDone <- resp
	}()

	select {
	case resp := <-secondDone:
		if resp != nil {
			_ = resp.Body.Close()
		}
		t.Fatal("second upstream request must wait while the first stream still holds the slot")
	case <-time.After(120 * time.Millisecond):
	}

	// 关闭第一个响应体 → 槽位归还 → 第二个请求放行。
	close(finish)
	require.NoError(t, holdResp.Body.Close())

	select {
	case resp := <-secondDone:
		require.NotNil(t, resp, "second request must succeed after the slot is released")
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
	case <-time.After(5 * time.Second):
		t.Fatal("second request never proceeded after body close")
	}
}

// TestHTTPUpstreamGateReleasesOnRequestFailure 出站失败必须归还槽位。
//
// 打一个指向已关闭端口的请求：Do 走 servertiming.Do 出错分支。若该分支漏掉释放，
// maxConcurrent=1 会让后续正常请求永久阻塞。
func TestHTTPUpstreamGateReleasesOnRequestFailure(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	svc := newGateTestService(0, 0, 1)
	ctx := t.Context()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, deadURL, nil)
	require.NoError(t, err)
	start := time.Now()
	_, err = svc.Do(req, "", 0, 1)
	require.Error(t, err, "request to a closed listener must fail")
	require.Less(t, time.Since(start), 3*time.Second)

	srv := newGateTestServer(t)
	okReq, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		resp, err := svc.Do(okReq, "", 0, 1)
		if err != nil {
			done <- err
			return
		}
		done <- resp.Body.Close()
	}()
	select {
	case err := <-done:
		require.NoError(t, err, "slot must be reusable after a failed request")
	case <-time.After(5 * time.Second):
		t.Fatal("gate leaked its only slot on the request-error path")
	}
}

// TestHTTPUpstreamGateDoWithTLSPlainHTTPSharesGate DoWithTLS 的 plain-HTTP 分支转发给 Do，
// 必须共用同一套闸门状态且不重复占用（占用两次会让间隔翻倍，被本用例的上下界同时钉住）。
func TestHTTPUpstreamGateDoWithTLSPlainHTTPSharesGate(t *testing.T) {
	const interval = 300 * time.Millisecond
	svc := newGateTestService(interval, 0, 0)
	srv := newGateTestServer(t)
	ctx := t.Context()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	start := time.Now()
	resp, err := svc.DoWithTLS(req, "", 0, 1, &tlsfingerprint.Profile{Name: "plain-http"})
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	// 首次占用立即放行；第二次只等一个 interval（若重复占用会等两个）。
	require.Less(t, elapsed, interval, "first acquire must pass immediately, elapsed=%v", elapsed)

	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	start2 := time.Now()
	resp2, err := svc.DoWithTLS(req2, "", 0, 1, &tlsfingerprint.Profile{Name: "plain-http"})
	elapsed2 := time.Since(start2)
	require.NoError(t, err)
	require.NoError(t, resp2.Body.Close())
	require.GreaterOrEqual(t, elapsed2, interval*3/4,
		"DoWithTLS must honor the shared global interval, elapsed=%v", elapsed2)
	require.Less(t, elapsed2, interval*3/2,
		"DoWithTLS must not consume two interval slots per request, elapsed=%v", elapsed2)
}

// TestHTTPUpstreamGateNilProfileUsesDo profile=nil 时 DoWithTLS 走 Do，同样受闸门约束。
func TestHTTPUpstreamGateNilProfileUsesDo(t *testing.T) {
	const interval = 100 * time.Millisecond
	svc := newGateTestService(interval, 0, 0)
	srv := newGateTestServer(t)
	ctx := t.Context()

	release, err := svc.acquireUpstreamGate(nil)
	require.NoError(t, err)
	release()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	start := time.Now()
	resp, err := svc.DoWithTLS(req, "", 0, 1, nil)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.GreaterOrEqual(t, elapsed, interval*4/5,
		"DoWithTLS(nil profile) must go through Do and honor the gate, elapsed=%v", elapsed)
}

// TestHTTPUpstreamGateNilServiceSafe nil 接收者不得 panic（装配缺失场景）。
func TestHTTPUpstreamGateNilServiceSafe(t *testing.T) {
	var svc *httpUpstreamService
	release, err := svc.acquireUpstreamGate(httptest.NewRequest(http.MethodGet, "http://example.com", nil))
	require.NoError(t, err)
	release()

	var held atomic.Int64
	held.Add(1)
	require.Equal(t, int64(1), held.Load())
}

// --- 6. 流式路径不 Close 也必须归还槽位（P1 回归护栏）---

// gatePassthroughReader 忠实模拟三家归一化 reader 的形态：只持有 io.Reader
// （Closer 在构造器形参上就已丢失），从源读取且从不关闭源。
type gatePassthroughReader struct{ src io.Reader }

func (r gatePassthroughReader) Read(p []byte) (int, error) { return r.src.Read(p) }

// TestHTTPUpstreamGateEOFReleasesSlotWithoutClose 是本文件最重要的一条：
//
// 上面的 BodyClose 用例只测了「对原始 resp.Body 调 Close」，而三家聊天出站实际写的是
// resp.Body = io.NopCloser(<归一化 reader>)（trae_client.go:204、qoder_client.go:225、
// workbuddy_client.go:191）：上层 defer Close 作在 NopCloser 上是 no-op，真正的
// trackedBody 被丢弃。那就是上轮 17 例全绿仍漏 P1 的原因。
//
// 本用例复刻该形态：只读到 EOF 、全程不调 Close，闸位必须归还。
func TestHTTPUpstreamGateEOFReleasesSlotWithoutClose(t *testing.T) {
	inHandler := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Stream") != "" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			_, _ = w.Write([]byte("data: hello\n\n"))
			close(inHandler)
			return // handler 返回→服务端结束响应→客户端读到 EOF
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	svc := newGateTestService(0, 0, 1)
	ctx := t.Context()

	firstReq, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	firstReq.Header.Set("X-Stream", "1")
	firstResp, err := svc.Do(firstReq, "", 0, 1)
	require.NoError(t, err)
	<-inHandler
	require.Equal(t, http.StatusOK, firstResp.StatusCode)

	// 关键动作：与三家流式成功路径同形——把 Body 换成不转发 Close 的 NopCloser。
	tracked := firstResp.Body
	firstResp.Body = io.NopCloser(gatePassthroughReader{src: tracked})

	// 只读完，从不调用 Close。
	body, readErr := io.ReadAll(firstResp.Body)
	require.NoError(t, readErr)
	require.Contains(t, string(body), "hello")

	// 槽位必须已归还：第二个出站无需 Close 就能拿到。
	secondDone := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if err != nil {
			secondDone <- err
			return
		}
		resp, err := svc.Do(req, "", 0, 1)
		if err != nil {
			secondDone <- err
			return
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		secondDone <- nil
	}()

	select {
	case err := <-secondDone:
		require.NoError(t, err, "second request must proceed once the stream is drained")
	case <-time.After(3 * time.Second):
		t.Fatal("slot not released on EOF: streaming path still leaks")
	}
}

// TestTrackedBodyEOFThenCloseFiresExactlyOnce 钉住 once 语义：读完再 Close
// 不得重复递减 inFlight / 重复归还槽位。
func TestTrackedBodyEOFThenCloseFiresExactlyOnce(t *testing.T) {
	var fires atomic.Int64
	b := wrapTrackedBody(io.NopCloser(strings.NewReader("abc")), func() { fires.Add(1) })
	require.NotNil(t, b)

	buf := make([]byte, 8)
	for {
		if _, err := b.Read(buf); err != nil {
			break
		}
	}
	require.Equal(t, int64(1), fires.Load(), "EOF must fire the callback exactly once")

	require.NoError(t, b.Close())
	require.Equal(t, int64(1), fires.Load(), "Close after EOF must not fire again")
}

// TestTrackedBodyReleasesOnReadError 非 EOF 的读错误同样意味着事务结束，
// 不释放就会把异常场景变成永久泄漏。
func TestTrackedBodyReleasesOnReadError(t *testing.T) {
	var fires atomic.Int64
	b := wrapTrackedBody(io.NopCloser(gateFailingReader{}), func() { fires.Add(1) })
	require.NotNil(t, b)

	_, err := b.Read(make([]byte, 4))
	require.Error(t, err)
	require.Equal(t, int64(1), fires.Load(), "a read error must also release the slot")
}

type gateFailingReader struct{}

func (gateFailingReader) Read([]byte) (int, error) { return 0, errors.New("gate test read failure") }

// --- 7. 排队独立上限（upstream_gate_wait_timeout_ms）---

// TestUpstreamGlobalGateMaxWaitReturnsGateTimeout 槽位被长期占用时，等待方必须
// 在 maxWait 到点时拿到可归因的错误，而不是无限阻塞。
func TestUpstreamGlobalGateMaxWaitReturnsGateTimeout(t *testing.T) {
	g := newUpstreamGlobalGate(0, 0, 1)
	g.maxWait = 80 * time.Millisecond

	release, err := g.acquire(context.Background())
	require.NoError(t, err)
	defer release()

	start := time.Now()
	_, err = g.acquire(context.Background())
	require.Error(t, err, "queueing with a full slot set must eventually fail, not block forever")
	require.ErrorIs(t, err, errUpstreamGateTimeout)
	// 原始错误必须仍可检：不能为了新语义打破调用方已有的 errors.Is 判据。
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.GreaterOrEqual(t, time.Since(start), 60*time.Millisecond)
}

// TestUpstreamGlobalGateMaxWaitPreservesCallerCancel 归因必须准确：调用方自己取消时
// 不得报成「闸门排队超时」，否则排障时会被引向错误的方向。
func TestUpstreamGlobalGateMaxWaitPreservesCallerCancel(t *testing.T) {
	g := newUpstreamGlobalGate(0, 0, 1)
	g.maxWait = 5 * time.Second

	release, err := g.acquire(context.Background())
	require.NoError(t, err)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = g.acquire(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, errUpstreamGateTimeout, "a canceled caller must not be reported as gate queueing timeout")
}

// TestNewHTTPUpstreamAppliesGateWaitTimeout 钉住配置→运行时的接线。
// 本项是字段赋值而非构造参数，属于最容易被改漏的形态（历史上出现过
// 「面板可配、后端静默无效」），因此必须直接断言 gate.maxWait。
func TestNewHTTPUpstreamAppliesGateWaitTimeout(t *testing.T) {
	svc, ok := NewHTTPUpstream(&config.Config{Gateway: config.GatewayConfig{
		UpstreamMinIntervalMs:         10,
		UpstreamMaxConcurrentRequests: 2,
		UpstreamGateWaitTimeoutMs:     1500,
	}}).(*httpUpstreamService)
	require.True(t, ok)
	require.NotNil(t, svc.gate)
	require.Equal(t, 1500*time.Millisecond, svc.gate.maxWait)
}

// TestNewHTTPUpstreamGateWaitTimeoutDefaultsToZero 默认必须是 0（不限），
// 否则就变成了默认改变行为。
func TestNewHTTPUpstreamGateWaitTimeoutDefaultsToZero(t *testing.T) {
	svc, ok := NewHTTPUpstream(&config.Config{Gateway: config.GatewayConfig{
		UpstreamMinIntervalMs: 10,
	}}).(*httpUpstreamService)
	require.True(t, ok)
	require.NotNil(t, svc.gate)
	require.Zero(t, svc.gate.maxWait, "zero config must mean unlimited waiting")
}
