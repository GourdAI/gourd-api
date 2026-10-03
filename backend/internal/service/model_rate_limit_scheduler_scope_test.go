package service

// 模型级限流的「写入口径 / 读出口径」对称性，以及高级调度器对 per-model 冷却的闸门。
//
// 背景缺陷：写入侧有两套模型名口径——
//   - 管理员自定义临时不可调度规则 (triggerTempUnschedulable) 写「客户端原始模型名」；
//   - model-not-found / Codex Spark / 生图冷却写「账号映射后的上游模型名」。
//
// 读取侧只认映射名时，配了 model_mapping 的账号里写在原始名上的冷却永远读不到，
// 限流账号继续被选中、继续撞同一个上游错误（对外表现为反复 404）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestModelRateLimitKeys_RequestScopedKeys 锁定调度读取键集合：
// 原始名与映射名都必须参与判定，家族 scope 不能被重复计入。
func TestModelRateLimitKeys_RequestScopedKeys(t *testing.T) {
	ctx := context.Background()

	t.Run("mapped and raw keys both readable", func(t *testing.T) {
		account := &Account{
			Platform: PlatformAnthropic,
			Credentials: map[string]any{
				"model_mapping": map[string]any{"claude-sonnet-4-5": "claude-sonnet-4-5-20250915"},
			},
		}
		keys := account.modelRateLimitKeysForRequest(ctx, "claude-sonnet-4-5")
		require.Equal(t, []string{"claude-sonnet-4-5-20250915", "claude-sonnet-4-5"}, keys)
	})

	t.Run("no mapping yields single key", func(t *testing.T) {
		account := &Account{Platform: PlatformAnthropic}
		require.Equal(t, []string{"claude-sonnet-4-5"}, account.modelRateLimitKeysForRequest(ctx, "claude-sonnet-4-5"))
	})

	t.Run("family scope is not duplicated", func(t *testing.T) {
		// 映射目标本身就是家族 scope 时，不能再追加一次同名键。
		account := &Account{
			Platform: PlatformAnthropic,
			Credentials: map[string]any{
				"model_mapping": map[string]any{"my-fable": anthropicFableRateLimitKey},
			},
		}
		keys := account.modelRateLimitKeysForRequest(ctx, "my-fable")
		require.Equal(t, anthropicFableRateLimitKey, keys[0])
		require.False(t, containsString(keys[1:], anthropicFableRateLimitKey),
			"family scope 必须只出现一次，got=%v", keys)
	})

	t.Run("raw alias still triggers family scope", func(t *testing.T) {
		// 映射名不含家族词、但原始名含时，家族 scope 仍要纳入。
		account := &Account{
			Platform: PlatformAnthropic,
			Credentials: map[string]any{
				"model_mapping": map[string]any{"claude-fable-5": "upstream-alias-9"},
			},
		}
		keys := account.modelRateLimitKeysForRequest(ctx, "claude-fable-5")
		require.Contains(t, keys, anthropicFableRateLimitKey)
	})
}

// TestIsModelRateLimited_RuleWrittenUnderRawModelName 复现并锁定原始缺陷：
// 限流规则按客户端原始名写入后，配了 model_mapping 的账号必须仍能被判为不可调度。
func TestIsModelRateLimited_RuleWrittenUnderRawModelName(t *testing.T) {
	ctx := context.Background()
	resetAt := time.Now().Add(30 * time.Minute)

	account := &Account{
		Platform:    PlatformAnthropic,
		Credentials: map[string]any{"model_mapping": map[string]any{"claude-sonnet-4-5": "claude-sonnet-4-5-20250915"}},
	}
	// triggerTempUnschedulable 的口径：key = firstRequestedModel(requestedModel)
	setAccountModelRateLimitSnapshot(account, "claude-sonnet-4-5", resetAt, `{"status_code":404}`, time.Now())

	require.True(t, account.isModelRateLimitedWithContext(ctx, "claude-sonnet-4-5"),
		"写在原始模型名上的冷却必须被调度侧读到")
	require.Greater(t, account.GetModelRateLimitRemainingTimeWithContext(ctx, "claude-sonnet-4-5"), time.Duration(0))
}

// TestIsModelRateLimited_UnrelatedModelUnaffected 守住边界：冷却只作用于该模型，
// 同账号的其他模型仍可调度（不能把模型级冷却放大成账号级停调）。
func TestIsModelRateLimited_UnrelatedModelUnaffected(t *testing.T) {
	ctx := context.Background()
	account := &Account{
		Platform:    PlatformAnthropic,
		Credentials: map[string]any{"model_mapping": map[string]any{"claude-sonnet-4-5": "claude-sonnet-4-5-20250915"}},
	}
	setAccountModelRateLimitSnapshot(account, "claude-sonnet-4-5", time.Now().Add(30*time.Minute), "rule", time.Now())

	require.False(t, account.isModelRateLimitedWithContext(ctx, "claude-opus-4-8"))
	require.False(t, account.isModelRateLimitedWithContext(ctx, "claude-haiku-4-5"))
}

// newAdvancedSchedulerProbeService 构造一台启用高级调度器的 OpenAI 网关，
// 池中两个同优先级账号：limited 对 modelName 处于模型级冷却，healthy 无冷却。
func newAdvancedSchedulerProbeService(t *testing.T, modelName string) (*OpenAIGatewayService, *Account, *Account) {
	t.Helper()
	resetAt := time.Now().Add(30 * time.Minute).Format(time.RFC3339)
	limited := &Account{
		ID: 42001, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 4, Priority: 0,
		Extra: map[string]any{modelRateLimitsKey: map[string]any{
			modelName: map[string]any{"rate_limit_reset_at": resetAt},
		}},
	}
	healthy := &Account{
		ID: 42002, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 4, Priority: 0,
	}
	snapshotCache := &openAISnapshotCacheStub{
		snapshotAccounts: []*Account{limited, healthy},
		accountsByID:     map[int64]*Account{42001: limited, 42002: healthy},
	}
	svc := &OpenAIGatewayService{
		accountRepo:      schedulerTestOpenAIAccountRepo{accounts: []Account{*limited, *healthy}},
		cache:            &schedulerTestGatewayCache{},
		cfg:              &config.Config{},
		rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true"),

		schedulerSnapshot:  NewSchedulerSnapshotService(snapshotCache, nil, nil, nil, nil),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	return svc, limited, healthy
}

// TestAdvancedScheduler_CandidateFilterExcludesModelRateLimited 确认高级调度器的
// 候选过滤闸门生效，且 reason 与 legacy 引擎同名（handler 依赖 model_rate_limited=N
// 这一字符串把「全池冷却」判成 429 而不是误导性的 404/503）。
func TestAdvancedScheduler_CandidateFilterExcludesModelRateLimited(t *testing.T) {
	ctx := context.Background()
	svc, limited, healthy := newAdvancedSchedulerProbeService(t, "gpt-5.4")
	scheduler := &defaultOpenAIAccountScheduler{service: svc}
	req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, RequestedModel: "gpt-5.4"}

	ok, reason := scheduler.isAccountRequestCompatibleReason(ctx, limited, req)
	require.False(t, ok, "模型级冷却账号不得进入候选池")
	require.Equal(t, "model_rate_limited", reason)

	// reason 必须与 legacy 引擎一致，否则 handler 的 429 分类会失效。
	legacy := openAICompatibleAccountEligibilityFailureReason(ctx, limited, PlatformOpenAI, "gpt-5.4", false, "")
	require.Equal(t, legacy, reason)

	compatible, reason := scheduler.isAccountRequestCompatibleReason(ctx, healthy, req)
	require.True(t, compatible, "健康账号不受同池冷却影响, reason=%q", reason)
}

// TestAdvancedScheduler_DoesNotSelectModelRateLimitedAccount 端到端锁定选号结果：
// 即便冷却号优先级/绑定关系更「近」，也必须让位给健康号。
func TestAdvancedScheduler_DoesNotSelectModelRateLimitedAccount(t *testing.T) {
	ctx := context.Background()
	svc, _, healthy := newAdvancedSchedulerProbeService(t, "gpt-5.4")

	selection, _, err := svc.SelectAccountWithScheduler(ctx, nil, "", "", "gpt-5.4", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, healthy.ID, selection.Account.ID)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}

	// 其他模型不受该冷却牵连，冷却号仍可服务。
	selection, _, err = svc.SelectAccountWithScheduler(ctx, nil, "", "", "gpt-5.3", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

// TestAdvancedScheduler_FullPoolCooldownSurfacesRateLimitedReason 全池冷却时，
// 错误必须带上 model_rate_limited=N，让 handler 分类为 429 而非误导的 404。
func TestAdvancedScheduler_FullPoolCooldownSurfacesRateLimitedReason(t *testing.T) {
	ctx := context.Background()
	resetAt := time.Now().Add(30 * time.Minute).Format(time.RFC3339)
	limited := &Account{
		ID: 42003, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 4, Priority: 0,
		Extra: map[string]any{modelRateLimitsKey: map[string]any{
			"gpt-5.4": map[string]any{"rate_limit_reset_at": resetAt},
		}},
	}
	snapshotCache := &openAISnapshotCacheStub{
		snapshotAccounts: []*Account{limited},
		accountsByID:     map[int64]*Account{42003: limited},
	}
	svc := &OpenAIGatewayService{
		accountRepo:      schedulerTestOpenAIAccountRepo{accounts: []Account{*limited}},
		cache:            &schedulerTestGatewayCache{},
		cfg:              &config.Config{},
		rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true"),

		schedulerSnapshot:  NewSchedulerSnapshotService(snapshotCache, nil, nil, nil, nil),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	_, _, err := svc.SelectAccountWithScheduler(ctx, nil, "", "", "gpt-5.4", nil, OpenAIUpstreamTransportAny, false)
	require.Error(t, err)
	msg := strings.ToLower(err.Error())
	require.Contains(t, msg, "model_rate_limited=1", "全池冷却必须给出可分类的计数, got=%q", msg)
	// 该计数正是 handler.no_account_error 里 selectionModelRateLimitedPattern 的匹配目标。
	require.Regexp(t, `(?:model_rate_limited|rate_limited)=\d+`, msg)
}
