//go:build unit

package repository

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 调度投影必须保留三平台积分快照键。
//
// 候选过滤走 ListSchedulableAccounts，读的是 buildSchedulerMetadataAccount 产出的
// 精简投影；service 侧的积分耗尽判定（shouldAutoPauseAccountByCredits）与阈值评估
// 都从 account.Extra 取数。一旦投影把 trae_credits / qoder_credits /
// workbuddy_credits 裁掉，这些判定在选号阶段就等于「从未探测」而全部放行，
// 只在抢槽后的 fresh/DB recheck 才生效 —— TopK 候选池仍被耗尽号占满，健康号落在
// 池外选不到。与历史上 model_rate_limits、OpenAI 透传开关被裁掉是同一类 bug
// （见 TestBuildSchedulerMetadataAccount_KeepsOpenAIPassthroughForModelGate）。
//
// 这里守的是喂给判定的**输入**，判定本身的语义由 service 包
// account_credit_quota_test.go 覆盖。
func TestBuildSchedulerMetadataAccount_KeepsCreditSnapshots(t *testing.T) {
	fetchedAt := time.Now().Unix()
	cases := []struct {
		name           string
		platform       string
		extraKey       string
		snapshot       map[string]any
		requiredFields []string
		droppedFields  []string
	}{
		{
			name:     "trae",
			platform: service.PlatformTrae,
			extraKey: "trae_credits",
			snapshot: map[string]any{
				"remain": 0.0, "size": 200.0, "used": 200.0, "credits": 0.0,
				"packs": 1.0, "fetched_at": float64(fetchedAt),
				// 观测明细：不应进投影。
				"packages":         []any{map[string]any{"entitlement_id": "plan_1", "limit": 200.0}},
				"token_expires_at": float64(fetchedAt),
			},
			// remain/size/credits/packs/fetched_at 全部参与耗尽与新鲜度判定。
			requiredFields: []string{"remain", "size", "credits", "packs", "fetched_at"},
			droppedFields:  []string{"packages", "token_expires_at"},
		},
		{
			name:     "qoder",
			platform: service.PlatformQoder,
			extraKey: "qoder_credits",
			snapshot: map[string]any{
				"remain": 0.0, "total": 100.0, "quota_exceeded": true,
				"fetched_at": float64(fetchedAt),
				"packages":   []any{map[string]any{"key": "pack"}},
				"claimable":  false,
			},
			requiredFields: []string{"remain", "total", "quota_exceeded", "fetched_at"},
			droppedFields:  []string{"packages", "claimable"},
		},
		{
			name:     "workbuddy",
			platform: service.PlatformWorkbuddy,
			extraKey: "workbuddy_credits",
			snapshot: map[string]any{
				"remain": float64(0), "size": float64(100), "fetched_at": float64(fetchedAt),
				// pack_size_sum 是区分「真耗尽」与「TotalDosage 伪装的降级读数」的
				// 唯一正向证据，丢了就会把一批健康号误停调。
				"pack_size_sum":  float64(100),
				"not_applicable": true, "enterprise": true,
				"packages": []any{map[string]any{"id": "p"}},
			},
			requiredFields: []string{"remain", "size", "pack_size_sum", "fetched_at", "not_applicable", "enterprise"},
			droppedFields:  []string{"packages"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := service.Account{
				ID:          4400,
				Platform:    tc.platform,
				Type:        service.AccountTypeOAuth,
				Status:      service.StatusActive,
				Schedulable: true,
				Credentials: map[string]any{"access_token": "must-be-dropped"},
				Extra:       map[string]any{tc.extraKey: tc.snapshot},
			}
			require.True(t, account.IsSchedulable(), "前置条件：账号本身应可调度")

			meta := buildSchedulerMetadataAccount(account)

			// 走一遍真实的序列化/反序列化路径（写入 sched:meta 再由 decodeCachedAccount 读回）。
			payload, err := json.Marshal(meta)
			require.NoError(t, err)
			var restored service.Account
			require.NoError(t, json.Unmarshal(payload, &restored))

			require.NotNil(t, restored.Extra[tc.extraKey],
				"投影裁掉积分快照会让候选过滤阶段读不到耗尽信号")
			// 裁剪后，service 层判定所需的字段必须逐个存活（任何一个缺失都会被
			// 读侧归一为零值，把「耗尽」读成「未配置」而不拦，或反过来误杀健康号）。
			trimmed, ok := restored.Extra[tc.extraKey].(map[string]any)
			require.True(t, ok, "进投影后必须是裁剪后的 map 形态（不得整块透传 Packages）")
			for _, field := range tc.requiredFields {
				require.Contains(t, trimmed, field, "调度判定依赖的字段被投影裁掉了")
			}
			// 反面对照：观测类大字段必须被裁掉，否则本裁剪等于空操作。
			for _, field := range tc.droppedFields {
				require.NotContains(t, trimmed, field,
					"Packages 等观测明细不应进 Redis 投影")
			}
			// 凭据仍必须被裁掉：本修复只放开三个观测键，不得顺带放宽 credentials。
			require.Nil(t, restored.Credentials["access_token"],
				"积分快照进投影不得连带泄漏 credentials")
		})
	}
}

// 锁定积分快照键在 schedulerNeutralExtraKeys 上的**现状**（三者并不一致）：
//
//	trae_credits / qoder_credits  —— 非 neutral：写入会 enqueue outbox（可触发整桶重建）
//	workbuddy_credits          —— neutral ：只刷单账号快照，不整桶重建
//
// 这个差异是**先前存在**的（workbuddy 当初作为纯观测数据入库时被标 neutral，
// trae/qoder 未跟进），不是一致性缺陷：两条路径都会经 syncSchedulerAccountSnapshot
// 刷新候选池读的 meta 投影，差别仅在重建粒度。
// 本用例的作用是把现状钉住：防止看到不一致就「顺手统一」——把 trae/qoder 改成
// neutral 看似省了重建，但三平台快照如今已参与调度决策（耗尽停调、阈值停调），
// 靠单账号快照同步能满，但 outbox 同时承担 sticky 会话的失效广播，改动前必须
// 重新评估；把 workbuddy 改成非 neutral 则会让签到批量刷积分变成整桶重建。
func TestSchedulerOutboxCreditSnapshotNeutralStatusIsIntentional(t *testing.T) {
	require.False(t, isSchedulerNeutralExtraKey("trae_credits"))
	require.False(t, isSchedulerNeutralExtraKey("qoder_credits"))
	require.True(t, isSchedulerNeutralExtraKey("workbuddy_credits"),
		"workbuddy_credits 的 neutral 标记是既有设定，改动前请评估签到批量刷新的重建成本")

	// 行为层对照：两条路径都必须仍能让候选池读到最新读数——即「进不进 outbox」
	// 不影响 meta 投影的新鲜度（由 SetAccount 写 writeAccountIDs 同时刷两份）。
	cache, _ := newSchedulerCacheUnitWithRedis(t)
	ctx := context.Background()
	fetchedAt := time.Now().Unix()

	for _, tc := range []struct {
		name     string
		platform string
		extraKey string
	}{
		{"trae (outbox path)", service.PlatformTrae, "trae_credits"},
		{"workbuddy (neutral path)", service.PlatformWorkbuddy, "workbuddy_credits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := service.Account{
				ID:          4411,
				Platform:    tc.platform,
				Type:        service.AccountTypeOAuth,
				Status:      service.StatusActive,
				Schedulable: true,
				Extra:       map[string]any{tc.extraKey: map[string]any{"remain": 0.0, "size": 100.0, "fetched_at": float64(fetchedAt)}},
			}
			require.NoError(t, cache.SetAccount(ctx, &account))

			payload, err := cache.rdb.Get(ctx, schedulerAccountMetaKey(strconv.FormatInt(account.ID, 10))).Result()
			require.NoError(t, err)
			var restored service.Account
			require.NoError(t, json.Unmarshal([]byte(payload), &restored))
			require.NotNil(t, restored.Extra[tc.extraKey],
				"不论是否走 outbox，meta 投影都必须带上积分快照供候选过滤读取")
		})
	}
}
