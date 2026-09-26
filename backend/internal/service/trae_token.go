package service

// trae_token.go Trae OAuth 域换票（refreshToken → 新 access token）的共用实现。
//
// 三条链路（网关请求、积分/签到、连接测试）各自持有不同的出站传输层与 URL 校验器，
// 但换票语义完全一致，故收敛到此：账号级锁 + 锁内双检 + ExchangeToken + 凭据写回。
//
// 两条实测纪律：
//  1. refreshToken 会轮换 —— 响应带新值时必须覆盖落库，否则下次换票必失败；
//  2. TokenExpireAt 是 epoch 毫秒（部分版本只给 TokenExpireDuration 秒），统一归一
//     为秒写入 credentials.expires_at，供 traeTokenNeedsRefresh 判定提前刷新。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// traeTokenBodyLenLimit 换票响应体读取上限（换票响应极小）。
const traeTokenBodyLenLimit int64 = 1 << 20

// traeTokenTransport 换票出站传输：由调用方提供（网关 doOpenAIUpstream /
// 积分服务 HTTPUpstream.Do / 测试服务 doOpenAIAccountTestUpstream）。
type traeTokenTransport func(ctx context.Context, req *http.Request, account *Account) (*http.Response, error)

// traeTokenRefresher Trae 换票器。
type traeTokenRefresher struct {
	repo     AccountRepository
	validate func(rawURL string) (string, error)
	proxyURL func(*Account) string
	do       traeTokenTransport
}

// refresh 执行一次换票并写回凭据。
//
// 并发控制：按账号 ID 的锁 + 锁内双检（锁等待期间 access_token 快照变化 → 认为另一
// goroutine 已完成刷新，直接返回成功）。
func (r *traeTokenRefresher) refresh(ctx context.Context, account *Account) error {
	if account == nil || !account.IsTrae() {
		return fmt.Errorf("trae token refresh requires a trae account")
	}
	if r == nil || r.do == nil {
		return fmt.Errorf("trae token refresher is not configured")
	}
	snapshot := strings.TrimSpace(account.GetTraeCredentials().AccessToken)
	lock := traeRefreshLock(account.ID)
	if err := lock.Lock(ctx); err != nil {
		return err
	}
	defer lock.Unlock()
	// 锁内双检：token 快照已被他人改写 → 并发刷新已完成，本次不重复换票。
	if strings.TrimSpace(account.GetTraeCredentials().AccessToken) != snapshot {
		return nil
	}
	creds := account.GetTraeCredentials()
	refreshToken := strings.TrimSpace(creds.RefreshToken)
	if refreshToken == "" {
		return fmt.Errorf("trae account %d has no refresh token", account.ID)
	}
	oauthBase := account.GetTraeOAuthBaseURL()
	if strings.TrimSpace(oauthBase) == "" {
		return fmt.Errorf("trae account %d missing oauth base_url", account.ID)
	}
	targetURL := strings.TrimRight(oauthBase, "/") + traeExchangeToken
	if r.validate != nil {
		validated, err := r.validate(targetURL)
		if err != nil {
			return fmt.Errorf("invalid trae oauth base_url: %w", err)
		}
		targetURL = validated
	}
	body, err := json.Marshal(map[string]any{
		"ClientID":     traeOAuthClientID(creds),
		"RefreshToken": refreshToken,
		"ClientSecret": "-",
		"UserID":       strings.TrimSpace(creds.UID),
	})
	if err != nil {
		return fmt.Errorf("marshal trae exchange request: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, traeRefreshTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build trae exchange request: %w", err)
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	applyTraeRefreshHeaders(req, creds, snapshot)

	// proxyURL 只用于预加载账号代理（实际代理地址由各调用方传入的 do 传输层自行解析）。
	if r.proxyURL != nil {
		_ = r.proxyURL(account)
	}
	resp, err := r.do(reqCtx, req, account)
	if err != nil {
		return fmt.Errorf("trae token refresh transport error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, traeTokenBodyLenLimit))
	if err != nil {
		return fmt.Errorf("read trae exchange response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("trae token refresh failed: HTTP %d: %s", resp.StatusCode, traeTruncateForError(string(raw)))
	}
	var parsed traeExchangeTokenResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("parse trae exchange response: %w", err)
	}
	token := strings.TrimSpace(parsed.Result.Token)
	if token == "" {
		return fmt.Errorf("trae token refresh failed: no Token in exchange response — re-login required")
	}
	expiresAt := traeExchangeExpiresAt(parsed)
	// 写回：先更新内存凭据（本次请求立即可用），再持久化（失败返回错误，但内存已更新，
	// 与网关既有刷新路径同语义）。
	next := shallowCopyMap(account.Credentials)
	next["access_token"] = token
	if rt := strings.TrimSpace(parsed.Result.RefreshToken); rt != "" {
		next["refresh_token"] = rt // 轮换：必须覆盖旧值
	}
	if expiresAt > 0 {
		next["expires_at"] = expiresAt
	}
	if refreshExpire := traeExchangeRefreshExpiresAt(parsed); refreshExpire > 0 {
		next["refresh_expires_at"] = refreshExpire
	}
	if strings.TrimSpace(creds.UID) == "" {
		if uid := traeJWTClaimString(token, "data", "id"); uid != "" {
			next["uid"] = uid
		}
	}
	account.Credentials = next
	if r.repo != nil {
		if err := persistAccountCredentials(ctx, r.repo, account, account.Credentials); err != nil {
			return fmt.Errorf("persist trae credentials: %w", err)
		}
	}
	return nil
}

// traeExchangeExpiresAt 解析 access token 过期时刻（epoch 秒）：优先绝对时间戳
// （毫秒归一），否则用 now + TokenExpireDuration 秒。
func traeExchangeExpiresAt(resp traeExchangeTokenResponse) int64 {
	if v := resp.Result.TokenExpireAt; v > 0 {
		return traeNormalizeEpochSeconds(v)
	}
	if d := resp.Result.TokenExpireDuration; d > 0 && d < int64((365*24*time.Hour)/time.Second) {
		return time.Now().Unix() + d
	}
	return 0
}

// traeExchangeRefreshExpiresAt 解析 refresh token 过期时刻（epoch 秒）。
func traeExchangeRefreshExpiresAt(resp traeExchangeTokenResponse) int64 {
	if v := resp.Result.RefreshExpireAt; v > 0 {
		return traeNormalizeEpochSeconds(v)
	}
	if d := resp.Result.RefreshExpireDuration; d > 0 && d < int64((365*24*time.Hour)/time.Second) {
		return time.Now().Unix() + d
	}
	return 0
}
