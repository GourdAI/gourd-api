//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// workbuddy_base_url_test.go WorkBuddy 存量裸域归一化的单测：
// 读取侧 GetWorkbuddyBaseURL / GetWorkbuddyBillingBaseURL 与写入侧
// NormalizeWorkbuddyCredentials 的固定行为。
//
// 背景（2026-09 国际版 404 事件）：裸域 workbuddy.ai 在上游边缘对 POST 无条件 301
// （Location 指向 www），Go http.Client 跟随重定向时把 POST 改写为 GET 并丢弃请求体，
// 上游只注册 POST 路由 → 返回 "404 page not found"。修复分两层：
//  1. 前端默认端点已改为带 www（credentialsBuilder.ts）；
//  2. 后端对存量账号读取/写入侧兜底归一化（本文件覆盖）。
//
// 关键不变量：精确 host 匹配裸域（workbuddy.ai），子域/自定义中转/CN 账号绝不改写。

func workbuddyAccount(id int64, creds map[string]any) *Account {
	if creds == nil {
		creds = map[string]any{}
	}
	return &Account{ID: id, Platform: PlatformWorkbuddy, Credentials: creds}
}

func TestGetWorkbuddyBaseURLNormalizesStoredBareDomain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		account  *Account
		expected string
	}{
		{
			name:     "global 存量裸域读取侧改写为 www",
			account:  workbuddyAccount(1, map[string]any{"realm": "global", "base_url": "https://workbuddy.ai"}),
			expected: "https://www.workbuddy.ai",
		},
		{
			name:     "global 裸域带尾斜杠同样改写",
			account:  workbuddyAccount(2, map[string]any{"realm": "global", "base_url": "https://workbuddy.ai/"}),
			expected: "https://www.workbuddy.ai",
		},
		{
			name:     "global 裸域大小写宽容",
			account:  workbuddyAccount(3, map[string]any{"realm": "global", "base_url": "HTTPS://WorkBuddy.AI"}),
			expected: "https://www.workbuddy.ai",
		},
		{
			name:     "global 已带 www 原样保留",
			account:  workbuddyAccount(4, map[string]any{"realm": "global", "base_url": "https://www.workbuddy.ai"}),
			expected: "https://www.workbuddy.ai",
		},
		{
			name:     "global 自定义中转不误伤",
			account:  workbuddyAccount(5, map[string]any{"realm": "global", "base_url": "https://relay.example.com"}),
			expected: "https://relay.example.com",
		},
		{
			name:     "global 子域不做改写（精确 host 匹配）",
			account:  workbuddyAccount(6, map[string]any{"realm": "global", "base_url": "https://api.workbuddy.ai"}),
			expected: "https://api.workbuddy.ai",
		},
		{
			name:     "cn realm 裸域不改写（仅 global 归一）",
			account:  workbuddyAccount(7, map[string]any{"realm": "cn", "base_url": "https://workbuddy.ai"}),
			expected: "https://workbuddy.ai",
		},
		{
			name:     "realm 缺省但 domain 含 workbuddy.ai 判 global 并改写",
			account:  workbuddyAccount(8, map[string]any{"domain": "www.workbuddy.ai", "base_url": "https://workbuddy.ai"}),
			expected: "https://www.workbuddy.ai",
		},
		{
			name:     "global 无 base_url 回落官方默认（带 www）",
			account:  workbuddyAccount(9, map[string]any{"realm": "global"}),
			expected: "https://www.workbuddy.ai",
		},
		{
			name:     "cn 无 base_url 回落 copilot.tencent.com",
			account:  workbuddyAccount(10, map[string]any{"realm": "cn"}),
			expected: "https://copilot.tencent.com",
		},
		{
			name:     "非 workbuddy 账号返回空",
			account:  &Account{ID: 11, Platform: PlatformOpenAI, Credentials: map[string]any{"realm": "global", "base_url": "https://workbuddy.ai"}},
			expected: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.expected, tt.account.GetWorkbuddyBaseURL())
		})
	}
}

func TestGetWorkbuddyBillingBaseURLNormalizesStoredBareDomain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		account  *Account
		expected string
	}{
		{
			name:     "global 存量裸域 billing 覆盖同样改写",
			account:  workbuddyAccount(20, map[string]any{"realm": "global", "billing_base_url": "https://workbuddy.ai"}),
			expected: "https://www.workbuddy.ai",
		},
		{
			name:     "global 自定义 billing 中转不误伤",
			account:  workbuddyAccount(21, map[string]any{"realm": "global", "billing_base_url": "https://billing.example.com"}),
			expected: "https://billing.example.com",
		},
		{
			name:     "global 无 billing 覆盖回落官网默认",
			account:  workbuddyAccount(22, map[string]any{"realm": "global"}),
			expected: "https://www.workbuddy.ai",
		},
		{
			name:     "cn 无 billing 覆盖回落 codebuddy.cn",
			account:  workbuddyAccount(23, map[string]any{"realm": "cn"}),
			expected: "https://www.codebuddy.cn",
		},
		{
			name:     "chat 自定义中转不得作用于 billing",
			account:  workbuddyAccount(24, map[string]any{"realm": "global", "base_url": "https://relay.example.com"}),
			expected: "https://www.workbuddy.ai",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.expected, tt.account.GetWorkbuddyBillingBaseURL())
		})
	}
}

func TestNormalizeWorkbuddyCredentialsRewritesBareDomain(t *testing.T) {
	t.Parallel()
	t.Run("realm=global 裸域写入侧改写", func(t *testing.T) {
		t.Parallel()
		credentials := map[string]any{
			"access_token": "at-1",
			"realm":        "global",
			"base_url":     "https://workbuddy.ai",
		}
		require.NoError(t, NormalizeWorkbuddyCredentials(credentials))
		require.Equal(t, "https://www.workbuddy.ai", credentials["base_url"])
	})

	t.Run("domain 判定 global 也改写（realm 键缺省）", func(t *testing.T) {
		t.Parallel()
		credentials := map[string]any{
			"access_token": "at-1",
			"domain":       "login.workbuddy.ai",
			"base_url":     "https://workbuddy.ai/",
		}
		require.NoError(t, NormalizeWorkbuddyCredentials(credentials))
		require.Equal(t, "https://www.workbuddy.ai", credentials["base_url"])
	})

	t.Run("billing_base_url 裸域同样改写", func(t *testing.T) {
		t.Parallel()
		credentials := map[string]any{
			"access_token":     "at-1",
			"realm":            "global",
			"billing_base_url": "https://workbuddy.ai",
		}
		require.NoError(t, NormalizeWorkbuddyCredentials(credentials))
		require.Equal(t, "https://www.workbuddy.ai", credentials["billing_base_url"])
	})

	t.Run("cn realm 不改写", func(t *testing.T) {
		t.Parallel()
		credentials := map[string]any{
			"access_token": "at-1",
			"realm":        "cn",
			"base_url":     "https://workbuddy.ai",
		}
		require.NoError(t, NormalizeWorkbuddyCredentials(credentials))
		require.Equal(t, "https://workbuddy.ai", credentials["base_url"])
	})

	t.Run("自定义中转/子域不误伤", func(t *testing.T) {
		t.Parallel()
		credentials := map[string]any{
			"access_token": "at-1",
			"realm":        "global",
			"base_url":     "https://relay.example.com",
		}
		require.NoError(t, NormalizeWorkbuddyCredentials(credentials))
		require.Equal(t, "https://relay.example.com", credentials["base_url"])

		subdomain := map[string]any{
			"access_token": "at-1",
			"realm":        "global",
			"base_url":     "https://api.workbuddy.ai",
		}
		require.NoError(t, NormalizeWorkbuddyCredentials(subdomain))
		require.Equal(t, "https://api.workbuddy.ai", subdomain["base_url"])
	})

	t.Run("已带 www 的存量原样保留（幂等）", func(t *testing.T) {
		t.Parallel()
		credentials := map[string]any{
			"access_token": "at-1",
			"realm":        "global",
			"base_url":     "https://www.workbuddy.ai",
		}
		require.NoError(t, NormalizeWorkbuddyCredentials(credentials))
		require.Equal(t, "https://www.workbuddy.ai", credentials["base_url"])
	})

	t.Run("非法 realm 仍在归一化前被拒", func(t *testing.T) {
		t.Parallel()
		credentials := map[string]any{
			"access_token": "at-1",
			"realm":        "globalx",
			"base_url":     "https://workbuddy.ai",
		}
		require.Error(t, NormalizeWorkbuddyCredentials(credentials))
	})
}
