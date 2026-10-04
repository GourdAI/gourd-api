//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 积分快照停调的行为锁。
//
// 这组用例守的是「宁可漏判，不可误杀」这条线：Trae / Qoder / WorkBuddy 的积分
// 快照过去只被管理面板读取，如今进入调度资格判定与阈值评估，一旦判定过宽就会
// 把健康号整体打下线（用户侧直接表现为 no available accounts），比原来的
// 「耗尽号照样接单」更严重。因此每个"不该拦"的形态都要单独钉住。

func creditTraeAccount(extra map[string]any) *Account {
	return &Account{ID: 9001, Platform: PlatformTrae, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Extra: extra}
}

func creditQoderAccount(extra map[string]any) *Account {
	return &Account{ID: 9002, Platform: PlatformQoder, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Extra: extra}
}

func creditWorkbuddyAccount(extra map[string]any) *Account {
	return &Account{ID: 9003, Platform: PlatformWorkbuddy, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Extra: extra}
}

func traeCreditExtra(remain, size, credits float64, packs int, fetchedAt int64) map[string]any {
	return map[string]any{
		traeCreditsExtraKey: map[string]any{
			"remain":     remain,
			"used":       size - remain,
			"size":       size,
			"credits":    credits,
			"packs":      packs,
			"fetched_at": fetchedAt,
		},
	}
}

func TestCreditSnapshotExhausted_TraeUsesUpAll(t *testing.T) {
	snapshot, ok := readAccountCreditSnapshot(creditTraeAccount(traeCreditExtra(0, 200, 0, 1, time.Now().Unix())))
	require.True(t, ok)
	require.True(t, creditSnapshotExhausted(snapshot), "额度池存在且剩余为 0 应判耗尽")
}

func TestCreditSnapshotExhausted_TraeAllPackagesExpired(t *testing.T) {
	// 上游把过期包的 remain 归零（traeEntitlementUsage:573）且聚合时跳过过期包
	// （traeAggregatePacks:592），因此"包全过期"表现为 size=0 但 packs>0。
	snapshot, ok := readAccountCreditSnapshot(creditTraeAccount(traeCreditExtra(0, 0, 0, 3, time.Now().Unix())))
	require.True(t, ok)
	require.True(t, creditSnapshotExhausted(snapshot), "权益包全部过期应判耗尽")
}

func TestCreditSnapshotExhausted_NeverProbedIsNotExhausted(t *testing.T) {
	// 从未探测：键不存在。此时绝不可停调，否则新加的号一进池就被拦死。
	extra := map[string]any{"trae_checkin": map[string]any{"date": "2026-10-01"}}
	snapshot, ok := readAccountCreditSnapshot(creditTraeAccount(extra))
	require.False(t, ok, "无快照键不应返回可用快照")
	require.Nil(t, snapshot)
	paused, decision := shouldAutoPauseAccountByCredits(creditTraeAccount(extra), time.Now())
	require.False(t, paused)
	require.Empty(t, decision.window)
}

func TestCreditSnapshotExhausted_AllZeroSnapshotDoesNotPause(t *testing.T) {
	// 探测降级留下的全零快照（status 成功、权益包明细失败 → Remain/Size/Packs 全 0，
	// 且 Credits 也是 0）：没有"确实存在过额度"的正向证据，必须放行。
	account := creditTraeAccount(traeCreditExtra(0, 0, 0, 0, time.Now().Unix()))
	paused, _ := shouldAutoPauseAccountByCredits(account, time.Now())
	require.False(t, paused, "全零且无任何额度证据的快照不可停调")
}

// ---- 平台口径分叉回归（自查发现的两处误杀，必须按平台分别定义可信证据）----

// Qoder 的 remain/total 只累加 userQuota + addOnQuota，dedicatedResourcePackages
// 只 Packs++ 不累加余额（qoder_campaign_service.go:443-453）。因此
// 「remain=0 && total=0 && packs>0」完全可能是「套餐用光但专属包还有余额」，
// 不能当耗尽。Qoder 只信上游 isQuotaExceeded。
func TestCreditSnapshotExhausted_QoderDedicatedPackagesAreNotInRemain(t *testing.T) {
	now := time.Now().Unix()
	account := creditQoderAccount(map[string]any{
		qoderCreditsExtraKey: map[string]any{
			"remain":     0.0, // 只覆盖套餐 + 加油包
			"total":      0.0,
			"packs":      3.0, // 3 个专属资源包（各剩 100）—— 余额不在 remain 里
			"fetched_at": float64(now),
		},
	})
	snapshot, ok := readAccountCreditSnapshot(account)
	require.True(t, ok)
	require.False(t, creditSnapshotExhausted(snapshot),
		"Qoder 专属资源包余额不计入 remain/total，packs>0 不得当作耗尽证据")
	paused, _ := shouldAutoPauseAccountByCredits(account, time.Now())
	require.False(t, paused, "还有 300 专属包余额的 Qoder 号不得被停调")

	// 同一形态下上游显式置位 isQuotaExceeded 则应停调（它是综合所有池的权威读数）。
	exceeded := creditQoderAccount(map[string]any{
		qoderCreditsExtraKey: map[string]any{
			"remain": 0.0, "total": 0.0, "packs": 3.0,
			"quota_exceeded": true, "fetched_at": float64(now),
		},
	})
	paused, _ = shouldAutoPauseAccountByCredits(exceeded, time.Now())
	require.True(t, paused, "isQuotaExceeded 是 Qoder 唯一可采信的耗尽信号")
}

// WorkBuddy 的 remain=0 只有在「确实查到过包、且能算出已消耗」时才是耗尽；
// 「Accounts 非空但容量字段全缺省」的降级读数必须放行。
func TestCreditSnapshotExhausted_WorkbuddyDegradedReadDoesNotPause(t *testing.T) {
	now := time.Now().Unix()
	degraded := creditWorkbuddyAccount(map[string]any{
		workbuddyCreditsExtraKey: map[string]any{
			"remain": float64(0), "used": float64(0), "size": float64(0),
			"packs":      float64(2), // 查到过两个包，但容量字段一个都没解析出来
			"fetched_at": float64(now),
		},
	})
	paused, _ := shouldAutoPauseAccountByCredits(degraded, time.Now())
	require.False(t, paused, "remain=0 且 used=0 是「查不到额度」，不是「用完了」")

	// 【P1-C 正向对照】降级读数与真耗尽的唯一区分点是 pack_size_sum（未被
	// TotalDosage 抬升的逐包容量之和）；旧快照缺该字段时读侧一律放行。
	usedUp := creditWorkbuddyAccount(map[string]any{
		workbuddyCreditsExtraKey: map[string]any{
			"remain": float64(0), "used": float64(5000), "size": float64(5000),
			"pack_size_sum": float64(5000),
			"packs":         float64(2), "fetched_at": float64(now),
		},
	})
	paused, _ = shouldAutoPauseAccountByCredits(usedUp, time.Now())
	require.True(t, paused, "额度池容量已读到且用光时应停调")

	// TotalDosage 伪造型：size/used/packs 全非零，但没有 pack_size_sum ⇒ 不得停调。
	forged := creditWorkbuddyAccount(map[string]any{
		workbuddyCreditsExtraKey: map[string]any{
			"remain": float64(0), "used": float64(10000), "size": float64(10000),
			"packs": float64(2), "fetched_at": float64(now),
		},
	})
	paused, _ = shouldAutoPauseAccountByCredits(forged, time.Now())
	require.False(t, paused,
		"size/used 可以是 TotalDosage 合成的假读数，缺 pack_size_sum 就不得当作耗尽")
}

// P1-C 根因回归：上游字段改名/降级导致容量读不到时，TotalDosage 会把 size/used
// 伪造成「全用光」形态。探针实测：两个无容量字段的包 + TotalDosage=10000
// → remain=0 used=10000 size=10000 packs=2。修复后 PackSizeSum=0 必须让调度层放行。
func TestWorkbuddyDegradedReadDoesNotForgeExhaustedSnapshot(t *testing.T) {
	resp := &workbuddyUserResourceResp{}
	require.NoError(t, json.Unmarshal([]byte(
		`{"Response":{"Data":{"TotalDosage":10000,"Accounts":[{"PackageName":"p1"},{"PackageName":"p2"}]}}}`,
	), resp))
	remain, used, size, packs, packages := workbuddyAggregateResource(resp)
	// 先钉住伪造成因本身（这些值全是 TotalDosage 衍生的，不是真实额度读数）。
	require.EqualValues(t, 0, remain)
	require.EqualValues(t, 10000, used)
	require.EqualValues(t, 10000, size)
	require.Equal(t, 2, packs)
	require.Zero(t, workbuddyPackSizeSum(packages), "无容量字段时池内容量之和必须为 0")

	snapshot := &WorkBuddyCreditsSnapshot{
		Remain: remain, Used: used, Size: size,
		PackSizeSum: workbuddyPackSizeSum(packages), Packs: packs,
		FetchedAt: time.Now().Unix(),
	}
	account := creditWorkbuddyAccount(map[string]any{workbuddyCreditsExtraKey: *snapshot})
	view, ok := readAccountCreditSnapshot(account)
	require.True(t, ok)
	require.False(t, creditSnapshotExhausted(view),
		"降级读数不得被 TotalDosage 的假 size/used 误判为耗尽（会停掉一批健康号）")
	paused, _ := shouldAutoPauseAccountByCredits(account, time.Now())
	require.False(t, paused)

	// 正向对照：同包带真实容量字段时 PackSizeSum>0， remain=0 就是真耗尽。
	require.NoError(t, json.Unmarshal([]byte(
		`{"Response":{"Data":{"TotalDosage":10000,"Accounts":[{"CycleCapacitySize":5000,"CycleCapacityRemain":0,"CycleCapacityUsed":5000}]}}}`,
	), resp))
	r2, u2, s2, p2, pkgs2 := workbuddyAggregateResource(resp)
	require.EqualValues(t, 0, r2)
	require.Positive(t, workbuddyPackSizeSum(pkgs2), "真耗尽时池内容量之和必须大于 0")
	honest := creditWorkbuddyAccount(map[string]any{workbuddyCreditsExtraKey: WorkBuddyCreditsSnapshot{
		Remain: r2, Used: u2, Size: s2, PackSizeSum: workbuddyPackSizeSum(pkgs2), Packs: p2,
		FetchedAt: time.Now().Unix(),
	}})
	paused, _ = shouldAutoPauseAccountByCredits(honest, time.Now())
	require.True(t, paused, "真耗尽（容量读到且用光）仍应停调")
}

func TestCreditSnapshotExhausted_CreditsBalanceKeepsAccountSchedulable(t *testing.T) {
	// 无权益包但 status 仍有积分余额：Trae 的 credits 与权益包是两个口径。
	snapshot, ok := readAccountCreditSnapshot(creditTraeAccount(traeCreditExtra(0, 0, 120, 0, time.Now().Unix())))
	require.True(t, ok)
	require.False(t, creditSnapshotExhausted(snapshot), "仍有积分余额时不得停调")
}

func TestCreditSnapshotExhausted_RemainPositiveKeepsAccountSchedulable(t *testing.T) {
	snapshot, ok := readAccountCreditSnapshot(creditTraeAccount(traeCreditExtra(0.08, 200, 0, 1, time.Now().Unix())))
	require.True(t, ok)
	require.False(t, creditSnapshotExhausted(snapshot), "积分是 float，微量剩余也必须算可用")
}

func TestCreditSnapshotExhausted_QoderUpstreamBooleanWins(t *testing.T) {
	// Qoder 的 isQuotaExceeded 是上游权威读数，即便本地算出仍有剩余也应停调。
	account := creditQoderAccount(map[string]any{
		qoderCreditsExtraKey: map[string]any{
			"remain":         50,
			"total":          200,
			"quota_exceeded": true,
			"fetched_at":     time.Now().Unix(),
		},
	})
	paused, decision := shouldAutoPauseAccountByCredits(account, time.Now())
	require.True(t, paused)
	require.Equal(t, creditSnapshotQuotaWindow, decision.window)
}

func TestCreditSnapshotExhausted_WorkbuddyEnterpriseNotApplicable(t *testing.T) {
	// 企业成员在个人 billing 资源池本就查不到额度（remain=0 是"查不到"而非
	// "用完了"），停调会把整批企业账号打出线。
	account := creditWorkbuddyAccount(map[string]any{
		workbuddyCreditsExtraKey: map[string]any{
			"remain":         0,
			"size":           0,
			"packs":          0,
			"not_applicable": true,
			"fetched_at":     time.Now().Unix(),
		},
	})
	paused, _ := shouldAutoPauseAccountByCredits(account, time.Now())
	require.False(t, paused, "企业成员无可查额度不得当作耗尽")
}

func TestCreditSnapshotExhausted_StaleSnapshotDoesNotPause(t *testing.T) {
	// 24h 之外的快照不可信：账号一旦被停调就不再收流量，也就没有任何路径再刷新
	// 积分；若不看新鲜度，一次误判会把账号永久锁死。
	stale := time.Now().Add(-25 * time.Hour).Unix()
	paused, _ := shouldAutoPauseAccountByCredits(creditTraeAccount(traeCreditExtra(0, 200, 0, 1, stale)), time.Now())
	require.False(t, paused, "陈旧快照不得参与停调判定")

	fresh := time.Now().Add(-23 * time.Hour).Unix()
	paused, _ = shouldAutoPauseAccountByCredits(creditTraeAccount(traeCreditExtra(0, 200, 0, 1, fresh)), time.Now())
	require.True(t, paused, "窗口内的快照应正常拦截")
}

func TestCreditSnapshotExhausted_MissingFetchedAtDoesNotPause(t *testing.T) {
	// 缺时间戳 ⇒ 无从判断可信度，一律放行（安全性全部建立在"读数够新"之上）。
	paused, _ := shouldAutoPauseAccountByCredits(creditTraeAccount(traeCreditExtra(0, 200, 0, 1, 0)), time.Now())
	require.False(t, paused)
}

func TestCreditSnapshotExhausted_NonCreditPlatformIsNoop(t *testing.T) {
	// 非这三平台即便 extra 里有同名键也不参与判定（平台门在读取侧）。
	account := &Account{
		ID: 9004, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Extra: traeCreditExtra(0, 200, 0, 1, time.Now().Unix()),
	}
	paused, _ := shouldAutoPauseAccountByCredits(account, time.Now())
	require.False(t, paused)
}

// 结构体形态（本进程刚 UpdateExtra、尚未过缓存）也必须能判：两条路径给出不同
// 结论会让"刚探测完"和"重启后"的行为不一致。
func TestCreditSnapshotReadsStructForm(t *testing.T) {
	account := creditTraeAccount(map[string]any{
		traeCreditsExtraKey: TraeCreditsSnapshot{Remain: 0, Size: 200, Used: 200, Packs: 1, FetchedAt: time.Now().Unix()},
	})
	paused, _ := shouldAutoPauseAccountByCredits(account, time.Now())
	require.True(t, paused, "结构体形态的快照与 map 形态必须给出同一结论")

	pointerForm := creditTraeAccount(map[string]any{
		traeCreditsExtraKey: &TraeCreditsSnapshot{Remain: 0, Size: 200, Used: 200, Packs: 1, FetchedAt: time.Now().Unix()},
	})
	paused, _ = shouldAutoPauseAccountByCredits(pointerForm, time.Now())
	require.True(t, paused, "指针形态同样要能判")
}

// 阈值通道：管理员给这三平台配的「用量达 X% 停调」过去恒为 nil 候选
// （候选读的是从不被写入的 <provider>_5h_used_percent），现在必须真能命中。
//
// 【只对 Trae 开】used/size 同源于未过期包聚合、比值可信。Qoder/WorkBuddy 不参与
// 百分比阈值（详见 creditSnapshotThresholdCandidate）：WorkBuddy 的 size 会被
// TotalDosage 抬成历史累计剂量、used 再由 size-remain 反推（占比系统性虚高），
// Qoder 的 total 漏了专属资源包——拿失真的比值去停健号就是误杀。
func TestCreditSnapshotThresholdCandidateUsesSnapshotUsage(t *testing.T) {
	now := time.Now()
	account := creditTraeAccount(traeCreditExtra(20, 200, 0, 1, now.Unix())) // used 90%

	candidates := creditSnapshotThresholdCandidates(account, now)
	require.Len(t, candidates, 1)
	require.Equal(t, 90.0, candidates[0].usedPercent)
	require.NotNil(t, candidates[0].until)
	require.True(t, candidates[0].until.After(now))

	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformTrae: 85}, now)
	require.True(t, decision.ShouldPause, "90% 用量在 85% 阈值下应停调")
	require.Equal(t, creditSnapshotQuotaWindow, decision.Window)

	// 阈值未配置时不得停调（回落语义与 openai/anthropic 一致）。
	decision = EvaluateAccountSchedulingThreshold(account, map[string]int{}, now)
	require.False(t, decision.ShouldPause)
}

func TestCreditSnapshotThresholdCandidateIgnoresStaleSnapshot(t *testing.T) {
	stale := time.Now().Add(-30 * time.Hour).Unix()
	account := creditTraeAccount(traeCreditExtra(20, 200, 0, 1, stale))
	require.Nil(t, creditSnapshotThresholdCandidates(account, time.Now()))
}

// Qoder / WorkBuddy 的 used÷size 不可比（见上方注释），必须不参与百分比阈值。
// 这两个平台的「耗尽」仍由硬闸门负责，不能因为本用例而误以为完全不拦。
func TestCreditSnapshotThresholdCandidateSkipsUnreliableUtilizationPlatforms(t *testing.T) {
	now := time.Now()

	// WorkBuddy：包内 5500/剩 1500（真实约 73%），但既有聚合会把 size 抬到
	// TotalDosage=10000、used 反推成 8500 → 算出 85%。若让这种读数进阈值通道，
	// 管理员设 80% 就会停掉一个还有 1500 余额的健康号。
	workbuddy := creditWorkbuddyAccount(map[string]any{
		workbuddyCreditsExtraKey: map[string]any{
			"remain": float64(1500), "used": float64(8500), "size": float64(10000),
			"pack_size_sum": float64(5500),
			"packs":         float64(2), "fetched_at": float64(now.Unix()),
		},
	})
	require.Nil(t, creditSnapshotThresholdCandidates(workbuddy, now),
		"WorkBuddy 的 size 被 TotalDosage 抬升，占比虚高，不得进百分比阈值")
	require.False(t, EvaluateAccountSchedulingThreshold(
		workbuddy, map[string]int{PlatformWorkbuddy: 80}, now, //nolint:testdata // 阈值不生效
	).ShouldPause)
	// 同时确认它也没被硬闸门误杀（remain>0）。
	paused, _ := shouldAutoPauseAccountByCredits(workbuddy, now)
	require.False(t, paused, "仍有 1500 余额的 WorkBuddy 号不得被停调")

	// Qoder：total 不含专属资源包，同样不得进阈值通道。
	qoder := creditQoderAccount(map[string]any{
		qoderCreditsExtraKey: map[string]any{
			"remain": 0.0, "used": 200.0, "total": 200.0, "packs": 1.0,
			"fetched_at": float64(now.Unix()),
		},
	})
	require.Nil(t, creditSnapshotThresholdCandidates(qoder, now))
	require.False(t, EvaluateAccountSchedulingThreshold(
		qoder, map[string]int{PlatformQoder: 80}, now,
	).ShouldPause)
}

// 两个调度引擎必须给出一致答案，否则「开不开高级调度器」会改变行为；
// reason 同名是 handler 错误分类与 filterStats 的字符串契约。
func TestCreditSnapshotPauseReasonsMatchAcrossSchedulers(t *testing.T) {
	now := time.Now()
	account := creditTraeAccount(traeCreditExtra(0, 200, 0, 1, now.Unix()))
	const requestedModel = "qwen3.8-flash"

	legacy := openAICompatibleAccountEligibilityFailureReasonBeforeProfit(
		context.Background(), account, PlatformTrae, requestedModel, false, OpenAIEndpointCapabilityChatCompletions,
	)
	require.Contains(t, legacy, "quota_auto_pause")

	scheduler := &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{}}
	advanced, reason := scheduler.isAccountRequestCompatibleReason(context.Background(), account, OpenAIAccountScheduleRequest{
		RequestedModel: requestedModel,
	})
	require.False(t, advanced)
	require.Equal(t, legacy, reason, "reason 必须与 legacy 严格同名，否则错误分类随引擎分叉")
}

// 健康号不得被误杀：积分充足时两个引擎都应放行。
func TestCreditSnapshotDoesNotPauseHealthyAccount(t *testing.T) {
	account := creditTraeAccount(traeCreditExtra(150, 200, 150, 2, time.Now().Unix()))
	require.Empty(t, openAICompatibleAccountEligibilityFailureReasonBeforeProfit(
		context.Background(), account, PlatformTrae, "qwen3.8-flash", false, OpenAIEndpointCapabilityChatCompletions,
	))
	scheduler := &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{}}
	compatible, reason := scheduler.isAccountRequestCompatibleReason(context.Background(), account, OpenAIAccountScheduleRequest{
		RequestedModel: "qwen3.8-flash",
	})
	require.True(t, compatible, "积分充足的账号不得被停调闸门拦下（reason=%q）", reason)
}

// 恢复时刻口径：积分耗尽的停调只到次日 0 点，且必须严格晚于当前时刻，
// 否则 candidateMatchesThreshold 的 until.After(now) 会把阈值候选整体作废
// （accountSchedulingThresholdEval.go:413）。
func TestCreditSnapshotRecoveryUntilAlwaysInFuture(t *testing.T) {
	for _, at := range []time.Time{
		time.Now(),
		time.Now().Add(-1 * time.Hour),
		time.Date(2026, 10, 3, 23, 59, 59, 0, time.Local),
	} {
		until := creditSnapshotRecoveryUntil(at)
		require.True(t, until.After(at), "恢复时刻必须晚于当前，否则停调永不生效")
	}
}
