package service

// qoder_gateway.go Qoder 网关集成：CC 入站转发（forwardQoderChatCompletions）、
// 供后续接线的发送分发分支（sendCCUpstreamForAccountQoder）与账号连接测试
// （testQoderAccountConnection）。
//
// 与 workbuddy_gateway.go 对齐：Qoder 上游只提供 COSY 协议的 chat SSE 端点，
// /v1/chat/completions 入站统一经 forwardQoderChatCompletions，出站一律走
// sendQoderUpstreamRequest（协议变换、签名头、SSE 归一/非流式聚合在其内完成）。

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

// defaultQoderTestModel 账号连接测试的缺省模型（官方目录中的通用对话模型）。
const defaultQoderTestModel = "auto"

// forwardQoderChatCompletions 服务 /v1/chat/completions 入站的 Qoder 账号。
//
// 与 forwardWorkbuddyChatCompletions 同构：不做平台专属预处理，出站经
// sendQoderUpstreamRequest（COSY 签名、SSE 归一/非流式聚合均在其内）。
func (s *OpenAIGatewayService) forwardQoderChatCompletions(
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

	// 3. 模型映射（与 ForwardAsChatCompletions 同链路）。billingModel 额外过一道
	// Qoder 别名归一：价格查找（渠道子串规则 + getFallbackPricing 的
	// isQoderModelCode 精确匹配）都以官方 key 为准，展示名（qwen3.8-flash 等）
	// 在未配 model_mapping 的账号上会查不到价，或被子串规则误命中到错价档。
	// normalizeOpenAIModelForUpstream 对 key 幂等，出站行为不变。
	billingModel := normalizeQoderModelKey(resolveOpenAIForwardModel(account, originalModel, defaultMappedModel))
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	SetOpsUpstreamModel(c, upstreamModel)

	upstreamBody := body
	if upstreamModel != originalModel {
		upstreamBody = ReplaceModelInBody(body, upstreamModel)
	}

	reasoningEffort := extractOpenAIReasoningEffortFromBody(body, upstreamModel, billingModel, originalModel)
	serviceTier := extractOpenAIServiceTierFromBody(upstreamBody)

	SetActualOpenAIUpstreamEndpoint(c, qoderChatPath)

	// 4. 出站（COSY 协议变换/签名头/SSE 归一/非流式聚合在发送函数内完成）。
	resp, err := s.sendQoderUpstreamRequest(ctx, c, account, upstreamBody, clientStream)
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
		result.UpstreamEndpoint = qoderChatPath
	}
	return result, forwardErr
}

// sendCCUpstreamForAccountQoder 是供后续接线批次调用的发送分发分支：
// 把 sendCCUpstreamForAccount 的 Qoder 分支抽为独立函数（接线批次在
// sendCCUpstreamForAccount 里补一个 IsQoderPlatform() 判断并路由到此）。
func (s *OpenAIGatewayService) sendCCUpstreamForAccountQoder(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	ccBody []byte,
	clientStream bool,
) (*http.Response, error) {
	return s.sendQoderUpstreamRequest(ctx, c, account, ccBody, clientStream)
}

// testQoderAccountConnection 测试 Qoder 账号连通性：向 chat SSE 端点发送最小 CC
// 测试请求（COSY 全套头 + 协议变换），流式读取规范化 SSE 直到 [DONE]。
// PAT/刷新令牌凭据先内联 jobToken 交换（与请求路径同语义：失败且有旧 token 时宽容放行）。
func (s *AccountTestService) testQoderAccountConnection(c *gin.Context, account *Account, modelID string, prompt string) error {
	ctx := c.Request.Context()

	testModelID := strings.TrimSpace(modelID)
	if testModelID == "" {
		testModelID = defaultQoderTestModel
	}
	testModelID = normalizeQoderModelKey(account.GetMappedModel(testModelID))

	// 预交换：与 sendQoderUpstreamRequest 的预交换语义一致。
	creds := account.GetQoderCredentials()
	if qoderNeedsJobTokenExchange(creds) {
		if err := s.exchangeQoderJobTokenForTest(ctx, account); err != nil {
			logger.L().Warn("qoder account test pre-flight jobToken exchange failed",
				zap.Int64("account_id", account.ID), zap.Error(err))
			creds = account.GetQoderCredentials()
			if strings.TrimSpace(creds.AccessToken) == "" {
				return s.sendErrorAndEnd(c, fmt.Sprintf("Qoder jobToken exchange failed: %s", err.Error()))
			}
		} else {
			creds = account.GetQoderCredentials()
		}
	}
	// 身份自愈：uid 缺失时拉 userinfo 回填（COSY uid 为登录态校验要素，
	// 伪 uid 会被上游 chat 网关判 105 Login expired）。
	creds = qoderHealIdentity(ctx, s.accountRepo, account, creds, s.qoderUserinfoFetch)
	if strings.TrimSpace(creds.AccessToken) == "" {
		return s.sendErrorAndEnd(c, "No Qoder access token available")
	}

	endpoints := resolveQoderEndpoints(account.GetQoderRealm(), account.GetQoderBaseURL())
	normalizedBaseURL, err := s.validateUpstreamBaseURL(endpoints.AlgoBase)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid base URL: %s", err.Error()))
	}
	apiURL := strings.TrimRight(normalizedBaseURL, "/") + qoderChatPath + qoderChatQuery

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	payloadBytes, _ := json.Marshal(createOpenAIChatCompletionsTestPayload(testModelID, prompt))
	userType := qoderResolveUserType(creds)
	prepared, meta, err := BuildQoderBody(payloadBytes, testModelID, userType)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Build Qoder request failed: %s", err.Error()))
	}
	prepared = []byte(QoderCosyEncode(prepared))

	s.sendEvent(c, TestEvent{Type: "test_start", Model: testModelID})
	s.sendEvent(c, TestEvent{Type: "status", Text: "正在通过 Qoder agent_chat_generation 测试连接"})

	securityToken := strings.TrimSpace(creds.AccessToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(prepared))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Qoder request")
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	if hErr := applyQoderChatHeaders(req, creds, securityToken, meta, prepared, apiURL, "text/event-stream"); hErr != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Build Qoder headers failed: %s", hErr.Error()))
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.doOpenAIAccountTestUpstream(req, proxyURL, account, true)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Qoder API request failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, workbuddyUpstreamErrorBodyLimit))
		bodyText := workbuddyTruncateForError(string(body))
		if resp.StatusCode == http.StatusUnauthorized && s.accountRepo != nil {
			_ = s.accountRepo.SetError(ctx, account.ID, fmt.Sprintf("Qoder authentication failed (401): %s", bodyText))
		}
		return s.sendErrorAndEnd(c, fmt.Sprintf("Qoder API (agent_chat_generation) returned %d: %s", resp.StatusCode, bodyText))
	}

	return s.processQoderTestStream(c, account, newQoderSSEReader(resp.Body, testModelID))
}

// processQoderTestStream 读取规范化后的 Qoder SSE 流：逐帧透出 content delta，
// 收到 [DONE] 判定连接成功；error 帧原样上报（登录态类错误同步标记账号，
// 使管理页可见失败原因而非静默失败）。
func (s *AccountTestService) processQoderTestStream(c *gin.Context, account *Account, body io.Reader) error {
	reader := bufio.NewReader(body)
	seenJSON := false

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				if seenJSON {
					return s.sendErrorAndEnd(c, "Qoder stream ended before [DONE]")
				}
				return s.sendErrorAndEnd(c, "Invalid Qoder response: expected SSE JSON data")
			}
			return s.sendErrorAndEnd(c, fmt.Sprintf("Qoder stream read error: %s", err.Error()))
		}

		line = strings.TrimSpace(line)
		if line == "" || !sseDataPrefix.MatchString(line) {
			continue
		}

		jsonStr := sseDataPrefix.ReplaceAllString(line, "")
		if jsonStr == "[DONE]" {
			s.sendEvent(c, TestEvent{Type: "status", Text: "已通过 Qoder agent_chat_generation 验证"})
			s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
			return nil
		}

		var data map[string]any
		if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
			return s.sendErrorAndEnd(c, "Invalid Qoder response: expected JSON data")
		}
		seenJSON = true

		if errData, ok := data["error"].(map[string]any); ok {
			errorMsg := "Qoder upstream returned an error"
			if msg, ok := errData["message"].(string); ok && msg != "" {
				errorMsg = msg
			}
			// 错误码分级落账号状态，与请求转发路径共用同一处置口径：
			// 额度/配额类（110 今日额度已用尽等）→ 临时不可调度至次日 0 点；
			// 登录态 105 / 账户类 104·108·109 → 标记错误态（需人工处理）；
			// 字典外错误码不落状态（请求级/环境级语义，避免误标）。
			//
			// ctx 口径：本函数是管理端 SSE 长连接，管理员看到失败提示后顺手关掉
			// 页面是常见操作，直接用 c.Request.Context() 会因取消而静默丢掉标记
			// （只留一条 Warn）。applyQoderBusinessErrorClass 内部统一走
			// openAIAccountStateContext（WithoutCancel + 超时），与请求路径一致。
			errorCode, _ := errData["code"].(string)
			resolvedCode, class := resolveQoderBusinessErrorCode(errorCode, errorMsg)
			applyQoderBusinessErrorClass(c.Request.Context(), s.accountRepo, account, resolvedCode, class, errorMsg)
			return s.sendErrorAndEnd(c, fmt.Sprintf("Qoder API error: %s", errorMsg))
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

// exchangeQoderJobTokenForTest 账号测试路径专用的 jobToken 交换：与网关
// exchangeQoderJobToken 同语义（账号级锁 + 锁内双检 + 凭据写回），仅传输层换成
// 测试服务自己的 HTTPUpstream。
func (s *AccountTestService) exchangeQoderJobTokenForTest(ctx context.Context, account *Account) error {
	if account == nil || !account.IsQoderPlatform() {
		return fmt.Errorf("qoder jobToken exchange requires a qoder account")
	}
	snapshot := strings.TrimSpace(account.GetQoderCredentials().AccessToken)
	lock := qoderJobTokenLock(account.ID)
	if err := lock.Lock(ctx); err != nil {
		return err
	}
	defer lock.Unlock()
	// 锁内双检：token 快照已被他人改写 → 并发交换已完成。
	if strings.TrimSpace(account.GetQoderCredentials().AccessToken) != snapshot {
		return nil
	}
	creds := account.GetQoderCredentials()
	token := strings.TrimSpace(creds.PersonalToken)
	if token == "" {
		token = strings.TrimSpace(creds.RefreshToken)
	}
	if token == "" {
		return fmt.Errorf("qoder account %d has no personal token or refresh token", account.ID)
	}
	endpoints := resolveQoderEndpoints(account.GetQoderRealm(), account.GetQoderBaseURL())
	seed := qoderFingerprintSeed(creds.UID, token)
	resp, err := QoderExchangeJobToken(&qoderUpstreamRoundTripper{upstream: s.httpUpstream}, endpoints, seed, token)
	if err != nil {
		return err
	}
	next := shallowCopyMap(account.Credentials)
	next["access_token"] = resp.SecurityOauthToken
	if rt := strings.TrimSpace(resp.RefreshToken); rt != "" {
		next["refresh_token"] = rt
	}
	if resp.ID != "" {
		if strings.TrimSpace(qoderCredentialValueString(next["uid"])) == "" {
			next["uid"] = resp.ID
		}
	}
	if resp.UserType != "" {
		next["user_type"] = resp.UserType
	}
	account.Credentials = next
	if err := persistAccountCredentials(ctx, s.accountRepo, account, account.Credentials); err != nil {
		return fmt.Errorf("persist qoder credentials: %w", err)
	}
	return nil
}
