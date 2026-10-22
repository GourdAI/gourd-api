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
	workbuddySpreadZeroWaits(t)

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
	workbuddySpreadZeroWaits(t)

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
	workbuddySpreadZeroWaits(t)

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
	workbuddySpreadZeroWaits(t)

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
