//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// qoder_campaign_service_test.go 积分查询与「每日领取活动 Credits」的单测。
//
// 上游响应体全部取自 2026-09-24 真实账号抓包（quota/usage、me/campaigns、
// campaigns/{id}/claim），因此本文件同时是接口契约的回归锁：上游若改字段命名
// （camelCase / float 数值 / replayed 幂等标志）这里会先红。
//
// openapi 域刻意不受 credentials.base_url 中转覆盖（qoder.go 的既定语义），所以
// 测试通过 endpointsOverride 把三个端点指向 httptest（生产恒 nil）。

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

type qoderExtraCall struct {
	accountID int64
	updates   map[string]any
}

// qoderCampaignFakeRepo 记录 extra 落库与账号枚举的仓库假实现。
type qoderCampaignFakeRepo struct {
	mockAccountRepoForGemini
	mu         sync.Mutex
	extraCalls []qoderExtraCall
}

func (r *qoderCampaignFakeRepo) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make(map[string]any, len(updates))
	for k, v := range updates {
		cp[k] = v
	}
	r.extraCalls = append(r.extraCalls, qoderExtraCall{accountID: id, updates: cp})
	return nil
}

func (r *qoderCampaignFakeRepo) calls() []qoderExtraCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]qoderExtraCall(nil), r.extraCalls...)
}

// lastExtraValue 返回指定 key 最近一次写入的值（无记录返回 nil）。
func (r *qoderCampaignFakeRepo) lastExtraValue(key string) any {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.extraCalls) - 1; i >= 0; i-- {
		if v, ok := r.extraCalls[i].updates[key]; ok {
			return v
		}
	}
	return nil
}

// ListByPlatform 覆写内嵌 mock（周期任务枚举入口）。
func (r *qoderCampaignFakeRepo) ListByPlatform(ctx context.Context, platform string) ([]Account, error) {
	var out []Account
	for i := range r.accounts {
		if r.accounts[i].Platform == platform {
			out = append(out, r.accounts[i])
		}
	}
	return out, nil
}

// qoderFakeUpstream 记录请求轨迹（方法/路径/头/请求体）。
type qoderFakeUpstream struct {
	mu      sync.Mutex
	entries []qoderUpstreamCall
	client  *http.Client
}

type qoderUpstreamCall struct {
	method  string
	path    string
	header  http.Header
	body    string
	request *http.Request
}

func (u *qoderFakeUpstream) Do(req *http.Request, proxyURL string, accountID int64, concurrency int) (*http.Response, error) {
	var body string
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(raw))
		body = string(raw)
	}
	u.mu.Lock()
	u.entries = append(u.entries, qoderUpstreamCall{
		method: req.Method, path: req.URL.Path, header: req.Header.Clone(), body: body, request: req,
	})
	u.mu.Unlock()
	return u.client.Do(req)
}

func (u *qoderFakeUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func (u *qoderFakeUpstream) snapshot() []qoderUpstreamCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]qoderUpstreamCall(nil), u.entries...)
}

func (u *qoderFakeUpstream) paths() []string {
	var out []string
	for _, e := range u.snapshot() {
		out = append(out, e.path)
	}
	return out
}

func (u *qoderFakeUpstream) postCount() int {
	n := 0
	for _, e := range u.snapshot() {
		if e.method == http.MethodPost {
			n++
		}
	}
	return n
}

// qoderScript 是假上游的可控响应脚本。
type qoderScript struct {
	quotaBody     string
	quotaStatus   int
	campaignBody  string
	campStatus    int
	claimBody     string
	claimStatus   int
	claimOverride func(w http.ResponseWriter, r *http.Request)
}

func newQoderCampaignServer(t *testing.T, script *qoderScript) (*httptest.Server, *qoderFakeUpstream) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v2/quota/usage":
			if script.quotaStatus >= 400 {
				w.WriteHeader(script.quotaStatus)
				_, _ = io.WriteString(w, "upstream unavailable")
				return
			}
			_, _ = io.WriteString(w, script.quotaBody)
		case r.URL.Path == "/sash/api/v1/me/campaigns":
			if script.campStatus >= 400 {
				w.WriteHeader(script.campStatus)
				_, _ = io.WriteString(w, "campaigns unavailable")
				return
			}
			_, _ = io.WriteString(w, script.campaignBody)
		case strings.HasPrefix(r.URL.Path, "/sash/api/v1/me/campaigns/") && strings.HasSuffix(r.URL.Path, "/claim"):
			if script.claimOverride != nil {
				script.claimOverride(w, r)
				return
			}
			if script.claimStatus >= 400 {
				w.WriteHeader(script.claimStatus)
				_, _ = io.WriteString(w, "claim rejected")
				return
			}
			_, _ = io.WriteString(w, script.claimBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, &qoderFakeUpstream{client: server.Client()}
}

// newQoderCreditsTestService 构造带端点覆盖的测试服务。
func newQoderCreditsTestService(repo AccountRepository, upstream HTTPUpstream, serverURL string) *QoderCreditsService {
	base := strings.TrimRight(serverURL, "/")
	return &QoderCreditsService{
		accountRepo:  repo,
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
			}
		},
	}
}

func qoderCreditsTestAccount(id int64, extra map[string]any) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformQoder,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_token": "dt-test-token",
			"realm":        "cn",
			"uid":          "u-1",
		},
		Extra: extra,
	}
}

// ---------------------------------------------------------------------------
// 真实抓包响应体
// ---------------------------------------------------------------------------

const qoderQuotaBody = `{
  "userId": "019f1f00-0000-7000-8000-000000000001",
  "userType": "personal_professional",
  "usageType": "credits",
  "totalUsagePercentage": 0.58,
  "isQuotaExceeded": false,
  "expiresAt": 1792684800000,
  "upgradeUrl": "https://qoder.com.cn/pricing",
  "userQuota": {"total": 2000.0, "used": 1117.0, "remaining": 883.0, "percentage": 0.56, "unit": "credits"},
  "addOnQuota": {"total": 300.0, "used": 200.0, "remaining": 100.0, "percentage": 0.67, "unit": "credits",
                 "detailUrl": "https://qoder.com.cn/account/usage"},
  "dedicatedResourcePackages": [
    {"id": "pkg-1", "name": "act-20260901-170", "description": "growth-campaign:x:grant:y",
     "total": 100.0, "used": 100.0, "remaining": 0.0, "percentage": 1.0, "unit": "credits",
     "expiresAt": 1790000000000, "available": false, "status": "QUOTA_DETAIL_STATUS_EXHAUSTED"}
  ],
  "isPlanQuotaExceeded": false,
  "isPlanQuotaProrated": false
}`

// qoderCampaignsBody 构造活动列表（领取态与窗口结束时间由用例决定）。
func qoderCampaignsBody(claimStatus string, endAt int64, amount int64) string {
	return fmt.Sprintf(`{
  "uid": "019f1f00-0000-7000-8000-000000000001",
  "showCampaign": true,
  "claimable": %v,
  "campaignUrl": "https://docs.qoder.cn/events/100credits",
  "campaigns": [{
    "campaignId": "01a0cd40-ea93-75c3-be36-23c22a84e619",
    "campaignKey": "act-20260923-159",
    "actionType": "CLAIM_BENEFIT",
    "startAt": %d,
    "endAt": %d,
    "claimStatus": %q,
    "benefit": {"kind": "CREDITS", "amount": %d,
      "modelScope": {"modelSeries": {"key": "ALL_MODELS"}},
      "validity": {"mode": "RELATIVE_DAYS", "days": 30}},
    "placements": [{"type": "POPUP", "content": {"zh": {"title": "每日签到", "description": "领取 100 Credits"}}}]
  }]
}`, claimStatus == qoderClaimStatusClaimable, endAt-86400, endAt, claimStatus, amount)
}

const qoderClaimReplayedBody = `{
  "grantId": "01a0d139-2442-788c-8662-d2d1172171e1",
  "status": "CLAIMED",
  "replayed": true,
  "benefit": {"kind": "CREDITS", "amount": 100,
    "modelScope": {"modelSeries": {"key": "ALL_MODELS"}},
    "validity": {"mode": "RELATIVE_DAYS", "days": 30}},
  "campaignId": "01a0cd40-ea93-75c3-be36-23c22a84e619",
  "campaignKey": "act-20260923-159",
  "campaignVersion": 1,
  "claimedAt": "2026-09-24T02:22:58.098877Z",
  "grantedAt": "2026-09-24T02:22:58.119954Z",
  "expiresAt": "2026-10-24T02:22:58.098877Z"
}`

const qoderClaimFreshBody = `{"grantId":"01a0d139-aaaa-7000-8000-000000000002","status":"CLAIMED","replayed":false,
 "benefit":{"kind":"CREDITS","amount":100},"campaignId":"01a0cd40-ea93-75c3-be36-23c22a84e619",
 "campaignKey":"act-20260923-159","claimedAt":"2026-09-24T11:00:00.000000Z",
 "expiresAt":"2026-10-24T11:00:00.000000Z"}`

const qoderEmptyCampaignsBody = `{"uid":"u","showCampaign":false,"claimable":false,"campaignUrl":"","campaigns":[]}`

func qoderFutureEndAt() int64 { return time.Now().Unix() + 6*3600 }

// ---------------------------------------------------------------------------
// 积分查询
// ---------------------------------------------------------------------------

func TestQoderQueryCreditsAggregatesQuotaAndCampaigns(t *testing.T) {
	t.Parallel()
	script := &qoderScript{
		quotaBody:    qoderQuotaBody,
		campaignBody: qoderCampaignsBody(qoderClaimStatusClaimable, qoderFutureEndAt(), 100),
	}
	server, upstream := newQoderCampaignServer(t, script)
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL)
	account := qoderCreditsTestAccount(11, nil)
	repo.accountsByID = map[int64]*Account{11: account}

	result, err := svc.QueryCredits(context.Background(), 11)
	require.NoError(t, err)
	require.True(t, result.Success, "查询应成功: %v", result.Error)

	// 余额 = 套餐内 + 资源包（数值是 float，不得截断成 int）。
	require.InDelta(t, 983.0, result.Remaining, 1e-6)
	require.InDelta(t, 1317.0, result.Used, 1e-6)
	require.InDelta(t, 2300.0, result.Total, 1e-6)
	require.InDelta(t, 883.0, result.PlanRemain, 1e-6)
	require.InDelta(t, 100.0, result.AddOnRemain, 1e-6)
	require.Equal(t, "personal_professional", result.UserType)
	require.Equal(t, "cn", result.Realm)
	require.False(t, result.QuotaExceeded)
	require.Equal(t, 1, result.Packs)
	require.Len(t, result.Packages, 1)
	require.Equal(t, "act-20260901-170", result.Packages[0].Name)

	// 活动侧：CLAIMABLE 条目透出额度与 key，并标记为可领。
	require.True(t, result.Claimable)
	require.Equal(t, int64(100), result.ClaimAmount)
	require.Equal(t, "act-20260923-159", result.ClaimCampaignKey)
	require.Empty(t, result.TodayClaimStatus)
	require.Equal(t, qoderCampaignRoundDate(time.Now()), result.Round)

	// 请求形状：GET 余额 + GET 活动；纯 Bearer 直连、桌面端 clientType=10。
	calls := upstream.snapshot()
	require.Len(t, calls, 2)
	require.Equal(t, []string{"/api/v2/quota/usage", "/sash/api/v1/me/campaigns"}, upstream.paths())
	for _, call := range calls {
		require.Equal(t, http.MethodGet, call.method)
		require.Equal(t, "Bearer dt-test-token", call.header.Get("authorization"))
		require.Equal(t, "10", call.header.Get("cosy-clienttype"), "活动/余额接口必须用桌面端 clientType")
		require.Equal(t, "0.3.4", call.header.Get("cosy-version"))
	}

	// 快照落 extra（qoder_credits），供列表免探测渲染。
	snapshot, ok := repo.lastExtraValue(qoderCreditsExtraKey).(QoderCreditsSnapshot)
	require.True(t, ok, "应写入积分快照")
	require.InDelta(t, 983.0, snapshot.Remaining, 1e-6)
	require.Equal(t, "cn", snapshot.Realm)
	require.Greater(t, snapshot.FetchedAt, int64(0))
}

func TestQoderQueryCreditsClaimedCampaignReportsAlready(t *testing.T) {
	t.Parallel()
	script := &qoderScript{
		quotaBody:    qoderQuotaBody,
		campaignBody: qoderCampaignsBody(qoderClaimStatusClaimed, qoderFutureEndAt(), 100),
	}
	server, upstream := newQoderCampaignServer(t, script)
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL)
	repo.accountsByID = map[int64]*Account{12: qoderCreditsTestAccount(12, nil)}

	result, err := svc.QueryCredits(context.Background(), 12)
	require.NoError(t, err)
	require.True(t, result.Success)
	// 「已领」判据是 claimStatus==CLAIMED（条目仍在列表里返回，不是消失）。
	require.False(t, result.Claimable)
	require.Equal(t, QoderCheckinStatusAlready, result.TodayClaimStatus)
	require.Equal(t, int64(100), result.ClaimAmount)
}

func TestQoderQueryCreditsExpiredWindowIgnored(t *testing.T) {
	t.Parallel()
	script := &qoderScript{
		quotaBody:    qoderQuotaBody,
		campaignBody: qoderCampaignsBody(qoderClaimStatusClaimable, time.Now().Unix()-60, 100),
	}
	server, upstream := newQoderCampaignServer(t, script)
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL)
	repo.accountsByID = map[int64]*Account{13: qoderCreditsTestAccount(13, nil)}

	result, err := svc.QueryCredits(context.Background(), 13)
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, int64(0), result.ClaimAmount, "窗口已过的条目不得算作可领")
	require.Empty(t, result.TodayClaimStatus)
}

// 活动接口挂掉时：余额结论必须保留（增强信息失败不得把整次查询判成失败），
// 且领取态回退本地快照，避免把「本轮已领」显示成「未领」。
func TestQoderQueryCreditsCampaignFailureKeepsQuotaAndFallsBackToSnapshot(t *testing.T) {
	t.Parallel()
	script := &qoderScript{quotaBody: qoderQuotaBody, campStatus: http.StatusInternalServerError}
	server, upstream := newQoderCampaignServer(t, script)
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL)
	account := qoderCreditsTestAccount(14, map[string]any{
		qoderCheckinExtraKey: map[string]any{
			"round":  qoderCampaignRoundDate(time.Now()),
			"status": QoderCheckinStatusOK,
		},
	})
	repo.accountsByID = map[int64]*Account{14: account}

	result, err := svc.QueryCredits(context.Background(), 14)
	require.NoError(t, err)
	require.True(t, result.Success, "活动失败不得把查询判为失败")
	require.InDelta(t, 983.0, result.Remaining, 1e-6)
	require.Equal(t, QoderCheckinStatusOK, result.TodayClaimStatus, "上游抖动时回退本地领取快照")
}

func TestQoderQueryCreditsQuotaFailureIsResultNotGoError(t *testing.T) {
	t.Parallel()
	script := &qoderScript{quotaBody: "boom", quotaStatus: http.StatusBadGateway}
	server, upstream := newQoderCampaignServer(t, script)
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL)
	repo.accountsByID = map[int64]*Account{15: qoderCreditsTestAccount(15, nil)}

	result, err := svc.QueryCredits(context.Background(), 15)
	require.NoError(t, err, "上游探测失败按 200+success=false 返回")
	require.NotNil(t, result)
	require.False(t, result.Success)
	require.Contains(t, result.Error, "HTTP 502")
	require.Nil(t, repo.lastExtraValue(qoderCreditsExtraKey), "失败查询不落积分快照")
}

func TestQoderCreditsRejectsNonQoderAccount(t *testing.T) {
	t.Parallel()
	repo := &qoderCampaignFakeRepo{
		accountsByID: map[int64]*Account{
			16: {ID: 16, Platform: PlatformWorkbuddy, Status: StatusActive},
		},
	}
	svc := newQoderCreditsTestService(repo, &qoderFakeUpstream{client: http.DefaultClient}, "http://127.0.0.1:1")

	_, err := svc.QueryCredits(context.Background(), 16)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))

	_, err = svc.Checkin(context.Background(), 16)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
}

func TestQoderCreditsAccountWithoutUsableTokenFails(t *testing.T) {
	t.Parallel()
	server, upstream := newQoderCampaignServer(t, &qoderScript{quotaBody: qoderQuotaBody})
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL)
	account := qoderCreditsTestAccount(17, nil)
	account.Credentials = map[string]any{"realm": "cn"}
	repo.accountsByID = map[int64]*Account{17: account}

	result, err := svc.QueryCredits(context.Background(), 17)
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Error, "no usable access token")
	require.Empty(t, upstream.paths(), "无凭据时不得打上游")
}

// ---------------------------------------------------------------------------
// 每日领取
// ---------------------------------------------------------------------------

func TestQoderCheckinClaimsFreshRoundAndPersists(t *testing.T) {
	t.Parallel()
	script := &qoderScript{
		quotaBody:    qoderQuotaBody,
		campaignBody: qoderCampaignsBody(qoderClaimStatusClaimable, qoderFutureEndAt(), 100),
		claimBody:    qoderClaimFreshBody,
	}
	server, upstream := newQoderCampaignServer(t, script)
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL)
	account := qoderCreditsTestAccount(21, nil)
	repo.accountsByID = map[int64]*Account{21: account}

	result := svc.CheckinForAccount(context.Background(), account)
	require.Equal(t, QoderCheckinStatusOK, result.Status)
	require.True(t, result.Success)
	require.Equal(t, int64(100), result.Amount)
	require.Equal(t, "01a0d139-aaaa-7000-8000-000000000002", result.GrantID)
	require.Equal(t, "2026-10-24T11:00:00.000000Z", result.ExpiresAt)
	require.InDelta(t, 983.0, result.Credits, 1e-6, "领取后须刷新余额")

	// 请求编排：GET campaigns → POST claim → GET quota（余额独立刷新）。
	require.Equal(t, []string{
		"/sash/api/v1/me/campaigns",
		"/sash/api/v1/me/campaigns/01a0cd40-ea93-75c3-be36-23c22a84e619/claim",
		"/api/v2/quota/usage",
	}, upstream.paths(), "claim 路径参数必须是 campaignId（不是 campaignKey）")
	calls := upstream.snapshot()
	require.Equal(t, []string{http.MethodGet, http.MethodPost, http.MethodGet},
		[]string{calls[0].method, calls[1].method, calls[2].method}, "领取编排应恰好一次写请求")
	require.Equal(t, "{}", calls[1].body, "claim 请求体为空 JSON 对象")
	// 写请求也必须带桌面端 clientType（上游按渠道定向）。
	require.Equal(t, "10", calls[1].header.Get("cosy-clienttype"))
	require.Equal(t, "application/json", calls[1].header.Get("content-type"))
	require.Equal(t, "Bearer dt-test-token", calls[1].header.Get("authorization"))

	// 两个快照分别落库。
	checkinSnapshot, ok := repo.lastExtraValue(qoderCheckinExtraKey).(QoderCheckinSnapshot)
	require.True(t, ok)
	require.Equal(t, QoderCheckinStatusOK, checkinSnapshot.Status)
	require.Equal(t, qoderCampaignRoundDate(time.Now()), checkinSnapshot.Round)
	require.Equal(t, int64(100), checkinSnapshot.Amount)
	require.NotNil(t, repo.lastExtraValue(qoderCreditsExtraKey))
}

// 上游对同一轮重复领取幂等（replayed=true、不再发钱）→ 本端语义 already 且 success。
func TestQoderCheckinReplayedIsAlreadyNotFailure(t *testing.T) {
	t.Parallel()
	script := &qoderScript{
		quotaBody:    qoderQuotaBody,
		campaignBody: qoderCampaignsBody(qoderClaimStatusClaimable, qoderFutureEndAt(), 100),
		claimBody:    qoderClaimReplayedBody,
	}
	server, upstream := newQoderCampaignServer(t, script)
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL)
	account := qoderCreditsTestAccount(22, nil)
	repo.accountsByID = map[int64]*Account{22: account}

	result := svc.CheckinForAccount(context.Background(), account)
	require.Equal(t, QoderCheckinStatusAlready, result.Status)
	require.True(t, result.Success)
	require.Equal(t, "01a0d139-2442-788c-8662-d2d1172171e1", result.GrantID)
	require.Equal(t, "2026-10-24T02:22:58.098877Z", result.ExpiresAt)
	require.Equal(t, 1, upstream.postCount())
}

// 活动已是 CLAIMED：直接判 already，零写请求（不白打 claim）。
func TestQoderCheckinClaimedCampaignSkipsWriteRequest(t *testing.T) {
	t.Parallel()
	script := &qoderScript{
		quotaBody:    qoderQuotaBody,
		campaignBody: qoderCampaignsBody(qoderClaimStatusClaimed, qoderFutureEndAt(), 100),
	}
	server, upstream := newQoderCampaignServer(t, script)
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL)
	account := qoderCreditsTestAccount(23, nil)
	repo.accountsByID = map[int64]*Account{23: account}

	result := svc.CheckinForAccount(context.Background(), account)
	require.Equal(t, QoderCheckinStatusAlready, result.Status)
	require.True(t, result.Success)
	require.Equal(t, int64(100), result.Amount)
	require.NotEmpty(t, result.ExpiresAt, "already 也应按 validity 推算到期")
	require.Equal(t, 0, upstream.postCount(), "已领轮次不得发写请求")
	require.Equal(t, []string{"/sash/api/v1/me/campaigns", "/api/v2/quota/usage"}, upstream.paths())
}

// 账号无资格（服务端不下发活动）= 正常态 skipped：零写请求、不落快照、不参与重试。
func TestQoderCheckinNoCampaignIsSkippedWithoutSnapshot(t *testing.T) {
	t.Parallel()
	script := &qoderScript{quotaBody: qoderQuotaBody, campaignBody: qoderEmptyCampaignsBody}
	server, upstream := newQoderCampaignServer(t, script)
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL)
	account := qoderCreditsTestAccount(24, nil)
	repo.accountsByID = map[int64]*Account{24: account}

	result := svc.CheckinForAccount(context.Background(), account)
	require.Equal(t, QoderCheckinStatusSkipped, result.Status)
	require.False(t, result.Success)
	require.Contains(t, result.Detail, "no claimable campaign")
	require.Equal(t, 0, upstream.postCount())
	require.Nil(t, repo.lastExtraValue(qoderCheckinExtraKey), "skipped 不落领取快照")
}

// 活动列表 HTTP 失败必须是 fail（绝不可被误记成幂等 already），并留痕到快照。
func TestQoderCheckinCampaignsHTTPErrorIsFail(t *testing.T) {
	t.Parallel()
	script := &qoderScript{
		quotaBody:   qoderQuotaBody,
		campStatus:  http.StatusServiceUnavailable,
		claimStatus: http.StatusServiceUnavailable,
	}
	server, upstream := newQoderCampaignServer(t, script)
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL)
	account := qoderCreditsTestAccount(25, nil)
	repo.accountsByID = map[int64]*Account{25: account}

	result := svc.CheckinForAccount(context.Background(), account)
	require.Equal(t, QoderCheckinStatusFail, result.Status)
	require.False(t, result.Success)
	require.Contains(t, result.Detail, "HTTP 503")
	snapshot, ok := repo.lastExtraValue(qoderCheckinExtraKey).(QoderCheckinSnapshot)
	require.True(t, ok, "失败也要落快照供列表显示失败态")
	require.Equal(t, QoderCheckinStatusFail, snapshot.Status)
	require.Equal(t, 0, upstream.postCount())
}

func TestQoderCheckinClaimHTTPErrorIsFail(t *testing.T) {
	t.Parallel()
	script := &qoderScript{
		quotaBody:    qoderQuotaBody,
		campaignBody: qoderCampaignsBody(qoderClaimStatusClaimable, qoderFutureEndAt(), 100),
		claimStatus:  http.StatusForbidden,
	}
	server, upstream := newQoderCampaignServer(t, script)
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL)
	account := qoderCreditsTestAccount(26, nil)
	repo.accountsByID = map[int64]*Account{26: account}

	result := svc.CheckinForAccount(context.Background(), account)
	require.Equal(t, QoderCheckinStatusFail, result.Status)
	require.Contains(t, result.Detail, "HTTP 403")
	require.Equal(t, 1, upstream.postCount())
	require.InDelta(t, 0.0, result.Credits, 1e-6, "claim 失败不得刷新余额")
}

// 2xx 但 status 非 CLAIMED：按成功处理但留痕（上游语义变化能被看见）。
func TestQoderCheckinUnexpectedClaimStatusKeepsOKWithDetail(t *testing.T) {
	t.Parallel()
	script := &qoderScript{
		quotaBody:    qoderQuotaBody,
		campaignBody: qoderCampaignsBody(qoderClaimStatusClaimable, qoderFutureEndAt(), 100),
		claimBody:    `{"grantId":"g-9","status":"GRANTED_IN_PROGRESS","replayed":false,"benefit":{"kind":"CREDITS","amount":100}}`,
	}
	server, upstream := newQoderCampaignServer(t, script)
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL)
	account := qoderCreditsTestAccount(27, nil)
	repo.accountsByID = map[int64]*Account{27: account}

	result := svc.CheckinForAccount(context.Background(), account)
	require.Equal(t, QoderCheckinStatusOK, result.Status)
	require.Contains(t, result.Detail, "unexpected claim status")
}

// 同一账号并发领取由 singleflight 合并：只打一次 claim。
func TestQoderCheckinConcurrentCallsShareSingleFlight(t *testing.T) {
	t.Parallel()
	script := &qoderScript{
		quotaBody:    qoderQuotaBody,
		campaignBody: qoderCampaignsBody(qoderClaimStatusClaimable, qoderFutureEndAt(), 100),
		claimBody:    qoderClaimFreshBody,
	}
	server, upstream := newQoderCampaignServer(t, script)
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL)
	account := qoderCreditsTestAccount(28, nil)
	repo.accountsByID = map[int64]*Account{28: account}

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := svc.CheckinForAccount(context.Background(), account)
			require.Contains(t, []string{QoderCheckinStatusOK, QoderCheckinStatusAlready}, res.Status)
		}()
	}
	wg.Wait()
	require.Equal(t, 1, upstream.postCount(), "并发领取必须合并成一次写请求")
}

// ---------------------------------------------------------------------------
// 快照合并 / 轮次归属 / 周期任务参数
// ---------------------------------------------------------------------------

func TestQoderCheckinSnapshotMergeKeepsSuccessOverLaterFailure(t *testing.T) {
	t.Parallel()
	round := qoderCampaignRoundDate(time.Now())
	existing := QoderCheckinSnapshot{Round: round, Status: QoderCheckinStatusOK}
	require.True(t, shouldKeepQoderCheckinSnapshot(&existing,
		QoderCheckinSnapshot{Round: round, Status: QoderCheckinStatusFail}))
	// 异轮 / 既有为失败 / 新结果成功：都要写入。
	require.False(t, shouldKeepQoderCheckinSnapshot(&existing,
		QoderCheckinSnapshot{Round: "2020-01-01", Status: QoderCheckinStatusFail}))
	require.False(t, shouldKeepQoderCheckinSnapshot(
		&QoderCheckinSnapshot{Round: round, Status: QoderCheckinStatusFail},
		QoderCheckinSnapshot{Round: round, Status: QoderCheckinStatusOK}))
	require.False(t, shouldKeepQoderCheckinSnapshot(nil,
		QoderCheckinSnapshot{Round: round, Status: QoderCheckinStatusFail}))

	// 端到端：本轮已成功 + 上游 5xx → 不覆盖写库。
	script := &qoderScript{quotaBody: qoderQuotaBody, campStatus: http.StatusServiceUnavailable}
	server, upstream := newQoderCampaignServer(t, script)
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL)
	account := qoderCreditsTestAccount(29, map[string]any{
		qoderCheckinExtraKey: map[string]any{"round": round, "status": QoderCheckinStatusOK},
	})
	repo.accountsByID = map[int64]*Account{29: account}

	result := svc.CheckinForAccount(context.Background(), account)
	require.Equal(t, QoderCheckinStatusFail, result.Status)
	require.Nil(t, repo.lastExtraValue(qoderCheckinExtraKey), "失败重试不得覆盖本轮已成功记录")
}

// 轮次口径：活动每天 10:00 开新一轮，0-10 点仍属前一天（用自然日会把已领显示成未领）。
func TestQoderCampaignRoundDateBoundaryAtTenOClock(t *testing.T) {
	t.Parallel()
	loc := timezone.Location()
	require.Equal(t, "2026-09-24", qoderCampaignRoundDate(time.Date(2026, 9, 24, 10, 0, 0, 0, loc)))
	require.Equal(t, "2026-09-23", qoderCampaignRoundDate(time.Date(2026, 9, 24, 9, 59, 59, 0, loc)))
	require.Equal(t, "2026-09-23", qoderCampaignRoundDate(time.Date(2026, 9, 24, 0, 0, 0, 0, loc)))
	// 月初 0 点回退不得跨年错月。
	require.Equal(t, "2026-12-31", qoderCampaignRoundDate(time.Date(2027, 1, 1, 8, 0, 0, 0, loc)))
}

func TestQoderCheckinStatusTodayIgnoresStaleRound(t *testing.T) {
	t.Parallel()
	fresh := qoderCreditsTestAccount(30, map[string]any{
		qoderCheckinExtraKey: map[string]any{
			"round":  qoderCampaignRoundDate(time.Now()),
			"status": QoderCheckinStatusAlready,
		},
	})
	require.Equal(t, QoderCheckinStatusAlready, qoderCheckinStatusToday(fresh))

	stale := qoderCreditsTestAccount(31, map[string]any{
		qoderCheckinExtraKey: map[string]any{"round": "2020-01-01", "status": QoderCheckinStatusOK},
	})
	require.Empty(t, qoderCheckinStatusToday(stale))

	require.Empty(t, qoderCheckinStatusToday(qoderCreditsTestAccount(32, nil)))
	require.Empty(t, qoderCheckinStatusToday(nil))
}

func TestQoderCheckinHoursNormalization(t *testing.T) {
	t.Parallel()
	require.Equal(t, []int{10, 21}, qoderCheckinHours(nil), "缺省配置用默认 10/21")
	require.Equal(t, []int{10, 21}, qoderCheckinHours(&config.Config{}), "空列表回落默认")
	cfg := &config.Config{}
	cfg.Gateway.Qoder.CheckinHours = []int{21, 10, 21, 25, -1}
	require.Equal(t, []int{10, 21}, qoderCheckinHours(cfg), "越界值丢弃、重复去重、升序")
	all := &config.Config{}
	all.Gateway.Qoder.CheckinHours = []int{24, -3}
	require.Empty(t, qoderCheckinHours(all), "全部非法视为关闭")
}

func TestNextQoderCheckinFirePicksEarliestFutureHour(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 8, 30, 0, 0, time.Local)
	require.Equal(t, 10, nextQoderCheckinFire(now, []int{10, 21}).Hour())
	require.Equal(t, 21, nextQoderCheckinFire(
		time.Date(2026, 9, 24, 11, 0, 0, 0, time.Local), []int{10, 21}).Hour())
	// 过了最后一个触发点 → 次日最早点。
	next := nextQoderCheckinFire(time.Date(2026, 9, 24, 23, 0, 0, 0, time.Local), []int{10, 21})
	require.Equal(t, 10, next.Hour())
	require.Equal(t, 25, next.Day(), "23 点后应落到次日 10 点")
	require.Equal(t, time.September, next.Month())
	// 单日列表：当天已过则次日同点。
	require.Equal(t, 9, nextQoderCheckinFire(time.Date(2026, 9, 24, 9, 0, 0, 0, time.Local), []int{9}).Hour())
	require.Equal(t, 25, nextQoderCheckinFire(time.Date(2026, 9, 24, 9, 0, 1, 0, time.Local), []int{9}).Day())
}

// Start 必须尊重开关与非法小时列表（关闭时不起协程）。
func TestQoderCheckinServiceStartRespectsConfig(t *testing.T) {
	t.Parallel()
	repo := &qoderCampaignFakeRepo{}
	credits := newQoderCreditsTestService(repo, &qoderFakeUpstream{client: http.DefaultClient}, "http://127.0.0.1:1")

	disabled := &config.Config{}
	disabled.Gateway.Qoder.CheckinEnabled = false
	svc := NewQoderCheckinService(credits, repo, disabled)
	svc.Start()
	svc.Stop() // 幂等，不得阻塞

	noHours := &config.Config{}
	noHours.Gateway.Qoder.CheckinEnabled = true
	noHours.Gateway.Qoder.CheckinHours = []int{99}
	svc2 := NewQoderCheckinService(credits, repo, noHours)
	svc2.Start()
	svc2.Stop()

	// 依赖缺失 / nil 接收者：不得 panic。
	nilSvc := &QoderCheckinService{}
	nilSvc.Start()
	nilSvc.Stop()
	var zero *QoderCheckinService
	zero.Stop()
}

// runOnce 枚举账号：禁用/无凭据/非 Qoder 账号跳过且零上游调用，正常账号被领取。
func TestQoderCheckinServiceRunOnceFiltersAccounts(t *testing.T) {
	script := &qoderScript{
		quotaBody:    qoderQuotaBody,
		campaignBody: qoderCampaignsBody(qoderClaimStatusClaimable, qoderFutureEndAt(), 100),
		claimBody:    qoderClaimFreshBody,
	}
	server, upstream := newQoderCampaignServer(t, script)
	repo := &qoderCampaignFakeRepo{}
	svc := newQoderCreditsTestService(repo, upstream, server.URL)

	enabled := qoderCreditsTestAccount(41, nil)
	disabled := qoderCreditsTestAccount(42, nil)
	disabled.Status = "inactive"
	credLess := qoderCreditsTestAccount(43, nil)
	credLess.Credentials = map[string]any{"realm": "cn"}
	other := qoderCreditsTestAccount(44, nil)
	other.Platform = PlatformWorkbuddy

	repo.accounts = []Account{*enabled, *disabled, *credLess, *other}
	repo.accountsByID = map[int64]*Account{41: enabled, 42: disabled, 43: credLess, 44: other}

	checkin := NewQoderCheckinService(svc, repo, &config.Config{})
	t.Cleanup(checkin.Stop)
	restore := qoderCheckinAccountDelay
	qoderCheckinAccountDelay = 0 // 测试免等待
	t.Cleanup(func() { qoderCheckinAccountDelay = restore })

	checkin.runOnce()

	require.Equal(t, 1, upstream.postCount(), "仅启用且有凭据的 Qoder 账号被领取")
	require.Equal(t, []string{
		"/sash/api/v1/me/campaigns",
		"/sash/api/v1/me/campaigns/01a0cd40-ea93-75c3-be36-23c22a84e619/claim",
		"/api/v2/quota/usage",
	}, upstream.paths())
}

// ---------------------------------------------------------------------------
// 纯函数：活动条目挑选与额度解析
// ---------------------------------------------------------------------------

// 多活动并存时（每日 100 + 节日礼包），一个已领条目不得遮住另一个可领条目。
func TestQoderApplyCampaignsPrefersClaimableAmongMixed(t *testing.T) {
	t.Parallel()
	now := time.Now().Unix()
	body := fmt.Sprintf(`{"claimable":true,"campaigns":[
      {"campaignId":"c1","campaignKey":"act-claimed","actionType":"CLAIM_BENEFIT",
       "endAt":%d,"claimStatus":"CLAIMED","benefit":{"kind":"CREDITS","amount":50}},
      {"campaignId":"c2","campaignKey":"act-daily","actionType":"CLAIM_BENEFIT",
       "endAt":%d,"claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":100}},
      {"campaignId":"c3","campaignKey":"act-view","actionType":"VIEW_DETAILS",
       "endAt":%d,"claimStatus":"CLAIMABLE"}
    ]}`, now+3600, now+3600, now+3600)
	var resp qoderCampaignsResp
	require.NoError(t, json.Unmarshal([]byte(body), &resp))

	svc := &QoderCreditsService{}
	result := &QoderCreditsResult{}
	svc.applyQoderCampaigns(result, &resp)
	require.True(t, result.Claimable)
	require.Equal(t, "act-daily", result.ClaimCampaignKey)
	require.Equal(t, int64(100), result.ClaimAmount)
	require.Empty(t, result.TodayClaimStatus)
}

// 已领 + 无可领 + 存在已过窗条目：不得把过期条目当可领。
func TestQoderApplyCampaignsIgnoresExpiredAndMarksAlready(t *testing.T) {
	t.Parallel()
	now := time.Now().Unix()
	body := fmt.Sprintf(`{"claimable":false,"campaigns":[
      {"campaignId":"c1","campaignKey":"act-expired","actionType":"CLAIM_BENEFIT",
       "endAt":%d,"claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":100}},
      {"campaignId":"c2","campaignKey":"act-claimed","actionType":"CLAIM_BENEFIT",
       "endAt":%d,"claimStatus":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}
    ]}`, now-10, now+3600)
	var resp qoderCampaignsResp
	require.NoError(t, json.Unmarshal([]byte(body), &resp))

	result := &QoderCreditsResult{}
	(&QoderCreditsService{}).applyQoderCampaigns(result, &resp)
	require.False(t, result.Claimable)
	require.Equal(t, QoderCheckinStatusAlready, result.TodayClaimStatus)
	require.Equal(t, "act-claimed", result.ClaimCampaignKey)
}

// 无下发条目（企业版/设备级规则）：claimable 取顶层字段、状态留空。
func TestQoderApplyCampaignsWithoutEntries(t *testing.T) {
	t.Parallel()
	var resp qoderCampaignsResp
	require.NoError(t, json.Unmarshal([]byte(qoderEmptyCampaignsBody), &resp))
	result := &QoderCreditsResult{}
	(&QoderCreditsService{}).applyQoderCampaigns(result, &resp)
	require.False(t, result.Claimable)
	require.Empty(t, result.TodayClaimStatus)
	require.Equal(t, int64(0), result.ClaimAmount)
}

func TestQoderBenefitHelpers(t *testing.T) {
	t.Parallel()
	body := `{"campaigns":[{"campaignId":"c","campaignKey":"k","actionType":"CLAIM_BENEFIT",
      "claimStatus":"CLAIMED","endAt":0,
      "benefit":{"kind":"CREDITS","amount":100,"validity":{"mode":"RELATIVE_DAYS","days":30}}}]}`
	var resp qoderCampaignsResp
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	require.Len(t, resp.Campaigns, 1)

	now := time.Date(2026, 9, 24, 2, 22, 58, 0, time.UTC).Unix()
	require.Equal(t, "2026-10-24T02:22:58Z", benefitExpiryText(&resp.Campaigns[0], now))
	require.Equal(t, int64(100), benefitAmount(&resp.Campaigns[0]))
	require.Empty(t, benefitExpiryText(nil, now))
	require.Equal(t, int64(0), benefitAmount(nil))
}

func TestQoderCampaignRoundMatchesFrontendContract(t *testing.T) {
	t.Parallel()
	// 前端 currentRound() 用「本地小时 <10 归前一天」，后端必须同口径。
	require.Equal(t,
		"2026-09-23",
		qoderCampaignRoundDate(time.Date(2026, 9, 24, 9, 30, 0, 0, timezone.Location())))
}
