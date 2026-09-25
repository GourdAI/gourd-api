//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// workbuddy_tasks_claim_idempotency_test.go 锁定 P0 修正：做任务领积分的幂等标记
// 必须有持久化落点，且落库不得随调度 ctx 一起被取消。
//
// 修正前的缺陷：幂等只靠进程内存 map（文件头自陈「重启清零」），发布/OOM 重启后
// 同一 CST 自然日内会再跑一遍完整奖励链（礼包 + 补偿 + 补签卡 + redeem + 抽奖），
// 唯一的拦截是上游返回「已领取」被静默吞掉 —— 幂等责任 100% 外包给上游。

// restartWorkBuddyAccount 模拟「进程重启后重新从 DB 读账号」：把 Extra 经 JSON
// 往返回原成 map[string]any 的真实存储形态，并用全新 Account 对象承载（全新内存态，
// 不含任何进程内标记）。
func restartWorkBuddyAccount(t *testing.T, account *Account) *Account {
	t.Helper()
	raw, err := json.Marshal(account.Extra)
	require.NoError(t, err)
	var revived map[string]any
	require.NoError(t, json.Unmarshal(raw, &revived))
	fresh := *account
	fresh.Extra = revived
	return &fresh
}

// stubWorkbuddyUpstream 只记录各路径命中次数的假上游。
type stubWorkbuddyUpstream struct {
	mu    sync.Mutex
	calls map[string]int
	// streakBody / makeupBody 用于按用例注入不同上游回执。
	streakBody string
	makeupBody string
}

func (u *stubWorkbuddyUpstream) hit(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.calls == nil {
		u.calls = map[string]int{}
	}
	u.calls[path]++
	return u.calls[path]
}

func (u *stubWorkbuddyUpstream) count(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls[path]
}

func (u *stubWorkbuddyUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := u.hit(r.URL.Path)
	body, _ := io.ReadAll(r.Body)
	_ = body
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case workbuddyTasksStreakPath:
		_, _ = io.WriteString(w, u.streakBody)
	case workbuddyTasksHeatmapPath:
		// 昨日有格且 score=0 → 构成补签判据（写后读延迟场景：上游聚合不变）。
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"cells":[{"date":"`+
			workbuddyTasksYesterday(time.Now())+`","score":0}]}}`)
	case workbuddyTasksMakeupCardUsePath:
		if n > 1 {
			// 第二次消耗时卡仍在（balance>0），上游会接受 —— 本地闸门是唯一防线。
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{}}`)
			return
		}
		_, _ = io.WriteString(w, u.makeupBody)
	case workbuddyTasksRedeemPath:
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"credit_granted":100}}`)
	case workbuddyTasksLotteryChancesPath:
		// claimGrowthLottery 是「chances>0 才 draw」：balance=0 时 draw 压根不会被调用，
		// 「重启后不得再次抽奖」就变成恒真断言。该函数单次查询 + 单次 draw（无循环），
		// 故固定 balance=1 既能走通首趟 draw，又不会造成刷次数。
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"balance":1}}`)
	case workbuddyTasksLotteryDrawPath:
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"prize_name":"6元红包"}}`)
	default:
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{}}`)
	}
}

func newStubWorkbuddyServer(t *testing.T, streakBody string) (*stubWorkbuddyUpstream, *httptest.Server) {
	t.Helper()
	upstream := &stubWorkbuddyUpstream{
		streakBody: streakBody,
		makeupBody: `{"code":0,"msg":"ok","data":{}}`,
	}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	return upstream, server
}

// mergingWorkbuddyTasksRepo 自带「jsonb 合并」语义的账号仓库替身。
//
// 为什么不能直接复用 workbuddyCreditsFakeRepo：真实 accountRepository.UpdateExtra
// 执行的是 UPDATE accounts SET extra = COALESCE(extra,'{}'::jsonb) || $1::jsonb
// （顶层 key 累积合并），而该替身只保留**最后一次**调用的整份 updates（覆盖语义）。
// 做任务领积分的编排对同一账号会分次写入活动快照与幂等标记两个不同的 key，
// 用覆盖语义的替身会让先写的那个凭空消失，断言出来的「丢标记」是替身造假而非
// 生产缺陷。故本文件使用与生产对齐的合并语义替身。
type mergingWorkbuddyTasksRepo struct {
	mockAccountRepoForGemini
	mu         sync.Mutex
	extraCalls int
	lastID     int64
	merged     map[string]any
}

func (r *mergingWorkbuddyTasksRepo) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.extraCalls++
	r.lastID = id
	if r.merged == nil {
		r.merged = map[string]any{}
	}
	for k, v := range updates {
		if v == nil {
			delete(r.merged, k)
			continue
		}
		r.merged[k] = v
	}
	// 同步回内存中的账号对象：模拟「写入即对后续 GetByID 可见」。
	// 生产实现里编排每次运行都从 DB 重新读账号，因此落库结果必须能被下一轮读到，
	// 否则「重启后持久标记接管闸门」这类断言根本无法成立。
	if acc, ok := r.accountsByID[id]; ok && acc != nil {
		if acc.Extra == nil {
			acc.Extra = map[string]any{}
		}
		for k, v := range updates {
			if v == nil {
				delete(acc.Extra, k)
				continue
			}
			acc.Extra[k] = v
		}
	}
	return nil
}

func (r *mergingWorkbuddyTasksRepo) snapshot() (int, int64, map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make(map[string]any, len(r.merged))
	for k, v := range r.merged {
		cp[k] = v
	}
	return r.extraCalls, r.lastID, cp
}

// 奖励链按日幂等标记必须落进 account.Extra：重启（内存标记清零）后二次运行
// 不得再打任何领取类写接口。
func TestWorkBuddyTasksRewardClaimMarkPersistsAcrossRestart(t *testing.T) {
	workbuddyTasksFastDelays(t)

	upstream, server := newStubWorkbuddyServer(t,
		// 连登 8 天 + 7d 档 available：确保首趟一定走到 redeem 并落标记。
		`{"code":0,"msg":"ok","data":{"streak":{"days":8},"makeup_cards":{"balance":0,"max":3},`+
			`"redemption_status":{"tier_7d_status":"available","tiers":[{"tier":"7d","days":7,"credit":100}]}}}`)

	repo := &mergingWorkbuddyTasksRepo{accountsByID: map[int64]*Account{}}
	account := workbuddyCreditsAccount(61, server.URL,
		map[string]any{"access_token": "at", "uid": "u-61", "realm": "cn"})
	repo.accountsByID[61] = account

	svc := newWorkBuddyTasksTestService(&workbuddyTestUpstream{client: server.Client()}, repo, workbuddyTasksTestConfig(1))
	first, err := svc.RunActivity(context.Background(), 61)
	require.NoError(t, err)
	require.True(t, first.Success)
	require.Equal(t, 1, upstream.count(workbuddyTasksRedeemPath))

	// 标记必须已落库，而不是只活在内存里。
	_, _, extra := repo.snapshot()
	require.Contains(t, extra, workbuddyTasksClaimExtraKey, "幂等标记必须写进 account.Extra")
	claims, ok := extra[workbuddyTasksClaimExtraKey].(map[string]string)
	require.True(t, ok, "标记必须是 {scope: date} 形态")
	require.Equal(t, workbuddyTasksToday(time.Now()), claims[workbuddyClaimScopeReward])

	// 重启：全新 service（内存 map 空）+ 从 DB 复活的账号对象。
	restarted := restartWorkBuddyAccount(t, account)
	repo.accountsByID[61] = restarted
	svc2 := newWorkBuddyTasksTestService(&workbuddyTestUpstream{client: server.Client()}, repo, workbuddyTasksTestConfig(1))
	second, err := svc2.RunActivity(context.Background(), 61)
	require.NoError(t, err)
	require.True(t, second.Success, "重启后上报仍应正常执行")

	require.Equal(t, 1, upstream.count(workbuddyTasksRedeemPath), "重启后不得再次 redeem（持久标记必须生效）")
	require.Equal(t, 1, upstream.count(workbuddyTasksClaimGiftPath), "重启后不得再次领新手礼包")
	require.Equal(t, 1, upstream.count(workbuddyTasksLotteryDrawPath), "重启后不得再次抽奖")
}

// 补签卡是不可再生资产，且消耗发生在挑档之前：必须有独立于奖励链的按日闸门。
// 场景刻意构造 tier==""（无达标档）→ 旧实现不标记 rewardClaimed，同一天再跑会
// 因 heatmap 只读判据未变而再吃一张卡。
func TestWorkBuddyTasksMakeupCardNotDoubleSpentOnSameDay(t *testing.T) {
	workbuddyTasksFastDelays(t)

	upstream, server := newStubWorkbuddyServer(t,
		// days=3 < 7 → workbuddyEligibleTier 返回 ""，走「不标记 rewardClaimed」分支。
		`{"code":0,"msg":"ok","data":{"streak":{"days":3},"makeup_cards":{"balance":2,"max":3},`+
			`"redemption_status":{"tiers":[{"tier":"7d","days":7,"credit":100}]}}}`)

	repo := &mergingWorkbuddyTasksRepo{accountsByID: map[int64]*Account{}}
	account := workbuddyCreditsAccount(62, server.URL,
		map[string]any{"access_token": "at", "uid": "u-62", "realm": "cn"})
	repo.accountsByID[62] = account

	svc := newWorkBuddyTasksTestService(&workbuddyTestUpstream{client: server.Client()}, repo, workbuddyTasksTestConfig(1))
	_, err := svc.RunActivity(context.Background(), 62)
	require.NoError(t, err)
	require.Equal(t, 1, upstream.count(workbuddyTasksMakeupCardUsePath), "首趟补签一次")

	// 同进程内再跑一次（管理员手点 / 配了多个触发小时）。
	_, err = svc.RunActivity(context.Background(), 62)
	require.NoError(t, err)
	require.Equal(t, 1, upstream.count(workbuddyTasksMakeupCardUsePath), "同日重跑不得再吃补签卡")

	// 重启后再跑：持久标记必须接管闸门。
	restarted := restartWorkBuddyAccount(t, account)
	repo.accountsByID[62] = restarted
	svc2 := newWorkBuddyTasksTestService(&workbuddyTestUpstream{client: server.Client()}, repo, workbuddyTasksTestConfig(1))
	_, err = svc2.RunActivity(context.Background(), 62)
	require.NoError(t, err)
	require.Equal(t, 1, upstream.count(workbuddyTasksMakeupCardUsePath), "重启后同日仍不得再吃补签卡")

	// 标记落库校验：scope=makeup，值=昨日日期（与「补的是哪一天」对齐，而非执行日）。
	_, _, extra := repo.snapshot()
	claims, ok := extra[workbuddyTasksClaimExtraKey].(map[string]string)
	require.True(t, ok)
	require.Equal(t, workbuddyTasksYesterday(time.Now()), claims[workbuddyClaimScopeMakeup])
}

// 调度 ctx 被取消（优雅停机 / 批量预算耗尽）时，快照与幂等标记仍必须落库：
// 上游 redeem 可能已经成功发放，丢记录等于丢掉「已领过」的唯一本地痕迹。
func TestWorkBuddyTasksPersistSurvivesCanceledContext(t *testing.T) {
	workbuddyTasksFastDelays(t)

	_, server := newStubWorkbuddyServer(t,
		`{"code":0,"msg":"ok","data":{"streak":{"days":8},"makeup_cards":{"balance":0},`+
			`"redemption_status":{"tier_7d_status":"available","tiers":[{"tier":"7d","days":7,"credit":100}]}}}`)

	repo := &mergingWorkbuddyTasksRepo{accountsByID: map[int64]*Account{}}
	account := workbuddyCreditsAccount(63, server.URL,
		map[string]any{"access_token": "at", "uid": "u-63", "realm": "cn"})
	repo.accountsByID[63] = account
	svc := newWorkBuddyTasksTestService(&workbuddyTestUpstream{client: server.Client()}, repo, workbuddyTasksTestConfig(1))

	// 核心断言：ctx 已取消（复现 stopCh 已关 / 批量预算耗尽）时，快照与幂等标记
	// 仍必须落库 —— 上游可能已经发放成功，丢记录等于丢掉「已领过」的唯一本地痕迹。
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	svc.persistTaskClaimMark(canceled, account, workbuddyClaimScopeReward, workbuddyTasksToday(time.Now()))
	_, lastID, extra := repo.snapshot()
	require.Equal(t, int64(63), lastID, "取消链不得阻止幂等标记落库")
	claims, ok := extra[workbuddyTasksClaimExtraKey].(map[string]string)
	require.True(t, ok)
	require.Equal(t, workbuddyTasksToday(time.Now()), claims[workbuddyClaimScopeReward])

	// 同账号在取消 ctx 下跑完整编排：上报照常执行，但快照必须仍然落库。
	result := svc.runActivityForAccount(canceled, account)
	require.NotNil(t, result)
	calls, _, extra2 := repo.snapshot()
	require.Greater(t, calls, 1, "快照落库必须发生")
	require.Contains(t, extra2, workbuddyTasksActivityExtraKey, "取消链不得影响活动快照落库")
}
