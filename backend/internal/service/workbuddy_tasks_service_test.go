//go:build unit

package service

// workbuddy_tasks_service_test.go 任务三件套单测（httptest 假上游）。
// 覆盖：上报形状与 CN/global 域名分流、上报循环 break 与 streak 自检、
// 连登奖励链全链与按天幂等、global 奖励链跳过、补签链重读状态挑档、
// 旅行状态机 4 态表驱动、领养按日防抖与跨日重置、global 旅行跳过、
// 快照落库、手动入口账号校验。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// workbuddyTasksTestConfig 测试用配置：放开 httptest 的 http 出站校验（与现有
// newWorkbuddyCreditsTestService 同做法），并指定上报条数。
func workbuddyTasksTestConfig(reportCount int) *config.Config {
	cfg := &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
		},
	}
	cfg.Gateway.Workbuddy.ActivityReportCount = reportCount
	return cfg
}

// workbuddyTasksFastDelays 把上报间隔与账号间隔置 0（与现有 fast 测试同做法）。
func workbuddyTasksFastDelays(t *testing.T) {
	t.Helper()
	oldGap, oldDelay := workbuddyTasksReportGap, workbuddyTasksAccountDelay
	workbuddyTasksReportGap, workbuddyTasksAccountDelay = 0, 0
	t.Cleanup(func() {
		workbuddyTasksReportGap, workbuddyTasksAccountDelay = oldGap, oldDelay
	})
}

// newWorkBuddyTasksTestService 构造任务测试服务：credits 走现有测试替身链路
// （workbuddyTestUpstream + workbuddyCreditsFakeRepo），任务服务复用其 httpUpstream。
func newWorkBuddyTasksTestService(upstream HTTPUpstream, repo AccountRepository, cfg *config.Config) *WorkBuddyTasksService {
	if cfg == nil {
		cfg = &config.Config{}
	}
	// 任务服务自身的 cfg 承担出站 URL 校验：httptest 为 http，需放开白名单校验。
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	return NewWorkBuddyTasksService(newWorkbuddyCreditsTestService(upstream, repo), repo, cfg)
}

// TestWorkBuddyTasksActivityReportShapeAndRealmRouting 上报形状（数组 body、userId/
// eventCode/mode 等 31 字段关键项）与 CN/global 域名分流（各自 billing 域出站）。
func TestWorkBuddyTasksActivityReportShapeAndRealmRouting(t *testing.T) {
	workbuddyTasksFastDelays(t)

	type capture struct {
		mu     sync.Mutex
		paths  []string
		bodies []string
	}
	newServer := func() (*capture, *httptest.Server) {
		cap := &capture{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			cap.mu.Lock()
			cap.paths = append(cap.paths, r.URL.Path)
			cap.bodies = append(cap.bodies, string(body))
			cap.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		}))
		return cap, server
	}
	capCN, serverCN := newServer()
	defer serverCN.Close()
	capGlobal, serverGlobal := newServer()
	defer serverGlobal.Close()

	repo := &workbuddyCreditsFakeRepo{}
	svc := newWorkBuddyTasksTestService(&workbuddyTestUpstream{client: serverCN.Client()}, repo, workbuddyTasksTestConfig(1))
	accountCN := workbuddyCreditsAccount(21, serverCN.URL, map[string]any{
		"access_token": "at-cn", "uid": "u-cn", "realm": "cn",
	})
	accountGlobal := workbuddyCreditsAccount(22, serverGlobal.URL, map[string]any{
		"access_token": "at-g", "uid": "u-g", "realm": "global",
	})

	cid := "sub2api-1000"
	require.NoError(t, svc.workbuddyReportChatActivity(context.Background(), accountCN, cid, cid+"-r1"))
	require.NoError(t, svc.workbuddyReportChatActivity(context.Background(), accountGlobal, cid, cid+"-r1"))

	assertReport := func(t *testing.T, cap *capture, wantUserID string) {
		t.Helper()
		cap.mu.Lock()
		defer cap.mu.Unlock()
		require.Equal(t, []string{workbuddyTasksReportPath}, cap.paths, "只发 /v2/report")
		require.Len(t, cap.bodies, 1)
		var events []map[string]any
		require.NoError(t, json.Unmarshal([]byte(cap.bodies[0]), &events), "body 为 JSON 数组")
		require.Len(t, events, 1)
		ev := events[0]
		require.Equal(t, wantUserID, ev["userId"], "userId 必填（= 账号 uid）")
		require.Equal(t, "chat_request_send", ev["eventCode"])
		require.Equal(t, "craft", ev["mode"])
		require.Equal(t, cid, ev["conversationId"])
		require.Equal(t, cid, ev["rootRequestId"])
		require.Equal(t, cid, ev["parentConversationId"])
		require.Equal(t, cid+"-r1", ev["requestId"])
		require.Equal(t, "deepseek-v4-flash", ev["requestModelId"])
		require.Equal(t, "DeepSeek V4 Flash", ev["requestModelName"])
		require.Equal(t, "default", ev["agentName"])
		require.Equal(t, "conversation", ev["agentType"])
		require.Contains(t, ev, "presentAt")
		require.Contains(t, ev, "timestamp")
	}
	assertReport(t, capCN, "u-cn")
	assertReport(t, capGlobal, "u-g")
}

// TestWorkBuddyTasksActivityReportLoopAndBreak N 条全发满 → streak 自检触发；
// 第 k 条 500 → break 只发 k 条且不自检、不领养。
func TestWorkBuddyTasksActivityReportLoopAndBreak(t *testing.T) {
	workbuddyTasksFastDelays(t)

	t.Run("all-reports-sent-triggers-streak-check", func(t *testing.T) {
		var (
			mu          sync.Mutex
			reportCalls int
			streakCalls int
		)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			switch r.URL.Path {
			case workbuddyTasksReportPath:
				reportCalls++
			case workbuddyTasksStreakPath:
				streakCalls++
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case workbuddyTasksStreakPath:
				_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"streak":{"days":5},"makeup_cards":{"balance":0},"redemption_status":{"tiers":[]}}}`))
			case workbuddyTasksBuddyInfoPath:
				_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"buddy":{"id":1,"name":"mimi"}}}`))
			case workbuddyTasksLotteryChancesPath:
				_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"balance":0}}`))
			case workbuddyTasksHeatmapPath:
				_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"cells":[]}}`))
			default:
				_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
			}
		}))
		defer server.Close()

		repo := &workbuddyCreditsFakeRepo{}
		repo.accountsByID = map[int64]*Account{}
		svc := newWorkBuddyTasksTestService(&workbuddyTestUpstream{client: server.Client()}, repo, workbuddyTasksTestConfig(3))
		account := workbuddyCreditsAccount(23, server.URL, map[string]any{"access_token": "at", "uid": "u-23", "realm": "cn"})
		repo.accountsByID[23] = account

		result, err := svc.RunActivity(context.Background(), 23)
		require.NoError(t, err)
		require.True(t, result.Success)
		require.Equal(t, 3, result.Reports)
		require.Equal(t, 3, result.ReportsRequested)
		require.True(t, result.StreakChecked)
		require.Equal(t, 5, result.StreakDays)

		mu.Lock()
		require.Equal(t, 3, reportCalls, "N 条全发")
		require.GreaterOrEqual(t, streakCalls, 1, "全发满 → streak 自检触发")
		mu.Unlock()
	})

	t.Run("failure-breaks-remaining-and-skips-streak", func(t *testing.T) {
		var (
			mu          sync.Mutex
			reportCalls int
			streakCalls int
			buddyCalls  int
		)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case workbuddyTasksReportPath:
				mu.Lock()
				reportCalls++
				n := reportCalls
				mu.Unlock()
				if n >= 2 {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
					return
				}
				_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
			case workbuddyTasksStreakPath:
				mu.Lock()
				streakCalls++
				mu.Unlock()
				_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"streak":{"days":1}}}`))
			case workbuddyTasksBuddyInfoPath:
				mu.Lock()
				buddyCalls++
				mu.Unlock()
				_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"buddy":{"id":1}}}`))
			default:
				_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
			}
		}))
		defer server.Close()

		repo := &workbuddyCreditsFakeRepo{}
		repo.accountsByID = map[int64]*Account{}
		svc := newWorkBuddyTasksTestService(&workbuddyTestUpstream{client: server.Client()}, repo, workbuddyTasksTestConfig(3))
		account := workbuddyCreditsAccount(24, server.URL, map[string]any{"access_token": "at", "uid": "u-24", "realm": "cn"})
		repo.accountsByID[24] = account

		result, err := svc.RunActivity(context.Background(), 24)
		require.NoError(t, err)
		require.False(t, result.Success)
		require.Equal(t, 1, result.Reports, "第 2 条失败 → 只成功 1 条")
		require.False(t, result.StreakChecked)
		require.Contains(t, result.Detail, "report 2/3")

		mu.Lock()
		require.Equal(t, 2, reportCalls, "第 2 条失败即 break（总请求 = 2）")
		require.Equal(t, 0, streakCalls, "未发满不自检")
		require.Equal(t, 0, buddyCalls, "未发满不领养")
		mu.Unlock()
	})
}

// TestWorkBuddyTasksRewardChainFullFlowAndDailyIdempotency 奖励链全链 + 按天幂等：
// gift/comp 各 1 次、streak 读、无漏签不补、redeem 带 client_token、
// chances + draw；二次调用不再 redeem/gift。
func TestWorkBuddyTasksRewardChainFullFlowAndDailyIdempotency(t *testing.T) {
	workbuddyTasksFastDelays(t)

	var (
		mu           sync.Mutex
		reportCalls  int
		giftCalls    int
		compCalls    int
		makeupCalls  int
		redeemCalls  int
		chancesCalls int
		drawCalls    int
		redeemToken  string
		drawToken    string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case workbuddyTasksReportPath:
			mu.Lock()
			reportCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case workbuddyTasksStreakPath:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"streak":{"days":8},"makeup_cards":{"balance":0,"max":3},"redemption_status":{"tier_7d_status":"available","tier_14d_status":"locked","tier_28d_status":"locked","tiers":[{"tier":"7d","days":7,"credit":100,"chances":2},{"tier":"14d","days":14,"credit":300},{"tier":"28d","days":28,"credit":800}]}}}`))
		case workbuddyTasksHeatmapPath:
			// 昨日无格（无漏签判据）→ 不应触发补签。
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"cells":[{"date":"` + workbuddyTasksToday(time.Now()) + `","score":1}]}}`))
		case workbuddyTasksClaimGiftPath:
			mu.Lock()
			giftCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"credit":50}}`))
		case workbuddyTasksClaimCompensationPath:
			mu.Lock()
			compCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":110,"msg":"no compensation"}`))
		case workbuddyTasksMakeupCardUsePath:
			mu.Lock()
			makeupCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case workbuddyTasksRedeemPath:
			var payload map[string]any
			_ = json.Unmarshal(body, &payload)
			mu.Lock()
			redeemCalls++
			if token, ok := payload["client_token"].(string); ok {
				redeemToken = token
			}
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"credit_granted":100,"cards_granted":1,"chances_granted":2}}`))
		case workbuddyTasksLotteryChancesPath:
			mu.Lock()
			chancesCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"balance":2}}`))
		case workbuddyTasksLotteryDrawPath:
			var payload map[string]any
			_ = json.Unmarshal(body, &payload)
			mu.Lock()
			drawCalls++
			if token, ok := payload["client_token"].(string); ok {
				drawToken = token
			}
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"prize_code":"p1","prize_name":"6元红包","prize_type":"credit","credit_amount":6}}`))
		case workbuddyTasksBuddyInfoPath:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"buddy":{"id":9,"name":"doudou"}}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		}
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	repo.accountsByID = map[int64]*Account{}
	svc := newWorkBuddyTasksTestService(&workbuddyTestUpstream{client: server.Client()}, repo, workbuddyTasksTestConfig(1))
	account := workbuddyCreditsAccount(25, server.URL, map[string]any{"access_token": "at", "uid": "u-25", "realm": "cn"})
	repo.accountsByID[25] = account

	first, err := svc.RunActivity(context.Background(), 25)
	require.NoError(t, err)
	require.True(t, first.Success)
	require.Equal(t, "7d", first.RedeemTier)
	require.EqualValues(t, 150, first.CreditGranted, "gift 50 + redeem 100")
	require.True(t, first.LotteryDrawn)

	mu.Lock()
	require.Equal(t, 1, giftCalls)
	require.Equal(t, 1, compCalls)
	require.Equal(t, 0, makeupCalls, "无漏签不补签")
	require.Equal(t, 1, redeemCalls)
	require.Equal(t, 1, chancesCalls)
	require.Equal(t, 1, drawCalls)
	require.Regexp(t, `^redeem-7d-[0-9a-f]{32}$`, redeemToken, "redeem client_token 每次新键")
	require.Regexp(t, `^draw-[0-9a-f]{32}$`, drawToken, "draw client_token 每次新键")
	afterFirst := reportCalls
	mu.Unlock()

	// 二次调用：上报照发（无按天闸），奖励链整体跳过（按天幂等）。
	second, err := svc.RunActivity(context.Background(), 25)
	require.NoError(t, err)
	require.True(t, second.Success)
	mu.Lock()
	require.Greater(t, reportCalls, afterFirst, "上报不走奖励链幂等闸")
	require.Equal(t, 1, giftCalls, "二次调用不再领礼包")
	require.Equal(t, 1, compCalls)
	require.Equal(t, 1, redeemCalls, "二次调用不再 redeem")
	require.Equal(t, 1, drawCalls, "二次调用不再 draw")
	mu.Unlock()
}

// TestWorkBuddyTasksRewardChainSkippedForGlobalRealm global 账号奖励链整链跳过（零上游调用）。
func TestWorkBuddyTasksRewardChainSkippedForGlobalRealm(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	svc := newWorkBuddyTasksTestService(&workbuddyTestUpstream{client: server.Client()}, repo, nil)
	account := workbuddyCreditsAccount(26, server.URL, map[string]any{
		"access_token": "at", "uid": "u-26g", "realm": "global",
	})
	result := &WorkBuddyActivityRunResult{}
	svc.claimGrowthRewards(context.Background(), account, result)

	require.EqualValues(t, 0, atomic.LoadInt32(&calls), "global 账号不发任何奖励链调用")
	require.Empty(t, result.RedeemTier)
	require.False(t, result.LotteryDrawn)
	require.EqualValues(t, 0, result.CreditGranted)
}

// TestWorkBuddyTasksMakeupChainRereadsStateBeforeRedeem 补签链：昨日漏签且有卡 →
// use 补签 → 重读 state 后挑到恢复后的档位（验证重读挑档）。
func TestWorkBuddyTasksMakeupChainRereadsStateBeforeRedeem(t *testing.T) {
	workbuddyTasksFastDelays(t)

	var (
		mu          sync.Mutex
		makeupCalls int
		usedTarget  string
		redeemCalls int
		redeemTiers []string
	)
	madeup := false
	yesterday := workbuddyTasksYesterday(time.Now())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case workbuddyTasksReportPath:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case workbuddyTasksStreakPath:
			mu.Lock()
			current := madeup
			mu.Unlock()
			days := 6
			if current {
				days = 7
			}
			_, _ = fmt.Fprintf(w, `{"code":0,"msg":"ok","data":{"streak":{"days":%d},"makeup_cards":{"balance":1,"max":3},"redemption_status":{"tier_7d_status":"available","tiers":[{"tier":"7d","days":7,"credit":66}]}}}`, days)
		case workbuddyTasksHeatmapPath:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"cells":[{"date":"` + yesterday + `","score":0}]}}`))
		case workbuddyTasksMakeupCardUsePath:
			var payload map[string]any
			_ = json.Unmarshal(body, &payload)
			target, _ := payload["target_date"].(string)
			mu.Lock()
			makeupCalls++
			usedTarget = target
			madeup = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case workbuddyTasksRedeemPath:
			var payload map[string]any
			_ = json.Unmarshal(body, &payload)
			tier, _ := payload["tier"].(string)
			mu.Lock()
			redeemCalls++
			redeemTiers = append(redeemTiers, tier)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"credit_granted":66,"chances_granted":0}}`))
		case workbuddyTasksLotteryChancesPath:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"balance":0}}`))
		case workbuddyTasksBuddyInfoPath:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"buddy":{"id":3,"name":"nana"}}}`))
		case workbuddyTasksClaimGiftPath, workbuddyTasksClaimCompensationPath:
			_, _ = w.Write([]byte(`{"code":10001,"msg":"already claimed"}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		}
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	repo.accountsByID = map[int64]*Account{}
	svc := newWorkBuddyTasksTestService(&workbuddyTestUpstream{client: server.Client()}, repo, workbuddyTasksTestConfig(1))
	account := workbuddyCreditsAccount(27, server.URL, map[string]any{"access_token": "at", "uid": "u-27", "realm": "cn"})
	repo.accountsByID[27] = account

	result, err := svc.RunActivity(context.Background(), 27)
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, 6, result.StreakDays, "自检读到补签前的天数")
	require.Equal(t, "7d", result.RedeemTier, "补签后重读状态挑到恢复后的档位")

	mu.Lock()
	require.Equal(t, 1, makeupCalls)
	require.Equal(t, yesterday, usedTarget, "补签目标为 CST 昨日")
	require.Equal(t, 1, redeemCalls)
	require.Equal(t, []string{"7d"}, redeemTiers)
	mu.Unlock()
}

// TestWorkBuddyTasksTravelStateMachine 旅行状态机 4 态表驱动：
// arrived→claim（record_id 必带）、idle→depart、idle+limit→skip、traveling→skip。
func TestWorkBuddyTasksTravelStateMachine(t *testing.T) {
	workbuddyTasksFastDelays(t)

	cases := []struct {
		name           string
		statusBody     string
		wantAction     string
		wantSuccess    bool
		wantReward     int64
		wantDepart     bool
		wantClaim      bool
		wantDetailPart string
	}{
		{
			name:        "arrived-claim",
			statusBody:  `{"state":"arrived","daily_limit_reached":false,"record_id":42,"reward_credit":50}`,
			wantAction:  "claim",
			wantSuccess: true,
			wantReward:  50,
			wantClaim:   true,
		},
		{
			name:        "idle-depart",
			statusBody:  `{"state":"idle","daily_limit_reached":false}`,
			wantAction:  "depart",
			wantSuccess: true,
			wantDepart:  true,
		},
		{
			name:           "idle-daily-limit-skip",
			statusBody:     `{"state":"idle","daily_limit_reached":true}`,
			wantAction:     "skip",
			wantDetailPart: "daily limit reached",
		},
		{
			name:           "traveling-skip",
			statusBody:     `{"state":"traveling","record_id":7}`,
			wantAction:     "skip",
			wantDetailPart: "traveling",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu          sync.Mutex
				departCalls int
				claimCalls  int
				departBody  map[string]any
				claimBody   map[string]any
			)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case workbuddyTasksBuddyInfoPath:
					_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"buddy":{"id":5,"name":"mimi"}}}`))
				case workbuddyTasksTravelStatusPath:
					_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":` + tc.statusBody + `}`))
				case workbuddyTasksTravelDepartPath:
					var payload map[string]any
					_ = json.Unmarshal(body, &payload)
					mu.Lock()
					departCalls++
					departBody = payload
					mu.Unlock()
					_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
				case workbuddyTasksTravelClaimPath:
					var payload map[string]any
					_ = json.Unmarshal(body, &payload)
					mu.Lock()
					claimCalls++
					claimBody = payload
					mu.Unlock()
					_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"reward_credit":50}}`))
				default:
					_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
				}
			}))
			defer server.Close()

			repo := &workbuddyCreditsFakeRepo{}
			repo.accountsByID = map[int64]*Account{}
			svc := newWorkBuddyTasksTestService(&workbuddyTestUpstream{client: server.Client()}, repo, nil)
			account := workbuddyCreditsAccount(30, server.URL, map[string]any{"access_token": "at", "uid": "u-30", "realm": "cn"})
			repo.accountsByID[30] = account

			result, err := svc.RunTravel(context.Background(), 30)
			require.NoError(t, err)
			require.Equal(t, tc.wantAction, result.Action)
			require.Equal(t, tc.wantSuccess, result.Success)
			require.EqualValues(t, tc.wantReward, result.RewardCredit)
			if tc.wantDetailPart != "" {
				require.Contains(t, result.Detail, tc.wantDetailPart)
			}
			require.Equal(t, "mimi", result.BuddyName)

			mu.Lock()
			defer mu.Unlock()
			if tc.wantDepart {
				require.Equal(t, 1, departCalls, "idle 且未达上限 → 派出一次")
				require.EqualValues(t, workbuddyTasksTravelLocationID, departBody["location_id"])
			} else {
				require.Equal(t, 0, departCalls)
			}
			if tc.wantClaim {
				require.Equal(t, 1, claimCalls)
				require.EqualValues(t, 42, claimBody["record_id"], "claim 必带 record_id")
			} else {
				require.Equal(t, 0, claimCalls)
			}
		})
	}
}

// TestWorkBuddyTasksTravelAdoptDebouncePerDay 领养：无猫 + 门槛 400 → adoptTried 置位，
// 当日第二次不再发 agreement/first；跨日（伪造昨日标记）可重试。
func TestWorkBuddyTasksTravelAdoptDebouncePerDay(t *testing.T) {
	workbuddyTasksFastDelays(t)

	var (
		mu             sync.Mutex
		infoCalls      int
		agreementCalls int
		firstCalls     int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case workbuddyTasksBuddyInfoPath:
			mu.Lock()
			infoCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"buddy":null}}`))
		case workbuddyTasksBuddyAgreementPath:
			mu.Lock()
			agreementCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case workbuddyTasksBuddyFirstPath:
			mu.Lock()
			firstCalls++
			mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":400,"msg":"first_buddy task not completed yet"}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		}
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	repo.accountsByID = map[int64]*Account{}
	svc := newWorkBuddyTasksTestService(&workbuddyTestUpstream{client: server.Client()}, repo, nil)
	account := workbuddyCreditsAccount(31, server.URL, map[string]any{"access_token": "at", "uid": "u-31", "realm": "cn"})
	repo.accountsByID[31] = account

	first, err := svc.RunTravel(context.Background(), 31)
	require.NoError(t, err)
	require.Equal(t, "skip", first.Action)
	require.Contains(t, first.Detail, "conversation threshold")
	require.True(t, svc.adoptTriedToday(31), "门槛未达 → 标记当日已试")
	mu.Lock()
	require.Equal(t, 1, agreementCalls)
	require.Equal(t, 1, firstCalls)
	mu.Unlock()

	second, err := svc.RunTravel(context.Background(), 31)
	require.NoError(t, err)
	require.Equal(t, "skip", second.Action)
	require.Contains(t, second.Detail, "tried today")
	mu.Lock()
	require.Equal(t, 1, agreementCalls, "当日不再重试领养")
	require.Equal(t, 1, firstCalls)
	require.Equal(t, 2, infoCalls)
	mu.Unlock()

	// 伪造跨日：标记替换为昨日（CST 自然日）→ 次日恢复重试能力。
	svc.mu.Lock()
	svc.adoptTried[strconv.FormatInt(31, 10)] = workbuddyTasksToday(time.Now().Add(-24 * time.Hour))
	svc.mu.Unlock()

	third, err := svc.RunTravel(context.Background(), 31)
	require.NoError(t, err)
	require.Equal(t, "skip", third.Action)
	mu.Lock()
	require.Equal(t, 2, agreementCalls, "跨日重置后可重试")
	require.Equal(t, 2, firstCalls)
	mu.Unlock()
}

// TestWorkBuddyTasksTravelSkippedForGlobalRealm global 账号旅行巡检零上游调用。
func TestWorkBuddyTasksTravelSkippedForGlobalRealm(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	repo.accountsByID = map[int64]*Account{}
	svc := newWorkBuddyTasksTestService(&workbuddyTestUpstream{client: server.Client()}, repo, nil)
	account := workbuddyCreditsAccount(32, server.URL, map[string]any{"access_token": "at", "uid": "u-32g", "realm": "global"})
	repo.accountsByID[32] = account

	result, err := svc.RunTravel(context.Background(), 32)
	require.NoError(t, err)
	require.Equal(t, "skip", result.Action)
	require.Contains(t, result.Detail, "global")
	require.EqualValues(t, 0, atomic.LoadInt32(&calls), "global 账号不发任何上游请求")
}

// TestWorkBuddyTasksSnapshotsPersisted 跑完后 account.Extra 含 workbuddy_activity /
// workbuddy_travel 快照（用 fakeRepo.snapshot 断言字段形态）。
func TestWorkBuddyTasksSnapshotsPersisted(t *testing.T) {
	workbuddyTasksFastDelays(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case workbuddyTasksStreakPath:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"streak":{"days":9},"makeup_cards":{"balance":0},"redemption_status":{"tier_7d_status":"claimed","tiers":[{"tier":"7d","days":7}]}}}`))
		case workbuddyTasksBuddyInfoPath:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"buddy":{"id":2,"name":"huahua"}}}`))
		case workbuddyTasksTravelStatusPath:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"state":"idle","daily_limit_reached":false}}`))
		case workbuddyTasksLotteryChancesPath:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"balance":0}}`))
		case workbuddyTasksHeatmapPath:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"cells":[]}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		}
	}))
	defer server.Close()

	repo := &workbuddyCreditsFakeRepo{}
	repo.accountsByID = map[int64]*Account{}
	svc := newWorkBuddyTasksTestService(&workbuddyTestUpstream{client: server.Client()}, repo, workbuddyTasksTestConfig(1))
	account := workbuddyCreditsAccount(33, server.URL, map[string]any{"access_token": "at", "uid": "u-33", "realm": "cn"})
	repo.accountsByID[33] = account

	activity, err := svc.RunActivity(context.Background(), 33)
	require.NoError(t, err)
	require.True(t, activity.Success)
	calls, id, extra := repo.snapshot()
	require.Equal(t, 1, calls)
	require.EqualValues(t, 33, id)
	snap, ok := extra[workbuddyTasksActivityExtraKey].(workbuddyActivitySnapshot)
	require.True(t, ok, "activity 快照应落 extra")
	require.True(t, snap.OK)
	require.Equal(t, workbuddyTasksToday(time.Now()), snap.Date)
	require.Equal(t, 1, snap.Reports)
	require.Equal(t, 9, snap.StreakDays)

	travel, err := svc.RunTravel(context.Background(), 33)
	require.NoError(t, err)
	require.True(t, travel.Success)
	calls, id, extra = repo.snapshot()
	require.Equal(t, 2, calls, "activity + travel 各写一次快照（fakeRepo 累计计数）")
	require.EqualValues(t, 33, id)
	travelSnap, ok := extra[workbuddyTasksTravelExtraKey].(workbuddyTravelSnapshot)
	require.True(t, ok, "travel 快照应落 extra")
	require.True(t, travelSnap.OK)
	require.Equal(t, workbuddyTasksToday(time.Now()), travelSnap.Date)
	require.Equal(t, "depart", travelSnap.Action)
	require.Equal(t, "idle", travelSnap.State)
	require.Equal(t, "huahua", travelSnap.BuddyName)
}

// TestWorkBuddyTasksManualRunRejectsUnknownAccount 手动 RunActivity/RunTravel：
// 账号不存在/平台不对返回错误（infraerrors 口径，不发起上游调用）。
func TestWorkBuddyTasksManualRunRejectsUnknownAccount(t *testing.T) {
	repo := &workbuddyCreditsFakeRepo{}
	repo.accountsByID = map[int64]*Account{
		40: {ID: 40, Platform: PlatformOpenAI, Credentials: map[string]any{}},
	}
	svc := newWorkBuddyTasksTestService(&workbuddyTestUpstream{client: http.DefaultClient}, repo, nil)

	_, err := svc.RunActivity(context.Background(), 999)
	require.Error(t, err)
	require.Contains(t, err.Error(), "account not found")

	_, err = svc.RunTravel(context.Background(), 998)
	require.Error(t, err)
	require.Contains(t, err.Error(), "account not found")

	_, err = svc.RunActivity(context.Background(), 40)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a workbuddy account")

	_, err = svc.RunTravel(context.Background(), 40)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a workbuddy account")
}

// TestWorkBuddyTasksHoursNormalization 小时列表归一：默认值、非法丢弃、去重、全非法关闭。
func TestWorkBuddyTasksHoursNormalization(t *testing.T) {
	// 缺省配置 → 默认 [10] / [9,21]。
	require.Equal(t, []int{10}, workbuddyTasksHours(&config.Config{}, "activity"))
	require.Equal(t, []int{9, 21}, workbuddyTasksHours(&config.Config{}, "travel"))
	require.Equal(t, []int{10}, workbuddyTasksHours(nil, "activity"))

	cfg := &config.Config{}
	cfg.Gateway.Workbuddy.ActivityHours = []int{22, 5, 22, 99, -1}
	require.Equal(t, []int{5, 22}, workbuddyTasksHours(cfg, "activity"))
	cfg.Gateway.Workbuddy.TravelHours = []int{42}
	require.Nil(t, workbuddyTasksHours(cfg, "travel"), "全非法 → 视为关闭")
}

// TestWorkBuddyTasksReportCountFallback 上报条数：<=0/缺省归 1。
func TestWorkBuddyTasksReportCountFallback(t *testing.T) {
	require.Equal(t, 1, workbuddyTasksReportCount(nil))
	require.Equal(t, 1, workbuddyTasksReportCount(&config.Config{}))
	cfg := &config.Config{}
	cfg.Gateway.Workbuddy.ActivityReportCount = 5
	require.Equal(t, 5, workbuddyTasksReportCount(cfg))
}
