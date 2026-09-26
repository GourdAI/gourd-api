package service

// workbuddy_gateway.go WorkBuddy 网关集成：CC 入站转发（forwardWorkbuddyChatCompletions）、
// 两条 CC 回退路径共用的发送分发（sendCCUpstreamForAccount）与账号连接测试
// （testWorkbuddyAccountConnection）。
//
// WorkBuddy 上游只提供 Chat Completions 形态的 /v2/chat/completions（无 /v1 前缀、
// 无 Responses / Anthropic 端点）：三条入站路径（/v1/chat/completions、/v1/messages、
// /v1/responses）统一收敛到各自的 raw-CC 回退函数，出站一律经
// sendWorkbuddyUpstreamRequest（协议变换、专用头、SSE 归一/非流式聚合在该函数内完成）。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// defaultWorkbuddyTestModel 账号连接测试的缺省模型（官方目录中的通用对话模型）。
const defaultWorkbuddyTestModel = "glm-5.3"

// forwardWorkbuddyChatCompletions 服务 /v1/chat/completions 入站的 WorkBuddy 账号。
//
// 与 forwardAsRawChatCompletions 的关键差异：
//   - 不做 grok/ollama 等平台专属处理，出站经 sendWorkbuddyUpstreamRequest
//     （workbuddy 协议变换、专用头、SSE 归一/非流式聚合均在其内）；
//   - 不调用 ApplyThinkingEnabledFallback：workbuddy 的 thinking 语义（deepseek
//     思维链注入等）在 payload 模块内处理；
//   - Responses 形状入站（Cursor 式客户端把 input 数组发到 CC URL）先转换为 CC。
func (s *OpenAIGatewayService) forwardWorkbuddyChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()
	beginUpstreamResponseModelObservation(c)

	// 1. 解析路由/计费所需的最小字段。
	originalModel := gjson.GetBytes(body, "model").String()
	if originalModel == "" {
		writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, fmt.Errorf("missing model in request")
	}
	clientStream := gjson.GetBytes(body, "stream").Bool()

	// 2. Responses 形状兼容：无 messages 但有 input（Cursor 等客户端把 Responses
	//    形状的 body 发到 /v1/chat/completions URL）→ 先转成 CC 再走后续管线。
	if !gjson.GetBytes(body, "messages").Exists() && gjson.GetBytes(body, "input").Exists() {
		var responsesReq apicompat.ResponsesRequest
		if err := json.Unmarshal(body, &responsesReq); err != nil {
			writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
			return nil, fmt.Errorf("parse responses-shaped chat completions request: %w", err)
		}
		chatReq, err := apicompat.ResponsesToChatCompletionsRequestWithOptions(
			&responsesReq,
			&apicompat.ResponsesToChatOptions{ReasoningContentByID: s.reasoningContentByID},
		)
		if err != nil {
			writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
			return nil, fmt.Errorf("convert responses-shaped chat completions request: %w", err)
		}
		converted, err := json.Marshal(chatReq)
		if err != nil {
			return nil, fmt.Errorf("marshal converted chat completions request: %w", err)
		}
		body = converted
	}

	// 3. 模型映射（与 ForwardAsChatCompletions 同链路）。
	billingModel := resolveOpenAIForwardModel(account, originalModel, defaultMappedModel)
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	SetOpsUpstreamModel(c, upstreamModel)

	upstreamBody := body
	if upstreamModel != originalModel {
		upstreamBody = ReplaceModelInBody(body, upstreamModel)
	}

	// workbuddy 的 thinking 语义在 payload 模块内处理，不做 ApplyThinkingEnabledFallback。
	reasoningEffort := extractOpenAIReasoningEffortFromBody(body, upstreamModel, billingModel, originalModel)
	serviceTier := extractOpenAIServiceTierFromBody(upstreamBody)

	SetActualOpenAIUpstreamEndpoint(c, workbuddyChatPath)

	// 4. 出站（workbuddy 协议变换/专用头/SSE 归一/非流式聚合在发送函数内完成）。
	resp, err := s.sendWorkbuddyUpstreamRequest(ctx, c, account, upstreamBody, clientStream)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	// 5. 错误响应：failover 判定优先，未命中走 CC 格式错误回写。
	if resp.StatusCode >= 400 {
		respBody, upstreamMsg := s.readOpenAIUpstreamError(resp)
		if foErr := s.failoverOpenAIUpstreamHTTPError(ctx, c, account, resp, respBody, upstreamMsg, upstreamModel); foErr != nil {
			return nil, foErr
		}
		return s.handleChatCompletionsErrorResponse(resp, c, account, billingModel)
	}

	// 6. 成功路径：流式透传 / 非流式聚合透传（发送函数已保证非流式假响应为 CC JSON）。
	var result *OpenAIForwardResult
	var forwardErr error
	if clientStream {
		result, forwardErr = s.streamRawChatCompletions(c, resp, account, originalModel, billingModel, upstreamModel, reasoningEffort, serviceTier, startTime, len(body))
	} else {
		result, forwardErr = s.bufferRawChatCompletions(c, resp, account, originalModel, billingModel, upstreamModel, reasoningEffort, serviceTier, startTime)
	}
	if result != nil {
		result.UpstreamEndpoint = workbuddyChatPath
	}
	return result, forwardErr
}

// sendCCUpstreamForAccount 分发两条 CC 回退路径（/v1/messages、/v1/responses）的上游
// 发送：WorkBuddy 账号走专用协议管线（sendWorkbuddyUpstreamRequest），Qoder/Trae
// 同理各自走专用管线，其余账号维持既有 CC 发送逻辑
// （resolveCCFallbackTarget + sendCCUpstreamRequest）。
func (s *OpenAIGatewayService) sendCCUpstreamForAccount(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	ccBody []byte,
	clientStream bool,
) (*http.Response, error) {
	if account.IsWorkbuddy() {
		return s.sendWorkbuddyUpstreamRequest(ctx, c, account, ccBody, clientStream)
	}
	if account.IsQoderPlatform() {
		return s.sendCCUpstreamForAccountQoder(ctx, c, account, ccBody, clientStream)
	}
	if account.IsTrae() {
		return s.sendCCUpstreamForAccountTrae(ctx, c, account, ccBody, clientStream)
	}
	apiKey, targetURL, err := s.resolveCCFallbackTarget(account)
	if err != nil {
		return nil, err
	}
	return s.sendCCUpstreamRequest(ctx, c, account, targetURL, ccBody, clientStream, apiKey, account.GetOpenAIUserAgent(), "")
}

// testWorkbuddyAccountConnection 测试 WorkBuddy 账号连通性：向
// {base_url}/v2/chat/completions 发送最小 CC 测试请求（workbuddy 全套头 + 协议
// 变换），流式读取规范化 SSE 直到 [DONE]。access_token 为空/临近过期且持有
// refresh_token 时先内联刷新一次（与请求路径同语义：失败且有旧 token 时宽容放行）。
func (s *AccountTestService) testWorkbuddyAccountConnection(c *gin.Context, account *Account, modelID string, prompt string) error {
	ctx := c.Request.Context()

	testModelID := strings.TrimSpace(modelID)
	if testModelID == "" {
		testModelID = defaultWorkbuddyTestModel
	}
	testModelID = account.GetMappedModel(testModelID)

	// token 预刷新：与 sendWorkbuddyUpstreamRequest 的预刷新语义一致。
	creds := account.GetWorkbuddyCredentials()
	if workbuddyTokenNeedsRefresh(creds, time.Now()) && strings.TrimSpace(creds.RefreshToken) != "" {
		if err := s.refreshWorkbuddyAccountToken(ctx, account); err != nil {
			logger.L().Warn("workbuddy account test pre-flight token refresh failed",
				zap.Int64("account_id", account.ID), zap.Error(err))
			creds = account.GetWorkbuddyCredentials()
			if strings.TrimSpace(creds.AccessToken) == "" {
				return s.sendErrorAndEnd(c, fmt.Sprintf("WorkBuddy token refresh failed: %s", err.Error()))
			}
		} else {
			creds = account.GetWorkbuddyCredentials()
		}
	}
	if strings.TrimSpace(creds.AccessToken) == "" {
		return s.sendErrorAndEnd(c, "No WorkBuddy access token available")
	}

	baseURL := account.GetWorkbuddyBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		return s.sendErrorAndEnd(c, "WorkBuddy base URL is not configured")
	}
	normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid base URL: %s", err.Error()))
	}
	apiURL := strings.TrimRight(normalizedBaseURL, "/") + workbuddyChatPath

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	payloadBytes, _ := json.Marshal(createOpenAIChatCompletionsTestPayload(testModelID, prompt))
	realm := account.GetWorkbuddyRealm()
	payloadCreds := creds
	payloadCreds.Realm = realm
	if strings.TrimSpace(payloadCreds.UID) == "" {
		payloadCreds.UID = workbuddyStableUID(payloadCreds, account)
	}
	prepared := PrepareWorkbuddyBody(payloadBytes, payloadCreds)

	s.sendEvent(c, TestEvent{Type: "test_start", Model: testModelID})
	s.sendEvent(c, TestEvent{Type: "status", Text: "正在通过 WorkBuddy /v2/chat/completions 测试连接"})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(prepared))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create WorkBuddy request")
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	applyWorkbuddyChatHeaders(req, creds, account, realm, resolveWorkbuddyChatMeta(c, prepared))

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.doOpenAIAccountTestUpstream(req, proxyURL, account, true)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("WorkBuddy API request failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, workbuddyUpstreamErrorBodyLimit))
		bodyText := workbuddyTruncateForError(string(body))
		if resp.StatusCode == http.StatusUnauthorized && s.accountRepo != nil {
			_ = s.accountRepo.SetError(ctx, account.ID, fmt.Sprintf("WorkBuddy authentication failed (401): %s", bodyText))
		}
		return s.sendErrorAndEnd(c, fmt.Sprintf("WorkBuddy API (/v2/chat/completions) returned %d: %s", resp.StatusCode, bodyText))
	}

	return s.processWorkbuddyTestStream(c, newWorkbuddySSEReader(resp.Body))
}

// processWorkbuddyTestStream 读取规范化后的 WorkBuddy SSE 流：逐帧透出 content
// delta，收到 [DONE]（normalizer 保证恰好一个）判定连接成功；error 帧原样上报。
func (s *AccountTestService) processWorkbuddyTestStream(c *gin.Context, body io.Reader) error {
	reader := bufio.NewReader(body)
	seenJSON := false

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				if seenJSON {
					return s.sendErrorAndEnd(c, "WorkBuddy stream ended before [DONE]")
				}
				return s.sendErrorAndEnd(c, "Invalid WorkBuddy response: expected SSE JSON data")
			}
			return s.sendErrorAndEnd(c, fmt.Sprintf("WorkBuddy stream read error: %s", err.Error()))
		}

		line = strings.TrimSpace(line)
		if line == "" || !sseDataPrefix.MatchString(line) {
			continue
		}

		jsonStr := sseDataPrefix.ReplaceAllString(line, "")
		if jsonStr == "[DONE]" {
			s.sendEvent(c, TestEvent{Type: "status", Text: "已通过 WorkBuddy /v2/chat/completions 验证"})
			s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
			return nil
		}

		var data map[string]any
		if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
			return s.sendErrorAndEnd(c, "Invalid WorkBuddy response: expected JSON data")
		}
		seenJSON = true

		if errData, ok := data["error"].(map[string]any); ok {
			errorMsg := "WorkBuddy upstream returned an error"
			if msg, ok := errData["message"].(string); ok && msg != "" {
				errorMsg = msg
			}
			return s.sendErrorAndEnd(c, fmt.Sprintf("WorkBuddy API error: %s", errorMsg))
		}

		choices, ok := data["choices"].([]any)
		if !ok {
			continue
		}
		for _, choiceValue := range choices {
			choice, ok := choiceValue.(map[string]any)
			if !ok {
				continue
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				if text, ok := delta["content"].(string); ok && text != "" {
					s.sendEvent(c, TestEvent{Type: "content", Text: text})
				}
			}
		}
	}
}

// refreshWorkbuddyAccountToken 账号测试路径专用的 WorkBuddy token 刷新：与网关
// refreshWorkbuddyToken 同语义（账号级锁 + 锁内双检 + 凭据写回），仅传输层换成
// 测试服务自己的 HTTPUpstream（含插件路径）。
func (s *AccountTestService) refreshWorkbuddyAccountToken(ctx context.Context, account *Account) error {
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
	if strings.TrimSpace(creds.RefreshToken) == "" {
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
	resp, err := s.doOpenAIAccountTestUpstream(req, proxyURL, account, true)
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
	// 写回：先更新内存凭据（测试请求立即可用），再持久化到账号库（失败返回错误，
	// 但内存凭据已更新，与网关刷新路径同语义）。
	next := shallowCopyMap(account.Credentials)
	next["access_token"] = env.Data.AccessToken
	if rt := strings.TrimSpace(env.Data.RefreshToken); rt != "" {
		next["refresh_token"] = rt
	}
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
