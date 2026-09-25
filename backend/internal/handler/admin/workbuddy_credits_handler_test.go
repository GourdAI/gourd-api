//go:build unit

package admin

// workbuddy_credits_handler_test.go WorkBuddy 积分查询/签到 handler 的全链路单测：
// handler → service → 假上游（httptest 或自定义 HTTPUpstream）→ 快照落库，
// 覆盖响应信封形状、参数校验、余额聚合、签到幂等、双域路径与会话不泄露凭据。

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
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// workbuddyCreditsHandlerRepo 账号仓库替身：按 ID 返回预置 WorkBuddy 账号，
// UpdateExtra 记录快照写入（供断言 handler 全链路的落库副作用）。
type workbuddyCreditsHandlerRepo struct {
	service.AccountRepository
	mu      sync.Mutex
	account *service.Account
	updates map[int64]map[string]any
}

func (r *workbuddyCreditsHandlerRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.account != nil && r.account.ID == id {
		return r.account, nil
	}
	return nil, service.ErrAccountNotFound
}

func (r *workbuddyCreditsHandlerRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updates == nil {
		r.updates = make(map[int64]map[string]any)
	}
	// merge 语义（与生产 UpdateExtra 一致）：同一账号多次写入不同快照键时保留既有键。
	merged, ok := r.updates[id]
	if !ok {
		merged = make(map[string]any)
		r.updates[id] = merged
	}
	for k, v := range updates {
		merged[k] = v
	}
	return nil
}

func (r *workbuddyCreditsHandlerRepo) snapshot(id int64) (map[string]any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	extra, ok := r.updates[id]
	if !ok {
		return nil, false
	}
	cp := make(map[string]any, len(extra))
	for k, v := range extra {
		cp[k] = v
	}
	return cp, true
}

// workbuddyCreditsHandlerUpstream 假上游：把请求转发给 httptest 服务器并记录请求，
// 覆盖 billing 局（积分查询/签到）与 token 刷新端点。
type workbuddyCreditsHandlerUpstream struct {
	mu       sync.Mutex
	client   *http.Client
	requests []*http.Request
	bodies   []string
}

func (u *workbuddyCreditsHandlerUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	var body string
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		body = string(raw)
		req.Body = io.NopCloser(strings.NewReader(body))
	}
	u.mu.Lock()
	u.requests = append(u.requests, req)
	u.bodies = append(u.bodies, body)
	u.mu.Unlock()
	return u.client.Do(req)
}

func (u *workbuddyCreditsHandlerUpstream) DoWithTLS(
	req *http.Request,
	proxyURL string,
	accountID int64,
	accountConcurrency int,
	_ *tlsfingerprint.Profile,
) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func (u *workbuddyCreditsHandlerUpstream) requestPaths() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, 0, len(u.requests))
	for _, req := range u.requests {
		out = append(out, req.URL.Path)
	}
	return out
}

// newWorkBuddyCreditsTestService 构造指向 fake upstream 的积分服务（URL 策略放行 httptest）。
func newWorkBuddyCreditsTestService(repo service.AccountRepository, upstream service.HTTPUpstream) *service.WorkBuddyCreditsService {
	return service.NewWorkBuddyCreditsService(repo, nil, upstream, &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
		},
	})
}

// workbuddyCreditsTestAccount 构造 CN realm 的 WorkBuddy 账号（billing 域指向假上游）。
func workbuddyCreditsTestAccount(id int64, serverURL string) *service.Account {
	return &service.Account{
		ID:          id,
		Platform:    service.PlatformWorkbuddy,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":     "wb-access-token",
			"realm":            "cn",
			"base_url":         serverURL,
			"billing_base_url": serverURL,
			"uid":              "u-1",
			"enterprise_id":    "e-1",
		},
	}
}

// workbuddyCreditsTestAccountPersonal 与 workbuddyCreditsTestAccount 同形但不带
// enterprise_id：企业成员账号无个人签到体系（会被签到 D5 门控直接 skipped），
// 因此签到链路的用例必须用个人号（X-Enterprise-Id 透传契约仍由查询类用例覆盖）。
func workbuddyCreditsTestAccountPersonal(id int64, serverURL string) *service.Account {
	account := workbuddyCreditsTestAccount(id, serverURL)
	delete(account.Credentials, "enterprise_id")
	return account
}

const workbuddyHandlerResourceBody = `{
  "code": 0, "msg": "ok",
  "data": {"Response": {"Data": {"TotalDosage": 10000, "Accounts": [
    {"PackageName": "基础包", "CycleEndTime": "2027-02-14 00:00:00",
     "CapacitySize": 5000, "CapacityRemain": 1200, "CapacityUsed": 3800,
     "CycleCapacitySize": 5000, "CycleCapacityRemain": 1200, "CycleCapacityUsed": 3800},
    {"PackageName": "奖励包", "CycleEndTime": "2027-03-01 00:00:00",
     "CapacitySize": 500, "CapacityRemain": 300, "CapacityUsed": 200,
     "CycleCapacitySize": 500, "CycleCapacityRemain": 300, "CycleCapacityUsed": 200}
  ]}}}
}`

// newWorkBuddyCreditsHandlerRouter 组装 gin 路由（与生产注册路径一致）。
func newWorkBuddyCreditsHandlerRouter(h *WorkBuddyCreditsHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/admin/workbuddy/accounts/:id/credits", h.QueryCredits)
	router.POST("/api/v1/admin/workbuddy/accounts/:id/checkin", h.Checkin)
	router.POST("/api/v1/admin/workbuddy/accounts/:id/activity", h.RunActivity)
	router.POST("/api/v1/admin/workbuddy/accounts/:id/travel", h.RunTravel)
	return router
}

// TestWorkBuddyCreditsHandlerQueryCreditsEndToEnd 覆盖全链路：
// GET credits → 200 信封，data 含聚合后的 remain/used/size/packages/realm/fetched_at，
// 且快照写入 extra（workbuddy_credits），响应体不泄露 access_token。
func TestWorkBuddyCreditsHandlerQueryCreditsEndToEnd(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/billing/meter/get-user-resource":
			_, _ = w.Write([]byte(workbuddyHandlerResourceBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	repo := &workbuddyCreditsHandlerRepo{account: workbuddyCreditsTestAccount(101, server.URL)}
	upstream := &workbuddyCreditsHandlerUpstream{client: server.Client()}
	svc := newWorkBuddyCreditsTestService(repo, upstream)
	handler := NewWorkBuddyCreditsHandler(svc, nil)

	router := newWorkBuddyCreditsHandlerRouter(handler)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workbuddy/accounts/101/credits", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.EqualValues(t, 0, envelope["code"])
	data, ok := envelope["data"].(map[string]any)
	require.True(t, ok, "envelope data must be an object: %s", rec.Body.String())
	require.EqualValues(t, true, data["success"])
	require.EqualValues(t, 1500, data["remain"], "1200 + 300")
	require.EqualValues(t, 8500, data["used"], "TotalDosage 10000 > size 5500 → used = 10000 - 1500")
	require.EqualValues(t, 10000, data["size"], "TotalDosage 10000 更大取宽口径")
	require.EqualValues(t, 2, data["packs"])
	require.Equal(t, "cn", data["realm"])
	packages, ok := data["packages"].([]any)
	require.True(t, ok)
	require.Len(t, packages, 2)
	require.NotEmpty(t, data["fetched_at"])
	// 凭据不泄露：响应体不得包含 access_token 原文。
	require.NotContains(t, rec.Body.String(), "wb-access-token")

	// 快照落库：extra["workbuddy_credits"] 写入（供列表页直接渲染）。
	extra, ok := repo.snapshot(101)
	require.True(t, ok, "credits snapshot must be persisted to account extra")
	require.Contains(t, extra, "workbuddy_credits")

	// 上游路径与鉴权头校验（全链路验证转发形态）。
	require.Equal(t, []string{"/v2/billing/meter/get-user-resource"}, upstream.requestPaths())
	upstream.mu.Lock()
	require.Equal(t, "Bearer wb-access-token", upstream.requests[0].Header.Get("Authorization"))
	require.Equal(t, "u-1", upstream.requests[0].Header.Get("X-User-Id"))
	require.Equal(t, "e-1", upstream.requests[0].Header.Get("X-Enterprise-Id"))
	require.Contains(t, upstream.bodies[0], `"ProductCode":"p_tcaca"`)
	upstream.mu.Unlock()
}

// TestWorkBuddyCreditsHandlerQueryCreditsInvalidIDRejected 覆盖路径参数校验：非数字为 400。
func TestWorkBuddyCreditsHandlerQueryCreditsInvalidIDRejected(t *testing.T) {
	t.Parallel()
	handler := NewWorkBuddyCreditsHandler(nil, nil)
	router := newWorkBuddyCreditsHandlerRouter(handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workbuddy/accounts/abc/credits", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestWorkBuddyCreditsHandlerServiceNilRejected 覆盖服务未启用时的防御（400 而非 panic）。
func TestWorkBuddyCreditsHandlerServiceNilRejected(t *testing.T) {
	t.Parallel()
	// 注意：handler 为非 nil 但 service 为 nil 的构造在 wire 中不会出现；
	// 这里用零值 handler 验证防御分支（h == nil 时也必须安全返回）。
	var handler *WorkBuddyCreditsHandler
	router := newWorkBuddyCreditsHandlerRouter(handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workbuddy/accounts/1/credits", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/workbuddy/accounts/1/checkin", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/workbuddy/accounts/1/activity", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/workbuddy/accounts/1/travel", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestWorkBuddyCreditsHandlerTasksServiceNilRejected 覆盖任务服务未启用（handler 非 nil、
// tasksService 为 nil）时的防御：两个新端点均 400 而非 panic。
func TestWorkBuddyCreditsHandlerTasksServiceNilRejected(t *testing.T) {
	t.Parallel()
	repo := &workbuddyCreditsHandlerRepo{account: workbuddyCreditsTestAccount(107, "http://127.0.0.1:1")}
	upstream := &workbuddyCreditsHandlerUpstream{client: http.DefaultClient}
	svc := newWorkBuddyCreditsTestService(repo, upstream)
	handler := NewWorkBuddyCreditsHandler(svc, nil)
	router := newWorkBuddyCreditsHandlerRouter(handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/workbuddy/accounts/107/activity", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/workbuddy/accounts/107/travel", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// newWorkBuddyTasksTestHandlerService 构造指向假上游的任务服务（仅放开 httptest 的 http 出站校验）。
func newWorkBuddyTasksTestHandlerService(repo service.AccountRepository, upstream service.HTTPUpstream) *service.WorkBuddyTasksService {
	cfg := &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
		},
	}
	return service.NewWorkBuddyTasksService(newWorkBuddyCreditsTestService(repo, upstream), repo, cfg)
}

// TestWorkBuddyCreditsHandlerRunActivityEndToEnd 覆盖活跃上报手动入口全链路：
// POST activity → 200 直出 service 回执（realm/reports/success/ran_at）；
// 上游收到 /v2/report 与后续 growth 链端点；快照写入 extra（workbuddy_activity）；
// 响应体不泄露 access_token。
func TestWorkBuddyCreditsHandlerRunActivityEndToEnd(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/report":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case "/activity/growth/streak":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"streak":{"days":8},"makeup_cards":{"balance":1,"max":5},"redemption_status":{"tier_7d_status":"claimed","tier_14d_status":"unclaimed","tier_28d_status":"unclaimed","remaining_days":6,"tiers":[{"tier":"7d","days":7,"credit":100},{"tier":"14d","days":14,"credit":300},{"tier":"28d","days":28,"credit":800}]}}}`))
		case "/activity/growth/heatmap":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"cells":[]}}`))
		case "/billing/meter/claim-gift", "/billing/meter/claim-compensation":
			_, _ = w.Write([]byte(`{"code":14001,"msg":"already claimed"}`))
		case "/activity/growth/buddy/info":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"buddy":{"id":1,"name":"小橘"}}}`))
		case "/activity/growth/lottery/chances":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"balance":0}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	repo := &workbuddyCreditsHandlerRepo{account: workbuddyCreditsTestAccount(108, server.URL)}
	upstream := &workbuddyCreditsHandlerUpstream{client: server.Client()}
	svc := newWorkBuddyTasksTestHandlerService(repo, upstream)
	handler := NewWorkBuddyCreditsHandler(nil, svc)
	router := newWorkBuddyCreditsHandlerRouter(handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/workbuddy/accounts/108/activity", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	data, ok := envelope["data"].(map[string]any)
	require.True(t, ok, "envelope data must be an object: %s", rec.Body.String())
	require.EqualValues(t, true, data["success"])
	require.Equal(t, "cn", data["realm"])
	require.EqualValues(t, 1, data["reports"])
	require.EqualValues(t, 1, data["reports_requested"])
	require.NotNil(t, data["ran_at"])
	require.NotContains(t, rec.Body.String(), "wb-access-token")

	paths := upstream.requestPaths()
	require.NotEmpty(t, paths)
	require.Equal(t, "/v2/report", paths[0], "活跃上报必须首打 /v2/report")

	extra, ok := repo.snapshot(108)
	require.True(t, ok, "activity snapshot must be persisted to account extra")
	require.Contains(t, extra, "workbuddy_activity")
}

// TestWorkBuddyCreditsHandlerRunTravelEndToEnd 覆盖猫猫旅行手动入口全链路：
// POST travel → 200 直出 service 回执（realm/buddy_name/state/action/ran_at）；
// 上游收到 buddy/info 与 travel/status；快照写入 extra（workbuddy_travel）。
func TestWorkBuddyCreditsHandlerRunTravelEndToEnd(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/activity/growth/buddy/info":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"buddy":{"id":9,"name":"奶牛"}}}`))
		case "/activity/growth/buddy/travel/status":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"state":"arrived","record_id":77,"reward_credit":120,"daily_limit_reached":false}}`))
		case "/activity/growth/buddy/travel/claim":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"reward_credit":120}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	repo := &workbuddyCreditsHandlerRepo{account: workbuddyCreditsTestAccount(109, server.URL)}
	upstream := &workbuddyCreditsHandlerUpstream{client: server.Client()}
	svc := newWorkBuddyTasksTestHandlerService(repo, upstream)
	handler := NewWorkBuddyCreditsHandler(nil, svc)
	router := newWorkBuddyCreditsHandlerRouter(handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/workbuddy/accounts/109/travel", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	data, ok := envelope["data"].(map[string]any)
	require.True(t, ok, "envelope data must be an object: %s", rec.Body.String())
	require.Equal(t, "cn", data["realm"])
	require.Equal(t, "奶牛", data["buddy_name"])
	require.Equal(t, "arrived", data["state"])
	require.Equal(t, "claim", data["action"])
	require.EqualValues(t, true, data["success"])
	require.EqualValues(t, 120, data["reward_credit"])
	require.NotNil(t, data["ran_at"])

	paths := upstream.requestPaths()
	require.Contains(t, paths, "/activity/growth/buddy/info")
	require.Contains(t, paths, "/activity/growth/buddy/travel/status")
	require.Contains(t, paths, "/activity/growth/buddy/travel/claim")

	extra, ok := repo.snapshot(109)
	require.True(t, ok, "travel snapshot must be persisted to account extra")
	require.Contains(t, extra, "workbuddy_travel")
}

// TestWorkBuddyCreditsHandlerQueryCreditsAccountNotFound 覆盖账号不存在：404（infraerrors 透传）。
func TestWorkBuddyCreditsHandlerQueryCreditsAccountNotFound(t *testing.T) {
	t.Parallel()
	repo := &workbuddyCreditsHandlerRepo{account: workbuddyCreditsTestAccount(102, "http://127.0.0.1:1")}
	upstream := &workbuddyCreditsHandlerUpstream{client: http.DefaultClient}
	svc := newWorkBuddyCreditsTestService(repo, upstream)
	handler := NewWorkBuddyCreditsHandler(svc, nil)
	router := newWorkBuddyCreditsHandlerRouter(handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workbuddy/accounts/999/credits", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestWorkBuddyCreditsHandlerQueryCreditsUpstreamFailureIsResultNotHTTPError 覆盖上游失败语义：
// 探测/网络失败时仍 200 + success=false + error 文案（管理端不把观测失败当请求失败）。
func TestWorkBuddyCreditsHandlerQueryCreditsUpstreamFailureIsResultNotHTTPError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":403,"msg":"forbidden"}`))
	}))
	defer server.Close()

	repo := &workbuddyCreditsHandlerRepo{account: workbuddyCreditsTestAccount(103, server.URL)}
	upstream := &workbuddyCreditsHandlerUpstream{client: server.Client()}
	svc := newWorkBuddyCreditsTestService(repo, upstream)
	handler := NewWorkBuddyCreditsHandler(svc, nil)
	router := newWorkBuddyCreditsHandlerRouter(handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workbuddy/accounts/103/credits", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	data := envelope["data"].(map[string]any)
	require.EqualValues(t, false, data["success"])
	require.NotEmpty(t, data["error"])
	// 失败不落快照。
	_, persisted := repo.snapshot(103)
	require.False(t, persisted)
}

// TestWorkBuddyCreditsHandlerCheckinEndToEnd 覆盖签到全链路：
// POST checkin → 200 + status=ok + credits 回填（签到后自动刷新积分），
// 上游收到 daily-checkin 与 get-user-resource 各一次，快照含 workbuddy_checkin。
func TestWorkBuddyCreditsHandlerCheckinEndToEnd(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case "/v2/billing/meter/get-user-resource":
			_, _ = w.Write([]byte(workbuddyHandlerResourceBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	repo := &workbuddyCreditsHandlerRepo{account: workbuddyCreditsTestAccountPersonal(104, server.URL)}
	upstream := &workbuddyCreditsHandlerUpstream{client: server.Client()}
	svc := newWorkBuddyCreditsTestService(repo, upstream)
	handler := NewWorkBuddyCreditsHandler(svc, nil)
	router := newWorkBuddyCreditsHandlerRouter(handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/workbuddy/accounts/104/checkin", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	data := envelope["data"].(map[string]any)
	require.EqualValues(t, true, data["success"])
	require.Equal(t, "ok", data["status"])
	require.EqualValues(t, 1500, data["credits"])
	require.Equal(t, "cn", data["realm"])

	require.Equal(t, []string{"/v2/billing/meter/daily-checkin", "/v2/billing/meter/get-user-resource"}, upstream.requestPaths())
	extra, ok := repo.snapshot(104)
	require.True(t, ok)
	require.Contains(t, extra, "workbuddy_checkin")
	require.Contains(t, extra, "workbuddy_credits")
}

// TestWorkBuddyCreditsHandlerCheckinAlreadyIdempotent 覆盖「今天已签到」幂等语义：
// 上游返回已签到业务码 → 200 + status=already + success=true，且不含误导性 detail。
func TestWorkBuddyCreditsHandlerCheckinAlreadyIdempotent(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/billing/meter/daily-checkin" {
			_, _ = w.Write([]byte(`{"code":14001,"msg":"今天已签到"}`))
			return
		}
		_, _ = w.Write([]byte(workbuddyHandlerResourceBody))
	}))
	defer server.Close()

	repo := &workbuddyCreditsHandlerRepo{account: workbuddyCreditsTestAccountPersonal(105, server.URL)}
	upstream := &workbuddyCreditsHandlerUpstream{client: server.Client()}
	svc := newWorkBuddyCreditsTestService(repo, upstream)
	handler := NewWorkBuddyCreditsHandler(svc, nil)
	router := newWorkBuddyCreditsHandlerRouter(handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/workbuddy/accounts/105/checkin", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	data := envelope["data"].(map[string]any)
	require.EqualValues(t, true, data["success"])
	require.Equal(t, "already", data["status"])
	require.NotContains(t, data, "detail")
	// 「已签到」也刷新积分（余额独立于签到结果）。
	require.EqualValues(t, 1500, data["credits"])
}

// TestWorkBuddyCreditsHandlerCheckinGlobalRealmSkipped 覆盖国际版门控：
// global 账号签到返回 skipped，且不发起任何上游调用（避免风控）。
func TestWorkBuddyCreditsHandlerCheckinGlobalRealmSkipped(t *testing.T) {
	t.Parallel()
	repo := &workbuddyCreditsHandlerRepo{}
	account := workbuddyCreditsTestAccount(106, "http://127.0.0.1:1")
	account.Credentials["realm"] = "global"
	repo.account = account
	upstream := &workbuddyCreditsHandlerUpstream{client: http.DefaultClient}
	svc := newWorkBuddyCreditsTestService(repo, upstream)
	handler := NewWorkBuddyCreditsHandler(svc, nil)
	router := newWorkBuddyCreditsHandlerRouter(handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/workbuddy/accounts/106/checkin", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	data := envelope["data"].(map[string]any)
	require.Equal(t, "skipped", data["status"])
	require.EqualValues(t, false, data["success"])
	require.Empty(t, upstream.requestPaths(), "global realm must not hit upstream for check-in")
}

// TestWorkBuddyCreditsHandlerCheckinEnterpriseSkipped 覆盖企业成员门控（D5）：
// CN + 持 enterprise_id → 签到 skipped，不发任何上游调用。
func TestWorkBuddyCreditsHandlerCheckinEnterpriseSkipped(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"Response":{"Data":{"TotalCount":0,"TotalDosage":0,"Accounts":null}}}}`))
	}))
	defer server.Close()

	repo := &workbuddyCreditsHandlerRepo{account: workbuddyCreditsTestAccount(110, server.URL)}
	upstream := &workbuddyCreditsHandlerUpstream{client: server.Client()}
	svc := newWorkBuddyCreditsTestService(repo, upstream)
	handler := NewWorkBuddyCreditsHandler(svc, nil)
	router := newWorkBuddyCreditsHandlerRouter(handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/workbuddy/accounts/110/checkin", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	data := envelope["data"].(map[string]any)
	require.Equal(t, "skipped", data["status"])
	require.EqualValues(t, false, data["success"])
	require.Contains(t, data["detail"], "enterprise")
	require.Empty(t, upstream.requestPaths(), "企业号不得发任何 billing 上游调用")
}

// TestWorkBuddyCreditsHandlerQueryCreditsEnterpriseNotApplicable 覆盖管理端 JSON 契约：
// 企业号空池时 success=true + not_applicable=true + enterprise=true（前端据此渲染
// 「由企业后台管理」而不是「剩余 0」）。
func TestWorkBuddyCreditsHandlerQueryCreditsEnterpriseNotApplicable(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"Response":{"Data":{"TotalCount":0,"TotalDosage":0,"Accounts":null}}}}`))
	}))
	defer server.Close()

	repo := &workbuddyCreditsHandlerRepo{account: workbuddyCreditsTestAccount(111, server.URL)}
	upstream := &workbuddyCreditsHandlerUpstream{client: server.Client()}
	svc := newWorkBuddyCreditsTestService(repo, upstream)
	handler := NewWorkBuddyCreditsHandler(svc, nil)
	router := newWorkBuddyCreditsHandlerRouter(handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workbuddy/accounts/111/credits", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	data := envelope["data"].(map[string]any)
	require.EqualValues(t, true, data["success"])
	require.EqualValues(t, true, data["not_applicable"], "空池企业号必须带 not_applicable")
	require.EqualValues(t, true, data["enterprise"])
	require.EqualValues(t, float64(0), data["packs"])
}
