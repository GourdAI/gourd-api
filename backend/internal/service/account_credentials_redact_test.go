//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMergePreservingSensitiveCreds_PreservesSensitiveWhenIncomingMissing(t *testing.T) {
	existing := map[string]any{
		"refresh_token": "rt-old",
		"access_token":  "at-old",
		"api_key":       "sk-old",
		"base_url":      "https://old.example.com",
	}
	incoming := map[string]any{
		"base_url":      "https://new.example.com",
		"model_mapping": map[string]any{"foo": "bar"},
	}

	out := MergePreservingSensitiveCreds(existing, incoming)

	require.Equal(t, "rt-old", out["refresh_token"], "incoming 没传 refresh_token，应保留 existing")
	require.Equal(t, "at-old", out["access_token"])
	require.Equal(t, "sk-old", out["api_key"])
	require.Equal(t, "https://new.example.com", out["base_url"], "非敏感键由 incoming 决定")
	require.Equal(t, map[string]any{"foo": "bar"}, out["model_mapping"])
}

func TestMergePreservingSensitiveCreds_OverwritesWhenIncomingProvidesSensitive(t *testing.T) {
	existing := map[string]any{
		"refresh_token": "rt-old",
		"api_key":       "sk-old",
	}
	incoming := map[string]any{
		"refresh_token": "rt-new",
		// 显式没传 api_key —— 应保留
	}
	out := MergePreservingSensitiveCreds(existing, incoming)
	require.Equal(t, "rt-new", out["refresh_token"], "incoming 显式传入应覆盖")
	require.Equal(t, "sk-old", out["api_key"], "incoming 没传应保留")
}

func TestMergePreservingSensitiveCreds_DoesNotMutateInputs(t *testing.T) {
	existing := map[string]any{"refresh_token": "rt"}
	incoming := map[string]any{"base_url": "x"}

	_ = MergePreservingSensitiveCreds(existing, incoming)

	require.Equal(t, "rt", existing["refresh_token"])
	require.NotContains(t, existing, "base_url")
	require.Equal(t, "x", incoming["base_url"])
	require.NotContains(t, incoming, "refresh_token")
}

func TestMergePreservingSensitiveCreds_NilInputs(t *testing.T) {
	out := MergePreservingSensitiveCreds(nil, map[string]any{"base_url": "x"})
	require.Equal(t, "x", out["base_url"])
	require.NotContains(t, out, "refresh_token")

	out2 := MergePreservingSensitiveCreds(map[string]any{"refresh_token": "rt"}, nil)
	require.Equal(t, "rt", out2["refresh_token"])
}

func TestMergePreservingSensitiveCreds_NonSensitiveDeletionAllowed(t *testing.T) {
	existing := map[string]any{
		"refresh_token": "rt",
		"base_url":      "https://old",
		"project_id":    "p1",
	}
	incoming := map[string]any{
		"base_url": "https://new",
		// 不带 project_id —— 等同删除（非敏感键由 incoming 决定）
	}
	out := MergePreservingSensitiveCreds(existing, incoming)
	require.Equal(t, "rt", out["refresh_token"], "敏感键保留")
	require.Equal(t, "https://new", out["base_url"])
	require.NotContains(t, out, "project_id", "非敏感键 incoming 不传 = 删除")
}

func TestIsSensitiveCredentialKey(t *testing.T) {
	require.True(t, IsSensitiveCredentialKey("refresh_token"))
	require.True(t, IsSensitiveCredentialKey("api_key"))
	require.True(t, IsSensitiveCredentialKey("private_key"))
	require.False(t, IsSensitiveCredentialKey("base_url"))
	require.False(t, IsSensitiveCredentialKey(""))
	require.False(t, IsSensitiveCredentialKey("model_mapping"))
}

// Trae 建档兼容直接粘贴 traework2api 的 auth 文件（camelCase 字段），而敏感清单用
// snake_case 书写：若判定做精确匹配，accessToken/refreshToken 会整体绕过响应脱敏、
// 在管理端明文回显。归一后两种键形都必须命中。
func TestIsSensitiveCredentialKeyMatchesCamelCaseAliases(t *testing.T) {
	require.True(t, IsSensitiveCredentialKey("accessToken"))
	require.True(t, IsSensitiveCredentialKey("refreshToken"))
	require.True(t, IsSensitiveCredentialKey("idToken"))
	require.True(t, IsSensitiveCredentialKey("ApiKey"))
	require.True(t, IsSensitiveCredentialKey("ACCESS_TOKEN"))
	require.True(t, IsSensitiveCredentialKey("access-token"))
	require.True(t, IsSensitiveCredentialKey("clearTextPassword"))

	// 非敏感键不得被误伤（否则前端编辑后会丢配置）。
	require.False(t, IsSensitiveCredentialKey("billingBaseUrl"))
	require.False(t, IsSensitiveCredentialKey("oauthBaseUrl"))
	require.False(t, IsSensitiveCredentialKey("deviceId"))
	require.False(t, IsSensitiveCredentialKey("machineId"))
	require.False(t, IsSensitiveCredentialKey("ideVersionCode"))
	require.False(t, IsSensitiveCredentialKey("uid"))
	require.False(t, IsSensitiveCredentialKey("nickname"))
	require.False(t, IsSensitiveCredentialKey("realm"))
	require.False(t, IsSensitiveCredentialKey("reqSource"))
}

// has_<key> 状态位的规范名：别名建档也要得到前端认得的 has_access_token。
func TestCanonicalCredentialKey(t *testing.T) {
	require.Equal(t, "access_token", CanonicalCredentialKey("accessToken"))
	require.Equal(t, "access_token", CanonicalCredentialKey("ACCESS_TOKEN"))
	require.Equal(t, "refresh_token", CanonicalCredentialKey("refreshToken"))
	require.Equal(t, "base_url", CanonicalCredentialKey("base_url"), "非敏感键原样返回")
	require.Equal(t, "deviceId", CanonicalCredentialKey("deviceId"))
}

// 用户以别名提交新 token 时，不得再把 existing 的 snake 旧副本回填上去——
// 否则一条凭据两份副本，且旧副本永不轮换（下次换票用失效的旧 refreshToken）。
func TestMergePreservingSensitiveCredsAliasCountsAsProvided(t *testing.T) {
	existing := map[string]any{"access_token": "at-old", "refresh_token": "rt-old"}
	incoming := map[string]any{"accessToken": "at-new", "realm": "cn"}

	out := MergePreservingSensitiveCreds(existing, incoming)

	require.Equal(t, "at-new", out["accessToken"])
	require.NotContains(t, out, "access_token", "别名已提供新值，不得回填旧的 snake 副本")
	require.Equal(t, "rt-old", out["refresh_token"], "未提交的敏感键仍须保留")
	require.Equal(t, "cn", out["realm"])
}
