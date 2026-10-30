//go:build unit

package service

// qoder_campaign_auth_retry_test.go openapi 业务域（签到/余额/领取）的 401 自愈回归锁。
//
// 背景（线上报障）：签到报
// `qoder openapi error: HTTP 401 {"code":"TOKEN_EXPIRE","message":"token is..."}`。
// 根因是 access_token 非空但已过期时，整条链路只有「为空才交换」一条路——过期令牌
// 被一直沿用，21 点兜底窗口同样救不回。修复为「撞 401 → 重换 jobToken → 重试一次」。
//
// 本文件同时锁住三条**刻意不改**的边界：
//  1. 403 不触发重换（活动域 403 是资格/权限问题，重换令牌只会白打上游）；
//  2. 锁内双检不得重复换票（并发下他人已换新则直接采纳）；
//  3. 换回的令牌仍等于失效令牌时不重试（否则变成对上游的无效连打）。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// qoderTokenExpiredBody 上游对过期凭据的实测响应体（截图报障原文，message 被上游截断）。
const qoderTokenExpiredBody = `{"code":"TOKEN_EXPIRE","message":"token is expired"}`

// qoderAuthRetryUpstream 可控上游：openapi 端点按出站 Bearer 决定放行或拒绝，
// jobToken 端点按脚本成功/失败。所有命中次数与出站 Authorization 都被记录，
// 用于断言「换了几次票」「重试用的是新令牌」。
type qoderAuthRetryUpstream struct {
	server   *httptest.Server
	upstream *qoderFakeUpstream

	mu           sync.Mutex
	jobTokenHits int
	quotaHits    int
	campHits     int
	claimHits    int
	seenAuth     []string
	// claimBodies 记录每次 claim 出站请求体：401 重试必须重发同一个体（
	// 原本的 io.Reader 不可重放，重试若忘重构造就会发空体）。
	claimBodies []string

	expiredToken string
	freshToken   string
	// jobTokenStatus >= 400 时 jobToken 端点直接返回该状态（模拟交换失败）。
	jobTokenStatus int
	// fixedStatus >= 400 时所有 openapi 端点恒定返回该状态（模拟不可自愈 / 403）。
	fixedStatus int
	// freshTokenRepeat 为 true 时 jobToken 交换回**同一个**失效令牌（上游异常态）。
	freshTokenRepeat bool
}

func (u *qoderAuthRetryUpstream) counts() (jobToken, quota, camp, claim int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.jobTokenHits, u.quotaHits, u.campHits, u.claimHits
}

func (u *qoderAuthRetryUpstream) authHeaders() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.seenAuth...)
}

func (u *qoderAuthRetryUpstream) claimRequestBody() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.claimBodies...)
}

// openapiAuthorized 判定本次出站是否带有效（新）令牌。
func (u *qoderAuthRetryUpstream) openapiAuthorized(r *http.Request) bool {
	auth := strings.TrimSpace(r.Header.Get("authorization"))
	u.mu.Lock()
	u.seenAuth = append(u.seenAuth, auth)
	u.mu.Unlock()
	return auth == "Bearer "+u.freshToken
}

func newQoderAuthRetryUpstream(t *testing.T, script *qoderAuthRetryUpstream) *qoderAuthRetryUpstream {
	t.Helper()
	u := script
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// jobToken 交换：响应为 COSY Encode 编码的 JSON 信封。
		if r.URL.Path == strings.SplitN(qoderJobTokenPath, "?", 2)[0] {
			u.mu.Lock()
			u.jobTokenHits++
			failStatus := u.jobTokenStatus
			u.mu.Unlock()
			if failStatus >= 400 {
				w.WriteHeader(failStatus)
				_, _ = io.WriteString(w, `{"code":"PAT_REJECTED"}`)
				return
			}
			next := u.freshToken
			if u.freshTokenRepeat {
				next = u.expiredToken
			}
			raw, err := json.Marshal(map[string]any{
				"name":               "tester",
				"id":                 "u-1",
				"userType":           "personal_standard",
				"securityOauthToken": next,
				"refreshToken":       "drt-fresh",
			})
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, QoderCosyEncode(raw))
			return
		}

		// openapi 三端点：fixedStatus 优先（不可自愈/403 场景）。
		var counter *int
		switch {
		case r.URL.Path == "/api/v2/quota/usage":
			counter = &u.quotaHits
		case r.URL.Path == "/sash/api/v1/me/campaigns":
			counter = &u.campHits
		case strings.HasPrefix(r.URL.Path, "/sash/api/v1/me/campaigns/") && strings.HasSuffix(r.URL.Path, "/claim"):
			counter = &u.claimHits
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		u.mu.Lock()
		*counter++
		fixed := u.fixedStatus
		if counter == &u.claimHits {
			// 先读体再回响应：无论授权与否都要记录，否则「401 重试发空体」测不到。
			bodyBytes, _ := io.ReadAll(r.Body)
			u.claimBodies = append(u.claimBodies, string(bodyBytes))
		}
		u.mu.Unlock()

		if fixed >= 400 {
			w.WriteHeader(fixed)
			if fixed == http.StatusUnauthorized {
				_, _ = io.WriteString(w, qoderTokenExpiredBody)
				return
			}
			_, _ = io.WriteString(w, `{"code":"FORBIDDEN","message":"no permission"}`)
			return
		}
		if !u.openapiAuthorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, qoderTokenExpiredBody)
			return
		}
		// 授权通过：按端点回真实抓包体。
		switch {
		case r.URL.Path == "/api/v2/quota/usage":
			_, _ = io.WriteString(w, qoderQuotaBody)
		case r.URL.Path == "/sash/api/v1/me/campaigns":
			_, _ = io.WriteString(w, qoderCampaignsBody(qoderClaimStatusClaimable, qoderFutureEndAt(), 100))
		default:
			_, _ = io.WriteString(w, qoderClaimFreshBody)
		}
	}))
	t.Cleanup(u.server.Close)
	u.upstream = &qoderFakeUpstream{client: u.server.Client()}
	return u
}

// newQoderAuthRetryService 构造带 jobToken 端点覆盖的测试服务（生产 endpointsOverride 恒 nil）。
func newQoderAuthRetryService(upstream HTTPUpstream, serverURL string) *QoderCreditsService {
	base := strings.TrimRight(serverURL, "/")
	return &QoderCreditsService{
		accountRepo:  &qoderCampaignFakeRepo{},
		httpUpstream: upstream,
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
			},
		},
		endpointsOverride: func(*Account) qoderEndpoints {
			return qoderEndpoints{
				QuotaURL:          base + "/api/v2/quota/usage",
				CampaignsURL:      base + "/sash/api/v1/me/campaigns",
				CampaignClaimBase: base + "/sash/api/v1/me/campaigns",
				// jobToken 域：生产走 resolveQoderEndpoints（受 base_url 中转覆盖），
				// 单测经 endpointsOverride 指到同一个 httptest。
				JobTokenURL: base + qoderJobTokenPath,
				CenterBase:  base,
			}
		},
	}
}

// qoderExpiredTokenAccount 构造「access_token 已过期但非空」的账号（线上报障形态）。
func qoderExpiredTokenAccount(id int64, extra map[string]any) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformQoder,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_token":   "expired-tok",
			"personal_token": "pt-tok",
			"realm":          "cn",
			"uid":            "u-1",
		},
		Extra: extra,
	}
}

// ---------------------------------------------------------------------------
// 正向：401 → 重换 jobToken → 重试一次成功
// ---------------------------------------------------------------------------

// TestQoderOpenAPI401RerexchangesJobTokenAndRetries 余额路径撞 401 后必须重换
// jobToken 并用新令牌重试成功（修复前：直接失败，永不恢复）。
func TestQoderOpenAPI401RerexchangesJobTokenAndRetries(t *testing.T) {
	t.Parallel()
	script := qoderAuthRetryUpstream{expiredToken: "expired-tok", freshToken: "sec-fresh"}
	up := newQoderAuthRetryUpstream(t, &script)
	svc := newQoderAuthRetryService(up.upstream, up.server.URL)
	account := qoderExpiredTokenAccount(901, nil)

	quota, err := svc.qoderFetchQuotaWithAuthRetry(context.Background(), account,
		svc.campaignEndpoints(account), "expired-tok")
	require.NoError(t, err, "401 后重换令牌应让本次调用成功")
	require.NotNil(t, quota)
	require.InDelta(t, 883.0, quota.UserQuota.Remaining, 1e-6, "重试必须拿回真实余额")

	jobTokenHits, quotaHits, _, _ := up.counts()
	require.Equal(t, 1, jobTokenHits, "401 必须触发一次 jobToken 重交换")
	require.Equal(t, 2, quotaHits, "必须重试一次（首发 401 + 重试成功）")
	require.Equal(t, "sec-fresh", account.GetQoderCredentials().AccessToken, "新令牌必须写回内存凭据")
	require.Equal(t, "drt-fresh", account.GetQoderCredentials().RefreshToken, "轮换的 refresh_token 必须写回")
	require.Contains(t, up.authHeaders(), "Bearer sec-fresh", "重试必须使用新令牌出站")
}

// TestQoderCheckin401SelfHealsEndToEnd 签到全链路：campaigns 撞 401 → 重换 →
// 领取成功，且 claim 复用同一次换得的新令牌（不重复换票）。
func TestQoderCheckin401SelfHealsEndToEnd(t *testing.T) {
	t.Parallel()
	script := qoderAuthRetryUpstream{expiredToken: "expired-tok", freshToken: "sec-fresh"}
	up := newQoderAuthRetryUpstream(t, &script)
	svc := newQoderAuthRetryService(up.upstream, up.server.URL)
	account := qoderExpiredTokenAccount(902, nil)

	result := svc.CheckinForAccount(context.Background(), account)
	require.Equal(t, QoderCheckinStatusOK, result.Status, "401 自愈后应真实领取成功")
	require.True(t, result.Success)
	require.Equal(t, int64(100), result.Amount)

	jobTokenHits, _, campHits, claimHits := up.counts()
	require.Equal(t, 1, jobTokenHits, "整轮只换一次票（campaigns 换得的新令牌被 claim 复用）")
	require.Equal(t, 2, campHits, "campaigns 首发 401 + 重试一次")
	require.Equal(t, 1, claimHits, "claim 直接成功，不得再撞一次 401")
	require.Equal(t, []string{"{}"}, up.claimRequestBody(), "claim 必须带 {} 请求体（不能因重试改造变空体）")
}

// TestQoderClaim401RetryResendsSameBody 领取自身撞 401（campaigns 未撞）时也必须
// 用同一个请求体重试：io.Reader 不可重放的年代，重试会发出空体。
func TestQoderClaim401RetryResendsSameBody(t *testing.T) {
	t.Parallel()
	script := qoderAuthRetryUpstream{expiredToken: "expired-tok", freshToken: "sec-fresh"}
	up := newQoderAuthRetryUpstream(t, &script)
	svc := newQoderAuthRetryService(up.upstream, up.server.URL)
	account := qoderExpiredTokenAccount(908, nil)

	// campaigns 用新令牌直接过（不碰 401），让 401 只发生在 claim 上：
	// 先手工把令牌换成新的，再把出站令牌传旧的（模拟 claim 时才过期）。
	account.Credentials["access_token"] = "sec-fresh"
	_, err := svc.qoderClaimCampaignWithAuthRetry(context.Background(), account,
		svc.campaignEndpoints(account), "expired-tok", "camp-1")
	require.NoError(t, err, "claim 撞 401 应自愈成功")

	bodies := up.claimRequestBody()
	require.Equal(t, []string{"{}", "{}"}, bodies, "首发与重试都必须带同一请求体（重试不得发空体）")
	jobTokenHits, _, _, claimHits := up.counts()
	// 本例内存令牌已被换成新的，双侧双检命中「他人已换新」→ 采纳不重复换票。
	// 本用例只关心请求体重发，不重复换票由
	// TestQoderOpenAPI401AdoptsTokenAlreadyRotatedByOthers 专管。
	require.Zero(t, jobTokenHits, "采纳他人已换的新令牌，不得重复换票")
	require.Equal(t, 2, claimHits, "首发 401 + 重试一次")
}

// TestQoderOpenAPI401AdoptsTokenAlreadyRotatedByOthers 锁内双检：若并发方已把
// 令牌换新（内存值不再等于失效令牌），401 自愈必须直接采纳、不重复换票。
func TestQoderOpenAPI401AdoptsTokenAlreadyRotatedByOthers(t *testing.T) {
	t.Parallel()
	script := qoderAuthRetryUpstream{expiredToken: "expired-tok", freshToken: "sec-fresh"}
	up := newQoderAuthRetryUpstream(t, &script)
	svc := newQoderAuthRetryService(up.upstream, up.server.URL)
	account := qoderExpiredTokenAccount(903, nil)
	// 模拟并发方已完成交换：内存里已是新令牌，但仍以旧令牌为双检基准进入。
	account.Credentials["access_token"] = "sec-fresh"

	token, ok := svc.qoderRenewCampaignTokenAfter401(context.Background(), account, "expired-tok")
	require.True(t, ok)
	require.Equal(t, "sec-fresh", token, "必须采纳他人已换的新令牌")
	jobTokenHits, _, _, _ := up.counts()
	require.Zero(t, jobTokenHits, "已换新时不得重复换票（防刷新风暴）")
}

// ---------------------------------------------------------------------------
// 反向：不可自愈 / 403 / 上游异常态
// ---------------------------------------------------------------------------

// TestQoderOpenAPI401WithoutExchangeableCredentialHintsRelogin 仅持 dt- 的账号
// 确实不可自愈：必须给出「需重新登录」提示，并保留原始上游 401 报文。
func TestQoderOpenAPI401WithoutExchangeableCredentialHintsRelogin(t *testing.T) {
	t.Parallel()
	script := qoderAuthRetryUpstream{
		expiredToken: "expired-tok", freshToken: "sec-fresh", fixedStatus: http.StatusUnauthorized,
	}
	up := newQoderAuthRetryUpstream(t, &script)
	svc := newQoderAuthRetryService(up.upstream, up.server.URL)
	account := qoderExpiredTokenAccount(904, nil)
	// 只持 dt-：删掉可交换凭据。
	delete(account.Credentials, "personal_token")

	_, err := svc.qoderFetchQuotaWithAuthRetry(context.Background(), account,
		svc.campaignEndpoints(account), "expired-tok")
	require.Error(t, err)
	require.Contains(t, err.Error(), "TOKEN_EXPIRE", "必须保留原始上游报文供排障")
	require.Contains(t, err.Error(), "re-login required", "必须给出重新登录的处置提示")
	jobTokenHits, _, _, _ := up.counts()
	require.Zero(t, jobTokenHits, "无可交换凭据时不得空打 jobToken 端点")
}

// TestQoderOpenAPI401ExchangeFailureKeepsOriginalError 交换本身失败时不得用交换
// 错误顶掉真实上游报文（排障需要看到真正的 TOKEN_EXPIRE）。
func TestQoderOpenAPI401ExchangeFailureKeepsOriginalError(t *testing.T) {
	t.Parallel()
	script := qoderAuthRetryUpstream{
		expiredToken: "expired-tok", freshToken: "sec-fresh",
		fixedStatus: http.StatusUnauthorized, jobTokenStatus: http.StatusForbidden,
	}
	up := newQoderAuthRetryUpstream(t, &script)
	svc := newQoderAuthRetryService(up.upstream, up.server.URL)
	account := qoderExpiredTokenAccount(905, nil)

	_, err := svc.qoderFetchQuotaWithAuthRetry(context.Background(), account,
		svc.campaignEndpoints(account), "expired-tok")
	require.Error(t, err)
	require.Contains(t, err.Error(), "TOKEN_EXPIRE", "原始 401 报文不得被交换错误顶掉")
	require.Contains(t, err.Error(), "re-login required")
	require.NotContains(t, err.Error(), "HTTP 403", "交换侧的 403 不应冒充上游业务错误")
}

// TestQoderOpenAPI403DoesNotTriggerRexchange 403 是资格/权限问题，重换令牌救不了：
// 必须零 jobToken 调用，直接按原错误返回。
func TestQoderOpenAPI403DoesNotTriggerRexchange(t *testing.T) {
	t.Parallel()
	script := qoderAuthRetryUpstream{
		expiredToken: "expired-tok", freshToken: "sec-fresh", fixedStatus: http.StatusForbidden,
	}
	up := newQoderAuthRetryUpstream(t, &script)
	svc := newQoderAuthRetryService(up.upstream, up.server.URL)
	account := qoderExpiredTokenAccount(906, nil)

	_, err := svc.qoderFetchQuotaWithAuthRetry(context.Background(), account,
		svc.campaignEndpoints(account), "expired-tok")
	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP 403")
	require.NotContains(t, err.Error(), "re-login required", "403 不应报成令牌过期")
	jobTokenHits, quotaHits, _, _ := up.counts()
	require.Zero(t, jobTokenHits, "403 不得触发重换")
	require.Equal(t, 1, quotaHits, "403 不得重试")
}

// TestQoderOpenAPI401RepeatTokenDoesNotRetry 上游换回同一个失效令牌（异常态）时
// 不得重试——否则就是拿死令牌连打上游。
func TestQoderOpenAPI401RepeatTokenDoesNotRetry(t *testing.T) {
	t.Parallel()
	script := qoderAuthRetryUpstream{
		expiredToken: "expired-tok", freshToken: "sec-fresh",
		fixedStatus: http.StatusUnauthorized, freshTokenRepeat: true,
	}
	up := newQoderAuthRetryUpstream(t, &script)
	svc := newQoderAuthRetryService(up.upstream, up.server.URL)
	account := qoderExpiredTokenAccount(907, nil)

	_, err := svc.qoderFetchQuotaWithAuthRetry(context.Background(), account,
		svc.campaignEndpoints(account), "expired-tok")
	require.Error(t, err)
	require.Contains(t, err.Error(), "TOKEN_EXPIRE")
	jobTokenHits, quotaHits, _, _ := up.counts()
	require.Equal(t, 1, jobTokenHits)
	require.Equal(t, 1, quotaHits, "换回同一个失效令牌时不得重试")
}
