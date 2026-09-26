package service

// trae_gateway.go Trae 网关集成：CC 入站转发（forwardTraeChatCompletions）、CC 回退
// 路径的发送分发分支（sendCCUpstreamForAccountTrae）与账号连接测试
// （testTraeAccountConnection）。
//
// Trae 上游只有自研 Cloud-IDE-JWT 协议的 SSE 端点（/api/agent/v3/llm_utils_chat，
// 回退 /api/ide/v1/chat），没有 /v1/responses 与 Anthropic /v1/messages：三条入站路径
// （CC / Messages / Responses）统一收敛到各自的 raw-CC 回退函数，出站一律经
// sendTraeUpstreamRequest（协议变换、三套指纹头、SSE 归一/非流式聚合均在其内完成）。

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

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// defaultTraeTestModel 账号连接测试的缺省模型（CN 目录里的通用对话模型）。
const defaultTraeTestModel = "glm-5.2"

// forwardTraeChatCompletions 服务 /v1/chat/completions 入站的 Trae 账号。
// 与 forwardWorkbuddyChatCompletions 同构（协议变换/发送/SSE 归一均在发送函数内）。
func (s *OpenAIGatewayService) forwardTraeChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()
	beginUpstreamResponseModelObservation(c)

	originalModel := gjson.GetBytes(body, "model").String()
	if originalModel == "" {
		writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, fmt.Errorf("missing model in request")
	}
	clientStream := gjson.GetBytes(body, "stream").Bool()

	billingModel := resolveOpenAIForwardModel(account, originalModel, defaultMappedModel)
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	SetOpsUpstreamModel(c, upstreamModel)

	upstreamBody := body
	if upstreamModel != originalModel {
		upstreamBody = ReplaceModelInBody(body, upstreamModel)
	}
	reasoningEffort := extractOpenAIReasoningEffortFromBody(body, upstreamModel, billingModel, originalModel)
	serviceTier := extractOpenAIServiceTierFromBody(upstreamBody)

	SetActualOpenAIUpstreamEndpoint(c, traeChatPath)

	resp, err := s.sendTraeUpstreamRequest(ctx, c, account, upstreamBody, clientStream)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, upstreamMsg := s.readOpenAIUpstreamError(resp)
		if foErr := s.failoverOpenAIUpstreamHTTPError(ctx, c, account, resp, respBody, upstreamMsg, upstreamModel); foErr != nil {
			return nil, foErr
		}
		return s.handleChatCompletionsErrorResponse(resp, c, account, billingModel)
	}

	var result *OpenAIForwardResult
	var forwardErr error
	if clientStream {
		result, forwardErr = s.streamRawChatCompletions(c, resp, account, originalModel, billingModel, upstreamModel, reasoningEffort, serviceTier, startTime, len(body))
	} else {
		result, forwardErr = s.bufferRawChatCompletions(c, resp, account, originalModel, billingModel, upstreamModel, reasoningEffort, serviceTier, startTime)
	}
	if result != nil {
		result.UpstreamEndpoint = traeChatPath
	}
	return result, forwardErr
}

// sendCCUpstreamForAccountTrae CC 回退路径（/v1/messages、/v1/responses）的 Trae 发送
// 分支：与 workbuddy/qoder 同构，一律走专用协议管线。
func (s *OpenAIGatewayService) sendCCUpstreamForAccountTrae(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	ccBody []byte,
	clientStream bool,
) (*http.Response, error) {
	return s.sendTraeUpstreamRequest(ctx, c, account, ccBody, clientStream)
}

// testTraeAccountConnection 测试 Trae 账号连通性：向聊天域发送最小测试请求（Trae
// 聊天域全套头 + 协议变换），流式读取规范化 SSE 直到 [DONE]。access_token 缺失/临近
// 过期且持有 refresh_token 时先内联换票一次（与请求路径同语义：失败且有旧 token 时
// 宽容放行）。
func (s *AccountTestService) testTraeAccountConnection(c *gin.Context, account *Account, modelID string, prompt string) error {
	ctx := c.Request.Context()

	testModelID := strings.TrimSpace(modelID)
	if testModelID == "" {
		testModelID = defaultTraeTestModel
	}
	testModelID = account.GetMappedModel(testModelID)

	creds := account.GetTraeCredentials()
	if traeTokenNeedsRefresh(creds, time.Now()) && strings.TrimSpace(creds.RefreshToken) != "" {
		if err := s.refreshTraeAccountToken(ctx, account); err != nil {
			logger.L().Warn("trae account test pre-flight token refresh failed",
				zap.Int64("account_id", account.ID), zap.Error(err))
			creds = account.GetTraeCredentials()
			if strings.TrimSpace(creds.AccessToken) == "" {
				return s.sendErrorAndEnd(c, fmt.Sprintf("Trae token refresh failed: %s", err.Error()))
			}
		} else {
			creds = account.GetTraeCredentials()
		}
	}
	if strings.TrimSpace(creds.AccessToken) == "" {
		return s.sendErrorAndEnd(c, "No Trae access token available")
	}
	baseURL := account.GetTraeBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		return s.sendErrorAndEnd(c, "Trae base URL is not configured")
	}
	normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid base URL: %s", err.Error()))
	}

	// traeChatAttempts 摄入站 CC 字节流，故连接测试报文需先编码（与真实请求路径同形态）。
	testBody, testBodyErr := json.Marshal(createOpenAIChatCompletionsTestPayload(testModelID, prompt))
	if testBodyErr != nil {
		return s.sendErrorAndEnd(c, "Failed to build Trae request")
	}
	attempts, err := traeChatAttempts(testBody, creds, account.ID)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to build Trae request: %s", err.Error()))
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	s.sendEvent(c, TestEvent{Type: "test_start", Model: testModelID})
	s.sendEvent(c, TestEvent{Type: "status", Text: "正在通过 Trae " + attempts[0].path + " 测试连接"})

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	var resp *http.Response
	for i, attempt := range attempts {
		apiURL := strings.TrimRight(normalizedBaseURL, "/") + attempt.path
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(attempt.body))
		if reqErr != nil {
			return s.sendErrorAndEnd(c, "Failed to create Trae request")
		}
		req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
		applyTraeChatHeaders(req, creds, account)
		// 与请求路径同纪律：仅 404（端点不存在）才回退下一候选端点。
		resp, err = s.doOpenAIAccountTestUpstream(req, proxyURL, account, true)
		if err != nil {
			return s.sendErrorAndEnd(c, fmt.Sprintf("Trae API request failed: %s", err.Error()))
		}
		if resp.StatusCode != http.StatusNotFound || i == len(attempts)-1 {
			break
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, traeUpstreamErrorBodyLimit))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(body))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, traeUpstreamErrorBodyLimit))
		bodyText := traeTruncateForError(string(body))
		if resp.StatusCode == http.StatusUnauthorized && s.accountRepo != nil {
			_ = s.accountRepo.SetError(ctx, account.ID, fmt.Sprintf("Trae authentication failed (401): %s", bodyText))
		}
		return s.sendErrorAndEnd(c, fmt.Sprintf("Trae API (%s) returned %d: %s", attempts[0].path, resp.StatusCode, bodyText))
	}
	return s.processTraeTestStream(c, traeNewSSEReader(resp.Body, traeStreamMeta{Model: testModelID}))
}

// processTraeTestStream 读取规范化后的 Trae SSE 流：逐帧透出 content delta，收到
// [DONE]（normalizer 保证恰好一个）判定连接成功；error 帧原样上报。
func (s *AccountTestService) processTraeTestStream(c *gin.Context, body io.Reader) error {
	reader := bufio.NewReader(body)
	seenJSON := false

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				if seenJSON {
					return s.sendErrorAndEnd(c, "Trae stream ended before [DONE]")
				}
				return s.sendErrorAndEnd(c, "Invalid Trae response: expected SSE JSON data")
			}
			return s.sendErrorAndEnd(c, fmt.Sprintf("Trae stream read error: %s", err.Error()))
		}
		line = strings.TrimSpace(line)
		if line == "" || !sseDataPrefix.MatchString(line) {
			continue
		}
		jsonStr := sseDataPrefix.ReplaceAllString(line, "")
		if jsonStr == "[DONE]" {
			s.sendEvent(c, TestEvent{Type: "status", Text: "已通过 Trae llm_utils_chat 验证"})
			s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
			return nil
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
			return s.sendErrorAndEnd(c, "Invalid Trae response: expected JSON data")
		}
		seenJSON = true
		if errData, ok := data["error"].(map[string]any); ok {
			errorMsg := "Trae upstream returned an error"
			if msg, ok := errData["message"].(string); ok && msg != "" {
				errorMsg = msg
			}
			return s.sendErrorAndEnd(c, fmt.Sprintf("Trae API error: %s", errorMsg))
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

// refreshTraeAccountToken 账号测试路径专用的 Trae 换票：与网关 refreshTraeToken
// 同语义（账号级锁 + 锁内双检 + 凭据写回），仅传输层换成测试服务自己的 HTTPUpstream
// （模式对齐 refreshWorkbuddyAccountToken）。
func (s *AccountTestService) refreshTraeAccountToken(ctx context.Context, account *Account) error {
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
			return s.doOpenAIAccountTestUpstream(req, proxyURL, account, true)
		},
	}
	return refresher.refresh(ctx, account)
}
