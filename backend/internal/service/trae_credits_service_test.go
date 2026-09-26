//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

// trae_credits_service_test.go 是本平台最核心的行为锁：UG 域（api.trae.cn）HTTP 恒 200、
// 成败只看 body code；签到状态机（0/9095/9074/1001/9004）；签到必须回查 checked_in 才算成功；
// 已 checked_in 不再发 claim；积分聚合跳过过期权益包；快照写入 extra 且当日成功不被后续失败覆盖；
// refreshToken 轮换写回；毫秒过期时间归一为秒；singleflight 合并同账号并发。

// traeCreditsFakeRepo 记录 extra 快照写入与凭据持久化的账号仓库替身。
type traeCreditsFakeRepo struct {
	mockAccountRepoForGemini
	mu               sync.Mutex
	extraCalls       int
	extraKeys        []string
	lastExtraID      int64
	extras           map[string]any
	credPersists     int
	updateCalls      int
	lastCredentials  map[string]any
	listByPlatformOK bool
}

func (r *traeCreditsFakeRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.extras == nil {
		r.extras = map[string]any{}
	}
	r.extraCalls++
	r.lastExtraID = id
	var keys []string
	for k, v := range updates {
		r.extras[k] = v
		keys = append(keys, k)
	}
	r.extraKeys = append(r.extraKeys, strings.Join(keys, ","))
	return nil
}

func (r *traeCreditsFakeRepo) UpdateCredentials(_ context.Context, id int64, credentials map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.credPersists++
	r.lastCredentials = shallowCopyMap(credentials)
	if acc, ok := r.accountsByID[id]; ok && acc != nil {
		acc.Credentials = shallowCopyMap(credentials)
	}
	return nil
}

func (r *traeCreditsFakeRepo) Update(_ context.Context, account *Account) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updateCalls++
	if account != nil {
		r.accountsByID[account.ID] = account
	}
	return nil
}

func (r *traeCreditsFakeRepo) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Account
	for _, acc := range r.accountsByID {
		if acc != nil && acc.Platform == platform {
			out = append(out, *acc)
		}
	}
	return out, nil
}

func (r *traeCreditsFakeRepo) extraSnapshot(key string) (any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.extras[key]
	return v, ok
}

func (r *traeCreditsFakeRepo) counts() (extraCalls int, credPersists int, updateCalls int, keys []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.extraCalls, r.credPersists, r.updateCalls, append([]string(nil), r.extraKeys...)
}

func newTraeCreditsTestService(upstream HTTPUpstream, repo AccountRepository) *TraeCreditsService {
	return &TraeCreditsService{
		accountRepo:  repo,
		httpUpstream: upstream,
		cfg: &config.Config{
			Security: config.SecurityConfig{
				// 测试用 httptest（http://127.0.0.1）：关闭白名单并允许 http 出站。
				URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
			},
		},
	}
}

// traeCreditsAccount 三域全部指向假上游（聊天/UG/OAuth 是三个真实主机，测试里同源便于断言路径序列）。
func traeCreditsAccount(id int64, serverURL string, creds map[string]any) *Account {
	if creds == nil {
		creds = map[string]any{}
	}
	creds["base_url"] = serverURL
	creds["billing_base_url"] = serverURL
	creds["oauth_base_url"] = serverURL
	if _, ok := creds["realm"]; !ok {
		creds["realm"] = "cn"
	}
	if _, ok := creds["access_token"]; !ok {
		creds["access_token"] = "at-test"
	}
	if _, ok := creds["uid"]; !ok {
		creds["uid"] = "7000000000000000001"
	}
	return &Account{ID: id, Platform: PlatformTrae, Type: AccountTypeAPIKey, Status: StatusActive, Concurrency: 1, Credentials: creds}
}

// traeUpstreamSpy 假上游：记录路径/请求头/请求体，按注入表返回响应。
type traeUpstreamSpy struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
	paths    []string
}

func (s *traeUpstreamSpy) record(r *http.Request, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Clone(context.Background()))
	s.bodies = append(s.bodies, body)
	s.paths = append(s.paths, r.URL.Path)
}

func (s *traeUpstreamSpy) pathLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func (s *traeUpstreamSpy) bodyLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

func (s *traeUpstreamSpy) countOf(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.paths {
		if p == path {
			n++
		}
	}
	return n
}

func writeTraeJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

func traeStatusBody(checkedIn, enable bool, credits float64) string {
	return fmt.Sprintf(`{"code":0,"message":"success","checked_in":%v,"enable":%v,"credits":%g}`, checkedIn, enable, credits)
}

// traePacksBody 用实测样本：两个有效包 (2000/200.8772) 与 (2000/1595.5964) → remain 2203.5264。
func traePacksBody(packs ...map[string]any) string {
	raw, err := json.Marshal(map[string]any{
		"code": 0, "message": "success",
		"data": map[string]any{"user_entitlement_pack_list": packs},
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func traePack(id string, limit, used float64, endTime int64) map[string]any {
	return map[string]any{
		"entitlement_id": id,
		"entitlement_base_info": map[string]any{
			"quota":    map[string]any{"credits_limit": limit},
			"end_time": endTime,
		},
		"usage": map[string]any{"credits_amount": used},
	}
}

func TestTraeQueryCreditsAggregatesPacksAndPersistsSnapshot(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	future := time.Now().AddDate(0, 0, 20).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeCheckinStatusPath:
			writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 2203.5264))
		case traeEntUsagePath:
			writeTraeJSON(w, http.StatusOK, traePacksBody(
				traePack("checkin_20260920_7001", 2000, 200.8772, future),
				traePack("plan_monthly_7001", 2000, 1595.5964, future),
			))
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404,"message":"no route"}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := traeCreditsAccount(41, server.URL, nil)
	repo.accountsByID = map[int64]*Account{41: account}

	result, err := svc.QueryCredits(context.Background(), 41)
	require.NoError(t, err)
	require.True(t, result.Success, "error: %s", result.Error)
	require.Equal(t, "cn", result.Realm)
	// 纪律 5：两个有效包 remain = 1799.1228 + 404.4036。
	require.InDelta(t, 2203.5264, result.Remain, 1e-6)
	require.InDelta(t, 1796.4736, result.Used, 1e-6)
	require.InDelta(t, 4000, result.Size, 1e-6)
	require.Equal(t, 2, result.Packs)
	require.Len(t, result.Packages, 2)
	require.InDelta(t, 1799.1228, result.Packages[0].Remain, 1e-6)
	require.InDelta(t, 404.4036, result.Packages[1].Remain, 1e-6)
	// 权益包按 entitlement_id 前缀归类。
	require.Equal(t, "checkin", result.Packages[0].Kind)
	require.Equal(t, "plan", result.Packages[1].Kind)
	require.False(t, result.Packages[0].Expired)
	require.InDelta(t, 2203.5264, result.Credits, 1e-6)
	require.False(t, result.CheckedIn)
	require.True(t, result.Checkable)

	// 两次上游调用：status 与 ent_usage，且 req_source 口径不同。
	require.Equal(t, []string{traeCheckinStatusPath, traeEntUsagePath}, spy.pathLog())
	bodies := spy.bodyLog()
	require.Contains(t, bodies[0], `"req_source":1`)
	require.Contains(t, bodies[1], `"require_usage":true`)
	require.Contains(t, bodies[1], `"req_source":2`, "权益包用量必须用 req_source=2 才返回完整 usage")

	// 快照落 extra。
	calls, _, _, keys := repo.counts()
	require.Equal(t, 1, calls)
	require.Equal(t, []string{traeCreditsExtraKey}, keys)
	snap, ok := repo.extras[traeCreditsExtraKey].(TraeCreditsSnapshot)
	require.True(t, ok)
	require.InDelta(t, 2203.5264, snap.Remain, 1e-6)
	require.Equal(t, "cn", snap.Realm)
	require.Len(t, snap.Packages, 2)
}

func TestTraeQueryCreditsSkipsExpiredPacks(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	future := time.Now().AddDate(0, 0, 15).Unix()
	past := time.Now().AddDate(0, 0, -3).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeCheckinStatusPath:
			writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 900))
		case traeEntUsagePath:
			writeTraeJSON(w, http.StatusOK, traePacksBody(
				traePack("checkin_20260901_7001", 500, 100, past),     // 已过期：remain 记 0
				traePack("plan_monthly_7001", 2000, 200.8772, future), // 有效
				traePack("other_bonus", 1000, 0, 0),                   // 无 end_time 视为不过期
			))
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	repo.accountsByID = map[int64]*Account{42: traeCreditsAccount(42, server.URL, nil)}

	result, err := svc.QueryCredits(context.Background(), 42)
	require.NoError(t, err)
	require.True(t, result.Success, "error: %s", result.Error)

	require.True(t, result.Packages[0].Expired)
	require.EqualValues(t, 0, result.Packages[0].Remain, "过期包 remain 必须记 0")
	require.InDelta(t, 1799.1228, result.Packages[1].Remain, 1e-6)
	// 纪律 5：过期包的 limit/used 也不得进入总额。
	require.InDelta(t, 2799.1228, result.Remain, 1e-6)
	require.InDelta(t, 200.8772, result.Used, 1e-6)
	require.InDelta(t, 3000, result.Size, 1e-6)
	require.Equal(t, "other", result.Packages[2].Kind)
	// 记录现状：Packs 是上游返回的全部包数（含过期），而非注释所述的"未过期包数"。
	require.Equal(t, 3, result.Packs)

	// end_time 毫秒形态同样归一为秒（纪律 10）。
	require.EqualValues(t, future, result.Packages[1].ExpireAt)
}

func TestTraeQueryCreditsEndMillisecondExpiryNormalizedToSeconds(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	millis := (time.Now().AddDate(0, 0, 7).Unix()) * 1000
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeCheckinStatusPath:
			writeTraeJSON(w, http.StatusOK, traeStatusBody(true, true, 10))
		case traeEntUsagePath:
			writeTraeJSON(w, http.StatusOK, traePacksBody(traePack("plan_x", 100, 40, millis)))
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	repo.accountsByID = map[int64]*Account{43: traeCreditsAccount(43, server.URL, nil)}

	result, err := svc.QueryCredits(context.Background(), 43)
	require.NoError(t, err)
	require.True(t, result.Success, "error: %s", result.Error)
	require.EqualValues(t, millis/1000, result.Packages[0].ExpireAt, "上游毫秒必须归一为秒")
	require.False(t, result.Packages[0].Expired, "毫秒未归一时会被当成远古时间而误判过期")
	require.InDelta(t, 60, result.Remain, 1e-6)
	require.True(t, result.CheckedIn)
}

// 纪律 1：UG 域 HTTP 恒 200，成败只看 body 的 code。
func TestTraeQueryCreditsBusinessCodeFailureStillHTTP200(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	var statusHTTPCode int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeCheckinStatusPath:
			statusHTTPCode = http.StatusOK
			// 缺设备头时上游的典型形态：HTTP 200 + code=9004。
			writeTraeJSON(w, http.StatusOK, `{"code":9004,"message":"param error"}`)
		case traeEntUsagePath:
			writeTraeJSON(w, http.StatusOK, traePacksBody(traePack("plan_x", 100, 10, time.Now().AddDate(0, 0, 5).Unix())))
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	repo.accountsByID = map[int64]*Account{44: traeCreditsAccount(44, server.URL, nil)}

	result, err := svc.QueryCredits(context.Background(), 44)
	require.NoError(t, err, "业务失败不返回 Go error（管理端 200 + success=false）")
	require.Equal(t, http.StatusOK, statusHTTPCode, "UG 域即使业务失败 HTTP 仍是 200")
	require.False(t, result.Success)
	require.Contains(t, result.Error, "9004")
	require.Contains(t, result.Error, "param error")
	calls, _, _, _ := repo.counts()
	require.Equal(t, 0, calls, "失败查询不落快照")
}

func TestTraeQueryCreditsUsageFailureIsNotFatal(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		if r.URL.Path == traeCheckinStatusPath {
			writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 1234.5))
			return
		}
		writeTraeJSON(w, http.StatusOK, `{"code":9004,"message":"usage unavailable"}`)
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	repo.accountsByID = map[int64]*Account{45: traeCreditsAccount(45, server.URL, nil)}

	result, err := svc.QueryCredits(context.Background(), 45)
	require.NoError(t, err)
	require.True(t, result.Success, "权益包明细失败不致命：退回 status 的总积分读数")
	require.InDelta(t, 1234.5, result.Credits, 1e-6)
	require.EqualValues(t, 0, result.Remain)
	require.Empty(t, result.Error)
}

// 纪律 3：claim 报成功但回查读数未跟上 → 不推翻上游结论，但记下待确认 detail。
func TestTraeCheckinClaimOKButStillUncheckedRecordsPendingStatus(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeCheckinStatusPath:
			// 两次回查都说 checked_in=false（上游未真正发放）。
			writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 100))
		case traeCheckinClaimPath:
			writeTraeJSON(w, http.StatusOK, `{"code":0,"message":"success"}`)
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	repo.accountsByID = map[int64]*Account{46: traeCreditsAccount(46, server.URL, nil)}

	result, err := svc.Checkin(context.Background(), 46)
	require.NoError(t, err)
	// 上游 code=0 是签到成功的唯一权威依据；checked_in 读数有设备缓存延迟，
	// 不得据它把真成功推翻为 fail（否则「当日成功优先」合并语义会长期误报）。
	require.Equal(t, TraeCheckinStatusOK, result.Status)
	require.True(t, result.Success)
	require.Contains(t, result.Detail, "not yet reflected")
	require.Equal(t, []string{
		traeCheckinStatusPath, traeCheckinClaimPath, traeCheckinStatusPath,
		traeCheckinStatusPath, traeEntUsagePath,
	}, spy.pathLog(), "回查一次取余额 + 成功后的积分快照刷新（status + ent_usage）")
	// fail 仍落签到快照（列表页要显示失败态），成功则附带刷新积分快照。
	_, _, _, keys := repo.counts()
	require.Contains(t, keys, traeCheckinExtraKey)
	require.Contains(t, keys, traeCreditsExtraKey)
}

func TestTraeCheckinHappyPathConfirmsViaStatus(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	var statusCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeCheckinStatusPath:
			n := atomic.AddInt32(&statusCalls, 1)
			if n == 1 {
				writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 100))
				return
			}
			writeTraeJSON(w, http.StatusOK, traeStatusBody(true, true, 110))
		case traeCheckinClaimPath:
			writeTraeJSON(w, http.StatusOK, `{"code":0,"message":"success"}`)
		case traeEntUsagePath:
			writeTraeJSON(w, http.StatusOK, traePacksBody(traePack("checkin_20260926_1", 110, 0, time.Now().AddDate(0, 0, 31).Unix())))
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	repo.accountsByID = map[int64]*Account{47: traeCreditsAccount(47, server.URL, nil)}

	result, err := svc.Checkin(context.Background(), 47)
	require.NoError(t, err)
	require.Equal(t, TraeCheckinStatusOK, result.Status)
	require.True(t, result.Success)
	require.Empty(t, result.Detail)
	require.InDelta(t, 110, result.Credits, 1e-6, "积分取回查 status 的读数（claim 不回传积分）")
	require.True(t, result.HasCredit)

	paths := spy.pathLog()
	require.Equal(t, []string{
		traeCheckinStatusPath, traeCheckinClaimPath, traeCheckinStatusPath,
		traeCheckinStatusPath, traeEntUsagePath,
	}, paths, "成功签到后必须刷新积分快照")
	require.Equal(t, 1, spy.countOf(traeCheckinClaimPath), "claim 只发一次")

	_, _, _, keys := repo.counts()
	require.Contains(t, keys, traeCheckinExtraKey)
	require.Contains(t, keys, traeCreditsExtraKey)
	snap, ok := repo.extras[traeCheckinExtraKey].(TraeCheckinSnapshot)
	require.True(t, ok)
	require.Equal(t, TraeCheckinStatusOK, snap.Status)
	require.Equal(t, timezone.Now().Format("2006-01-02"), snap.Date)
}

// 纪律 4：status.checked_in=true 时不再发 claim。
func TestTraeCheckinAlreadyCheckedInDoesNotClaim(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeCheckinStatusPath:
			writeTraeJSON(w, http.StatusOK, traeStatusBody(true, true, 500))
		case traeEntUsagePath:
			writeTraeJSON(w, http.StatusOK, traePacksBody(traePack("plan_a", 500, 100, time.Now().AddDate(0, 0, 10).Unix())))
		default:
			writeTraeJSON(w, http.StatusOK, `{"code":404,"message":"no route"}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	repo.accountsByID = map[int64]*Account{48: traeCreditsAccount(48, server.URL, nil)}

	result, err := svc.Checkin(context.Background(), 48)
	require.NoError(t, err)
	require.Equal(t, TraeCheckinStatusAlready, result.Status)
	require.True(t, result.Success, "already 是幂等成功语义")
	require.Empty(t, result.Detail)
	require.Zero(t, spy.countOf(traeCheckinClaimPath), "checked_in=true 不得再发 claim")
	for _, p := range spy.pathLog() {
		require.NotEqual(t, traeCheckinClaimPath, p, "路径序列里不应出现 claim: %v", spy.pathLog())
	}
}

// 纪律 2：9095 = 今日已签到，属幂等成功，绝不得判为失败。
func TestTraeCheckinCode9095IsIdempotentSuccessNotFail(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeCheckinStatusPath:
			// status 未反映已签到（上游读缓存），claim 直接给 9095。
			writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 100))
		case traeCheckinClaimPath:
			writeTraeJSON(w, http.StatusOK, `{"code":9095,"message":"今日已签到"}`)
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := traeCreditsAccount(49, server.URL, nil)
	repo.accountsByID = map[int64]*Account{49: account}

	result, err := svc.Checkin(context.Background(), 49)
	require.NoError(t, err)
	require.True(t, result.Success, "9095 必须是幂等成功，不能记成失败")
	require.NotEqual(t, TraeCheckinStatusFail, result.Status)
	// claim 返回 9095 时，编排层必须落到 already（而非被吞成 ok 误报为“刚领到积分”）。
	require.Equal(t, TraeCheckinStatusAlready, result.Status)

	// 判据本身仍是 already（供其它链路复用）。
	require.True(t, traeIsCheckinAlreadyError(&traeBillingError{Status: http.StatusOK, Code: traeCodeAlreadyChecked}))
	require.True(t, traeIsCheckinAlreadyError(&traeBillingError{Code: 9004, Msg: "今天已签到"}))
	require.True(t, traeIsCheckinAlreadyError(&traeBillingError{Code: 9004, Msg: "already checked in"}))
	require.False(t, traeIsCheckinAlreadyError(&traeBillingError{Code: traeCodeBusy, Msg: "当前参与用户太多"}))
}

func TestTraeCheckinClaimPropagates9095AsAlready(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		writeTraeJSON(w, http.StatusOK, `{"code":9095,"message":"今日已签到"}`)
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := traeCreditsAccount(50, server.URL, nil)
	// claim 层不得吞掉 9095：否则编排层无法区分「刚领到」与「今早已签」，
	// 前端会把未产生收益的重复签到误报为成功领取。
	err := svc.traeCheckinClaim(context.Background(), account)
	require.Error(t, err)
	require.True(t, traeIsCheckinAlreadyError(err), "9095 必须被分类为幂等已签到")
	require.False(t, traeIsBusyError(err), "9095 不得误判为 9074 繁忙")
}

// 纪律 2：9074 是账号级稳定拒绝（换 deviceId / token / UA / 请求体均无效）——
// 不得做任何参数变体重试，也不得把失败掩盖成成功。
func TestTraeCheckinCode9074FailsWithoutParamRetry(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	var claimCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeCheckinStatusPath:
			writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 100))
		case traeCheckinClaimPath:
			if atomic.AddInt32(&claimCalls, 1) == 1 {
				writeTraeJSON(w, http.StatusOK, `{"code":9074,"message":"当前参与用户太多"}`)
				return
			}
			writeTraeJSON(w, http.StatusOK, `{"code":0,"message":"success"}`)
		case traeEntUsagePath:
			writeTraeJSON(w, http.StatusOK, traePacksBody(traePack("plan_a", 200, 50, time.Now().AddDate(0, 0, 9).Unix())))
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	repo.accountsByID = map[int64]*Account{51: traeCreditsAccount(51, server.URL, map[string]any{"req_source": "1"})}

	result, err := svc.Checkin(context.Background(), 51)
	require.NoError(t, err)
	require.Equal(t, TraeCheckinStatusFail, result.Status, "9074 不得被重试掩盖成成功")
	require.False(t, result.Success)
	require.Contains(t, result.Detail, "9074")
	require.Equal(t, 1, spy.countOf(traeCheckinClaimPath), "9074 不得做任何参数变体重试")
	claimBodies := traeClaimBodies(spy)
	require.Len(t, claimBodies, 1)
	require.Contains(t, claimBodies[0], `"req_source":1`)
	require.Equal(t, 0, spy.countOf(traeEntUsagePath), "失败不刷新积分快照")
}

// traeClaimBodies 抽出 claim 请求对应的 body（按路径配对，不受后续积分刷新调用影响）。
func traeClaimBodies(spy *traeUpstreamSpy) []string {
	paths, bodies := spy.pathLog(), spy.bodyLog()
	out := make([]string, 0, len(paths))
	for i, p := range paths {
		if p == traeCheckinClaimPath && i < len(bodies) {
			out = append(out, bodies[i])
		}
	}
	return out
}

func TestTraeCheckinCode9074TwiceFails(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeCheckinStatusPath:
			writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 100))
		case traeCheckinClaimPath:
			writeTraeJSON(w, http.StatusOK, `{"code":9074,"message":"当前参与用户太多"}`)
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	repo.accountsByID = map[int64]*Account{52: traeCreditsAccount(52, server.URL, map[string]any{"req_source": "1"})}

	result, err := svc.Checkin(context.Background(), 52)
	require.NoError(t, err)
	require.Equal(t, TraeCheckinStatusFail, result.Status)
	require.False(t, result.Success)
	require.Contains(t, result.Detail, "9074")
	require.Equal(t, 1, spy.countOf(traeCheckinClaimPath), "9074 一次即判失败，不得重试")
	require.Equal(t, 0, spy.countOf(traeEntUsagePath), "失败不刷新积分快照")
}

func TestTraeCheckinReqSourceTwoDoesNotRetry(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeCheckinStatusPath:
			writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 100))
		case traeCheckinClaimPath:
			writeTraeJSON(w, http.StatusOK, `{"code":9074,"message":"当前参与用户太多"}`)
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	repo.accountsByID = map[int64]*Account{53: traeCreditsAccount(53, server.URL, map[string]any{"req_source": "2"})}

	result, err := svc.Checkin(context.Background(), 53)
	require.NoError(t, err)
	require.Equal(t, TraeCheckinStatusFail, result.Status)
	require.Equal(t, 1, spy.countOf(traeCheckinClaimPath), "9074 与 req_source 无关，不得兜底重试")
}

func TestTraeCheckinEnableFalseSkipsWithoutSnapshot(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeCheckinStatusPath:
			writeTraeJSON(w, http.StatusOK, traeStatusBody(false, false, 100))
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	repo.accountsByID = map[int64]*Account{54: traeCreditsAccount(54, server.URL, nil)}

	result, err := svc.Checkin(context.Background(), 54)
	require.NoError(t, err)
	require.Equal(t, TraeCheckinStatusSkipped, result.Status)
	require.False(t, result.Success)
	require.Contains(t, result.Detail, "not available")
	require.Equal(t, []string{traeCheckinStatusPath}, spy.pathLog(), "skipped 只查一次状态，不发 claim/回查")
	calls, _, _, _ := repo.counts()
	require.Equal(t, 0, calls, "skipped 不写任何快照")
}

func TestTraeCheckinStatusCodeErrorFails(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		writeTraeJSON(w, http.StatusOK, `{"code":9004,"message":"param error"}`)
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	repo.accountsByID = map[int64]*Account{55: traeCreditsAccount(55, server.URL, nil)}

	result, err := svc.Checkin(context.Background(), 55)
	require.NoError(t, err)
	require.Equal(t, TraeCheckinStatusFail, result.Status)
	require.Contains(t, result.Detail, "9004")
	require.Equal(t, []string{traeCheckinStatusPath}, spy.pathLog(), "status 失败不得继续发 claim")
}

// 纪律 10/9：code=1001 会话失效 → 换票一次 → 重试成功，且新 refresh_token 写回凭据。
func TestTraeBillingCode1001TriggersTokenRefreshAndRetry(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	var statusCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeCheckinStatusPath:
			if atomic.AddInt32(&statusCalls, 1) == 1 {
				writeTraeJSON(w, http.StatusOK, `{"code":1001,"message":"session expired"}`)
				return
			}
			writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 777))
		case traeEntUsagePath:
			writeTraeJSON(w, http.StatusOK, traePacksBody(traePack("plan_a", 800, 23, time.Now().AddDate(0, 0, 6).Unix())))
		case traeExchangeToken:
			writeTraeJSON(w, http.StatusOK, `{"Result":{"Token":"at-new","RefreshToken":"rt-rotated","TokenExpireAt":1790000000000,"RefreshExpireAt":1800000000000}}`)
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := traeCreditsAccount(56, server.URL, map[string]any{
		"access_token":  "at-old",
		"refresh_token": "rt-old",
	})
	repo.accountsByID = map[int64]*Account{56: account}

	result, err := svc.QueryCredits(context.Background(), 56)
	require.NoError(t, err)
	require.True(t, result.Success, "换票后重试必须成功: %s", result.Error)
	require.InDelta(t, 777, result.Credits, 1e-6)

	require.Equal(t, 1, spy.countOf(traeExchangeToken), "1001 必须触发一次 ExchangeToken")
	require.GreaterOrEqual(t, spy.countOf(traeCheckinStatusPath), 2, "换票后必须重试原请求")
	// 纪律 9：refreshToken 轮换 —— 新值必须写回凭据（旧值不可复用）。
	require.Equal(t, "at-new", account.Credentials["access_token"])
	require.Equal(t, "rt-rotated", account.Credentials["refresh_token"])
	// 纪律 10：上游毫秒过期时间落库为秒。
	require.EqualValues(t, 1790000000, traeAnyInt64(account.Credentials["expires_at"]))
	require.EqualValues(t, 1800000000, traeAnyInt64(account.Credentials["refresh_expires_at"]))
	require.Greater(t, repo.credPersists+repo.updateCalls, 0, "换票结果必须持久化")

	// 换票请求体与头：OAuth 域只发极简头族，且带 client_id/refresh_token。
	bodies := spy.bodyLog()
	exchanged := false
	for _, b := range bodies {
		if strings.Contains(b, `"RefreshToken"`) {
			exchanged = true
			require.Contains(t, b, `"ClientID":"`+traeCNClientID+`"`)
			require.Contains(t, b, `"ClientSecret":"-"`)
			break
		}
	}
	require.True(t, exchanged, "换票请求体必须携带 ClientID/RefreshToken")
}

func TestTraeBillingCode1001WithoutRefreshTokenDoesNotLoop(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		writeTraeJSON(w, http.StatusOK, `{"code":1001,"message":"session expired"}`)
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	repo.accountsByID = map[int64]*Account{57: traeCreditsAccount(57, server.URL, map[string]any{"access_token": "at"})}

	result, err := svc.QueryCredits(context.Background(), 57)
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Error, "1001")
	require.Zero(t, spy.countOf(traeExchangeToken), "无 refresh_token 不得打换票端点")
	require.Equal(t, 1, spy.countOf(traeCheckinStatusPath), "无 refresh_token 只发一次 status")
}

func TestTraeQueryCreditsPreflightRefreshWhenExpiring(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeExchangeToken:
			writeTraeJSON(w, http.StatusOK, `{"Result":{"Token":"at-fresh","RefreshToken":"rt-fresh","TokenExpireDuration":7200}}`)
		case traeCheckinStatusPath:
			writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 10))
		case traeEntUsagePath:
			writeTraeJSON(w, http.StatusOK, traePacksBody())
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := traeCreditsAccount(58, server.URL, map[string]any{
		"access_token":  "at-expired",
		"refresh_token": "rt",
		// 已落在 24h 提前刷新窗口内 → 出站前先换票。
		"expires_at": time.Now().Add(30 * time.Minute).Unix(),
	})
	repo.accountsByID = map[int64]*Account{58: account}

	result, err := svc.QueryCredits(context.Background(), 58)
	require.NoError(t, err)
	require.True(t, result.Success, "error: %s", result.Error)
	require.Equal(t, 1, spy.countOf(traeExchangeToken), "临近过期必须预刷新")
	require.Equal(t, "at-fresh", account.Credentials["access_token"])
	// 上游只给 TokenExpireDuration（秒）时，归一为「now + duration」的 epoch 秒。
	var withDuration traeExchangeTokenResponse
	withDuration.Result.TokenExpireDuration = 7200
	require.InDelta(t, 7200, float64(traeExchangeExpiresAt(withDuration)-time.Now().Unix()), 5)
	var withoutExpiry traeExchangeTokenResponse
	require.Zero(t, traeExchangeExpiresAt(withoutExpiry), "无过期信息时返回 0（不写 expires_at）")
}

// 纪律 6：当日内已成功（ok/already）的签到记录不被后续失败覆盖。
func TestTraeCheckinSnapshotKeepsEarlierSuccessWithinDay(t *testing.T) {
	t.Parallel()
	today := timezone.Now().Format("2006-01-02")
	yesterday := timezone.Now().AddDate(0, 0, -1).Format("2006-01-02")

	okExisting := &TraeCheckinSnapshot{Date: today, Status: TraeCheckinStatusOK}
	require.True(t, shouldKeepTraeCheckinSnapshot(okExisting, TraeCheckinSnapshot{Date: today, Status: TraeCheckinStatusFail}))
	alreadyExisting := &TraeCheckinSnapshot{Date: today, Status: TraeCheckinStatusAlready}
	require.True(t, shouldKeepTraeCheckinSnapshot(alreadyExisting, TraeCheckinSnapshot{Date: today, Status: TraeCheckinStatusFail}))
	// 先失败后成功要允许覆盖。
	require.False(t, shouldKeepTraeCheckinSnapshot(
		&TraeCheckinSnapshot{Date: today, Status: TraeCheckinStatusFail},
		TraeCheckinSnapshot{Date: today, Status: TraeCheckinStatusOK}))
	// 跨日重置。
	require.False(t, shouldKeepTraeCheckinSnapshot(
		&TraeCheckinSnapshot{Date: yesterday, Status: TraeCheckinStatusOK},
		TraeCheckinSnapshot{Date: today, Status: TraeCheckinStatusFail}))
	require.False(t, shouldKeepTraeCheckinSnapshot(nil, TraeCheckinSnapshot{Date: today, Status: TraeCheckinStatusFail}))
	require.False(t, shouldKeepTraeCheckinSnapshot(&TraeCheckinSnapshot{}, TraeCheckinSnapshot{Date: today, Status: TraeCheckinStatusFail}))

	// readTraeCheckinSnapshot 的形态兼容（extra 经 JSON 反序列化后是 float64）。
	acc := traeCreditsAccount(59, "https://x", nil)
	acc.Extra = map[string]any{traeCheckinExtraKey: map[string]any{
		"date": today, "status": TraeCheckinStatusAlready, "checked_at": float64(1760000000), "credits": float64(12.5),
	}}
	snap := readTraeCheckinSnapshot(acc)
	require.NotNil(t, snap)
	require.Equal(t, today, snap.Date)
	require.Equal(t, TraeCheckinStatusAlready, snap.Status)
	require.EqualValues(t, 1760000000, snap.CheckedAt)
	require.InDelta(t, 12.5, snap.Credits, 1e-9)
	require.Equal(t, TraeCheckinStatusAlready, traeCheckinStatusToday(acc))

	// 非当日记录视为无记录。
	stale := traeCreditsAccount(60, "https://x", nil)
	stale.Extra = map[string]any{traeCheckinExtraKey: map[string]any{"date": yesterday, "status": TraeCheckinStatusOK}}
	require.Equal(t, "", traeCheckinStatusToday(stale))
	require.Equal(t, "", traeCheckinStatusToday(traeCreditsAccount(61, "https://x", nil)))

	// 形态异常（非对象）不得 panic。
	broken := traeCreditsAccount(62, "https://x", nil)
	broken.Extra = map[string]any{traeCheckinExtraKey: "not-an-object"}
	require.Nil(t, readTraeCheckinSnapshot(broken))
}

func TestTraeCheckinFailureDoesNotOverwriteExistingSuccessSnapshot(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	today := timezone.Now().Format("2006-01-02")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		if r.URL.Path == traeCheckinStatusPath {
			writeTraeJSON(w, http.StatusOK, `{"code":9004,"message":"param error"}`)
			return
		}
		writeTraeJSON(w, http.StatusOK, `{"code":404}`)
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := traeCreditsAccount(63, server.URL, nil)
	account.Extra = map[string]any{traeCheckinExtraKey: map[string]any{"date": today, "status": TraeCheckinStatusOK}}
	repo.accountsByID = map[int64]*Account{63: account}

	result := svc.CheckinForAccount(context.Background(), account)
	require.Equal(t, TraeCheckinStatusFail, result.Status)
	calls, _, _, _ := repo.counts()
	require.Equal(t, 0, calls, "当日已成功 → 后续失败不得覆盖快照")

	// 无既有成功记录时，失败必须落盘（否则列表页看不到失败态）。
	fresh := traeCreditsAccount(64, server.URL, nil)
	repo.accountsByID[64] = fresh
	require.Equal(t, TraeCheckinStatusFail, svc.CheckinForAccount(context.Background(), fresh).Status)
	calls2, _, _, keys2 := repo.counts()
	require.Equal(t, 1, calls2)
	require.Equal(t, []string{traeCheckinExtraKey}, keys2)
	snap, ok := repo.extras[traeCheckinExtraKey].(TraeCheckinSnapshot)
	require.True(t, ok)
	require.Equal(t, TraeCheckinStatusFail, snap.Status)
	require.Equal(t, today, snap.Date)
}

// singleflight：同一账号并发查询只打一次上游。
func TestTraeQueryCreditsSingleflightMergesConcurrentCalls(t *testing.T) {
	t.Parallel()
	var statusCalls, usageCalls int32
	spy := &traeUpstreamSpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeCheckinStatusPath:
			atomic.AddInt32(&statusCalls, 1)
			time.Sleep(80 * time.Millisecond) // 制造并发窗口
			writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 50))
		case traeEntUsagePath:
			atomic.AddInt32(&usageCalls, 1)
			writeTraeJSON(w, http.StatusOK, traePacksBody(traePack("plan_a", 50, 10, time.Now().AddDate(0, 0, 4).Unix())))
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	repo.accountsByID = map[int64]*Account{65: traeCreditsAccount(65, server.URL, nil)}

	const workers = 6
	results := make([]*TraeCreditsResult, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = svc.QueryCredits(context.Background(), 65)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < workers; i++ {
		require.NoError(t, errs[i])
		require.NotNil(t, results[i])
		require.True(t, results[i].Success, "error: %s", results[i].Error)
	}
	require.EqualValues(t, 1, atomic.LoadInt32(&statusCalls), "并发查询必须被 singleflight 合并")
	require.EqualValues(t, 1, atomic.LoadInt32(&usageCalls), "并发查询必须被 singleflight 合并")
	calls, _, _, _ := repo.counts()
	require.Equal(t, 1, calls, "合并后只落一次快照")

	// 返回的是各自副本，改一份不影响另一份。
	results[0].Credits = -1
	require.InDelta(t, 50, results[1].Credits, 1e-6)
}

func TestTraeCheckinSingleflightMergesConcurrentCalls(t *testing.T) {
	t.Parallel()
	var claimCalls int32
	spy := &traeUpstreamSpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeCheckinStatusPath:
			time.Sleep(40 * time.Millisecond)
			writeTraeJSON(w, http.StatusOK, traeStatusBody(true, true, 100))
		case traeCheckinClaimPath:
			atomic.AddInt32(&claimCalls, 1)
			writeTraeJSON(w, http.StatusOK, `{"code":0,"message":"success"}`)
		default:
			writeTraeJSON(w, http.StatusNotFound, `{"code":404}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	account := traeCreditsAccount(66, server.URL, nil)
	repo.accountsByID = map[int64]*Account{66: account}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := svc.CheckinForAccount(context.Background(), account)
			require.Contains(t, []string{TraeCheckinStatusAlready, TraeCheckinStatusOK}, res.Status)
		}()
	}
	wg.Wait()
	require.Zero(t, atomic.LoadInt32(&claimCalls), "checked_in=true 时任何并发路径都不得发 claim")
	require.LessOrEqual(t, spy.countOf(traeCheckinStatusPath), 2, "并发签到应被 singleflight 合并")
}

func TestTraeCreditsRejectsNonTraeAccountAndMissingAccount(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 1))
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	repo.accountsByID = map[int64]*Account{
		70: {ID: 70, Platform: PlatformOpenAI, Credentials: map[string]any{"api_key": "k"}},
	}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)

	_, err := svc.QueryCredits(context.Background(), 70)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a trae account")

	_, err = svc.Checkin(context.Background(), 71)
	require.Error(t, err)
	require.Contains(t, err.Error(), "TRAE_ACCOUNT_NOT_FOUND")

	require.Zero(t, len(spy.pathLog()), "校验失败不得打上游")

	// 未配置服务时的防御分支。
	empty := &TraeCreditsService{cfg: &config.Config{}}
	_, err = empty.QueryCredits(context.Background(), 1)
	require.Error(t, err)
	_, err = empty.Checkin(context.Background(), 1)
	require.Error(t, err)

	// nil 账号签到不得 panic。
	require.Equal(t, TraeCheckinStatusFail, svc.CheckinForAccount(context.Background(), nil).Status)
}

func TestTraeCreditsURLAllowlistRejectsUnknownHost(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 1))
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := &TraeCreditsService{
		accountRepo:  repo,
		httpUpstream: &workbuddyTestUpstream{client: server.Client()},
		cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
			Enabled: true, UpstreamHosts: []string{"api.trae.cn"},
		}}},
	}
	repo.accountsByID = map[int64]*Account{72: traeCreditsAccount(72, server.URL, nil)}

	result, err := svc.QueryCredits(context.Background(), 72)
	require.NoError(t, err)
	require.False(t, result.Success, "白名单外的主机必须被安全策略拦下")
	require.Contains(t, result.Error, "TRAE_BILLING_URL_REJECTED")
	require.Empty(t, spy.pathLog(), "拦截发生在出站之前")
}

func TestTraeCreditsUsesAccountProxyWhenLoaded(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeCheckinStatusPath:
			writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 5))
		case traeEntUsagePath:
			writeTraeJSON(w, http.StatusOK, traePacksBody())
		default:
			writeTraeJSON(w, http.StatusNotFound, `{}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	proxyID := int64(9)
	account := traeCreditsAccount(73, server.URL, nil)
	account.ProxyID = &proxyID
	account.Proxy = &Proxy{ID: 9, Protocol: "http", Host: "127.0.0.1", Port: 8080}
	repo.accountsByID = map[int64]*Account{73: account}

	result, err := svc.QueryCredits(context.Background(), 73)
	require.NoError(t, err)
	require.True(t, result.Success, "error: %s", result.Error)
	require.Equal(t, "http://127.0.0.1:8080", account.Proxy.URL())
}

// traeBillingHeadersSentOnUGRequests 端到端确认积分/签到链路真的带上 UG 指纹（纪律 7）。
func TestTraeCreditsUGRequestsCarryDeviceHeadersAndNoUID(t *testing.T) {
	t.Parallel()
	spy := &traeUpstreamSpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.record(r, string(body))
		switch r.URL.Path {
		case traeCheckinStatusPath:
			writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 5))
		case traeEntUsagePath:
			writeTraeJSON(w, http.StatusOK, traePacksBody())
		default:
			writeTraeJSON(w, http.StatusNotFound, `{}`)
		}
	}))
	defer server.Close()

	repo := &traeCreditsFakeRepo{extras: map[string]any{}}
	svc := newTraeCreditsTestService(&workbuddyTestUpstream{client: server.Client()}, repo)
	repo.accountsByID = map[int64]*Account{74: traeCreditsAccount(74, server.URL, nil)}

	result, err := svc.QueryCredits(context.Background(), 74)
	require.NoError(t, err)
	require.True(t, result.Success, "error: %s", result.Error)

	reqs := func() []*http.Request {
		spy.mu.Lock()
		defer spy.mu.Unlock()
		return append([]*http.Request(nil), spy.requests...)
	}()
	require.Len(t, reqs, 2)
	for _, req := range reqs {
		require.Regexp(t, `^\d{15,16}$`, req.Header.Get("X-Device-Id"))
		require.Regexp(t, `^[0-9a-f]{32}$`, req.Header.Get("X-Machine-Id"))
		require.Equal(t, "CN", req.Header.Get("X-User-Region"))
		require.Empty(t, req.Header.Get("X-Uid"), "UG 域绝不发 X-Uid")
		require.Contains(t, req.Header.Get("User-Agent"), "VSCode ")
		require.Contains(t, req.Header.Get("Authorization"), "Cloud-IDE-JWT ")
	}
}

func TestTraePackageKindClassification(t *testing.T) {
	t.Parallel()
	require.Equal(t, "", traePackageKind(""))
	require.Equal(t, "checkin", traePackageKind("checkin_20260920_7001"))
	require.Equal(t, "checkin", traePackageKind("CHECKIN-daily"))
	require.Equal(t, "plan", traePackageKind("plan_monthly"))
	require.Equal(t, "plan", traePackageKind("subscription_annual"))
	require.Equal(t, "other", traePackageKind("bonus_event"))
}

func TestTraeAggregatePacksSkipsExpired(t *testing.T) {
	t.Parallel()
	packs := []TraeCreditsPackage{
		{Limit: 2000, Used: 200.8772, Remain: 1799.1228},
		{Limit: 2000, Used: 1595.5964, Remain: 404.4036},
		{Limit: 500, Used: 100, Remain: 0, Expired: true},
	}
	remain, used, size := traeAggregatePacks(packs)
	require.InDelta(t, 2203.5264, remain, 1e-6)
	require.InDelta(t, 1796.4736, used, 1e-6)
	require.InDelta(t, 4000, size, 1e-6)
	emptyRemain, emptyUsed, emptySize := traeAggregatePacks(nil)
	require.Zero(t, emptyRemain)
	require.Zero(t, emptyUsed)
	require.Zero(t, emptySize)
}

func TestTraeReqSourceDefaultsToOne(t *testing.T) {
	t.Parallel()
	require.Equal(t, traeDefaultReqSource, traeReqSource(nil))
	require.Equal(t, 1, traeReqSource(traeCreditsAccount(75, "https://x", nil)))
	require.Equal(t, 2, traeReqSource(traeCreditsAccount(76, "https://x", map[string]any{"req_source": 2})))
	require.Equal(t, 1, traeReqSource(traeCreditsAccount(77, "https://x", map[string]any{"req_source": 5})), "越界取值回落默认")
}

func TestTraeFirstNonEmptyAndNumericHelpers(t *testing.T) {
	t.Parallel()
	require.Equal(t, "b", traeFirstNonEmpty("", " b ", "c"))
	require.Equal(t, "", traeFirstNonEmpty("", "  "))
	require.EqualValues(t, 7, traeAnyInt64("7"))
	require.EqualValues(t, 0, traeAnyInt64(nil))
	require.InDelta(t, 1.5, traeAnyFloat64("1.5"), 1e-9)
	require.InDelta(t, 3, traeAnyFloat64(int64(3)), 1e-9)
	require.EqualValues(t, 0, traeAnyFloat64("abc"))
}
