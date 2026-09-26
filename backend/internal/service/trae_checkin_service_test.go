//go:build unit

package service

// trae_checkin_service_test.go Trae 每日签到周期任务的行为锁。
//
// 锁定三件事（都是"跑起来才会暴露"的那类问题）：
//  1. 触发时刻计算：今日剩余小时优先，全部已过则顺延到次日最早小时；
//  2. 配置解析：缺省 [9,21]、越界丢弃、去重排序、全非法视为关闭（Start 不启协程）；
//  3. 批量枚举的门控：禁用账号、非 trae 账号、无任何令牌的账号一律跳过且不打上游。
//
// 上游状态机本身（0/9095/9074/回查/快照）由 trae_credits_service_test.go 覆盖，
// 本文件只验证"任务层挑出了该签的账号并逐个调用"。
//
// 计数口径用假上游收到的 claim 请求的 Authorization（每账号 token 唯一），比断言
// repo 回调更直接：runOnce 拿 ListByPlatform 的结果直接签到，不经过 GetByID。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// traeCheckinCallRepo 记录 ListByPlatform 结果并承接快照写入（不 panic 即可）。
type traeCheckinCallRepo struct {
	traeCreditsFakeRepo
	mu       sync.Mutex
	accounts []Account
}

func newTraeCheckinCallRepo(accounts ...Account) *traeCheckinCallRepo {
	repo := &traeCheckinCallRepo{
		traeCreditsFakeRepo: traeCreditsFakeRepo{extras: map[string]any{}},
		accounts:            accounts,
	}
	repo.accountsByID = map[int64]*Account{}
	for i := range accounts {
		copyAcc := accounts[i]
		repo.accountsByID[copyAcc.ID] = &copyAcc
	}
	return repo
}

func (r *traeCheckinCallRepo) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	return r.listByPlatform(platform), nil
}

func (r *traeCheckinCallRepo) listByPlatform(platform string) []Account {
	out := make([]Account, 0, len(r.accounts))
	for _, acc := range r.accounts {
		if acc.Platform == platform {
			out = append(out, acc)
		}
	}
	return out
}

// traeCheckinTestCredits 构造打向假上游的积分服务（三域同源便于路径断言）。
func traeCheckinTestCredits(server *httptest.Server, repo AccountRepository) *TraeCreditsService {
	return &TraeCreditsService{
		accountRepo:  repo,
		httpUpstream: &workbuddyTestUpstream{client: server.Client()},
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
			},
		},
	}
}

// traeCheckinAccount 生成一个可签到账号（base/billing/oauth 全指向假上游）。
func traeCheckinAccount(id int64, serverURL string, status string) Account {
	return Account{
		ID: id, Platform: PlatformTrae, Type: AccountTypeAPIKey, Status: status, Concurrency: 1,
		Credentials: map[string]any{
			"realm": "cn", "access_token": "at-" + strconv.FormatInt(id, 10),
			"uid":      "700000000000000010" + strconv.FormatInt(id, 10),
			"base_url": serverURL, "billing_base_url": serverURL, "oauth_base_url": serverURL,
			"expires_at": time.Now().Add(48 * time.Hour).Unix(),
		},
	}
}

// traeCheckinClaimRecorder 假上游：按 Authorization 归类 claim 调用次数。
type traeCheckinClaimRecorder struct {
	mu     sync.Mutex
	claims map[string]int
	paths  []string
}

func (rec *traeCheckinClaimRecorder) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case traeCheckinStatusPath:
		rec.record("", r.URL.Path)
		writeTraeJSON(w, http.StatusOK, traeStatusBody(false, true, 200))
	case traeCheckinClaimPath:
		rec.record(r.Header.Get("Authorization"), r.URL.Path)
		writeTraeJSON(w, http.StatusOK, `{"code":0,"message":"success"}`)
	case traeEntUsagePath:
		rec.record("", r.URL.Path)
		writeTraeJSON(w, http.StatusOK, traePacksBody())
	default:
		rec.record("", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":404}`))
	}
}

func (rec *traeCheckinClaimRecorder) record(auth, path string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.claims == nil {
		rec.claims = map[string]int{}
	}
	if auth != "" {
		rec.claims[auth]++
	}
	rec.paths = append(rec.paths, path)
}

func (rec *traeCheckinClaimRecorder) snapshotClaims() map[string]int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make(map[string]int, len(rec.claims))
	for k, v := range rec.claims {
		out[k] = v
	}
	return out
}

// 1) 触发时刻：今日剩余小时优先，全部已过顺延次日。
func TestNextTraeCheckinFirePicksEarliestUpcomingHour(t *testing.T) {
	t.Parallel()
	today := time.Date(2026, 9, 26, 8, 0, 0, 0, time.Local)

	next := nextTraeCheckinFire(today, []int{9, 21})
	require.Equal(t, 9, next.Hour(), "8 点时下一次应是 9 点")
	require.Equal(t, today.Day(), next.Day())

	next = nextTraeCheckinFire(time.Date(2026, 9, 26, 22, 30, 0, 0, time.Local), []int{9, 21})
	require.Equal(t, 9, next.Hour(), "22:30 已过当日全部小时 → 顺延次日 9 点")
	require.Equal(t, 27, next.Day())

	next = nextTraeCheckinFire(time.Date(2026, 9, 26, 12, 0, 0, 0, time.Local), []int{9, 21})
	require.Equal(t, 21, next.Hour(), "中午应命中当晚 21 点兜底")
}

// 2) 配置解析：默认值、越界丢弃、去重排序、全非法关闭。
func TestTraeCheckinHoursNormalization(t *testing.T) {
	t.Parallel()
	require.Equal(t, []int{9, 21}, traeCheckinHours(nil), "无配置用默认 [9,21]")
	require.Equal(t, []int{9, 21}, traeCheckinHours(&config.Config{}), "空配置同样回落默认")

	cfg := &config.Config{}
	cfg.Gateway.Trae.CheckinHours = []int{21, 9, 9, 30, -1, 7}
	require.Equal(t, []int{7, 9, 21}, traeCheckinHours(cfg), "越界丢弃、去重、升序")

	all := &config.Config{}
	all.Gateway.Trae.CheckinHours = []int{24, -5}
	require.Nil(t, traeCheckinHours(all), "全部非法视为关闭")
}

// Start 在显式关闭时不得启动协程；零值实例 Stop 不得 panic（单测/禁用分支常见形态）。
func TestTraeCheckinServiceStartRespectsDisabledConfig(t *testing.T) {
	t.Parallel()
	repo := newTraeCheckinCallRepo()
	cfg := &config.Config{}
	cfg.Gateway.Trae.CheckinEnabled = false
	svc := NewTraeCheckinService(&TraeCreditsService{}, repo, cfg)
	svc.Start()
	time.Sleep(50 * time.Millisecond)
	svc.Stop()
	require.Zero(t, len(repo.listByPlatform(PlatformTrae)), "关闭配置下不得枚举账号")

	(&TraeCheckinService{}).Stop()
}

// 3) 批量枚举门控：只有"trae + active + 有令牌"的账号会被签到。
func TestTraeCheckinRunOnceSkipsIneligibleAccounts(t *testing.T) {
	t.Parallel()
	rec := &traeCheckinClaimRecorder{}
	server := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer server.Close()

	good := traeCheckinAccount(201, server.URL, StatusActive)
	good2 := traeCheckinAccount(202, server.URL, StatusActive)
	disabled := traeCheckinAccount(203, server.URL, StatusDisabled)
	noToken := traeCheckinAccount(204, server.URL, StatusActive)
	noToken.Credentials = map[string]any{"realm": "cn", "billing_base_url": server.URL}
	otherPlatform := traeCheckinAccount(205, server.URL, StatusActive)
	otherPlatform.Platform = PlatformWorkbuddy

	repo := &traeCheckinCallRepo{
		traeCreditsFakeRepo: traeCreditsFakeRepo{extras: map[string]any{}},
		accounts:            []Account{good, good2, disabled, noToken, otherPlatform},
	}
	credits := traeCheckinTestCredits(server, repo)

	// 账号间隔置 0（源文件声明为 var 正是为测试），收尾恢复。
	originalDelay := traeCheckinAccountDelay
	traeCheckinAccountDelay = 0
	defer func() { traeCheckinAccountDelay = originalDelay }()

	NewTraeCheckinService(credits, repo, &config.Config{}).runOnce()

	require.Equal(t, map[string]int{
		traeJWTAuthScheme + " at-201": 1,
		traeJWTAuthScheme + " at-202": 1,
	}, rec.snapshotClaims(), "只给合格账号各发一次 claim")
}
