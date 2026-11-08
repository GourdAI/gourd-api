//go:build unit

package service

// qoder_device_token_test.go 设备授权登录（dt-/drt-）账号 401 自愈的回归锁。
//
// 线上报障（2026-11-08）：4 个 Qoder 账号全部由设备授权登录产生，凭据为
// access_token=dt-、refresh_token=drt-、无 pt-。dt- 过期后 openapi 回
// HTTP 401 TOKEN_EXPIRE，旧自愈把 drt- 送进 jobToken（PAT 家族）端点，上游恒回
// "personal token is invalid"，签到/积分永久失败。修复为按 refresh_token 前缀
// 分派：drt- → openapi /api/v1/deviceToken/refresh，pt-/jrt- → center jobToken。
//
// 端点形态经 CN 域实测：POST {"refresh_token":"drt-..."} → 200 携
// device_token + 轮换后的 refresh_token + expires_at（RFC3339）。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// qoderDeviceRefreshUpstream 记录 deviceToken/refresh 与 jobToken 端点各自的命中，
// 用于断言「凭据家族分派正确」——设备流账号绝不能碰到 jobToken 端点。
type qoderDeviceRefreshUpstream struct {
	server   *httptest.Server
	upstream *qoderFakeUpstream

	mu               sync.Mutex
	deviceHits       int
	jobTokenHits     int
	deviceBodies     []string
	seenAuth         []string
	deviceFailStatus int
	deviceNewAccess  string
	deviceNewRefresh string
	expiredToken     string
}

func (u *qoderDeviceRefreshUpstream) counts() (device, jobToken int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.deviceHits, u.jobTokenHits
}

func (u *qoderDeviceRefreshUpstream) bodies() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.deviceBodies...)
}

func (u *qoderDeviceRefreshUpstream) authHeaders() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.seenAuth...)
}

func newQoderDeviceRefreshUpstream(t *testing.T, script *qoderDeviceRefreshUpstream) *qoderDeviceRefreshUpstream {
	t.Helper()
	u := script
	if u.deviceNewAccess == "" {
		u.deviceNewAccess = "dt-fresh"
	}
	if u.deviceNewRefresh == "" {
		u.deviceNewRefresh = "drt-fresh"
	}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// 设备流刷新端点：明文 JSON，返回新 token pair（含轮换的 refresh_token）。
		if r.URL.Path == qoderDeviceTokenRefreshPath {
			body, _ := io.ReadAll(r.Body)
			u.mu.Lock()
			u.deviceHits++
			u.deviceBodies = append(u.deviceBodies, string(body))
			fail := u.deviceFailStatus
			u.mu.Unlock()
			if fail >= 400 {
				w.WriteHeader(fail)
				_, _ = io.WriteString(w, `{"errorCode":"InvalidRefreshToken"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_token":             u.deviceNewAccess,
				"refresh_token":            u.deviceNewRefresh,
				"token_type":               "Bearer",
				"expires_at":               "2026-12-07T17:35:18Z",
				"refresh_token_expires_at": "2027-11-02T17:35:18Z",
			})
			return
		}

		// jobToken（PAT 家族）端点：设备流账号进来即为分派错误，直接报错让测试可见。
		if r.URL.Path == strings.SplitN(qoderJobTokenPath, "?", 2)[0] {
			u.mu.Lock()
			u.jobTokenHits++
			u.mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `personal token is invalid`)
			return
		}

		// openapi 业务端点：只认新令牌。
		auth := strings.TrimSpace(r.Header.Get("authorization"))
		u.mu.Lock()
		u.seenAuth = append(u.seenAuth, auth)
		u.mu.Unlock()
		if auth != "Bearer "+u.deviceNewAccess {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, qoderTokenExpiredBody)
			return
		}
		switch {
		case r.URL.Path == "/api/v2/quota/usage":
			_, _ = io.WriteString(w, qoderQuotaBody)
		case r.URL.Path == "/sash/api/v1/me/campaigns":
			_, _ = io.WriteString(w, qoderCampaignsBody(qoderClaimStatusClaimable, qoderFutureEndAt(), 100))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			_, _ = io.WriteString(w, qoderClaimFreshBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(u.server.Close)
	u.upstream = &qoderFakeUpstream{client: u.server.Client()}
	return u
}

func newQoderDeviceRefreshService(upstream *qoderFakeUpstream, serverURL string) *QoderCreditsService {
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
				QuotaURL:              base + "/api/v2/quota/usage",
				CampaignsURL:          base + qoderCampaignsPath,
				CampaignClaimBase:     base + qoderCampaignsPath,
				JobTokenURL:           base + qoderJobTokenPath,
				DeviceTokenRefreshURL: base + qoderDeviceTokenRefreshPath,
				CenterBase:            base,
			}
		},
	}
}

// qoderDeviceAccount 复刻线上真实凭据形态（无 pt-，dt- 已过期但非空）。
func qoderDeviceAccount(id int64) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformQoder,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_token":  "dt-expired",
			"refresh_token": "drt-seed",
			"device_token":  "dt-expired",
			"uid":           "u-device",
			"realm":         "cn",
		},
	}
}

// TestQoderDeviceAccount401SelfHealsViaDeviceRefresh 核心回归：drt- 账号撞 401
// 必须走 deviceToken/refresh（零 jobToken 命中），换回的新 token pair 成对写回，
// 并用新令牌重试成功。修复前此用例必失败（drt- 被送进 jobToken 端点）。
func TestQoderDeviceAccount401SelfHealsViaDeviceRefresh(t *testing.T) {
	t.Parallel()
	up := newQoderDeviceRefreshUpstream(t, &qoderDeviceRefreshUpstream{expiredToken: "dt-expired"})
	svc := newQoderDeviceRefreshService(up.upstream, up.server.URL)
	account := qoderDeviceAccount(910)

	quota, err := svc.qoderFetchQuotaWithAuthRetry(context.Background(), account,
		svc.campaignEndpoints(account), "dt-expired")
	require.NoError(t, err, "设备流账号 401 后必须自愈成功")
	require.NotNil(t, quota)

	deviceHits, jobTokenHits := up.counts()
	require.Equal(t, 1, deviceHits, "401 必须触发一次 deviceToken/refresh")
	require.Zero(t, jobTokenHits, "drt- 绝不能进 jobToken（PAT 家族）端点")

	creds := account.GetQoderCredentials()
	require.Equal(t, "dt-fresh", creds.AccessToken, "新 access_token 必须写回")
	require.Equal(t, "drt-fresh", creds.RefreshToken, "轮换的 refresh_token 必须写回，否则下次续期必败")
	require.Equal(t, "dt-fresh", account.GetCredential("device_token"), "device_token 同源字段须同步刷新")
	wantExpiry, err := time.Parse(time.RFC3339, "2026-12-07T17:35:18Z")
	require.NoError(t, err)
	require.Equal(t, strconv.FormatInt(wantExpiry.Unix(), 10), account.GetCredential("expires_at"),
		"上游返回的 expires_at 须写回凭据（供观测与将来的主动刷新）")
	require.Contains(t, up.authHeaders(), "Bearer dt-fresh", "重试必须用新令牌出站")

	// 请求体契约：明文 JSON，仅 refresh_token。
	require.Len(t, up.bodies(), 1)
	var sent map[string]string
	require.NoError(t, json.Unmarshal([]byte(up.bodies()[0]), &sent))
	require.Equal(t, "drt-seed", sent["refresh_token"])
}

// TestQoderDeviceAccountCheckin401SelfHealsEndToEnd 签到全链路：设备流账号
// campaigns 撞 401 → 设备刷新 → 领取成功，且整轮只换一次票。
func TestQoderDeviceAccountCheckin401SelfHealsEndToEnd(t *testing.T) {
	t.Parallel()
	up := newQoderDeviceRefreshUpstream(t, &qoderDeviceRefreshUpstream{expiredToken: "dt-expired"})
	svc := newQoderDeviceRefreshService(up.upstream, up.server.URL)
	account := qoderDeviceAccount(911)

	result := svc.CheckinForAccount(context.Background(), account)
	require.Equal(t, QoderCheckinStatusOK, result.Status, "401 自愈后应真实领取成功")
	require.True(t, result.Success)

	deviceHits, jobTokenHits := up.counts()
	require.Equal(t, 1, deviceHits, "整轮只换一次票（新令牌被后续调用复用）")
	require.Zero(t, jobTokenHits)
	require.Equal(t, "drt-fresh", account.GetQoderCredentials().RefreshToken)
}

// TestQoderPATAccountStillUsesJobToken 防回归：PAT 家族（pt-，无 drt-）仍必须走
// 原来的 jobToken 端点，不得被设备流分派抢走。
func TestQoderPATAccountStillUsesJobToken(t *testing.T) {
	t.Parallel()
	// 复用既有 PAT 骨架（jobToken 端点在 qoderAuthRetryUpstream 中实现）。
	script := qoderAuthRetryUpstream{expiredToken: "expired-tok", freshToken: "sec-fresh"}
	up := newQoderAuthRetryUpstream(t, &script)
	svc := newQoderAuthRetryService(up.upstream, up.server.URL)
	account := qoderExpiredTokenAccount(912, nil) // personal_token=pt-tok，无 drt-

	_, err := svc.qoderFetchQuotaWithAuthRetry(context.Background(), account,
		svc.campaignEndpoints(account), "expired-tok")
	require.NoError(t, err)
	jobTokenHits, quotaHits, _, _ := up.counts()
	require.Equal(t, 1, jobTokenHits, "PAT 家族必须仍走 jobToken")
	require.Equal(t, 2, quotaHits)
}

// TestQoderDeviceRefreshFailureKeepsOriginal401 设备刷新本身失败（如 drt- 被吊销）
// 时保留原始上游 401 报文并给出重新登录提示，不得用刷新错误顶掉。
func TestQoderDeviceRefreshFailureKeepsOriginal401(t *testing.T) {
	t.Parallel()
	up := newQoderDeviceRefreshUpstream(t, &qoderDeviceRefreshUpstream{
		expiredToken: "dt-expired", deviceFailStatus: http.StatusUnauthorized,
	})
	svc := newQoderDeviceRefreshService(up.upstream, up.server.URL)
	account := qoderDeviceAccount(913)

	_, err := svc.qoderFetchQuotaWithAuthRetry(context.Background(), account,
		svc.campaignEndpoints(account), "dt-expired")
	require.Error(t, err)
	require.Contains(t, err.Error(), "TOKEN_EXPIRE", "必须保留原始上游报文")
	require.Contains(t, err.Error(), "re-login required")
}

// TestQoderRefreshDeviceTokenRejectsIncompletePair 只回 access_token、不回轮换
// refresh_token 的异常响应必须判失败（否则下一次续期拿旧 drt- 必败）。
func TestQoderRefreshDeviceTokenRejectsIncompletePair(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"device_token":"dt-only"}`)
	}))
	defer server.Close()
	_, err := QoderRefreshDeviceToken(context.Background(), server.Client().Transport,
		qoderEndpoints{DeviceTokenRefreshURL: server.URL + qoderDeviceTokenRefreshPath}, "drt-seed")
	require.ErrorContains(t, err, "incomplete token pair")
}
