//go:build unit

package service

// trae_token_renewal_test.go 钉住 Trae 令牌续期的三个缺口（2026-10-27 排查）。
//
// 三个缺口缺一不可，否则「access_token 到期后全量报错且无法自愈」会重演：
//  1. 凭据缺 expires_at 时预刷新永不触发 → 现在读路径有 JWT exp 兜底；
//  2. TokenRefreshService 注册表里没有 Trae → 现在 SetTraeRefresher 注册；
//  3. 撞 401 / 流内 1001 时永久 SetError → 现在改为临时冷却（有 refresh_token 时）。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// zzTraeJWT 造一个 payload 段含指定 exp 的（未签名）JWT。
// 真实上游 token 是签名 JWT，但本仓库读 exp 时明确不做签名校验（见 trae_jwt.go）。
func zzTraeJWT(t *testing.T, exp int64) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{
		"exp":  exp,
		"iat":  exp - 86400,
		"data": map[string]any{"id": "uid-from-jwt"},
	})
	require.NoError(t, err)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

// TestGetTraeCredentials_FallsBackToJWTExpiry 凭据缺 expires_at 时必须从 JWT 的 exp 补齐。
//
// 这是预刷新能否触发的**唯一**输入：traeTokenNeedsRefresh 对 ExpiresAt<=0 直接
// return false。存量账号（手工粘贴 / 旧版前端 / traework2api 导入）普遍没有该键。
//
// 反向护栏：把 trae.go 里那段 `if creds.ExpiresAt <= 0 { creds.ExpiresAt = traeJWTExpiry(...) }`
// 整段删掉，本用例必须 FAIL（ExpiresAt 会变成 0）。
func TestGetTraeCredentials_FallsBackToJWTExpiry(t *testing.T) {
	exp := time.Now().Add(72 * time.Hour).Unix()
	token := zzTraeJWT(t, exp)

	account := &Account{
		ID:       101,
		Platform: PlatformTrae,
		Type:     AccountTypeAPIKey,
		// 刻意不写 expires_at：模拟存量账号。
		Credentials: map[string]any{
			"access_token":  token,
			"refresh_token": "rt-101",
		},
	}
	creds := account.GetTraeCredentials()
	require.Equal(t, exp, creds.ExpiresAt, "缺 expires_at 时必须用 JWT exp 兜底，否则预刷新永不触发")

	// 有了 ExpiresAt，预刷新判据才会在临近到期时返回 true。
	require.True(t, traeTokenNeedsRefresh(creds, time.Unix(exp-3600, 0)),
		"剩余寿命不足 24h 时必须判定需要预刷新")
	require.False(t, traeTokenNeedsRefresh(creds, time.Unix(exp-7*86400, 0)),
		"离到期还有 7 天时不应白换一次票（每次换票都会轮换 refresh_token）")
}

// TestGetTraeCredentials_ExplicitExpiresAtWins 凭据里的显式 expires_at 优先于 JWT。
//
// 理由：显式值来自上游 TokenExpireAt（换票响应），是权威来源；JWT exp 只是兜底。
// 若顺序反过来，上游给出的「绝对到期时刻」会被 token 自带的 exp 静默覆盖。
func TestGetTraeCredentials_ExplicitExpiresAtWins(t *testing.T) {
	jwtExp := time.Now().Add(72 * time.Hour).Unix()
	explicit := time.Now().Add(5 * time.Hour).Unix()

	account := &Account{
		ID:       102,
		Platform: PlatformTrae,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"access_token":  zzTraeJWT(t, jwtExp),
			"refresh_token": "rt-102",
			"expires_at":    explicit,
		},
	}
	require.Equal(t, explicit, account.GetTraeCredentials().ExpiresAt,
		"凭据里的 expires_at 是上游权威值，不得被 JWT exp 覆盖")
}

// TestTraeTokenRefresher_CanRefresh 判据必须只认「Trae + 有 refresh_token」，不看 Type。
func TestTraeTokenRefresher_CanRefresh(t *testing.T) {
	r := NewTraeTokenRefresher(nil, nil, nil)

	require.False(t, r.CanRefresh(nil), "nil 账号不可刷新")
	require.False(t, r.CanRefresh(&Account{ID: 1, Platform: PlatformTrae, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "at"}}),
		"没有 refresh_token 的号确实不可自愈，必须返回 false（让它走 401 的永久禁用那一支）")
	require.True(t, r.CanRefresh(&Account{ID: 2, Platform: PlatformTrae, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"refresh_token": "rt"}}),
		"Trae 恒为 apikey，但持 refresh_token 就必须可刷新（不得按 Type 分流）")
	require.False(t, r.CanRefresh(&Account{ID: 3, Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"refresh_token": "rt"}}),
		"不得越界处理其它平台的账号")
}

// TestTraeTokenRefresher_NeedsRefresh 钉住判定优先级与 24h 提前量。
func TestTraeTokenRefresher_NeedsRefresh(t *testing.T) {
	r := NewTraeTokenRefresher(nil, nil, nil)
	now := time.Now()

	cases := []struct {
		name  string
		creds map[string]any
		want  bool
	}{
		{"无 access_token", map[string]any{"refresh_token": "rt"}, true},
		{"无 refresh_token", map[string]any{"access_token": "at"}, false},
		{"到期时刻未知且非 JWT", map[string]any{
			"access_token": "not-a-jwt", "refresh_token": "rt"}, false},
		{"剩余 48h（超出 24h 窗口）", map[string]any{
			"access_token": "at", "refresh_token": "rt",
			"expires_at": now.Add(48 * time.Hour).Unix()}, false},
		{"剩余 2h（进入 24h 窗口）", map[string]any{
			"access_token": "at", "refresh_token": "rt",
			"expires_at": now.Add(2 * time.Hour).Unix()}, true},
	}
	for _, tc := range cases {
		account := &Account{ID: 200, Platform: PlatformTrae, Type: AccountTypeAPIKey, Credentials: tc.creds}
		require.Equal(t, tc.want, r.NeedsRefresh(account, 30*time.Minute),
			"case=%s：后台刷新必须与请求路径共用 traeTokenRefreshSkew(24h) 提前量", tc.name)
	}
}

// TestTokenRefreshService_RegistersTrae 注册 Trae 后必须出现在候选平台列表里，
// 且必须打开 IncludeAPIKeyPlatforms（否则仓储的 type='oauth' 硬条件会把 Trae 全部排除）。
//
// 注意：本用例直接调 SetTraeRefresher，因此 **只能证明 setter 自己对了**，
// 拦不住「wire.go 忘了调它」这类回归。那一层由下面的
// TestProvideTokenRefreshService_RegistersWorkbuddyAndTrae 从真实装配入口盯住。
func TestTokenRefreshService_RegistersTrae(t *testing.T) {
	svc := NewTokenRefreshService(&stubOAuthRefreshAccountRepo{}, nil, nil, nil, nil, nil, nil,
		&config.Config{}, nil)

	require.NotContains(t, svc.eligiblePlatforms(), PlatformTrae,
		"未注入时必须不注册（既有部署与测试零行为变更）")
	require.False(t, svc.includeAPIKeyRefreshCandidates,
		"未注入 apikey 型 refresher 时不得放宽候选类型门")

	svc.SetTraeRefresher(NewTraeTokenRefresher(nil, nil, nil))
	require.Contains(t, svc.eligiblePlatforms(), PlatformTrae)
	require.True(t, svc.includeAPIKeyRefreshCandidates,
		"Trae 恒为 apikey，不开这道门就永远查不到候选账号（= 到期无人续命）")

	// 幂等：重复注册不得让同一平台被处理两遍。
	count := 0
	for _, p := range svc.eligiblePlatforms() {
		if p == PlatformTrae {
			count++
		}
	}
	svc.SetTraeRefresher(NewTraeTokenRefresher(nil, nil, nil))
	after := 0
	for _, p := range svc.eligiblePlatforms() {
		if p == PlatformTrae {
			after++
		}
	}
	require.Equal(t, count, after, "重复注册必须幂等")
}

// TestProvideTokenRefreshService_RegistersWorkbuddyAndTrae 从**真实装配入口**断言
// WorkBuddy 与 Trae 两个后台刷新器都被注册，且 apikey 候选类型门确实打开了。
//
// 【为何必须从 ProvideTokenRefreshService 进入（2026-10-30 自查补上）】原来的注册用例
// 直接调 SetTraeRefresher，自己在注释里写着「wire 里删掉注册本用例仍会 PASS」——即
// **注册断链没有任何测试拦截**。而注册断链的后果正是本次要修的那个问题：到期无人续命，
// 只能等真请求撞 401。本用例从 wire 级入口跑，删掉 wire.go 里任一行注册都会 FAIL。
//
// 反向护栏（已实证）：注释掉 wire.go 的 svc.SetTraeRefresher(...) → 本用例 FAIL。
func TestProvideTokenRefreshService_RegistersWorkbuddyAndTrae(t *testing.T) {
	// cfg.TokenRefresh.Enabled 保持 false：Start() 会在第一行就返回，不起任何 goroutine，
	// 适合单测（注册发生在 Start 之前，注释里也写明必须如此）。
	cfg := &config.Config{}

	svc := ProvideTokenRefreshService(
		nil,                     // accountRepo：只需满足接口断言，不参与本次断言
		nil, nil, nil, nil, nil, // 五家 OAuth service
		nil, nil, // cacheInvalidator / schedulerCache
		cfg,
		nil, nil, nil, // tempUnschedCache / privacyClientFactory / proxyRepo
		nil,      // refreshAPI
		nil,      // runtimeBlocker
		nil, nil, // httpUpstream / tlsProfileService
	)
	require.NotNil(t, svc)

	platforms := svc.eligiblePlatforms()
	require.Contains(t, platforms, PlatformTrae,
		"wire.go 必须注册 Trae 后台刷新，否则 access_token 到期无人续命（与 Qoder 签到、WorkBuddy 401 同一个坑）")
	require.Contains(t, platforms, PlatformWorkbuddy,
		"WorkBuddy 的注册不得被本次改动带掉")
	require.True(t, svc.includeAPIKeyRefreshCandidates,
		"Trae/WorkBuddy 恒为 apikey，不开类型门就永远查不到候选账号")

	// 基础五家仍在（单一注册表同时供执行与仓储资格使用，不得因追加而丢失）。
	for _, platform := range []string{
		PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok,
	} {
		require.Contains(t, platforms, platform, "基础平台注册不得丢失")
	}

	// 幂等：每个平台恰好一次（重复注册会把同一批候选处理两遍）。
	counts := make(map[string]int, len(platforms))
	for _, p := range platforms {
		counts[p]++
	}
	for platform, n := range counts {
		require.Equal(t, 1, n, "platform=%s 不得重复注册", platform)
	}
}

// TestRefreshableAPIKeyCredential_CoversTraeAndWorkbuddy 401 分流判据必须覆盖两个平台，
// 且必须经平台自己的归一化读取函数（不得各自硬编码键名）。
//
// 反向护栏：把 isRefreshableAPIKeyCredential 的 PlatformTrae 分支删掉，本用例必须 FAIL。
func TestRefreshableAPIKeyCredential_CoversTraeAndWorkbuddy(t *testing.T) {
	require.True(t, isRefreshableAPIKeyCredential(&Account{
		ID: 1, Platform: PlatformTrae, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"refresh_token": "rt"}}),
		"Trae 持 refresh_token 时必须走临时冷却而不是永久下线")

	// camelCase 别名：WorkBuddy 合法接受 refreshToken，两处口径必须同源。
	require.True(t, isRefreshableAPIKeyCredential(&Account{
		ID: 2, Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"refreshToken": "rt"}}),
		"WorkBuddy 的 camelCase 别名必须被识别（否则修复实际失效）")

	require.False(t, isRefreshableAPIKeyCredential(&Account{
		ID: 3, Platform: PlatformTrae, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "at"}}),
		"无 refresh_token 的号确实不可自愈，必须保持永久禁用语义")
	require.False(t, isRefreshableAPIKeyCredential(&Account{
		ID: 4, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"refresh_token": "rt"}}),
		"其它平台零行为变更，不得被本次泛化扫到")
	require.False(t, isRefreshableAPIKeyCredential(nil))

	// 兼容别名仍然成立。
	require.True(t, isWorkbuddyRefreshableCredential(&Account{
		ID: 5, Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"refresh_token": "rt"}}))
}

// TestHandleUpstreamError_Trae401CoolsDownInsteadOfDisabling 驱动**真实调用点**：
// RateLimitService.HandleUpstreamError(账号, 401, ...) 对有 refresh_token 的 Trae 必须
// 走临时冷却而不是永久 SetError。
//
// 【为何这一条不可省】上一版只断言了判据函数 isRefreshableAPIKeyCredential，结果把
// 调用点里的判据换回 `authAccount.Type == AccountTypeOAuth` 后测试仍然 PASS（反向护栏
// 没打响）。判据正确 ≠ 调用点用上了它，必须从外部入口驱动。
func TestHandleUpstreamError_Trae401CoolsDownInsteadOfDisabling(t *testing.T) {
	repo := &errorPolicyRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := &Account{
		ID:       410,
		Platform: PlatformTrae,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_token":  "at-410",
			"refresh_token": "rt-410",
		},
	}

	shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusUnauthorized,
		http.Header{}, []byte(`{"code":"1001","message":"session expired"}`))

	require.True(t, shouldDisable, "本次请求仍应失败并换号")
	require.Zero(t, repo.setErrCalls,
		"Trae 持 refresh_token 时 401 绝不能 SetError——那会把 status 写成 error，"+
			"而后台刷新候选查询带 status='active'，账号从此再也不会被刷新")
	require.Equal(t, 1, repo.tempCalls, "必须改为临时冷却，到期后由预刷新/后台刷新自愈")
}

// TestHandleUpstreamError_Trae401WithoutRefreshTokenStillDisables 无 refresh_token 时
// 仍走永久禁用——它确实不可自愈，冷却结束后只是再撞一次。
func TestHandleUpstreamError_Trae401WithoutRefreshTokenStillDisables(t *testing.T) {
	repo := &errorPolicyRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := &Account{
		ID:       411,
		Platform: PlatformTrae,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_token": "at-411",
		},
	}

	svc.HandleUpstreamError(context.Background(), account, http.StatusUnauthorized,
		http.Header{}, []byte("unauthorized"))

	require.Equal(t, 1, repo.setErrCalls)
	require.Zero(t, repo.tempCalls)
}

// TestHandleUpstreamError_OtherAPIKey401StillDisables 其它平台零行为变更：
// 普通 apikey 账号（例：自定义中转）撞 401 仍应永久下线。
func TestHandleUpstreamError_OtherAPIKey401StillDisables(t *testing.T) {
	repo := &errorPolicyRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := &Account{
		ID:       412,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"api_key":       "sk-411",
			"refresh_token": "rt-411",
		},
	}

	svc.HandleUpstreamError(context.Background(), account, http.StatusUnauthorized,
		http.Header{}, []byte("unauthorized"))

	require.Equal(t, 1, repo.setErrCalls,
		"本次泛化只该扫到 WorkBuddy/Trae，其它 apikey 平台必须保持原永久禁用语义")
	require.Zero(t, repo.tempCalls)
}

// TestTraeConnectionTest401CoolsDownInsteadOfDisabling 账号「测试连接」路径的 401 也必须
// 走临时冷却。那段代码曾是主聊天路径之外**另一份**独立的 SetError，修复只改了主路径时
// 它会变成一个更险的陷阱：管理员正是因为怀疑 token 过期才去点测试，点一次就把号打死。
//
// 反向护栏：把 trae_gateway.go 里该分支改回无条件 `SetError`，本用例必须 FAIL。
func TestTraeConnectionTest401CoolsDownInsteadOfDisabling(t *testing.T) {
	server, _, upstream := newModelCatalogTestServer(t, http.StatusUnauthorized,
		`{"code":"1001","message":"session expired"}`)
	repo := &zzTraeStateRepo{}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          modelSyncInsecureTestConfig(),
		accountRepo:  repo,
	}

	account := traeModelSyncTestAccount(server.URL)
	account.ID = 420
	account.Type = AccountTypeAPIKey
	account.Credentials["refresh_token"] = "rt-420"

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)

	_ = svc.testTraeAccountConnection(c, account, "glm-5.2", "ping")

	require.Zero(t, repo.setErrorCalls,
		"测试连接撞 401 时不得永久下线账号（Trae 持 refresh_token ⇒ 可自愈）")
	require.Equal(t, 1, repo.tempUnschedCalls)
	require.Contains(t, repo.tempReason, "connection test")
}

// TestTraeConnectionTest401WithoutRefreshTokenStillDisables 无 refresh_token 时
// 测试路径仍应永久禁用（它确实不可自愈）。
func TestTraeConnectionTest401WithoutRefreshTokenStillDisables(t *testing.T) {
	server, _, upstream := newModelCatalogTestServer(t, http.StatusUnauthorized, `{"error":"unauthorized"}`)
	repo := &zzTraeStateRepo{}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          modelSyncInsecureTestConfig(),
		accountRepo:  repo,
	}

	account := traeModelSyncTestAccount(server.URL)
	account.ID = 421
	account.Type = AccountTypeAPIKey

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)

	_ = svc.testTraeAccountConnection(c, account, "glm-5.2", "ping")

	require.Equal(t, 1, repo.setErrorCalls)
	require.Zero(t, repo.tempUnschedCalls)
	require.Contains(t, repo.errorMsg, "no refresh_token")
}

// TestApplyTraeBusinessErrorClass_SessionWithRefreshTokenCoolsDown 流内 1001/401 必须
// 从「永久 SetError」改为「临时冷却」，否则账号再也不会进入后台刷新候选。
//
// 之所以要求「不进候选」而不是「刷不刷」：仓储 ListOAuthRefreshCandidatePage 带
// `status = 'active'` 过滤，SetError 会写 status=error ⇒ 该账号永久退出候选集，
// 后台刷新器再也不会看它一眼。
func TestApplyTraeBusinessErrorClass_SessionWithRefreshTokenCoolsDown(t *testing.T) {
	repo := &zzTraeStateRepo{}
	account := &Account{ID: 300, Platform: PlatformTrae, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"refresh_token": "rt"}}

	applyTraeBusinessErrorClass(context.Background(), repo, account, "1001",
		classifyTraeBusinessError("1001"), "session expired")

	require.Zero(t, repo.setErrorCalls, "有 refresh_token 时绝不能永久下线账号")
	require.Equal(t, 1, repo.tempUnschedCalls)
	require.False(t, repo.tempUntil.IsZero())
	require.WithinDuration(t, time.Now().Add(traeSessionRecoveryCooldown), repo.tempUntil, time.Minute)
	require.Contains(t, repo.tempReason, "cooling down for automatic recovery")
	require.NotContains(t, repo.tempReason, "OAuth",
		"Trae 是 apikey，报 OAuth 会把排障引向授权配置")
}

// TestApplyTraeBusinessErrorClass_SessionWithoutRefreshTokenStillDisables 无 refresh_token
// 时仍走永久禁用 —— 冷却结束后只是再撞一次，那一支是正确的，不要一并放宽。
func TestApplyTraeBusinessErrorClass_SessionWithoutRefreshTokenStillDisables(t *testing.T) {
	repo := &zzTraeStateRepo{}
	account := &Account{ID: 301, Platform: PlatformTrae, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "at"}}

	applyTraeBusinessErrorClass(context.Background(), repo, account, "1001",
		classifyTraeBusinessError("1001"), "session expired")

	require.Equal(t, 1, repo.setErrorCalls)
	require.Zero(t, repo.tempUnschedCalls)
	require.Contains(t, repo.errorMsg, "no refresh_token")
}

// TestApplyTraeBusinessErrorClass_SessionOAuthTypeCoolsDown 流内 1001 的可自愈判据
// 必须与 HTTP 401 路径同源：OAuth 类型的 Trae 账号持 refresh_token 时，同样要临时冷却。
//
// 【要防的回归（2026-10-30 代码自查实证）】修复前该分支只调
// isRefreshableAPIKeyCredential（内含 Type==APIKey 门），临时探针实测：
//
//	Type=apikey → tempUnsched=1 / setError=0（正确）
//	Type=oauth  → tempUnsched=0 / setError=1（错误：永久退出刷新候选集，永不恢复）
//
// 即「同一个坑在流内路径上没堵完」。反向护栏：把该分支改回
// !isRefreshableAPIKeyCredential(account)，本用例必须 FAIL。
func TestApplyTraeBusinessErrorClass_SessionOAuthTypeCoolsDown(t *testing.T) {
	repo := &zzTraeStateRepo{}
	account := &Account{ID: 310, Platform: PlatformTrae, Type: AccountTypeOAuth,
		Credentials: map[string]any{"refresh_token": "rt-oauth"}}

	applyTraeBusinessErrorClass(context.Background(), repo, account, "1001",
		classifyTraeBusinessError("1001"), "session expired")

	require.Zero(t, repo.setErrorCalls,
		"OAuth 类型且持 refresh_token ⇒ 可自愈，绝不能永久下线（SetError 会把它踢出后台刷新候选集）")
	require.Equal(t, 1, repo.tempUnschedCalls)
	require.Contains(t, repo.tempReason, "cooling down for automatic recovery")
}

// TestTraeConnectionTestOAuthType401CoolsDown 测试连接路径同样不得按类型分流。
//
// 反向护栏：把 trae_gateway.go 该分支改回 isRefreshableAPIKeyCredential(account)，必 FAIL。
func TestTraeConnectionTestOAuthType401CoolsDown(t *testing.T) {
	server, _, upstream := newModelCatalogTestServer(t, http.StatusUnauthorized,
		`{"code":"1001","message":"session expired"}`)
	repo := &zzTraeStateRepo{}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          modelSyncInsecureTestConfig(),
		accountRepo:  repo,
	}

	account := traeModelSyncTestAccount(server.URL)
	account.ID = 421
	account.Type = AccountTypeOAuth
	account.Credentials["refresh_token"] = "rt-421"

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)

	_ = svc.testTraeAccountConnection(c, account, "glm-5.2", "ping")

	require.Zero(t, repo.setErrorCalls, "OAuth 类型持票账号在测试路径也不得永久下线")
	require.Equal(t, 1, repo.tempUnschedCalls)
}

// TestCanSelfHealSessionFailure_Matrix 钉住唯一判据的四个象限。
//
// 这是三条路径共用的那个函数，任何“只看类型”或“只看有票”的简化都会在这里翻车。
// 反向护栏：把 canSelfHealSessionFailure 改成 `return account != nil`、
// 或改成只看 Type==OAuth、或改成只看票非空，三个子例至少各挂一个。
func TestCanSelfHealSessionFailure_Matrix(t *testing.T) {
	cases := []struct {
		name    string
		account *Account
		want    bool
	}{
		{"nil 账号", nil, false},
		{"Trae+apikey+有票 → 可自愈", &Account{Platform: PlatformTrae, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"refresh_token": "rt"}}, true},
		{"Trae+oauth+有票 → 可自愈（本次修复的靶子）", &Account{Platform: PlatformTrae, Type: AccountTypeOAuth,
			Credentials: map[string]any{"refresh_token": "rt"}}, true},
		{"Trae+apiKey+无票 → 不可自愈", &Account{Platform: PlatformTrae, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"access_token": "at"}}, false},
		{"Trae+oauth+无票 → 不可自愈", &Account{Platform: PlatformTrae, Type: AccountTypeOAuth,
			Credentials: map[string]any{"access_token": "at"}}, false},
		{"非自愈族平台+有票 → 不可自愈", &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"refresh_token": "rt"}}, false},
		// WorkBuddy 的 camelCase 别名必须经归一化读取函数认出来。
		{"WorkBuddy+camelCase 别名 → 可自愈", &Account{Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"refreshToken": "rt"}}, true},
	}
	for _, tc := range cases {
		require.Equalf(t, tc.want, canSelfHealSessionFailure(tc.account), "case=%s", tc.name)
	}

	// 判据族与判据本身必须一致：族外永不 true。
	for _, tc := range cases {
		if canSelfHealSessionFailure(tc.account) {
			require.True(t, isSessionSelfHealFamily(tc.account),
				"canSelfHeal 为 true 时 family 必须为 true（case=%s）", tc.name)
		}
	}
}

// TestApplyTraeBusinessErrorClass_QuotaUnchanged 额度类（1005）语义不得被本次改动带偏：
// 它一直是「次日 0 点自动恢复」的临时不可调度。
func TestApplyTraeBusinessErrorClass_QuotaUnchanged(t *testing.T) {
	repo := &zzTraeStateRepo{}
	account := &Account{ID: 302, Platform: PlatformTrae, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"refresh_token": "rt"}}

	applyTraeBusinessErrorClass(context.Background(), repo, account, "1005",
		classifyTraeBusinessError("1005"), "quota")

	require.Zero(t, repo.setErrorCalls)
	require.Equal(t, 1, repo.tempUnschedCalls)
	require.True(t, repo.tempUntil.After(time.Now().Add(time.Hour)),
		"额度类必须恢复到次日 0 点，而不是 10 分钟冷却")
}

// TestTraeTokenRefresher_RejectsUnsafeOAuthBaseURL 后台刷新器出站前必须过 URL 安全策略。
//
// 【为何必须有这条（2026-10-30 自查发现）】oauth_base_url 来自凭据、可由管理员
// 写入或由导入文件带入。开启 security.url_allowlist 时不过校验就是绕过白名单的 SSRF 面。
// 同平台另三条换票路径（trae_client.go:298 / trae_gateway.go:274 /
// trae_credits_service.go:563）都已校验，只有本文件（新增）漏了。
//
// 反向护栏：把 Refresh 里的 cnValidateProbeURL 调用删掉，本用例必须 FAIL。
func TestTraeTokenRefresher_RejectsUnsafeOAuthBaseURL(t *testing.T) {
	// 服务端什么都不做：策略校验必须在建连之前就拒掉。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("不得真的发出请求（策略应在建连前拒绝），实得 %s", r.URL.Path)
	}))
	defer srv.Close()

	account := &Account{
		ID:       401,
		Platform: PlatformTrae,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"refresh_token": "rt-401",
			// httptest 给的是 http://，而默认策略不允许明文 http。
			"oauth_base_url": strings.TrimSuffix(srv.URL, "/"),
		},
	}

	// cfg 为 nil ⇒ 走 cnValidateProbeURL 的默认分支（ValidateURLFormat(raw, false)）：拒绝明文 http。
	r := NewTraeTokenRefresher(nil, nil, nil)
	_, err := r.Refresh(context.Background(), account)

	require.Error(t, err, "不安全的 oauth_base_url 必须在出站前被拒")
	require.Contains(t, err.Error(), "URL security policy")
}

// TestTraeTokenRefresher_RefreshWritesRotatedTokens 换票必须覆盖轮换后的 refresh_token，
// 并从**新 token** 取过期时刻（不得沿用旧凭据里的 expires_at）。
//
// 后者是本仓库已经踩过一次的坑：留旧值会让「刚刷完又认为不需刷」。
func TestTraeTokenRefresher_RefreshWritesRotatedTokens(t *testing.T) {
	newExp := time.Now().Add(96 * time.Hour).Unix()
	newToken := zzTraeJWT(t, newExp)
	staleExp := time.Now().Add(time.Minute).Unix()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, traeExchangeToken, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		// 刻意只给 Token/RefreshToken，不给 TokenExpireAt/Duration：
		// 强制走 JWT exp 兜底分支。
		_, _ = w.Write([]byte(`{"Result":{"Token":"` + newToken + `","RefreshToken":"rt-rotated"}}`))
	}))
	defer srv.Close()

	baseURL := strings.TrimSuffix(srv.URL, "/")
	account := &Account{
		ID:       400,
		Platform: PlatformTrae,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"access_token":      zzTraeJWT(t, time.Now().Add(time.Hour).Unix()),
			"refresh_token":     "rt-old",
			"expires_at":        staleExp,
			"oauth_base_url":    baseURL,
			"realm":             "cn",
			"device_id":         "dev-400",
			"billing_base_url":  "https://api.trae.cn",
			"ide_version_code":  "20260101",
			"machine_id":        "mach-400",
			"nickname":          "keep-me",
			"some_future_field": "must-survive",
		},
	}

	r := NewTraeTokenRefresher(nil, nil, modelSyncInsecureTestConfig())
	next, err := r.Refresh(context.Background(), account)
	require.NoError(t, err)

	require.Equal(t, newToken, next["access_token"])
	require.Equal(t, "rt-rotated", next["refresh_token"],
		"refresh_token 会轮换，不覆盖旧值会导致下次换票必失败")
	require.EqualValues(t, newExp, next["expires_at"],
		"必须从**新** token 取 exp；沿用旧凭据的 expires_at 会让「刚刷完又认为不需刷」")
	require.NotEqualValues(t, staleExp, next["expires_at"])

	// 其余字段必须原样保留（Refresh 返回的是「完整」凭据 map）。
	for _, key := range []string{"realm", "device_id", "billing_base_url", "ide_version_code",
		"machine_id", "nickname", "some_future_field"} {
		require.Contains(t, next, key, "字段 %s 必须保留", key)
	}
	require.Equal(t, "must-survive", next["some_future_field"])
}

// zzTraeStateRepo 记录 SetError / SetTempUnschedulable 的调用，其余方法内嵌 nil 接口。
type zzTraeStateRepo struct {
	AccountRepository
	setErrorCalls    int
	tempUnschedCalls int
	errorMsg         string
	tempUntil        time.Time
	tempReason       string
}

func (r *zzTraeStateRepo) SetError(_ context.Context, _ int64, msg string) error {
	r.setErrorCalls++
	r.errorMsg = msg
	return nil
}

func (r *zzTraeStateRepo) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.tempUnschedCalls++
	r.tempUntil = until
	r.tempReason = reason
	return nil
}

// stubOAuthRefreshAccountRepo 只用于构造 TokenRefreshService，不实现候选分页。
// 内嵌 nil 接口已提供全部方法，故不需要（也不要）自己实现 ListActive —— 签名一旦
// 与 AccountRepository 不一致就会变成「想实现却实现了错的方法」。
type stubOAuthRefreshAccountRepo struct {
	AccountRepository
}
