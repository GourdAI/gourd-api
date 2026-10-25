package service

// workbuddy_token_refresher.go WorkBuddy 后台定时刷新器（TokenRefreshService 的第六个注册项）。
//
// 【为什么必须存在（2026-10-24 全量 401 事件的第三条根因）】
// 在本次改造前，TokenRefreshService 的注册表只有 Anthropic / OpenAI / Gemini /
// Antigravity / Grok 五家，**没有 WorkBuddy**。后果：
//   - WorkBuddy 的 access_token 是有效期约 45 天的 JWT（实测 exp 距 iat 精确 45 天）；
//   - 请求路径的「预刷新」因凭据缺 expires_at 而恒不触发（见 workbuddy_jwt.go 的说明）；
//   - 于是 token 到期后没有任何角色会去换新，只能等请求撞 401；
//   - 而 401 在改造前对 apikey 账号是永久 SetError（status=error + schedulable=false），
//     刷新链里也没有恢复路径 —— 一旦撞上，该账号永远不再被调度，只能管理员手工重登。
// 三者叠加的结果就是「接口调用全部 401」且**无法自愈**。
//
// 本文件补上第三个缺口：让 WorkBuddy 进入后台刷新周期，在到期前主动换新。
//
// 【与另五家的差异（刻意为之，不是遗漏）】
//   - CanRefresh 不判 Type==OAuth：WorkBuddy 恒为 apikey，但它的凭据语义与 OAuth 同构
//     （access_token 短期 + refresh_token 轮换）。判据收窄到「平台 + 有 refresh_token」。
//   - NeedsRefresh 不读 expires_at 凭据字段，而是经 account.GetWorkbuddyCredentials()，
//     该读路径已内置 JWT exp 兜底 ⇒ 存量缺 expires_at 的账号也能被正确判定为「即将过期」。
//   - Refresh 复用 workbuddyRefreshResponse 的同一信封解析，并沿用
//     refreshWorkbuddyToken 的并发锁语义（见下方 lock 说明）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// workbuddyTokenRefresher 是 WorkBuddy 平台的后台刷新策略实现。
//
// 传输层走 HTTPUpstream（而不是直接 http.Client）：与聊天主链路同一条出站通道，
// 因此账号代理、TLS 指纹、限速闸门、连接池全部复用，不会出现「后台刷新裸奔、
// 聊天带指纹」的身份不一致（那正是 2026-10-22 那轮改造要消除的形态）。
type workbuddyTokenRefresher struct {
	httpUpstream HTTPUpstream
	profileSvc   *TLSFingerprintProfileService
	cfg          *config.Config
}

// 刷新窗口不在此处定义：统一由 workbuddyEffectiveRefreshSkew 决定（与请求路径预刷新
// 同源、同参数），避免两处各自维护一份窗口而产生「后台换票早、请求路径晚」的裂缝。

// NewWorkBuddyTokenRefresher 创建 WorkBuddy 后台刷新器。
//
// 返回 OAuthRefreshExecutor（= TokenRefresher + CacheKey）：后台刷新周期需要 executor
// 来做分布式锁，光是 TokenRefresher 不够。
//
// 参数可为 nil（对应能力自动降级）：
//   - httpUpstream 为 nil ⇒ 走 http.DefaultClient 兜底，保证刷新链路仍能工作；
//   - profileSvc 为 nil ⇒ 不做 TLS 指纹伪装（与 resolvePlatformDefaultTLSProfile 的 nil 语义一致）。
func NewWorkBuddyTokenRefresher(httpUpstream HTTPUpstream, profileSvc *TLSFingerprintProfileService, cfg *config.Config) OAuthRefreshExecutor {
	return &workbuddyTokenRefresher{httpUpstream: httpUpstream, profileSvc: profileSvc, cfg: cfg}
}

// CacheKey 返回用于分布式锁的缓存键（与其它平台的关键字空间隔离）。
func (r *workbuddyTokenRefresher) CacheKey(account *Account) string {
	if account == nil {
		return "workbuddy_token:0"
	}
	return fmt.Sprintf("workbuddy_token:%d", account.ID)
}

// CanRefresh 报告本刷新器能否处理该账号。
//
// 判据（三条同时满足）：
//   - 平台是 WorkBuddy；
//   - 凭据里有非空 refresh_token（没有就真的不可自愈，让它在 401 分支走永久禁用是正确的）；
//   - 不判 account.Type：WorkBuddy 恒为 apikey，但凭据语义与 OAuth 同构。
//
// 刻意不看 status/schedulable：暂停中（schedulable=false 但 status=active）的账号
// 仍持有有效 refresh_token，让它的 access_token 保持新鲜，管理员恢复调度时才能立即可用。
// 这一条与 account_repo.ListOAuthRefreshCandidatePage 里「Deliberately NO schedulable filter」
// 的既有取舍同口径。
func (r *workbuddyTokenRefresher) CanRefresh(account *Account) bool {
	if account == nil || !account.IsWorkbuddy() {
		return false
	}
	return strings.TrimSpace(account.GetWorkbuddyCredentials().RefreshToken) != ""
}

// NeedsRefresh 报告该账号的 access_token 是否需要在本次周期内换新。
//
// 语义优先级：
//  1. access_token 为空 → 必须刷新（没有可用票据）；
//  2. refresh_token 为空 → 不可刷新（由 CanRefresh 已挡，此处防御）；
//  3. expires_at（经 JWT 兜底解析后可读）缺失 → **不主动刷新**。
//     这一条沿用 workbuddyTokenNeedsRefresh 的既有取舍：无过期信息时避免把有效 token 刷掉。
//     与旧实现的差别在于，JWT 兜底让「缺失」只在 token 根本不是 JWT 时才发生，
//     手工粘贴的标准 WorkBuddy token 都能读出 exp。
//  4. 剩余寿命 < refreshWindow → 刷新。
//
// 注：refreshWindow 由 TokenRefreshService 传入（配置的 RefreshBeforeExpiryHours）。
// 对 45 天寿命的 token，默认窗口（通常几小时）触发得非常晚；运维可调大该值提前预热。
func (r *workbuddyTokenRefresher) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	if account == nil {
		return false
	}
	creds := account.GetWorkbuddyCredentials()
	if strings.TrimSpace(creds.RefreshToken) == "" {
		return false
	}
	if strings.TrimSpace(creds.AccessToken) == "" {
		return true
	}
	if creds.ExpiresAt <= 0 {
		return false
	}
	// 统一走 workbuddyEffectiveRefreshSkew（min(24h, 寿命/4)，下限 10min）：
	// 既避开全局 30min 窗口对 45 天 token 的容错过窄问题，
	// 又不会因固定大窗口把短寿命 token 刷出风暴。与请求路径的预刷新判据同源，不另建一套。
	effective := workbuddyEffectiveRefreshSkew(creds)
	if refreshWindow < effective {
		refreshWindow = effective
	}
	return time.Until(time.Unix(creds.ExpiresAt, 0)) < refreshWindow
}

// Refresh 执行一次 token 换新，返回**完整**的新 credentials map（保留所有旧字段）。
//
// 与 workbuddyRefreshToken 的关键差异：
//   - 不复用 workbuddyRefreshLock：那把锁是请求路径专用的进程内锁，而本方法由
//     TokenRefreshService 在分布式锁（CacheKey）保护下调用，两层锁叠加只会增加死锁面；
//   - 不做写库：TokenRefreshService 负责落库（它统一走 refreshWithRetry 的持久化路径），
//     本方法只负责「拿到新凭据」这一件事，职责更单一。
func (r *workbuddyTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if account == nil || !account.IsWorkbuddy() {
		return nil, errors.New("workbuddy token refresh requires a workbuddy account")
	}
	creds := account.GetWorkbuddyCredentials()
	if strings.TrimSpace(creds.RefreshToken) == "" {
		return nil, fmt.Errorf("workbuddy account %d has no refresh token", account.ID)
	}
	baseURL := account.GetWorkbuddyBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("workbuddy account %d missing base_url", account.ID)
	}
	targetURL := strings.TrimRight(baseURL, "/") + workbuddyRefreshPath

	reqCtx, cancel := context.WithTimeout(ctx, workbuddyRefreshTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build workbuddy refresh request: %w", err)
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	applyWorkbuddyRefreshHeaders(req, creds, account, account.GetWorkbuddyRealm())

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := r.do(req, proxyURL, account)
	if err != nil {
		return nil, fmt.Errorf("workbuddy token refresh transport error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, workbuddyUpstreamErrorBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("read workbuddy refresh response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("workbuddy token refresh failed: HTTP %d: %s",
			resp.StatusCode, workbuddyTruncateForError(string(raw)))
	}
	var env workbuddyRefreshResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse workbuddy refresh response: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("workbuddy token refresh failed: code=%d msg=%s",
			env.Code, strings.TrimSpace(env.Msg))
	}
	if strings.TrimSpace(env.Data.AccessToken) == "" {
		return nil, fmt.Errorf("workbuddy token refresh failed: no accessToken in response — re-login required")
	}

	// 保留全部旧字段（model_mapping / realm / uid / enterprise_id 等），只覆盖 token 相关键。
	next := shallowCopyMap(account.Credentials)
	next["access_token"] = env.Data.AccessToken
	if rt := strings.TrimSpace(env.Data.RefreshToken); rt != "" {
		next["refresh_token"] = rt
	}
	// expiresIn 优先（上游明文）；缺失或超量级（脏数据）时从新 token 的 exp 声明兜底。
	// 这一点比旧实现更稳：旧实现只在 expiresIn 有效时才写 expires_at，否则留空，
	// 而「留空」正是本次 401 事件的起点（空 expires_at ⇒ 永不预刷新）。
	if env.Data.ExpiresIn > 0 && time.Duration(env.Data.ExpiresIn)*time.Second < workbuddyRefreshExpiresInMax {
		next["expires_at"] = time.Now().Add(time.Duration(env.Data.ExpiresIn) * time.Second).Unix()
	} else if exp := workbuddyJWTExpiry(env.Data.AccessToken); exp > 0 {
		next["expires_at"] = exp
	}
	if d := strings.TrimSpace(env.Data.Domain); d != "" {
		next["domain"] = d
	}
	// uid 缺失时用新 token 的 sub 自证（与读路径的兜底同口径）。
	if strings.TrimSpace(workbuddyCredentialValueString(next["uid"])) == "" {
		if sub := workbuddyJWTSubject(env.Data.AccessToken); sub != "" {
			next["uid"] = sub
		}
	}
	logger.LegacyPrintf("service.token_refresh", "[WorkBuddy] token refreshed for account %d", account.ID)
	return next, nil
}

// do 经注入的 HTTPUpstream 出站；未注入时回落到 http.DefaultClient。
// TLS 指纹按「平台默认开启」语义解析（platformDefaultTLSFor 已收敛到唯一实现，
// 与聊天主链路同一判据）。
func (r *workbuddyTokenRefresher) do(req *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if r == nil || r.httpUpstream == nil {
		return http.DefaultClient.Do(req)
	}
	concurrency := maxInt(account.Concurrency, 1)
	platformDefault := platformDefaultTLSFor(account, r.cfg)
	if profile := resolvePlatformDefaultTLSProfile(r.profileSvc, account, platformDefault); profile != nil {
		slog.Debug("workbuddy_token_refresh_tls",
			"account_id", account.ID, "profile", profile.Name)
		return r.httpUpstream.DoWithTLS(req, proxyURL, account.ID, concurrency, profile)
	}
	return r.httpUpstream.Do(req, proxyURL, account.ID, concurrency)
}
