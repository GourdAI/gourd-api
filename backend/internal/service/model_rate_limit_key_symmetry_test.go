package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 冷却键口径对称性回归（本轮 P0-2 / P0-3）。
//
// 背景：model_rate_limits 的写入侧有四套模型名口径（原始名 / 映射名 /
// 上游官方 ID（normalize、无 thinking 后缀）/ 家族常量），而读取侧曾只生成
// [映射名, 原始名]。两个口径不交集的场景下，冷却写进了 DB 却永远读不到，
// 同一个号被反复选中并再撞一遍上游 404。这里把每个场景钉死。

func newAntigravityScopedAccount(id int64, mapping map[string]any, scope string) *Account {
	return &Account{
		ID: id, Platform: PlatformAntigravity,
		Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"model_mapping": mapping},
		Extra: map[string]any{modelRateLimitsKey: map[string]any{
			scope: map[string]any{"rate_limit_reset_at": time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)},
		}},
	}
}

// P0-2：别名 + thinking。写侧 (setModelRateLimitByModelName) 用上游 error metadata
// 里的官方模型 ID（无 -thinking 后缀），而读侧此前只会得到
// [官方名-thinking, 别名] —— 恰好漏掉官方名本身。
func TestModelRateLimitKeys_AntigravityAliasWithThinkingMatchesUpstreamOfficialID(t *testing.T) {
	thinkingCtx := WithThinkingEnabled(context.Background(), true, false)
	account := newAntigravityScopedAccount(
		42901,
		map[string]any{"sonnet": "claude-sonnet-4-5"},
		"claude-sonnet-4-5", // 上游报告的官方模型 ID
	)

	keys := account.modelRateLimitKeysForRequest(thinkingCtx, "sonnet")
	require.Contains(t, keys, "claude-sonnet-4-5",
		"别名+thinking 必须能命中上游写入的官方模型 ID, got=%v", keys)
	require.True(t, account.isModelRateLimitedWithContext(thinkingCtx, "sonnet"),
		"别名用户必须与直接用官方名的用户获得一致的拦截能力")
	require.Greater(t, account.GetModelRateLimitRemainingTimeWithContext(thinkingCtx, "sonnet"), time.Duration(0))

	// 未被冷却的别名不得被误封（appendKey 只放宽键集，不放宽判定）。
	require.False(t, account.isModelRateLimitedWithContext(thinkingCtx, "opus"),
		"其它别名不应命中 claude-sonnet-4-5 的冷却")
}

// P0-3：Antigravity 映射表未覆盖请求模型。写侧 (modelRateLimitKeyForUpstreamModelNotFound)
// 在解析不出时回退原始名，而读侧此前在 modelKey=="" 处直接 return nil，
// 使双键兜底在本平台整体失效。
func TestModelRateLimitKeys_AntigravityUnmappedModelFallsBackToRawName(t *testing.T) {
	account := newAntigravityScopedAccount(
		42902,
		map[string]any{"claude-sonnet-4-5": "claude-sonnet-4-5"},
		"claude-opus-4-8", // 写侧回退到的原始名
	)

	require.Empty(t, mapAntigravityModel(account, "claude-opus-4-8"), "前提：映射表未覆盖该模型")

	keys := account.modelRateLimitKeysForRequest(context.Background(), "claude-opus-4-8")
	require.Equal(t, []string{"claude-opus-4-8"}, keys,
		"读侧必须像写侧一样回退到原始名，而不是返回空键集, got=%v", keys)
	require.True(t, account.isModelRateLimitedWithContext(context.Background(), "claude-opus-4-8"))
}

// 空模型名仍应短路（不得因新增回退逻辑而把「无模型」误判为可冷却）。
func TestModelRateLimitKeys_EmptyRequestedModelStillReturnsNil(t *testing.T) {
	account := newAntigravityScopedAccount(42903, map[string]any{"a": "a"}, "claude-opus-4-8")
	require.Nil(t, account.modelRateLimitKeysForRequest(context.Background(), "   "))
	require.False(t, account.isModelRateLimitedWithContext(context.Background(), ""))
}

// 家族键不得因同一模型名同时命中谓词而重复追加（appendKey 去重）。
func TestModelRateLimitKeys_FamilyScopeAppendedOnce(t *testing.T) {
	account := newAntigravityScopedAccount(
		42904,
		map[string]any{"gemini-3-pro": "gemini-3-pro"},
		antigravityGeminiModelRateLimitKey,
	)
	keys := account.modelRateLimitKeysForRequest(context.Background(), "gemini-3-pro")
	occurrences := 0
	for _, key := range keys {
		if key == antigravityGeminiModelRateLimitKey {
			occurrences++
		}
	}
	require.Equal(t, 1, occurrences, "家族 key 必须只出现一次, keys=%v", keys)
	require.True(t, account.isModelRateLimitedWithContext(context.Background(), "gemini-3-pro"))
}
