package service

// trae_client.go Trae 聊天域出站发送：预刷新 → 协议变换 → 主端点（404 回落次端点）
// → 401 刷新重试一次 → 流式规范化 / 非流式聚合。
//
// 与 workbuddy_client.go 同构，两处刻意差异：
//  1. Trae 有两个候选对话端点（llm_utils_chat 与 ide/v1/chat），报文形态不同，故
//     回退单位是「(路径, 请求体)」而非仅路径；且只在 HTTP 404 时回退——4xx 业务错误
//     回退等于同一 prompt 扣两次积分；
//  2. token 刷新走 OAuth 域（ExchangeToken），与聊天域不同主机；refreshToken 会轮换，
//     每次成功换票必须把新 refreshToken 落库，否则下次刷新必失败。

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
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const (
	// traeUpstreamErrorBodyLimit 错误体读取上限（1MB）。
	traeUpstreamErrorBodyLimit int64 = 1 << 20
	// traeAggregateReadLimit 非流式聚合的整流体上限（16MB）。
	traeAggregateReadLimit int64 = 16 << 20
	// traeRefreshTimeout 换票请求超时。
	traeRefreshTimeout = 20 * time.Second
	// traeTokenRefreshSkew 提前刷新窗口：上游 access token 寿命 3~14 天且不可自
	// 续期，提前 24h 换票可避免长会话中途 401。
	traeTokenRefreshSkew = 24 * time.Hour
)

// traeRefreshLocks 同一账号并发刷新互斥（按 account ID）。请求路径的预刷新与 401
// 重试刷新共用：锁内双检（token 快照变化即认为他人已完成），同一账号并发刷新只做一次。
var traeRefreshLocks sync.Map // key: int64(accountID), value: *contextMutex

// traeRefreshLock 返回账号级刷新锁（不存在则惰性创建）。
func traeRefreshLock(accountID int64) *contextMutex {
	actual, _ := traeRefreshLocks.LoadOrStore(accountID, newContextMutex())
	mu, ok := actual.(*contextMutex)
	if !ok {
		mu = newContextMutex()
		traeRefreshLocks.Store(accountID, mu)
	}
	return mu
}

// traeTokenNeedsRefresh 报告 access_token 是否需要（提前）刷新：无 token 或已临近
// 过期（expires_at 未知时不判定为需刷新，交由 401 路径兜底）。
func traeTokenNeedsRefresh(creds TraeCredentials, now time.Time) bool {
	if strings.TrimSpace(creds.AccessToken) == "" {
		return true
	}
	if creds.ExpiresAt <= 0 {
		return false
	}
	return now.Add(traeTokenRefreshSkew).Unix() >= creds.ExpiresAt
}

// sendTraeUpstreamRequest 向 Trae 上游发送一次对话请求，返回已归一为 OpenAI CC
// 形态的响应（流式：规范化 SSE 流；非流式：聚合后的 CC JSON 假 resp）。
//
// 流程：
//  1. 取凭据；access_token 缺失/临近过期且有 refresh_token → 先换票；
//  2. 构造两条候选报文（llm_utils_chat 主、ide/v1/chat 回退）；
//  3. 目标 URL = 账号聊天域 base_url + 候选路径（经 validateUpstreamBaseURL 校验）；
//  4. detachUpstreamContext + OpenAI HTTP profile + Trae 聊天域全套头；
//  5. 404 → 换下一候选端点；401 且有 refresh_token → 刷新一次后重试一次；
//  6. <400 → 流式包装规范化 SSE / 非流式聚合为 CC JSON。
func (s *OpenAIGatewayService) sendTraeUpstreamRequest(ctx context.Context, c *gin.Context, account *Account, ccBody []byte, clientStream bool) (*http.Response, error) {
	if account == nil || !account.IsTrae() {
		return nil, fmt.Errorf("trae upstream request requires a trae account")
	}
	creds := account.GetTraeCredentials()
	// 1) 预刷新（失败且无可用 token 时中止；有旧 token 则宽容放行，上游拒绝走 401 重试）。
	if traeTokenNeedsRefresh(creds, time.Now()) && strings.TrimSpace(creds.RefreshToken) != "" {
		if err := s.refreshTraeToken(ctx, account); err != nil {
			logger.L().Warn("trae pre-flight token refresh failed",
				zap.Int64("account_id", account.ID), zap.Error(err))
			creds = account.GetTraeCredentials()
			if strings.TrimSpace(creds.AccessToken) == "" {
				return nil, fmt.Errorf("trae account %d has no usable access token: %w", account.ID, err)
			}
		} else {
			creds = account.GetTraeCredentials()
		}
	}
	if strings.TrimSpace(creds.AccessToken) == "" {
		return nil, fmt.Errorf("trae account %d has no access token (提供 refresh_token 以自动换票)", account.ID)
	}
	// 2) 候选报文。
	attempts, err := traeChatAttempts(ccBody, creds, account.ID)
	if err != nil {
		return nil, fmt.Errorf("build trae upstream payload: %w", err)
	}
	// 3) 目标基址。
	baseURL := account.GetTraeBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("trae account %d missing base_url", account.ID)
	}
	validatedBase, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid trae base_url: %w", err)
	}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	current := creds
	send := func(attempt traeAttempt) (*http.Response, error) {
		targetURL := strings.TrimRight(validatedBase, "/") + attempt.path
		upstreamCtx, release := detachUpstreamContext(ctx)
		req, reqErr := http.NewRequestWithContext(upstreamCtx, http.MethodPost, targetURL, bytes.NewReader(attempt.body))
		release()
		if reqErr != nil {
			return nil, fmt.Errorf("build trae upstream request: %w", reqErr)
		}
		req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
		applyTraeChatHeaders(req, current, account)
		return s.doOpenAIUpstream(req, proxyURL, account)
	}

	resp, err := send(attempts[0])
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	// 5a) 端点回退：仅 404（路由不存在）换下一候选，业务错误不回退（防重复扣积分）。
	for i := 1; i < len(attempts) && resp.StatusCode == http.StatusNotFound; i++ {
		traeReadAndRewindBody(resp)
		_ = resp.Body.Close()
		current = account.GetTraeCredentials()
		next, nextErr := send(attempts[i])
		if nextErr != nil {
			return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, nextErr, false)
		}
		resp = next
	}
	// 5b) 401 刷新重试一次。
	if resp.StatusCode == http.StatusUnauthorized && strings.TrimSpace(current.RefreshToken) != "" {
		traeReadAndRewindBody(resp)
		if refreshErr := s.refreshTraeToken(ctx, account); refreshErr != nil {
			logger.L().Warn("trae 401 retry token refresh failed",
				zap.Int64("account_id", account.ID), zap.Error(refreshErr))
			return resp, nil
		}
		current = account.GetTraeCredentials()
		retryResp, retryErr := send(attempts[0])
		if retryErr != nil {
			return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, retryErr, false)
		}
		if retryResp.StatusCode >= 400 {
			traeReadAndRewindBody(retryResp)
			return retryResp, nil
		}
		resp = retryResp
	} else if resp.StatusCode >= 400 {
		traeReadAndRewindBody(resp)
		return resp, nil
	}
	// 6) 成功路径。
	endpoint := attempts[0].path
	if resp.Request != nil && resp.Request.URL != nil {
		endpoint = resp.Request.URL.Path
	}
	SetActualOpenAIUpstreamEndpoint(c, endpoint)
	// HTTP 200 + 流内 event:error 是 Trae 上游的主要拒绝形态（4001/1005/1001 等）。
	// 两条消费路径均接入分级落状态（口径对齐 qoder_client.go）：流式经 reader 回调；
	// 非流式在聚合失败后检查错误对象。
	if clientStream {
		resp.Body = io.NopCloser(traeNewSSEReaderWithErrorHook(resp.Body,
			traeStreamMeta{Model: traeRequestedModel(ccBody)},
			func(code, message string) {
				// 回调运行于下游读流 goroutine（脱离入站 ctx 取消链），落状态使用分离
				// context，避免客户端提前断开时丢标记。
				healCtx, releaseHealCtx := detachUpstreamContext(ctx)
				defer releaseHealCtx()
				s.markTraeAccountFromBusinessError(healCtx, c, account, code, message)
			}))
		resp.Header.Set("Content-Type", "text/event-stream")
		return resp, nil
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, traeAggregateReadLimit))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read trae upstream stream: %w", readErr)
	}
	aggregated, aggErr := traeAggregateSSE(bytes.NewReader(raw), traeStreamMeta{Model: traeRequestedModel(ccBody)})
	if aggErr != nil {
		var streamErr *traeStreamError
		if errors.As(aggErr, &streamErr) {
			healCtx, releaseHealCtx := detachUpstreamContext(ctx)
			defer releaseHealCtx()
			s.markTraeAccountFromBusinessError(healCtx, c, account, streamErr.Code, streamErr.Message)
		}
		return nil, aggErr
	}
	resp.Body = io.NopCloser(bytes.NewReader(aggregated))
	resp.Header = resp.Header.Clone()
	resp.Header.Set("Content-Type", "application/json")
	resp.StatusCode = http.StatusOK
	resp.Status = "200 OK"
	resp.ContentLength = int64(len(aggregated))
	return resp, nil
}

// traeRequestedModel 从入站 CC body 取模型名（用于归一化响应回填 model 字段）。
func traeRequestedModel(ccBody []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(ccBody, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Model)
}

// traeReadAndRewindBody 读取上游错误体（1MB 上限）后把 resp.Body 回卷为可重读副本
// （上层错误处理链需要再次读取），返回读到的原始字节。
func traeReadAndRewindBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, traeUpstreamErrorBodyLimit))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return body
}

// traeTruncateForError 错误信息截断（避免把整个上游 body 塞进日志/响应）。
//
// 两处约束：
//  1. **按 rune 截**：上游错误体多为中文（「当前参与用户太多」），按字节切会在
//     多字节中间断开产出 U+FFFD，而这些字符串会写进账号 error_message 并在管理页
//     反复渲染；
//  2. **先脱敏再截**：该产物会流入 slog / 账号 error_message / 三个管理端响应，
//     而上游异常形态的 body（如 token 出现在非预期字段）可能含凭证，过一遗 logredact。
func traeTruncateForError(s string) string {
	const maxRunes = 200
	s = logredact.RedactText(strings.TrimSpace(s))
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

// traeExchangeTokenResponse ExchangeToken 响应信封（字段名是大写开头的上游实样）。
type traeExchangeTokenResponse struct {
	Result struct {
		Token                 string `json:"Token"`
		TokenExpireAt         int64  `json:"TokenExpireAt"`       // epoch 毫秒
		RefreshToken          string `json:"RefreshToken"`        // 轮换后的新 refreshToken
		RefreshExpireAt       int64  `json:"RefreshExpireAt"`     // epoch 毫秒
		TokenExpireDuration   int64  `json:"TokenExpireDuration"` // 秒（部分版本仅此字段）
		RefreshExpireDuration int64  `json:"RefreshExpireDuration"`
	} `json:"Result"`
}

// refreshTraeToken 用 refreshToken 换取新 access token 并写回凭据（账号级锁 + 锁内双检）。
//
// refreshToken 会轮换：响应里带新值时必须覆盖落库（旧值一次性），否则下次刷新必失败。
func (s *OpenAIGatewayService) refreshTraeToken(ctx context.Context, account *Account) error {
	refresher := &traeTokenRefresher{
		repo: s.accountRepo,
		validate: func(rawURL string) (string, error) {
			return s.validateUpstreamBaseURL(rawURL)
		},
		do: func(ctx context.Context, req *http.Request, account *Account) (*http.Response, error) {
			proxyURL := ""
			if account.ProxyID != nil && account.Proxy != nil {
				proxyURL = account.Proxy.URL()
			}
			return s.doOpenAIUpstream(req, proxyURL, account)
		},
	}
	return refresher.refresh(ctx, account)
}
