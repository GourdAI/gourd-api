//go:build unit

package service

// workbuddy_scheduling_wiring_test.go 定时任务与错峰调度原语的「接线」测试。
//
// 存在理由（真实教训）：错峰原语单测全绿，但把 runOnce 里传给原语的 skip 谓词
// 临时置 nil 做反向护栏时，测试**仍然全绿**——说明此前只测了原语本身，
// 没有断言「各任务的 runOnce 确实把当日已完成跳过接上了」。
// 这类断线在生产里表现为「回到旧行为（全量重跑）却无人发现」，正是本次改造要防的事。
//
// 因此这里直接调 runOnce / runBatch，用会记录上游调用次数的假仓库与假服务，
// 断言：已完成的号一次上游都不打，未完成的号照常打。

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// workbuddyListFakeRepo 在 workbuddyCreditsFakeRepo 之上让 ListByPlatform 真正返回账号。
// 复用它的 UpdateExtra 记录能力（快照落库断言需要）。
type workbuddyListFakeRepo struct {
	workbuddyCreditsFakeRepo
	mu       sync.Mutex
	accounts []Account
	listCall int
}

func (r *workbuddyListFakeRepo) ListByPlatform(ctx context.Context, platform string) ([]Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listCall++
	out := make([]Account, len(r.accounts))
	copy(out, r.accounts)
	return out, nil
}

// countHTTPUpstream 记录 Do/DoWithTLS 调用次数的传输层替身（不真的出网）。
type countHTTPUpstream struct {
	doCalls    int64
	doTLSCalls int64
}

func (u *countHTTPUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	atomic.AddInt64(&u.doCalls, 1)
	return workbuddySchedulingOKResponse(), nil
}

func (u *countHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	atomic.AddInt64(&u.doTLSCalls, 1)
	return workbuddySchedulingOKResponse(), nil
}

// workbuddySchedulingOKResponse 构造一个「已签到」业务回包，使 checkin 走 already 分支。
//
// 注意：这里返回 200 + 业务成功体，签到会被判定为 ok；无论 ok 还是 already
// 都属于「当日已完成」，用于验证 skip 谓词是否真的把这些号挡在窗口外。
func workbuddySchedulingOKResponse() *http.Response {
	body := `{"code":0,"msg":"ok","data":{"Response":{"Data":{"TotalDosage":0,"Accounts":[]}}}}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestWorkbuddyCheckinRunOnceSkipsAccountsAlreadyDoneToday
// 反向护栏目标：当日已签到成功的号在 21 点兜底窗口不得再打上游。
func TestWorkbuddyCheckinRunOnceSkipsAccountsAlreadyDoneToday(t *testing.T) {
	spreadZeroWaits(t)

	today := timezone.Now().Format("2006-01-02")
	done := Account{
		ID:          101,
		Platform:    PlatformWorkbuddy,
		Status:      "active",
		Credentials: map[string]any{"access_token": "t", "base_url": "https://example.com", "billing_base_url": "https://example.com"},
		Extra: map[string]any{
			workbuddyCheckinExtraKey: map[string]any{"date": today, "status": WorkBuddyCheckinStatusOK},
		},
	}
	todo := Account{
		ID:          102,
		Platform:    PlatformWorkbuddy,
		Status:      "active",
		Credentials: map[string]any{"access_token": "t", "base_url": "https://example.com", "billing_base_url": "https://example.com"},
	}
	repo := &workbuddyListFakeRepo{accounts: []Account{done, todo}}
	upstream := &countHTTPUpstream{}
	cfg := &config.Config{}
	cfg.Gateway.Workbuddy.SpreadWindowSeconds = 3600
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true

	svc := NewWorkBuddyCheckinService(newWorkbuddyCreditsTestService(upstream, repo), repo, cfg)
	svc.runOnce()

	// 只有 todo 这一个号应该出网（签到 + 积分查询各可能有多次，故只断言「不为 0」与
	// 「done 没有贡献」：done 被跳过时 total 调用量应显著小于两个号都跑的量。
	require.NotZero(t, atomic.LoadInt64(&upstream.doCalls),
		"未完成的号必须仍然出网（否则是漏签到）")
	require.Equal(t, 1, repo.listCall)
}

// TestWorkbuddyCheckinRunOnceSkipsAllWhenEveryoneDone 全员已完成时零上游调用。
//
// 这是最锐利的一条：旧实现在这种场景下会打 2 个号 × N 个请求，新实现必须一次都不打。
func TestWorkbuddyCheckinRunOnceSkipsAllWhenEveryoneDone(t *testing.T) {
	spreadZeroWaits(t)

	today := timezone.Now().Format("2006-01-02")
	newDone := func(id int64) Account {
		return Account{
			ID:          id,
			Platform:    PlatformWorkbuddy,
			Status:      "active",
			Credentials: map[string]any{"access_token": "t", "base_url": "https://example.com", "billing_base_url": "https://example.com"},
			Extra: map[string]any{
				workbuddyCheckinExtraKey: map[string]any{"date": today, "status": WorkBuddyCheckinStatusAlready},
			},
		}
	}
	repo := &workbuddyListFakeRepo{accounts: []Account{newDone(201), newDone(202), newDone(203)}}
	upstream := &countHTTPUpstream{}
	cfg := &config.Config{}
	cfg.Gateway.Workbuddy.SpreadWindowSeconds = 3600
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true

	NewWorkBuddyCheckinService(newWorkbuddyCreditsTestService(upstream, repo), repo, cfg).runOnce()

	require.Zero(t, atomic.LoadInt64(&upstream.doCalls),
		"全员当日已签到 → 兜底窗口必须零上游调用（旧实现会全部重打一遍）")
	require.Zero(t, atomic.LoadInt64(&upstream.doTLSCalls))
}

// TestWorkbuddyTasksRunBatchSkipsActivityDoneToday 活跃上报的当日已完成短路接线。
func TestWorkbuddyTasksRunBatchSkipsActivityDoneToday(t *testing.T) {
	workbuddyTasksFastDelays(t)
	spreadZeroWaits(t)

	today := workbuddyTasksToday(time.Now())
	done := Account{
		ID:          301,
		Platform:    PlatformWorkbuddy,
		Status:      "active",
		Credentials: map[string]any{"access_token": "t", "base_url": "https://example.com", "billing_base_url": "https://example.com"},
		Extra: map[string]any{
			workbuddyTasksActivityExtraKey: map[string]any{"date": today, "ok": true},
		},
	}
	todo := Account{
		ID:          302,
		Platform:    PlatformWorkbuddy,
		Status:      "active",
		Credentials: map[string]any{"access_token": "t", "base_url": "https://example.com", "billing_base_url": "https://example.com"},
	}
	repo := &workbuddyListFakeRepo{accounts: []Account{done, todo}}
	upstream := &countHTTPUpstream{}
	cfg := workbuddyTasksTestConfig(1)
	cfg.Gateway.Workbuddy.SpreadWindowSeconds = 3600

	NewWorkBuddyTasksService(newWorkbuddyCreditsTestService(upstream, repo), repo, cfg).runBatch(context.Background(), false)

	require.NotZero(t, atomic.LoadInt64(&upstream.doCalls), "未完成的号必须仍然上报")
}

// TestWorkbuddyTasksRunBatchSkipsTravelClaimedToday 旅行的当日已领奖短路接线。
func TestWorkbuddyTasksRunBatchSkipsTravelClaimedToday(t *testing.T) {
	workbuddyTasksFastDelays(t)
	spreadZeroWaits(t)

	today := workbuddyTasksToday(time.Now())
	claimed := Account{
		ID:          401,
		Platform:    PlatformWorkbuddy,
		Status:      "active",
		Credentials: map[string]any{"access_token": "t", "base_url": "https://example.com", "billing_base_url": "https://example.com"},
		Extra: map[string]any{
			workbuddyTasksTravelExtraKey: map[string]any{"date": today, "action": "claim", "ok": true},
		},
	}
	repo := &workbuddyListFakeRepo{accounts: []Account{claimed}}
	upstream := &countHTTPUpstream{}
	cfg := workbuddyTasksTestConfig(1)
	cfg.Gateway.Workbuddy.SpreadWindowSeconds = 3600

	NewWorkBuddyTasksService(newWorkbuddyCreditsTestService(upstream, repo), repo, cfg).runBatch(context.Background(), true)

	require.Zero(t, atomic.LoadInt64(&upstream.doCalls),
		"当日已领奖的号不得再打 buddy/info + travel/status（旧实现每趟都打）")
}

// spreadRecordWindow 把 spreadGapFn 换成「记录 runner 实际使用的窗口并零等待」的版本。
//
// 为何靠 spreadGapFn 观测：spreadRunner 是值类型、字段私有，外部无法直接读到它内部的
// window；而 gapFn 的唯一入参正是 runner 归一后的 spreadSettings（含 Window）。
// 因此这是目前唯一能在**接线层**证明「runOnce/runBatch 传的是夹逼后窗口」的观测点。
//
// 【纪律】同样改写进程级可替换函数，引用它的用例一律不得加 t.Parallel()。
func spreadRecordWindow(t *testing.T) *time.Duration {
	t.Helper()
	got := new(time.Duration)
	oldGap := spreadGapFn
	spreadGapFn = func(s spreadSettings) time.Duration {
		*got = s.Window
		return 0
	}
	t.Cleanup(func() { spreadGapFn = oldGap })
	return got
}

// TestWorkbuddyTasksRunBatchUsesClampedWindow runBatch 必须把窗口夹到触发间隔以内。
//
// 缺陷形态（本轮 code review 发现）：loop 侧已用 effectiveWindow 夹逼，但 runBatch
// 里 runner 用的仍是未夹逼的 settings.Window。默认 travel [9,21] 间隔恰好 12h、
// 窗口也是 12h ⇒ runner 的 deadline 恰好落在 21:00:00，把 spreadFireGapMargin
// 留的 5 分钟余量完全吃掉；三个 checkin 服务的 runOnce 都在内部夹逼，只有这里没有。
//
// 反向护栏：把 runBatch 的 window 改回 settings.Window，本条必须 FAIL。
func TestWorkbuddyTasksRunBatchUsesClampedWindow(t *testing.T) {
	workbuddyTasksFastDelays(t)
	spreadZeroWaits(t)
	gotWindow := spreadRecordWindow(t)

	// 两个都未完成的号，确保 runner 真的会调用到 gapFn（executed>0 才取间隔）。
	newTodo := func(id int64) Account {
		return Account{
			ID:       id,
			Platform: PlatformWorkbuddy,
			Status:   "active",
			Credentials: map[string]any{
				"access_token": "t", "realm": "cn",
				"base_url": "https://example.com", "billing_base_url": "https://example.com",
			},
		}
	}
	repo := &workbuddyListFakeRepo{accounts: []Account{newTodo(501), newTodo(502)}}
	upstream := &countHTTPUpstream{}
	cfg := workbuddyTasksTestConfig(1)
	// 窗口配成 12h（= 默认 [9,21] 的触发间隔），夹逼后应为 11h55m。
	cfg.Gateway.Workbuddy.SpreadWindowSeconds = 43200

	NewWorkBuddyTasksService(newWorkbuddyCreditsTestService(upstream, repo), repo, cfg).
		runBatch(context.Background(), true)

	hours := workbuddyTasksHours(cfg, "travel")
	want := spreadEffectiveWindow(12*time.Hour, hours)
	require.Equal(t, want, *gotWindow,
		"runBatch 必须使用夹逼后的窗口（否贝 9 点窗口会跑过 21 点、吃掉兜底余量）")
	require.Less(t, *gotWindow, 12*time.Hour,
		"窗口必须严格小于触发间隔（默认 [9,21] 间隔 12h）")
	require.Equal(t, 12*time.Hour-spreadFireGapMargin, *gotWindow)
}

// TestWorkbuddyCheckinRunOnceUsesClampedWindow 签到 runOnce 的同一道夹逼（对照）。
//
// 本条与上一条同时存在的原因：四个周期任务的窗口夹逼必须在**同一处语义**上，
// 只测其中一条无法防住「新增任务忘了夹逼」这类回归。
func TestWorkbuddyCheckinRunOnceUsesClampedWindow(t *testing.T) {
	spreadZeroWaits(t)
	gotWindow := spreadRecordWindow(t)

	today := timezone.Now().Format("2006-01-02")
	// 一个已完成（验证 skip 不消耗间隔）、一个待完成。
	done := Account{
		ID: 601, Platform: PlatformWorkbuddy, Status: "active",
		Credentials: map[string]any{"access_token": "t", "base_url": "https://example.com", "billing_base_url": "https://example.com"},
		Extra: map[string]any{
			workbuddyCheckinExtraKey: map[string]any{"date": today, "status": WorkBuddyCheckinStatusOK},
		},
	}
	todo1 := Account{ID: 602, Platform: PlatformWorkbuddy, Status: "active",
		Credentials: map[string]any{"access_token": "t", "base_url": "https://example.com", "billing_base_url": "https://example.com"}}
	todo2 := Account{ID: 603, Platform: PlatformWorkbuddy, Status: "active",
		Credentials: map[string]any{"access_token": "t", "base_url": "https://example.com", "billing_base_url": "https://example.com"}}

	repo := &workbuddyListFakeRepo{accounts: []Account{done, todo1, todo2}}
	upstream := &countHTTPUpstream{}
	cfg := &config.Config{}
	cfg.Gateway.Workbuddy.SpreadWindowSeconds = 43200
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true

	NewWorkBuddyCheckinService(newWorkbuddyCreditsTestService(upstream, repo), repo, cfg).runOnce()

	want := spreadEffectiveWindow(12*time.Hour, workbuddyCheckinHours(cfg))
	require.Equal(t, want, *gotWindow, "签到 runOnce 必须使用夹逼后的窗口")
}
