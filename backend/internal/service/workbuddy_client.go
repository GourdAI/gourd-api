package service

// workbuddy_client.go WorkBuddy 上游客户端：chat 出站发送与 token 刷新
// （移植自 workbuddy2api/internal/upstream/client.go 的 ChatStreamContext / RefreshToken，
// 发送模式对齐 sub2api 的 sendCCUpstreamRequest：detachUpstreamContext、
// WithHTTPUpstreamProfile(HTTPUpstreamProfileOpenAI)、doOpenAIUpstream、
// handleOpenAIUpstreamTransportError）。
//
// 上游只支持流式：请求体恒被 PrepareWorkbuddyBody 强制 stream:true；
// clientStream=false（客户端要非流式）时网关读取全流聚合为单个 CC JSON 响应。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const (
	// workbuddyTokenRefreshSkew 预刷新窗口：access_token 距过期不足该时长时先刷新。
	workbuddyTokenRefreshSkew = 10 * time.Minute

	// workbuddyRefreshTimeout 单次 refresh 网络调用的总时长上限。
	workbuddyRefreshTimeout = 30 * time.Second

	// workbuddyRefreshExpiresInMax refresh 响应 expiresIn 的量级上限（10 年，纯防御值：
	// 超限视为上游脏数据，不写 expires_at，防止 NeedsRefresh 永假导致 token 永不刷新）。
	workbuddyRefreshExpiresInMax = 10 * 365 * 24 * time.Hour

	// workbuddyUpstreamErrorBodyLimit 上游错误体读取上限（错误透传只需摘要）。
	workbuddyUpstreamErrorBodyLimit int64 = 1 << 20

	// workbuddyAggregateReadLimit 非流式聚合路径的整流体读取上限
	// （16MB 已远超最大输出上限对应的文本量）。
	workbuddyAggregateReadLimit int64 = 16 << 20
)

// workbuddyRefreshLocks 同一账号并发刷新互斥（按 account ID）。
// 请求路径的预刷新与 401 重试刷新共用：锁内双检（token 快照变化即认为他人已完成），
// 同一账号并发刷新只做一次。
var workbuddyRefreshLocks sync.Map // key: int64(accountID), value: *contextMutex

// workbuddyRefreshLock 返回账号级刷新锁（不存在则惰性创建）。
func workbuddyRefreshLock(accountID int64) *contextMutex {
	actual, _ := workbuddyRefreshLocks.LoadOrStore(accountID, newContextMutex())
	mu, ok := actual.(*contextMutex)
	if !ok {
		mu = newContextMutex()
		workbuddyRefreshLocks.Store(accountID, mu)
	}
	return mu
}

// workbuddyTokenNeedsRefresh 报告凭据是否需要刷新：
//   - access_token 为空 → 必须刷新；
//   - expires_at 缺失/非正 → 不主动刷新（无过期信息时避免把有效 token 刷掉）；
//   - 距过期不足 workbuddyTokenRefreshSkew → 刷新。
func workbuddyTokenNeedsRefresh(creds WorkbuddyCredentials, now time.Time) bool {
	if strings.TrimSpace(creds.AccessToken) == "" {
		return true
	}
	if creds.ExpiresAt <= 0 {
		return false
	}
	return now.Add(workbuddyTokenRefreshSkew).Unix() >= creds.ExpiresAt
}

// sendWorkbuddyUpstreamRequest 构建并发送 WorkBuddy chat 上游请求。
//
// 流程：
//  1. 取凭据；access_token 为空或临近过期且有 refresh_token → 先刷新；
//  2. PrepareWorkbuddyBody 变换 body（强制 stream、协议归一、cache key、global 兜底 system）；
//  3. 目标 URL = 账号 base_url + /v2/chat/completions（经 validateUpstreamBaseURL 校验）；
//  4. 分离上游 context + OpenAI HTTP profile + 全套 workbuddy 头（会话头族从 gin.Context 透传）；
//  5. >=400：读 body（1MB 上限）后回卷；401 且有 refresh_token → 刷新一次后重试一次
//     （重试仍失败原样返回 resp 交由上层处理）；非 401 直接返回；
//  6. <400：clientStream=true 包装规范化 SSE 流；false 读取全流聚合为 CC JSON 假 resp。
func (s *OpenAIGatewayService) sendWorkbuddyUpstreamRequest(ctx context.Context, c *gin.Context, account *Account, ccBody []byte, clientStream bool) (*http.Response, error) {
	if account == nil || !account.IsWorkbuddy() {
		return nil, fmt.Errorf("workbuddy upstream request requires a workbuddy account")
	}
	realm := account.GetWorkbuddyRealm()
	creds := account.GetWorkbuddyCredentials()
	// 1) 预刷新：临近过期先换 token（失败且无可用 token 时中止；有旧 token 则宽容放行，
	//    上游若拒绝会走 401 重试路径）。
	if workbuddyTokenNeedsRefresh(creds, time.Now()) && strings.TrimSpace(creds.RefreshToken) != "" {
		if err := s.refreshWorkbuddyToken(ctx, account); err != nil {
			logger.L().Warn("workbuddy pre-flight token refresh failed",
				zap.Int64("account_id", account.ID), zap.Error(err))
			creds = account.GetWorkbuddyCredentials()
			if strings.TrimSpace(creds.AccessToken) == "" {
				return nil, fmt.Errorf("workbuddy account %d has no usable access token: %w", account.ID, err)
			}
		} else {
			creds = account.GetWorkbuddyCredentials()
		}
	}
	// 2) body 变换：realm 归一化 + uid 兜底（空 uid 用 acct-<id>，保证 cache key 账号隔离）。
	payloadCreds := creds
	payloadCreds.Realm = realm
	if strings.TrimSpace(payloadCreds.UID) == "" {
		payloadCreds.UID = workbuddyStableUID(payloadCreds, account)
	}
	prepared := PrepareWorkbuddyBody(ccBody, payloadCreds)
	// 3) 目标 URL：账号 base_url + chat 路径，出站前经安全校验。
	baseURL := account.GetWorkbuddyBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("workbuddy account %d missing base_url", account.ID)
	}
	validatedBase, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid workbuddy base_url: %w", err)
	}
	targetURL := strings.TrimRight(validatedBase, "/") + workbuddyChatPath

	// 会话头族元数据：入站透传优先；本函数内多次出站（401 重试）复用同一聚合主键。
	meta := resolveWorkbuddyChatMeta(c, prepared)
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	buildRequest := func(current WorkbuddyCredentials) (*http.Request, error) {
		upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
		upstreamReq, reqErr := http.NewRequestWithContext(upstreamCtx, http.MethodPost, targetURL, bytes.NewReader(prepared))
		releaseUpstreamCtx()
		if reqErr != nil {
			return nil, fmt.Errorf("build workbuddy upstream request: %w", reqErr)
		}
		upstreamReq = upstreamReq.WithContext(WithHTTPUpstreamProfile(upstreamReq.Context(), HTTPUpstreamProfileOpenAI))
		applyWorkbuddyChatHeaders(upstreamReq, current, account, realm, meta)
		return upstreamReq, nil
	}
	// 4) 首次发送。
	upstreamReq, err := buildRequest(creds)
	if err != nil {
		return nil, err
	}
	resp, err := s.doOpenAIUpstream(upstreamReq, proxyURL, account)
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	// 5) >=400 错误路径。
	if resp.StatusCode >= 400 {
		workbuddyReadAndRewindBody(resp)
		if resp.StatusCode == http.StatusUnauthorized && strings.TrimSpace(creds.RefreshToken) != "" {
			// 401：刷新一次 token 后重建请求重试一次。
			if refreshErr := s.refreshWorkbuddyToken(ctx, account); refreshErr != nil {
				logger.L().Warn("workbuddy 401 retry token refresh failed",
					zap.Int64("account_id", account.ID), zap.Error(refreshErr))
				return resp, nil
			}
			refreshed := account.GetWorkbuddyCredentials()
			retryReq, buildErr := buildRequest(refreshed)
			if buildErr != nil {
				logger.L().Warn("workbuddy 401 retry request build failed",
					zap.Int64("account_id", account.ID), zap.Error(buildErr))
				return resp, nil
			}
			retryResp, retryErr := s.doOpenAIUpstream(retryReq, proxyURL, account)
			if retryErr != nil {
				return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, retryErr, false)
			}
			if retryResp.StatusCode >= 400 {
				// 重试仍失败：读 body 后回卷，原样返回交由上层处理（错误透传语义）。
				workbuddyReadAndRewindBody(retryResp)
				return retryResp, nil
			}
			// 重试成功：替换 resp 后落入下方成功路径，与首次成功同样执行 SSE 规范化
			// （流式）/ 整流体聚合（非流式）。早期实现在此直接 return，导致重试成功的
			// 非流式请求把原始 SSE 文本当 JSON 回给客户端、流式请求绕过规范化保障。
			resp = retryResp
		} else {
			return resp, nil
		}
	}
	// 6) <400 成功路径。
	if clientStream {
		// 流式：包装为规范化 SSE 流（Content-Type 提示下游按 SSE 透传）。
		resp.Body = io.NopCloser(newWorkbuddySSEReader(resp.Body))
		resp.Header.Set("Content-Type", "text/event-stream")
		return resp, nil
	}
	// 非流式：读取全流（有界）聚合为单个 CC JSON 响应，构造假 resp 返回。
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, workbuddyAggregateReadLimit))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read workbuddy upstream stream: %w", readErr)
	}
	aggregated, usage, aggErr := aggregateWorkbuddySSE(bytes.NewReader(raw))
	if aggErr != nil {
		return nil, aggErr
	}
	// usage 已内嵌于聚合 JSON（body.usage，含补全的 total_tokens），由上层解析；
	// 此处返回值供测试与后续集成使用。
	_ = usage
	resp.Body = io.NopCloser(bytes.NewReader(aggregated))
	resp.Header = resp.Header.Clone()
	resp.Header.Set("Content-Type", "application/json")
	resp.StatusCode = http.StatusOK
	resp.Status = "200 OK"
	resp.ContentLength = int64(len(aggregated))
	return resp, nil
}

// workbuddyReadAndRewindBody 读取上游错误体（1MB 上限）后把 resp.Body 回卷为
// 可重读副本（上层错误处理链需要再次读取），返回读取到的原始字节。
func workbuddyReadAndRewindBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, workbuddyUpstreamErrorBodyLimit))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return body
}

// workbuddyRefreshResponse refresh 接口的响应信封：
// {code, msg, data:{accessToken, refreshToken, expiresIn, domain}}。
type workbuddyRefreshResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	} `json:"data"`
}

// refreshWorkbuddyToken 刷新账号 access token 并写回凭据。
//
// 并发控制：按账号 ID 的锁 + 锁内双检（锁等待期间 access_token 快照变化 → 认为
// 另一 goroutine 已完成刷新，直接返回；与现有 refresher 的「并发刷新只做一次」一致）。
//
// 成功时更新 account.Credentials（access_token/refresh_token/expires_at/domain，
// 缺省字段保留旧值）并通过 persistAccountCredentials 写库；写库失败返回错误，
// 但内存凭据已更新（请求路径可继续使用新 token）。
func (s *OpenAIGatewayService) refreshWorkbuddyToken(ctx context.Context, account *Account) error {
	if account == nil || !account.IsWorkbuddy() {
		return fmt.Errorf("workbuddy token refresh requires a workbuddy account")
	}
	snapshot := strings.TrimSpace(account.GetWorkbuddyCredentials().AccessToken)
	lock := workbuddyRefreshLock(account.ID)
	if err := lock.Lock(ctx); err != nil {
		return err
	}
	defer lock.Unlock()
	// 锁内双检：token 快照已被他人改写 → 并发刷新已完成，本次不重复刷新。
	if strings.TrimSpace(account.GetWorkbuddyCredentials().AccessToken) != snapshot {
		return nil
	}
	creds := account.GetWorkbuddyCredentials()
	refreshToken := strings.TrimSpace(creds.RefreshToken)
	if refreshToken == "" {
		return fmt.Errorf("workbuddy account %d has no refresh token", account.ID)
	}
	baseURL := account.GetWorkbuddyBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		return fmt.Errorf("workbuddy account %d missing base_url", account.ID)
	}
	validatedBase, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return fmt.Errorf("invalid workbuddy base_url: %w", err)
	}
	targetURL := strings.TrimRight(validatedBase, "/") + workbuddyRefreshPath

	reqCtx, cancel := context.WithTimeout(ctx, workbuddyRefreshTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, nil)
	if err != nil {
		return fmt.Errorf("build workbuddy refresh request: %w", err)
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	applyWorkbuddyRefreshHeaders(req, creds, account, account.GetWorkbuddyRealm())
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.doOpenAIUpstream(req, proxyURL, account)
	if err != nil {
		return fmt.Errorf("workbuddy token refresh transport error: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, workbuddyUpstreamErrorBodyLimit))
	if err != nil {
		return fmt.Errorf("read workbuddy refresh response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("workbuddy token refresh failed: HTTP %d: %s", resp.StatusCode, workbuddyTruncateForError(string(raw)))
	}
	var env workbuddyRefreshResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("parse workbuddy refresh response: %w", err)
	}
	if env.Code != 0 {
		return fmt.Errorf("workbuddy token refresh failed: code=%d msg=%s", env.Code, strings.TrimSpace(env.Msg))
	}
	if strings.TrimSpace(env.Data.AccessToken) == "" {
		return fmt.Errorf("workbuddy token refresh failed: no accessToken in response — re-login required")
	}
	// 写回：先更新内存凭据（请求路径立即可用），再持久化到账号库。
	next := shallowCopyMap(account.Credentials)
	next["access_token"] = env.Data.AccessToken
	if rt := strings.TrimSpace(env.Data.RefreshToken); rt != "" {
		next["refresh_token"] = rt
	}
	// preserveExpiry：响应缺 expiresIn 或超量级（脏数据）时保留旧过期时间，避免过期判定漂移。
	if env.Data.ExpiresIn > 0 && time.Duration(env.Data.ExpiresIn)*time.Second < workbuddyRefreshExpiresInMax {
		next["expires_at"] = time.Now().Add(time.Duration(env.Data.ExpiresIn) * time.Second).Unix()
	}
	if d := strings.TrimSpace(env.Data.Domain); d != "" {
		next["domain"] = d
	}
	account.Credentials = next
	if err := persistAccountCredentials(ctx, s.accountRepo, account, account.Credentials); err != nil {
		return fmt.Errorf("persist workbuddy credentials: %w", err)
	}
	return nil
}

// workbuddyTruncateForError 错误消息里的上游 body 摘要（截断到 200 字符，
// 与仓库错误透传链路的既有口径一致）。
func workbuddyTruncateForError(s string) string {
	s = strings.TrimSpace(s)
	const maxLen = 200
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// workbuddyIsEmptyStreamError 报告错误是否为「上游空流」（无有效 SSE 帧）——
// 供上层在非流式聚合路径把空流记为 502 upstream_parse 观测。
func workbuddyIsEmptyStreamError(err error) bool {
	return errors.Is(err, errWorkbuddyEmptyStream)
}
