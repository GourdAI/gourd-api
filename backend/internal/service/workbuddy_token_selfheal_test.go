//go:build unit

package service

// workbuddy_token_selfheal_test.go WorkBuddy token 自愈链的三条缺陷回归护栏。
//
// 背景（2026-10-24 全量 401 事件）：线上 WorkBuddy 接口调用全部 401。排查确认真实
// 凭据本身有效（用本机真实 CN 账号跑生产路径，chat/billing/模型目录全部 200），
// 失效点在「token 过期后无法自愈」这条链上，由三处缺陷串联而成：
//
//	① 凭据缺 expires_at（前端不写）→ workbuddyTokenNeedsRefresh 恒 false → 永不预刷新；
//	② 401 对 apikey 账号走 SetError 永久下线（status=error + schedulable=false）；
//	③ TokenRefreshService 注册表没有 WorkBuddy → 后台没有任何角色会换票。
//
// 三者叠加 ⇒ token 到期后必然撞 401，且 401 又永久杀号，全程无恢复路径。
//
// 本文件把三条修复分别钉死，并保持「不制造假阳性」：只在确有正向前提时才放宽，
// 缺 refresh_token / 解不出 exp 的情况仍然维持原有保守语义。

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
	"github.com/stretchr/testify/require"
)

// workbuddyTestJWT 构造一个仅含 exp/sub 的未签名 JWT（测试用；本仓解析不做验签）。
func workbuddyTestJWT(t *testing.T, exp int64, sub string) string {
	t.Helper()
	return workbuddyTestJWTWithIat(t, 0, exp, sub)
}

// workbuddyTestJWTWithIat 同上，并带上 iat（用于验证「按 token 寿命推导刷新窗口」）。
func workbuddyTestJWTWithIat(t *testing.T, iat, exp int64, sub string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := `{"exp":` + formatInt64(exp) + `,"sub":"` + sub + `"`
	if iat > 0 {
		claims += `,"iat":` + formatInt64(iat)
	}
	claims += `}`
	payload := base64.RawURLEncoding.EncodeToString([]byte(claims))
	return header + "." + payload + ".fakesig"
}

// lifetime45d 是实测的 WorkBuddy access_token 寿命（exp - iat = 45 天）。
const lifetime45d = 45 * 24 * 3600

func formatInt64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ---------------------------------------------------------------------------
// 缺陷①：expires_at 缺失时从 JWT exp 兜底（读取侧）
// ---------------------------------------------------------------------------

// TestWorkbuddyJWTExpiryParsesStandardToken 标准 WorkBuddy token 的 exp 必须能解出。
func TestWorkbuddyJWTExpiryParsesStandardToken(t *testing.T) {
	exp := time.Now().Add(20 * 24 * time.Hour).Unix()
	token := workbuddyTestJWT(t, exp, "uid-1")
	require.Equal(t, exp, workbuddyJWTExpiry(token))
	require.Equal(t, "uid-1", workbuddyJWTSubject(token))
}

// TestWorkbuddyJWTExpiryRejectsNonJWT 非 JWT 必须返回 0（维持「未知」语义，不伪造过期时刻）。
func TestWorkbuddyJWTExpiryRejectsNonJWT(t *testing.T) {
	for _, token := range []string{"", "plain-api-key", "only.two", "a.b.c"} {
		require.Zero(t, workbuddyJWTExpiry(token), "token=%q 不应解出 exp", token)
		require.Empty(t, workbuddyJWTSubject(token), "token=%q 不应解出 sub", token)
	}
}

// TestWorkbuddyCredentialsFillsExpiresAtFromJWT 凭据无 expires_at 时读取侧自动补齐 ——
// 这是缺陷①的修复点：不补则 needsRefresh 恒假，token 到期无任何预刷新。
func TestWorkbuddyCredentialsFillsExpiresAtFromJWT(t *testing.T) {
	exp := time.Now().Add(10 * time.Hour).Unix()
	token := workbuddyTestJWT(t, exp, "uid-7")
	account := &Account{
		ID:       41,
		Platform: PlatformWorkbuddy,
		Credentials: map[string]any{
			"access_token":  token,
			"refresh_token": "rt-41",
			"realm":         "cn",
		},
	}

	creds := account.GetWorkbuddyCredentials()
	require.Equal(t, exp, creds.ExpiresAt, "应从 JWT exp 补齐 expires_at")
	require.Equal(t, "uid-7", creds.UID, "应同时从 sub 补齐 uid")
}

// TestWorkbuddyCredentialsKeepsStoredExpiresAt 已有正数 expires_at 时绝不被 JWT 覆盖。
// 上游 refresh 响应给的 expiresIn 是明文权威值，不能被推导值顶掉。
func TestWorkbuddyCredentialsKeepsStoredExpiresAt(t *testing.T) {
	exp := time.Now().Add(30 * 24 * time.Hour).Unix()
	stored := time.Now().Add(99 * time.Hour).Unix()
	account := &Account{
		ID:       42,
		Platform: PlatformWorkbuddy,
		Credentials: map[string]any{
			"access_token":  workbuddyTestJWT(t, exp, "uid-42"),
			"refresh_token": "rt-42",
			"expires_at":    stored,
		},
	}
	require.Equal(t, stored, account.GetWorkbuddyCredentials().ExpiresAt,
		"库里的 expires_at 优先于 JWT 推导值")
}

// TestWorkbuddyPreflightRefreshWindowIsDayScale 预刷新窗口按「寿命的 1/4、上限 24h」：
// 既不能是分钟量级（45 天 token 会静默过期），也不能是固定 24h（短寿命 token 会风暴）。
// 这是 2026-10-24 全量 401 事件 + 2026-10-25 刷新风暴回归的双向教训。
func TestWorkbuddyPreflightRefreshWindowIsDayScale(t *testing.T) {
	require.GreaterOrEqual(t, workbuddyTokenRefreshSkewMax, 12*time.Hour,
		"预刷新窗口上限必须是小时/天量级；分钟量级会让 token 在无流量时段静默过期")

	// 真实形态（45 天寿命 ⇒ 取 24h 上限）。
	t.Run("45天寿命且剩19天不刷", func(t *testing.T) {
		exp := time.Now().Add(19 * 24 * time.Hour).Unix()
		creds := WorkbuddyCredentials{
			AccessToken: workbuddyTestJWTWithIat(t, exp-lifetime45d, exp, "u"),
			ExpiresAt:   exp,
		}
		require.False(t, workbuddyTokenNeedsRefresh(creds, time.Now()),
			"剩 19 天不应预刷新（避免无谓换票）")
	})
	t.Run("45天寿命且剩12小时必刷", func(t *testing.T) {
		exp := time.Now().Add(12 * time.Hour).Unix()
		creds := WorkbuddyCredentials{
			AccessToken: workbuddyTestJWTWithIat(t, exp-lifetime45d, exp, "u"),
			ExpiresAt:   exp,
		}
		require.True(t, workbuddyTokenNeedsRefresh(creds, time.Now()),
			"剩 12 小时必须预刷新（旧 10min 窗口在这里不会触发）")
	})

	// 短寿命 token（1h ⇒ 实际窗口 15min）：刚刷完（剩 45min）不得再刷，否则风暴。
	t.Run("1小时寿命刚刷完不再刷", func(t *testing.T) {
		exp := time.Now().Add(45 * time.Minute).Unix()
		creds := WorkbuddyCredentials{
			AccessToken: workbuddyTestJWTWithIat(t, exp-3600, exp, "u"),
			ExpiresAt:   exp,
		}
		require.False(t, workbuddyTokenNeedsRefresh(creds, time.Now()),
			"1h 寿命 token 刷完后剩 45min，不应立即再刷（否则刷新风暴）")
	})
	t.Run("1小时寿命剩10分钟必刷", func(t *testing.T) {
		exp := time.Now().Add(10 * time.Minute).Unix()
		creds := WorkbuddyCredentials{
			AccessToken: workbuddyTestJWTWithIat(t, exp-3600, exp, "u"),
			ExpiresAt:   exp,
		}
		require.True(t, workbuddyTokenNeedsRefresh(creds, time.Now()),
			"1h 寿命 token 剩 10min 必须刷新")
	})
}

// TestWorkbuddyPreflightRefreshNowTriggers 端到端语义：补齐 expires_at 后，
// 临近过期的账号必须被 workbuddyTokenNeedsRefresh 判为「需要刷新」。
func TestWorkbuddyPreflightRefreshNowTriggers(t *testing.T) {
	nearExpiry := time.Now().Add(5 * time.Minute).Unix() // 落在 10min skew 内
	account := &Account{
		ID:       43,
		Platform: PlatformWorkbuddy,
		Credentials: map[string]any{
			"access_token":  workbuddyTestJWT(t, nearExpiry, "uid-43"),
			"refresh_token": "rt-43",
		},
	}
	creds := account.GetWorkbuddyCredentials()
	require.True(t, workbuddyTokenNeedsRefresh(creds, time.Now()),
		"临近过期必须触发预刷新（修复前此断言为 false：expires_at 缺失）")

	// 反向：远离过期的账号不应被误刷。
	farExpiry := time.Now().Add(30 * 24 * time.Hour).Unix()
	far := &Account{
		ID:       44,
		Platform: PlatformWorkbuddy,
		Credentials: map[string]any{
			"access_token":  workbuddyTestJWT(t, farExpiry, "uid-44"),
			"refresh_token": "rt-44",
		},
	}
	require.False(t, workbuddyTokenNeedsRefresh(far.GetWorkbuddyCredentials(), time.Now()),
		"远离过期不应预刷新")
}

// ---------------------------------------------------------------------------
// 缺陷②：401 不再永久下线（改为可恢复的临时停调）
// ---------------------------------------------------------------------------

// TestWorkbuddy401CoolsDownInsteadOfPermanentDisable 持 refresh_token 的 WorkBuddy
// 账号撞 401 时必须「临时不可调度」而不是 SetError 永久下线。
//
// 修复前实测：shouldDisable=true setError=1 setTempUnsched=0
func TestWorkbuddy401CoolsDownInsteadOfPermanentDisable(t *testing.T) {
	repo := &workbuddy401Repo{}
	svc := newWorkbuddy401TestService(repo)
	account := &Account{
		ID: 51, Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey,
		Status: "active", Schedulable: true,
		Credentials: map[string]any{"access_token": "at", "refresh_token": "rt"},
	}

	disabled := svc.HandleUpstreamError(context.Background(), account,
		http.StatusUnauthorized, http.Header{},
		[]byte(`<html><head><title>401 Authorization Required</title></head></html>`), "glm-5.3")

	require.Zero(t, repo.setErrorCalls, "WorkBuddy 持 refresh_token 时不得走永久 SetError")
	require.Equal(t, 1, repo.tempCalls, "应改为 SetTempUnschedulable 给自愈留窗口")
	require.True(t, disabled, "本请求仍然视为失败（需要换号重试）")
}

// TestWorkbuddy401WithoutRefreshTokenStaysPermanent 无 refresh_token 时保持永久禁用。
// 这类账号确实不可自愈，放宽了只会让它反复被选中再撞 401。
func TestWorkbuddy401WithoutRefreshTokenStaysPermanent(t *testing.T) {
	repo := &workbuddy401Repo{}
	svc := newWorkbuddy401TestService(repo)
	account := &Account{
		ID: 52, Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey,
		Status: "active", Credentials: map[string]any{"access_token": "at"},
	}

	_ = svc.HandleUpstreamError(context.Background(), account,
		http.StatusUnauthorized, http.Header{}, []byte(`{"error":{"code":"401"}}`), "glm-5.3")

	require.Equal(t, 1, repo.setErrorCalls, "无 RefreshToken 应保持永久禁用")
	require.Zero(t, repo.tempCalls, "无 RefreshToken 不应进入临时停调")
}

// TestWorkbuddy401PredicateIsNarrow 判据必须只认 WorkBuddy apikey + refresh_token，
// 不得泛化到其它 apikey 平台（否则会改变那些平台既有的 401 语义）。
func TestWorkbuddy401PredicateIsNarrow(t *testing.T) {
	withRT := map[string]any{"refresh_token": "rt"}
	require.True(t, isWorkbuddyRefreshableCredential(&Account{
		Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey, Credentials: withRT,
	}), "WorkBuddy apikey + refresh_token 应命中")

	require.False(t, isWorkbuddyRefreshableCredential(&Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: withRT,
	}), "其它 apikey 平台不得命中")
	require.False(t, isWorkbuddyRefreshableCredential(&Account{
		Platform: PlatformWorkbuddy, Type: AccountTypeOAuth, Credentials: withRT,
	}), "OAuth 类型已由既有分支覆盖，不重复命中")
	require.False(t, isWorkbuddyRefreshableCredential(&Account{
		Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey, Credentials: map[string]any{},
	}), "无 refresh_token 不得命中")
	require.False(t, isWorkbuddyRefreshableCredential(nil), "nil 账号不得命中")
}

// ---------------------------------------------------------------------------
// 缺陷③：WorkBuddy 进入后台刷新注册表
// ---------------------------------------------------------------------------

// TestTokenRefreshServiceRegistersWorkbuddyOnlyWhenInjected 未注入时不注册（既有部署零变更），
// 注入后必须出现在 eligiblePlatforms（否则仓储查询的 platform = ANY(...) 永远不会含它）。
func TestTokenRefreshServiceRegistersWorkbuddyOnlyWhenInjected(t *testing.T) {
	accountRepo := &workbuddyRefreshRepo{}
	svc := NewTokenRefreshService(accountRepo, nil, nil, nil, nil, nil, nil, nil, nil)

	for _, platform := range svc.eligiblePlatforms() {
		require.NotEqual(t, PlatformWorkbuddy, platform, "未注入时不应注册 WorkBuddy")
	}
	require.False(t, svc.includeAPIKeyRefreshCandidates, "未注入时不应放宽候选类型门")

	svc.SetWorkBuddyRefresher(NewWorkBuddyTokenRefresher(nil, nil, nil))

	found := false
	for _, platform := range svc.eligiblePlatforms() {
		if platform == PlatformWorkbuddy {
			found = true
		}
	}
	require.True(t, found, "注入后 WorkBuddy 必须出现在候选平台里")
	require.True(t, svc.includeAPIKeyRefreshCandidates,
		"注入后必须放宽候选类型门（仓储默认只查 type='oauth'）")

	// 幂等：重复注入不得产生两条注册（否则候选被处理两遍）。
	svc.SetWorkBuddyRefresher(NewWorkBuddyTokenRefresher(nil, nil, nil))
	count := 0
	for _, registration := range svc.registrations {
		if registration.platform == PlatformWorkbuddy {
			count++
		}
	}
	require.Equal(t, 1, count, "重复注入应幂等")
}

// TestWorkbuddyRefresherNeedsRefreshSemantics 后台刷新判据：
// 有 refresh_token + 临近过期 → true；缺 refresh_token / 解不出 exp → false。
func TestWorkbuddyRefresherNeedsRefreshSemantics(t *testing.T) {
	refresher := NewWorkBuddyTokenRefresher(nil, nil, nil)
	window := 24 * time.Hour

	t.Run("临近过期需刷新", func(t *testing.T) {
		exp := time.Now().Add(2 * time.Hour).Unix()
		account := &Account{Platform: PlatformWorkbuddy, Credentials: map[string]any{
			"access_token": workbuddyTestJWT(t, exp, "u"), "refresh_token": "rt",
		}}
		require.True(t, refresher.NeedsRefresh(account, window))
		require.True(t, refresher.CanRefresh(account))
	})

	t.Run("远离过期不需刷新", func(t *testing.T) {
		exp := time.Now().Add(40 * 24 * time.Hour).Unix()
		account := &Account{Platform: PlatformWorkbuddy, Credentials: map[string]any{
			"access_token": workbuddyTestJWT(t, exp, "u"), "refresh_token": "rt",
		}}
		require.False(t, refresher.NeedsRefresh(account, window))
	})

	t.Run("无 refresh_token 不可刷新", func(t *testing.T) {
		account := &Account{Platform: PlatformWorkbuddy, Credentials: map[string]any{
			"access_token": workbuddyTestJWT(t, time.Now().Add(time.Hour).Unix(), "u"),
		}}
		require.False(t, refresher.CanRefresh(account))
		require.False(t, refresher.NeedsRefresh(account, window))
	})

	t.Run("既非 WorkBuddy 不接管", func(t *testing.T) {
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Credentials: map[string]any{"refresh_token": "rt"}}
		require.False(t, refresher.CanRefresh(account))
	})

	t.Run("access_token 为空必须刷新", func(t *testing.T) {
		account := &Account{Platform: PlatformWorkbuddy, Credentials: map[string]any{
			"refresh_token": "rt",
		}}
		require.True(t, refresher.NeedsRefresh(account, window))
	})
}

// TestWorkbuddyRefresherRotatesCredentialsAndFillsExpiresAt 后台刷新必须：
// 换上新 token、保留旧字段、并写入 expires_at（expiresIn 优先，缺失则 JWT exp 兜底）。
func TestWorkbuddyRefresherRotatesCredentialsAndFillsExpiresAt(t *testing.T) {
	newExp := time.Now().Add(45 * 24 * time.Hour).Unix()
	newToken := workbuddyTestJWT(t, newExp, "uid-88")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, workbuddyRefreshPath, r.URL.Path)
		require.Equal(t, "rt-old", r.Header.Get("X-Refresh-Token"),
			"refresh 端点用 X-Refresh-Token（绝不用 Authorization）")
		w.Header().Set("Content-Type", "application/json")
		// 刻意不给 expiresIn：验证 JWT exp 兜底路径。
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"accessToken":"` + newToken + `","refreshToken":"rt-new"}}`))
	}))
	defer server.Close()

	refresher := NewWorkBuddyTokenRefresher(nil, nil, nil)
	account := &Account{
		ID: 61, Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"access_token":  "at-old",
			"refresh_token": "rt-old",
			"realm":         "cn",
			"base_url":      server.URL,
			"model_mapping": map[string]any{"glm-5.3": "glm-5.3"},
		},
	}

	next, err := refresher.Refresh(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, newToken, next["access_token"])
	require.Equal(t, "rt-new", next["refresh_token"])
	require.EqualValues(t, newExp, next["expires_at"], "expiresIn 缺失时应由 JWT exp 兜底")
	require.Equal(t, "cn", next["realm"], "必须保留 realm")
	require.Equal(t, server.URL, next["base_url"], "必须保留 base_url")
	require.NotNil(t, next["model_mapping"], "必须保留 model_mapping")
}

// TestWorkbuddyRefresherRejectsUpstreamError 上游拒绝必须报错，不得静默返回空凭据。
func TestWorkbuddyRefresherRejectsUpstreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":12153,"msg":"refresh token failed"}`))
	}))
	defer server.Close()

	refresher := NewWorkBuddyTokenRefresher(nil, nil, nil)
	account := &Account{
		ID: 62, Platform: PlatformWorkbuddy, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"access_token": "at", "refresh_token": "rt", "base_url": server.URL,
		},
	}
	_, err := refresher.Refresh(context.Background(), account)
	require.Error(t, err)
	require.Contains(t, err.Error(), "401")
}

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// workbuddy401Repo 记录 401 分支落下的状态变更调用。
type workbuddy401Repo struct {
	mockAccountRepoForGemini
	setErrorCalls int
	tempCalls     int
	lastTempMsg   string
}

func (r *workbuddy401Repo) SetError(_ context.Context, _ int64, _ string) error {
	r.setErrorCalls++
	return nil
}

func (r *workbuddy401Repo) SetTempUnschedulable(_ context.Context, _ int64, _ time.Time, reason string) error {
	r.tempCalls++
	r.lastTempMsg = reason
	return nil
}

// workbuddyRefreshRepo 是后台刷新候选查询的最小仓储桩。
type workbuddyRefreshRepo struct {
	mockAccountRepoForGemini
}

func (r *workbuddyRefreshRepo) ListOAuthRefreshCandidatePage(
	_ context.Context, _ OAuthRefreshPageOptions,
) (*OAuthRefreshCandidatePage, error) {
	return &OAuthRefreshCandidatePage{}, nil
}

func newWorkbuddy401TestService(repo AccountRepository) *RateLimitService {
	return NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
}

var _ = json.Marshal
var _ = strings.TrimSpace
