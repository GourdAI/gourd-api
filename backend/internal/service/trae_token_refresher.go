package service

// trae_token_refresher.go Trae 后台定时刷新器（TokenRefreshService 注册项）。
//
// 【为什么必须存在（与 2026-10-24 WorkBuddy 事件、2026-10-29 Qoder 签到事件同一个坑）】
// 本次改造前 Trae 的续期能力只有「请求路径预刷新 + 撞 401 重试一次」这一层，它有三个缺口：
//  1. TokenRefreshService 的注册表里**没有 Trae**（只有 Anthropic/OpenAI/Gemini/
//     Antigravity/Grok 五家，加 WorkBuddy 共六家）。⇒ 没有任何角色会周期性地续命，
//     只能等某次真实用户请求把它撞出来。
//  2. 预刷新的前提是凭据里有 expires_at。手工粘贴 / 旧版前端创建的账号常常没有该键，
//     traeTokenNeedsRefresh 对 ExpiresAt<=0 直接 return false ⇒ 预刷新**永不触发**。
//     （本轮已在 GetTraeCredentials 里补 JWT exp 兜底，两条路径从此同源。）
//  3. 一旦真的撞上 401 或流内 1001，Trae 是 **apikey 类型**而 401 分支历史只按
//     Type==OAuth 分流 ⇒ 落到 handleAuthError 的永久 SetError（status=error +
//     schedulable=false），账号直接下线且刷新链里没有恢复路径。
// 三者叠加 = 「access_token 到期后全量报错，且无法自愈」。本文件补第 1 个缺口，
// 第 2/3 个缺口分别由 trae.go 的 JWT 兜底与 ratelimit_service/trae_upstream_error
// 的临时冷却修复。三者必须同时到位，少任何一个都还是「会烂且救不回来」。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// traeTokenRefresherBackend 是 Trae 平台的后台刷新策略实现。
//
// 传输层走 HTTPUpstream（而不是直接 http.Client）：与聊天主链路同一条出站通道，
// 因此账号代理、TLS 指纹、限速闸门、连接池全部复用，不会出现「后台刷新裸奔、
// 聊天带指纹」的身份不一致。
type traeTokenRefresherBackend struct {
	httpUpstream HTTPUpstream
	profileSvc   *TLSFingerprintProfileService
	cfg          *config.Config
}

// NewTraeTokenRefresher 创建 Trae 后台刷新器。
//
// 返回 OAuthRefreshExecutor（= TokenRefresher + CacheKey）：后台刷新周期需要 executor
// 来做分布式锁，光有 TokenRefresher 不够。
//
// 参数可为 nil（对应能力自动降级）：
//   - httpUpstream 为 nil ⇒ 走 http.DefaultClient 兜底；
//   - profileSvc 为 nil ⇒ 不做 TLS 指纹伪装（与 resolvePlatformDefaultTLSProfile 的 nil 语义一致）。
func NewTraeTokenRefresher(httpUpstream HTTPUpstream, profileSvc *TLSFingerprintProfileService, cfg *config.Config) OAuthRefreshExecutor {
	return &traeTokenRefresherBackend{httpUpstream: httpUpstream, profileSvc: profileSvc, cfg: cfg}
}

// CacheKey 返回用于分布式锁的缓存键（与其它平台的关键字空间隔离）。
func (r *traeTokenRefresherBackend) CacheKey(account *Account) string {
	if account == nil {
		return "trae_token:0"
	}
	return fmt.Sprintf("trae_token:%d", account.ID)
}

// CanRefresh 报告本刷新器能否处理该账号。
//
// 判据（两条同时满足）：
//   - 平台是 Trae；
//   - 凭据里有非空 refresh_token（没有就真的不可自愈，让它在 401 分支走永久禁用是正确的）。
//
// 刻意不判 account.Type：Trae 恒为 apikey，但凭据语义与 OAuth 同构（短期 access +
// 可轮换 refresh）。也刻意不判 status/schedulable：暂停中（schedulable=false 但
// status=active）的账号仍持有有效 refresh_token，让它保持新鲜，管理员恢复调度时才能
// 立即可用 —— 与仓储 ListOAuthRefreshCandidatePage 里「Deliberately NO schedulable
// filter」的既有取舍同口径。
func (r *traeTokenRefresherBackend) CanRefresh(account *Account) bool {
	if account == nil || !account.IsTrae() {
		return false
	}
	return strings.TrimSpace(account.GetTraeCredentials().RefreshToken) != ""
}

// NeedsRefresh 报告该账号的 access_token 是否需要在本次周期内换新。
//
// 语义优先级：
//  1. access_token 为空 → 必须刷新（没有可用票据）；
//  2. refresh_token 为空 → 不可刷新（CanRefresh 已挡，此处防御）；
//  3. expires_at 缺失（JWT 也解不出 exp）→ **不主动刷新**，避免把有效 token 白换一次
//     （每次换票都会轮换 refresh_token，无谓换票会放大轮换失败的风险面）；
//  4. 剩余寿命 < 有效窗口 → 刷新。
//
// 有效窗口 = max(refreshWindow, traeTokenRefreshSkew)：traeTokenRefreshSkew 是请求路径
// 正在用的 24h 提前量（上游 access token 寿命 3~14 天），直接复用同一个常量而不是另建
// 一套，避免出现「后台换票早、请求路径晚」的裂缝。
func (r *traeTokenRefresherBackend) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	if account == nil {
		return false
	}
	creds := account.GetTraeCredentials()
	if strings.TrimSpace(creds.RefreshToken) == "" {
		return false
	}
	if strings.TrimSpace(creds.AccessToken) == "" {
		return true
	}
	if creds.ExpiresAt <= 0 {
		return false
	}
	effective := traeTokenRefreshSkew
	if refreshWindow > effective {
		effective = refreshWindow
	}
	return time.Until(time.Unix(creds.ExpiresAt, 0)) < effective
}

// Refresh 执行一次换票，返回**完整**的新 credentials map（保留所有旧字段）。
//
// 与请求路径 refreshTraeToken 的关键差异（刻意为之）：
//   - 不复用 traeRefreshLock：那把锁是请求路径专用的进程内锁，而本方法由
//     TokenRefreshService 在分布式锁（CacheKey）保护下调用，两层锁叠加只增加死锁面；
//   - 不做写库：TokenRefreshService 负责落库（统一走 refreshWithRetry 的持久化路径），
//     本方法只负责「拿到新凭据」这一件事。
func (r *traeTokenRefresherBackend) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if account == nil || !account.IsTrae() {
		return nil, errors.New("trae token refresh requires a trae account")
	}
	creds := account.GetTraeCredentials()
	if strings.TrimSpace(creds.RefreshToken) == "" {
		return nil, fmt.Errorf("trae account %d has no refresh token", account.ID)
	}
	oauthBase := account.GetTraeOAuthBaseURL()
	if strings.TrimSpace(oauthBase) == "" {
		return nil, fmt.Errorf("trae account %d missing oauth base_url", account.ID)
	}
	targetURL := strings.TrimRight(oauthBase, "/") + traeExchangeToken
	// 【出站前必须过 URL 安全策略（2026-10-30 自查补上）】oauth_base_url 来自凭据、
	// 可由管理员写入/由导入文件带入，开启 security.url_allowlist 时不过校验就是**绕过
	// 白名单的 SSRF 面**。同平台另外三条换票路径都已经做了，只有本文件（新增）漏了：
	//   - trae_client.go:298（聊天路径预刷新）→ validateUpstreamBaseURL
	//   - trae_gateway.go:274（账号测试）    → validateUpstreamBaseURL
	//   - trae_credits_service.go:563（积分/签到）→ cnValidateProbeURL
	// 这里用 cnValidateProbeURL：与积分/签到同口径（它读的也是 s.cfg，且不强加网关
	// 专属的 base_url 语义），并且它的返回值为归一化后的 URL（需采纳）。
	if validated, vErr := cnValidateProbeURL(r.cfg, targetURL); vErr != nil {
		return nil, fmt.Errorf("trae token refresh rejected by URL security policy: %w", vErr)
	} else {
		targetURL = validated
	}

	body, err := json.Marshal(map[string]any{
		"ClientID":     traeOAuthClientID(creds),
		"RefreshToken": strings.TrimSpace(creds.RefreshToken),
		"ClientSecret": "-",
		"UserID":       strings.TrimSpace(creds.UID),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal trae exchange request: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, traeRefreshTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build trae exchange request: %w", err)
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	// 第三个参数是「本次换票使用的 access_token 快照」，用于上游侧并发/设备判定；
	// 后台刷新没有请求上下文，传当前值即可（与请求路径 applyTraeRefreshHeaders 同口径）。
	applyTraeRefreshHeaders(req, creds, strings.TrimSpace(creds.AccessToken))

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := r.do(req, proxyURL, account)
	if err != nil {
		return nil, fmt.Errorf("trae token refresh transport error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, traeTokenBodyLenLimit))
	if err != nil {
		return nil, fmt.Errorf("read trae exchange response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("trae token refresh failed: HTTP %d: %s",
			resp.StatusCode, traeTruncateForError(string(raw)))
	}
	var parsed traeExchangeTokenResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse trae exchange response: %w", err)
	}
	token := strings.TrimSpace(parsed.Result.Token)
	if token == "" {
		return nil, fmt.Errorf("trae token refresh failed: no Token in exchange response — re-login required")
	}

	// 保留全部旧字段（realm / base_url / billing_base_url / device_id / machine_id /
	// ide_version_code 等），只覆盖 token 相关键。
	next := shallowCopyMap(account.Credentials)
	next["access_token"] = token
	if rt := strings.TrimSpace(parsed.Result.RefreshToken); rt != "" {
		next["refresh_token"] = rt // 轮换：必须覆盖旧值，否则下次换票必失败
	}
	// 过期时刻优先级：TokenExpireAt（绝对时刻）→ TokenExpireDuration（秒）→ 新 token 的 JWT exp。
	// 三者都没有就不写 —— 不伪造 expires_at：写一个猜的值会让预刷新判据失效得更隐蔽。
	// 注意这里必须从**新 token** 取 exp，不能沿用旧凭据里的 expires_at（那是本轮修过的
	// 同类缺陷：留旧值会让「刚刷完又认为不需刷」）。
	if expiresAt := traeExchangeExpiresAt(parsed); expiresAt > 0 {
		next["expires_at"] = expiresAt
	} else if exp := traeJWTExpiry(token); exp > 0 {
		next["expires_at"] = exp
	}
	if refreshExpire := traeExchangeRefreshExpiresAt(parsed); refreshExpire > 0 {
		next["refresh_expires_at"] = refreshExpire
	}
	// uid 缺失时用新 token 自证（与读路径兜底同口径）。
	if strings.TrimSpace(creds.UID) == "" {
		if uid := traeJWTClaimString(token, "data", "id"); uid != "" {
			next["uid"] = uid
		}
	}
	logger.LegacyPrintf("service.token_refresh", "[Trae] token refreshed for account %d", account.ID)
	return next, nil
}

// do 经注入的 HTTPUpstream 出站；未注入时回落到 http.DefaultClient。
// TLS 指纹按「平台默认开启」语义解析（platformDefaultTLSFor 已收敛到唯一实现，
// 与聊天主链路同一判据）——Trae 拿到的会是 NodeOpenSSLH1Profile，原因见
// tlsfingerprint.NodeOpenSSLH2Profile 的告警（Trae CN 聊天域只支持 h1）。
func (r *traeTokenRefresherBackend) do(req *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if r == nil || r.httpUpstream == nil {
		return http.DefaultClient.Do(req)
	}
	concurrency := maxInt(account.Concurrency, 1)
	platformDefault := platformDefaultTLSFor(account, r.cfg)
	if profile := resolvePlatformDefaultTLSProfile(r.profileSvc, account, platformDefault); profile != nil {
		return r.httpUpstream.DoWithTLS(req, proxyURL, account.ID, concurrency, profile)
	}
	return r.httpUpstream.Do(req, proxyURL, account.ID, concurrency)
}
