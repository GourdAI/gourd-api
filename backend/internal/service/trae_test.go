//go:build unit

package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// trae_test.go 覆盖 trae.go 的账号侧基础行为：realm 判定、三域 base_url 解析与
// 覆盖、凭据读写的 snake/camel 兼容，以及 NormalizeTraeCredentials 的归一/校验纪律。

func traeAccount(id int64, creds map[string]any) *Account {
	if creds == nil {
		creds = map[string]any{}
	}
	return &Account{ID: id, Platform: PlatformTrae, Credentials: creds}
}

func TestTraeIsTrae(t *testing.T) {
	t.Parallel()
	require.False(t, (*Account)(nil).IsTrae(), "nil 账号不得 panic")
	require.False(t, (&Account{Platform: PlatformOpenAI}).IsTrae())
	require.True(t, (&Account{Platform: PlatformTrae}).IsTrae())
}

func TestTraeGetTraeRealm(t *testing.T) {
	t.Parallel()
	require.Equal(t, "", (&Account{Platform: PlatformOpenAI}).GetTraeRealm(), "非 trae 账号无 realm")
	require.Equal(t, "", (*Account)(nil).GetTraeRealm())

	cases := []struct {
		name     string
		creds    map[string]any
		expected string
	}{
		{"explicit cn", map[string]any{"realm": "cn"}, "cn"},
		{"explicit global", map[string]any{"realm": "global"}, "global"},
		{"realm 大小写与空白宽容", map[string]any{"realm": " GLOBAL "}, "global"},
		{"realm 小写 CN", map[string]any{"realm": "CN"}, "cn"},
		// 显式 realm 优先于主机推断（故意与 base_url 冲突）。
		{"explicit realm 覆盖主机推断", map[string]any{
			"realm": "global", "base_url": "https://trae-api-cn.mchost.guru",
		}, "global"},
		{"由聊天域主机推断 cn", map[string]any{"base_url": "https://trae-api-cn.mchost.guru"}, "cn"},
		{"由聊天域主机推断 global", map[string]any{"base_url": "https://a0ai-api-sg.byteintlapi.com"}, "global"},
		{"由 billing 域主机推断 cn", map[string]any{"billing_base_url": "https://api.trae.cn"}, "cn"},
		{"由 billing 域主机推断 global", map[string]any{"billing_base_url": "https://api.trae.ai"}, "global"},
		{"oauth 域 trae.com.cn 归 cn（base_url 承载）", map[string]any{"base_url": "api.trae.com.cn"}, "cn"},
		// 缺省与不可识别主机一律按 cn（保守口径）。
		{"无凭据缺省 cn", map[string]any{}, "cn"},
		{"未知主机缺省 cn", map[string]any{"base_url": "https://relay.example.com"}, "cn"},
		{"realm 非法值不参与判定（读取侧宽容，写入侧才报错）", map[string]any{"realm": "eu"}, "cn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, traeAccount(1, tc.creds).GetTraeRealm())
		})
	}
}

func TestTraeRealmFromValuesIsSharedByReaderAndWriter(t *testing.T) {
	t.Parallel()
	// 读取侧与凭据归一共用同一判定函数：同输入必须同输出（防两处漂移）。
	require.Equal(t,
		traeRealmFromValues("", "https://a0ai-api-sg.byteintlapi.com", ""),
		traeAccount(2, map[string]any{"base_url": "https://a0ai-api-sg.byteintlapi.com", "access_token": "t"}).GetTraeRealm())
}

func TestTraeBaseURLsResolveByRealmAndOverride(t *testing.T) {
	t.Parallel()

	cn := traeAccount(3, map[string]any{"access_token": "t"})
	require.Equal(t, DefaultTraeBaseURL, cn.GetTraeBaseURL())
	require.Equal(t, DefaultTraeBillingBaseURL, cn.GetTraeBillingBaseURL())
	require.Equal(t, DefaultTraeOAuthBaseURL, cn.GetTraeOAuthBaseURL())

	global := traeAccount(4, map[string]any{"access_token": "t", "realm": "global"})
	require.Equal(t, DefaultTraeGlobalBaseURL, global.GetTraeBaseURL())
	require.Equal(t, DefaultTraeGlobalBillingBaseURL, global.GetTraeBillingBaseURL())
	require.Equal(t, DefaultTraeGlobalOAuthBaseURL, global.GetTraeOAuthBaseURL())

	// 三域各自独立覆盖：聊天自定义中转不得作用于 UG / OAuth 域。
	custom := traeAccount(5, map[string]any{
		"access_token":     "t",
		"base_url":         "https://relay.example.com/",
		"billing_base_url": "https://ug.example.com",
		"oauth_base_url":   "https://oauth.example.com",
	})
	require.Equal(t, "https://relay.example.com", custom.GetTraeBaseURL(), "尾斜杠必须被裁掉")
	require.Equal(t, "https://ug.example.com", custom.GetTraeBillingBaseURL())
	require.Equal(t, "https://oauth.example.com", custom.GetTraeOAuthBaseURL())

	// 只覆盖聊天域时，UG/OAuth 仍按 realm 取默认值。
	chatOnly := traeAccount(6, map[string]any{"access_token": "t", "base_url": "https://relay.example.com"})
	require.Equal(t, "https://relay.example.com", chatOnly.GetTraeBaseURL())
	require.Equal(t, DefaultTraeBillingBaseURL, chatOnly.GetTraeBillingBaseURL(), "base_url 不得污染 UG 域")
	require.Equal(t, DefaultTraeOAuthBaseURL, chatOnly.GetTraeOAuthBaseURL())

	require.Equal(t, "", (*Account)(nil).GetTraeBaseURL())
	require.Equal(t, "", (&Account{Platform: PlatformOpenAI}).GetTraeBillingBaseURL())
	require.Equal(t, "", (&Account{Platform: PlatformOpenAI}).GetTraeOAuthBaseURL())
}

func TestTraeGetTraeCredentialsReadsSnakeAndCamel(t *testing.T) {
	t.Parallel()
	require.Equal(t, TraeCredentials{}, (*Account)(nil).GetTraeCredentials())
	require.Equal(t, TraeCredentials{}, (&Account{Platform: PlatformOpenAI, Credentials: map[string]any{"access_token": "x"}}).GetTraeCredentials(),
		"非 trae 账号不得读到 trae 凭据")

	// snake_case 全量。
	snake := traeAccount(8, map[string]any{
		"access_token": "at", "refresh_token": "rt", "uid": "u1", "nickname": "nick",
		"realm": "global", "base_url": "https://b", "device_id": "1234567890123456",
		"machine_id": "abc", "ide_version": "0.1.62", "ide_version_code": "20260901",
		"device_brand": "83DG", "device_type": "windows", "os_version": "Win11",
		"billing_base_url": "https://ug", "oauth_base_url": "https://oa", "client_id": "cid",
		"expires_at": "1767225600", "refresh_expires_at": "1798848000", "req_source": "2",
	})
	creds := snake.GetTraeCredentials()
	require.Equal(t, "at", creds.AccessToken)
	require.Equal(t, "rt", creds.RefreshToken)
	require.Equal(t, "u1", creds.UID)
	require.Equal(t, "nick", creds.Nickname)
	require.Equal(t, "global", creds.Realm)
	require.Equal(t, "1234567890123456", creds.DeviceID)
	require.Equal(t, "20260901", creds.IDEVersionCode)
	require.Equal(t, "cid", creds.ClientID)
	require.EqualValues(t, 1767225600, creds.ExpiresAt)
	require.EqualValues(t, 1798848000, creds.RefreshExpireAt)
	require.Equal(t, 2, creds.ReqSource)

	// camelCase 直接粘贴 traework2api auth 文件字段。
	camel := traeAccount(9, map[string]any{
		"accessToken": "at-c", "refreshToken": "rt-c", "userId": "u9",
		"deviceId": "1234567890123457", "machineId": "deadbeef",
		"ideVersion": "0.1.63", "ideVersionCode": "20261001",
		"deviceBrand": "AA", "deviceType": "mac", "osVersion": "macOS",
		"billingBaseUrl": "https://ug-c", "oauthBaseUrl": "https://oa-c",
		"clientId": "cid-c", "expiresAt": "1767225600000", "refreshExpiredAt": "1798848000",
		"reqSource": "2",
	}).GetTraeCredentials()
	require.Equal(t, "at-c", camel.AccessToken)
	require.Equal(t, "rt-c", camel.RefreshToken)
	require.Equal(t, "u9", camel.UID)
	require.Equal(t, "1234567890123457", camel.DeviceID)
	require.Equal(t, "deadbeef", camel.MachineID)
	require.Equal(t, "20261001", camel.IDEVersionCode)
	require.Equal(t, "https://ug-c", camel.BillingBaseURL)
	require.Equal(t, "cid-c", camel.ClientID)
	require.EqualValues(t, 1767225600, camel.ExpiresAt, "毫秒必须归一为秒")
	require.Equal(t, 2, camel.ReqSource)

	// snake 优先于 camel（同键并存时不互相覆盖）。
	both := traeAccount(10, map[string]any{"access_token": "snake", "accessToken": "camel"}).GetTraeCredentials()
	require.Equal(t, "snake", both.AccessToken)

	// 数值形态（JSON 反序列化后的 float64）也要能读出来。
	numeric := traeAccount(11, map[string]any{"expires_at": float64(1767225600), "req_source": float64(2)}).GetTraeCredentials()
	require.EqualValues(t, 1767225600, numeric.ExpiresAt)
	require.Equal(t, 2, numeric.ReqSource)

	// req_source 非正数/非数字一律回落 0（由 traeReqSource 兜成默认 1）。
	require.Zero(t, traeAccount(12, map[string]any{"req_source": "-1"}).GetTraeCredentials().ReqSource)
	require.Zero(t, traeAccount(13, map[string]any{"req_source": "abc"}).GetTraeCredentials().ReqSource)
	require.Equal(t, traeDefaultReqSource, traeReqSource(traeAccount(13, map[string]any{"access_token": "t"})))
	require.Equal(t, 2, traeReqSource(traeAccount(14, map[string]any{"access_token": "t", "req_source": "2"})))
}

// 回归（code review P1）：用户粘的源是 Trae 本地 storage.json，字段是**过去式**
// expiredAt / refreshExpiredAt（且常为 ISO 串或毫秒），不是 expiresAt。
// 旧实现只读 expiresAt，粘进来的到期时间会被静默丢弃，直接错过 refreshToken
// 过期告警（过期后无法自动续期，必须人工重登重取凭据）。
func TestTraeGetTraeCredentialsReadsStorageJSONShapes(t *testing.T) {
	t.Parallel()

	pastTense := traeAccount(15, map[string]any{
		"access_token": "at", "expiredAt": "1767225600", "refreshExpiredAt": "1798848000",
	}).GetTraeCredentials()
	require.EqualValues(t, 1767225600, pastTense.ExpiresAt, "expiredAt 必须被读到")
	require.EqualValues(t, 1798848000, pastTense.RefreshExpireAt)

	presentTense := traeAccount(16, map[string]any{
		"access_token": "at", "expiresAt": "1767225600", "refreshExpiresAt": "1798848000",
	}).GetTraeCredentials()
	require.EqualValues(t, 1767225600, presentTense.ExpiresAt, "expiresAt 同样支持")
	require.EqualValues(t, 1798848000, presentTense.RefreshExpireAt)

	// storage.json 的到期字段常是 ISO 串（"2026-09-30T12:00:00+08:00"）：旧实现
	// ParseInt 失败就当不存在，现在必须回落到 ISO 解析。
	iso := traeAccount(17, map[string]any{
		"access_token": "at", "expiredAt": "2026-09-30T12:00:00+08:00",
	}).GetTraeCredentials()
	require.EqualValues(t, 1790740800, iso.ExpiresAt)

	// 毫秒同样归一为秒。
	millis := traeAccount(18, map[string]any{"access_token": "at", "expiredAt": "1767225600000"}).GetTraeCredentials()
	require.EqualValues(t, 1767225600, millis.ExpiresAt)

	// snake 优先于别名（同键并存不互相覆盖）。
	preferSnake := traeAccount(19, map[string]any{"expires_at": "111", "expiredAt": "222"}).GetTraeCredentials()
	require.EqualValues(t, 111, preferSnake.ExpiresAt)

	// 归一写回：expiredAt 入库后变成 expires_at，库里不留两套名字。
	credentials := map[string]any{
		"access_token": "at", "expiredAt": float64(1767225600000), "refreshExpiredAt": "1798848000",
	}
	require.NoError(t, NormalizeTraeCredentials(credentials))
	require.NotContains(t, credentials, "expiredAt")
	require.NotContains(t, credentials, "refreshExpiredAt")
	// 断言到「读回来的语义值」而不是 map 里的存储类型：归一只在需要换算（毫秒→秒）
	// 时改写值，已是秒的字符串保持原样，由 GetTraeCredentials 统一解析。
	require.Equal(t, "1767225600", traeCredentialValueString(credentials["expires_at"]),
		"毫秒必须归一为秒后写回")
	require.Equal(t, "1798848000", traeCredentialValueString(credentials["refresh_expires_at"]))
	stored := traeAccount(20, credentials).GetTraeCredentials()
	require.EqualValues(t, 1767225600, stored.ExpiresAt)
	require.EqualValues(t, 1798848000, stored.RefreshExpireAt)
}

func TestTraeNormalizeEpochSeconds(t *testing.T) {
	t.Parallel()
	require.EqualValues(t, 1_000_000_000_000, traeNormalizeEpochSeconds(1_000_000_000_000), "恰好等于阈值不视为毫秒")
	require.EqualValues(t, 1_767_225_600, traeNormalizeEpochSeconds(1_767_225_600_000), "毫秒必须除 1000")
	require.EqualValues(t, 1_767_225_600, traeNormalizeEpochSeconds(1_767_225_600), "秒保持原值")
	require.Zero(t, traeNormalizeEpochSeconds(0))
}

func TestTraeParseISOTimeOrZero(t *testing.T) {
	t.Parallel()
	want, err := time.Parse(time.RFC3339, "2026-09-30T12:00:00+08:00")
	require.NoError(t, err)
	require.EqualValues(t, want.Unix(), traeParseISOTimeOrZero("2026-09-30T12:00:00+08:00"))
	require.EqualValues(t, want.Unix(), traeParseISOTimeOrZero(" 2026-09-30T12:00:00+08:00 "))

	bare, err := time.Parse("2006-01-02T15:04:05", "2026-09-30T12:00:00")
	require.NoError(t, err)
	require.EqualValues(t, bare.Unix(), traeParseISOTimeOrZero("2026-09-30T12:00:00"))

	spaced, err := time.Parse("2006-01-02 15:04:05", "2026-09-30 12:00:00")
	require.NoError(t, err)
	require.EqualValues(t, spaced.Unix(), traeParseISOTimeOrZero("2026-09-30 12:00:00"))

	require.Zero(t, traeParseISOTimeOrZero(""))
	require.Zero(t, traeParseISOTimeOrZero("not-a-time"))
}

func TestTraeNormalizeCredentialsCamelToSnake(t *testing.T) {
	t.Parallel()
	creds := map[string]any{
		"accessToken":      "at",
		"refreshToken":     "rt",
		"expiresAt":        json.Number("1767225600"),
		"refreshExpiredAt": json.Number("1798848000"),
		"deviceId":         "1234567890123456",
		"machineId":        "deadbeef",
		"ideVersion":       "0.1.62",
		"ideVersionCode":   "20260901",
		"deviceBrand":      "83DG",
		"deviceType":       "windows",
		"osVersion":        "Win11",
		"billingBaseUrl":   "https://api.trae.cn",
		"oauthBaseUrl":     "https://api.trae.com.cn",
		"clientId":         "cid",
		"reqSource":        json.Number("2"),
		"userId":           "u1",
		"api_key":          "keep-me",
	}
	require.NoError(t, NormalizeTraeCredentials(creds))

	for _, camel := range []string{"accessToken", "refreshToken", "expiresAt", "refreshExpiredAt",
		"deviceId", "machineId", "ideVersion", "ideVersionCode", "deviceBrand", "deviceType",
		"osVersion", "billingBaseUrl", "oauthBaseUrl", "clientId", "reqSource", "userId"} {
		require.NotContains(t, creds, camel, "camelCase 键必须被移除")
	}
	require.Equal(t, "at", creds["access_token"])
	require.Equal(t, "rt", creds["refresh_token"])
	require.Equal(t, "u1", creds["uid"])
	require.Equal(t, "1234567890123456", creds["device_id"])
	require.Equal(t, "20260901", creds["ide_version_code"])
	require.Equal(t, "https://api.trae.cn", creds["billing_base_url"])
	require.Equal(t, "cid", creds["client_id"])
	require.Equal(t, "keep-me", creds["api_key"], "未识别键原样保留")
	require.Equal(t, "cn", creds["realm"], "realm 缺省按主机推断后回填")
	require.Equal(t, 2, creds["req_source"])
}

func TestTraeNormalizeCredentialsRejectsInvalidRealm(t *testing.T) {
	t.Parallel()
	err := NormalizeTraeCredentials(map[string]any{"access_token": "at", "realm": "eu"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "INVALID_TRAE_CREDENTIALS")
	require.Contains(t, err.Error(), "realm")

	require.NoError(t, NormalizeTraeCredentials(map[string]any{"access_token": "at", "realm": " Global "}))
	// 非法 realm 的判定不受大小写影响，但未知值一律拒绝（防止静默走错域）。
	require.Error(t, NormalizeTraeCredentials(map[string]any{"refresh_token": "rt", "realm": "JP"}))
}

func TestTraeNormalizeCredentialsRequiresToken(t *testing.T) {
	t.Parallel()
	// 只有 realm 键也算 trae 凭据，但 access/refresh 全空必须报错。
	err := NormalizeTraeCredentials(map[string]any{"realm": "cn", "uid": "u"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "access_token or refresh_token is required")

	// 仅 refresh_token 合法（建档后首次出站前换票）。
	require.NoError(t, NormalizeTraeCredentials(map[string]any{"refresh_token": "rt"}))

	// 空白串等同缺失。
	require.Error(t, NormalizeTraeCredentials(map[string]any{"access_token": "  ", "refresh_token": ""}))
}

func TestTraeNormalizeCredentialsEpochMillisAndISO(t *testing.T) {
	t.Parallel()
	creds := map[string]any{
		"access_token":       "at",
		"expires_at":         int64(1_767_225_600_000),
		"refresh_expires_at": "2026-09-30T12:00:00+08:00",
	}
	require.NoError(t, NormalizeTraeCredentials(creds))
	require.EqualValues(t, 1_767_225_600, creds["expires_at"], "毫秒必须归一为秒")
	want, err := time.Parse(time.RFC3339, "2026-09-30T12:00:00+08:00")
	require.NoError(t, err)
	require.EqualValues(t, want.Unix(), creds["refresh_expires_at"], "ISO 串必须解析为 epoch 秒")

	// 解析不了的时间串直接丢弃，不得污染后续 ParseInt。
	junk := map[string]any{"access_token": "at", "expires_at": "tomorrow", "refresh_expires_at": "  "}
	require.NoError(t, NormalizeTraeCredentials(junk))
	require.NotContains(t, junk, "expires_at")
	require.NotContains(t, junk, "refresh_expires_at")

	// 已是秒的值保持原样（不触发写回）。
	sec := map[string]any{"access_token": "at", "expires_at": "1767225600"}
	require.NoError(t, NormalizeTraeCredentials(sec))
	require.Equal(t, "1767225600", sec["expires_at"])
}

func TestTraeNormalizeCredentialsReqSource(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   any
		want int
	}{
		{1, 1}, {2, 2}, {"2", 2}, {"1", 1},
		{float64(2), 2}, {json.Number("2"), 2},
		{3, 1}, {0, 1}, {-5, 1}, {"abc", 1}, {true, 1},
	}
	for _, tc := range cases {
		creds := map[string]any{"access_token": "at", "req_source": tc.in}
		require.NoError(t, NormalizeTraeCredentials(creds), "in=%v", tc.in)
		require.Equal(t, tc.want, creds["req_source"], "in=%v 应归一为 %d", tc.in, tc.want)
	}
	// 未提供 req_source 时不得凭空写入该键。
	absent := map[string]any{"access_token": "at"}
	require.NoError(t, NormalizeTraeCredentials(absent))
	require.NotContains(t, absent, "req_source")
}

func TestNormalizeTraeCredentialsIsNoopWithoutTraeKeys(t *testing.T) {
	t.Parallel()
	mixed := map[string]any{"api_key": "sk-1", "base_url": "https://other.example.com"}
	require.NoError(t, NormalizeTraeCredentials(mixed))
	require.Equal(t, map[string]any{"api_key": "sk-1", "base_url": "https://other.example.com"}, mixed,
		"批量更新等混合平台路径必须完全无副作用")

	require.NoError(t, NormalizeTraeCredentials(nil))

	// 值为 nil 的 trae 键不算"携带 trae 凭据"（hasTraeCredentialKeys 判定）。
	nilOnly := map[string]any{"access_token": nil, "api_key": "k"}
	require.NoError(t, NormalizeTraeCredentials(nilOnly))
	require.Equal(t, "k", nilOnly["api_key"])
	require.NotContains(t, nilOnly, "realm", "nil 值键不得触发归一化")

	// 但只要有任一非 nil 的 trae 键，归一化就生效。
	normalized := map[string]any{"refresh_token": "rt", "req_source": 9}
	require.NoError(t, NormalizeTraeCredentials(normalized))
	require.Equal(t, 1, normalized["req_source"])
	require.Equal(t, "cn", normalized["realm"])
}

func TestTraeDefaultModelIDs(t *testing.T) {
	t.Parallel()
	ids := DefaultTraeModelIDs()
	require.NotEmpty(t, ids)
	require.Contains(t, ids, "auto")
	require.Contains(t, ids, defaultTraeTestModel, "连接测试缺省模型必须在默认目录里")
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		require.NotEmpty(t, id)
		_, dup := seen[id]
		require.False(t, dup, "默认模型目录不得有重复项: %s", id)
		seen[id] = struct{}{}
	}
}

func TestTraeURILowerHost(t *testing.T) {
	t.Parallel()
	require.Equal(t, "", traeURILowerHost(""))
	require.Equal(t, "", traeURILowerHost("   "))
	require.Equal(t, "trae-api-cn.mchost.guru", traeURILowerHost("https://TRAE-API-CN.mchost.guru/api/x?y=1"))
	require.Equal(t, "api.trae.cn", traeURILowerHost("api.trae.cn:443/trae"))
	require.Equal(t, "127.0.0.1", traeURILowerHost("http://user:pass@127.0.0.1:8080/p"))
}
