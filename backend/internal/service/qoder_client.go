package service

// qoder_client.go Qoder 上游客户端：chat 出站发送与 PAT → securityOauthToken 交换。
//
// 发送模式对齐 sendWorkbuddyUpstreamRequest：detachUpstreamContext、
// WithHTTPUpstreamProfile(HTTPUpstreamProfileOpenAI)、doOpenAIUpstream、
// handleOpenAIUpstreamTransportError。
//
// Qoder 无独立 token 刷新端点：dt- 设备令牌失效只能报错（重新 OAuth 登录）；
// 持 PAT/刷新令牌（pt-/drt-）且无 securityOauthToken 时先经 jobToken 交换
// （仿 workbuddy 的预刷新模式，账号级锁 + 凭据写回）。
//
// 上游只支持流式：请求体恒被 BuildQoderBody 强制 stream:true；clientStream=false
// 时网关读取全流聚合为单个 CC JSON 响应。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	// qoderJobTokenExchangeTimeout 单次 jobToken 交换的总时长上限。
	qoderJobTokenExchangeTimeout = 30 * time.Second

	// qoderJobTokenExpiresInMax 交换响应 expires_in 的量级上限（10 年防御值）。
	qoderJobTokenExpiresInMax = 10 * 365 * 24 * time.Hour
)

// qoderJobTokenLocks 同一账号并发交换互斥（按 account ID，模式对齐 workbuddyRefreshLocks）。
var qoderJobTokenLocks sync.Map // key: int64(accountID), value: *contextMutex

// qoderJobTokenLock 返回账号级交换锁（不存在则惰性创建）。
func qoderJobTokenLock(accountID int64) *contextMutex {
	actual, _ := qoderJobTokenLocks.LoadOrStore(accountID, newContextMutex())
	mu, ok := actual.(*contextMutex)
	if !ok {
		mu = newContextMutex()
		qoderJobTokenLocks.Store(accountID, mu)
	}
	return mu
}

// qoderNeedsJobTokenExchange 报告凭据是否需要先做 jobToken 交换：
// access_token 为空且持有 PAT/刷新令牌时必须先换取 securityOauthToken。
// （dt- 设备令牌直用；securityOauthToken 过期无刷新端点，失效即 401 报错。）
func qoderNeedsJobTokenExchange(creds QoderCredentials) bool {
	if strings.TrimSpace(creds.AccessToken) != "" {
		return false
	}
	return strings.TrimSpace(creds.PersonalToken) != "" || strings.TrimSpace(creds.RefreshToken) != ""
}

// qoderUserinfoFetcher 是身份拉取函数签名（生产传 qoderFetchUserinfo，单测注入 stub）。
type qoderUserinfoFetcher func(ctx context.Context, token, userinfoURL string) (*qoderUserinfoResponse, error)

// qoderHealIdentity 出站身份自愈：COSY 会话的 uid 必须为真实账号 ID
// （实测：uid 缺失回落伪值时上游 chat 恒返 105 Login expired，而 userinfo
// 对同一 dt- 仍 200——仅 chat 网关校验 uid）。uid 为空且持可用令牌时
// best-effort 拉 userinfo 回填 uid/nickname 并持久化；任何失败不阻断主流程。
// userinfo URL 随账号端点解析（base_url 自定义中转时同源，单测得以注入假上游）。
func qoderHealIdentity(ctx context.Context, repo AccountRepository, account *Account, creds QoderCredentials, fetch qoderUserinfoFetcher) QoderCredentials {
	if account == nil || strings.TrimSpace(creds.UID) != "" {
		return creds
	}
	token := strings.TrimSpace(creds.AccessToken)
	if token == "" {
		return creds
	}
	if fetch == nil {
		fetch = qoderFetchUserinfo
	}
	eps := resolveQoderEndpoints(account.GetQoderRealm(), account.GetQoderBaseURL())
	ui, err := fetch(ctx, token, eps.UserinfoURL)
	if err != nil || ui == nil || strings.TrimSpace(ui.ID) == "" {
		return creds
	}
	next := shallowCopyMap(account.Credentials)
	next["uid"] = strings.TrimSpace(ui.ID)
	if nick := firstNonEmptyQoder(strings.TrimSpace(ui.Username), strings.TrimSpace(ui.Name)); nick != "" {
		next["nickname"] = nick
	}
	account.Credentials = next
	if perr := persistAccountCredentials(ctx, repo, account, next); perr != nil {
		logger.L().Warn("qoder identity heal persist failed",
			zap.Int64("account_id", account.ID), zap.Error(perr))
	}
	return account.GetQoderCredentials()
}

// sendQoderUpstreamRequest 构建并发送 Qoder chat 上游请求。
//
// 流程：
//  1. 取凭据；access_token 为空且持 PAT/刷新令牌 → 先 jobToken 交换并写回凭据；
//  2. BuildQoderBody 构造请求体（模板 + 消息转换 + 强制 stream）；
//  3. 目标 URL = 端点解析（realm/base_url）+ chat SSE 路径（经 validateUpstreamBaseURL 校验）；
//  4. 分离上游 context + OpenAI HTTP profile + COSY 全套签名头；
//  5. >=400：读 body（1MB 上限）后回卷原样返回（Qoder 无刷新重试路径）；
//  6. <400：clientStream=true 包装规范化 SSE 流；false 读取全流聚合为 CC JSON 假 resp。
func (s *OpenAIGatewayService) sendQoderUpstreamRequest(ctx context.Context, c *gin.Context, account *Account, ccBody []byte, clientStream bool) (*http.Response, error) {
	if account == nil || !account.IsQoderPlatform() {
		return nil, fmt.Errorf("qoder upstream request requires a qoder account")
	}
	creds := account.GetQoderCredentials()
	// 1) 预交换：PAT/刷新令牌先换 securityOauthToken（失败且有 dt-/旧 token 则宽容放行）。
	if qoderNeedsJobTokenExchange(creds) {
		if err := s.exchangeQoderJobToken(ctx, account); err != nil {
			logger.L().Warn("qoder pre-flight jobToken exchange failed",
				zap.Int64("account_id", account.ID), zap.Error(err))
			creds = account.GetQoderCredentials()
			if strings.TrimSpace(creds.AccessToken) == "" {
				return nil, fmt.Errorf("qoder account %d has no usable access token: %w", account.ID, err)
			}
		} else {
			creds = account.GetQoderCredentials()
		}
	}
	// 1.5) 身份自愈：uid 缺失时拉 userinfo 回填（COSY uid 为登录态校验要素）。
	creds = qoderHealIdentity(ctx, s.accountRepo, account, creds, s.qoderUserinfoFetch)
	securityToken := strings.TrimSpace(creds.AccessToken)
	if securityToken == "" {
		return nil, fmt.Errorf("qoder account %d has no access token (dt- device token or exchanged securityOauthToken required)", account.ID)
	}

	// 2) 请求体构造（模板 + 消息转换 + 签名所需的 requestId/modelKey 元数据）。
	modelKey := qoderResolveUpstreamModelKey(account, ccBody)
	userType := qoderResolveUserType(creds)
	prepared, meta, err := BuildQoderBody(ccBody, modelKey, userType)
	if err != nil {
		return nil, err
	}
	// 出站体 COSY 编码（Encode=1）：签名与签名体均为编码后形态。
	encodedBody := QoderCosyEncode(prepared)

	// 3) 目标 URL。
	endpoints := resolveQoderEndpoints(account.GetQoderRealm(), account.GetQoderBaseURL())
	validatedBase, err := s.validateUpstreamBaseURL(endpoints.AlgoBase)
	if err != nil {
		return nil, fmt.Errorf("invalid qoder base_url: %w", err)
	}
	targetURL := strings.TrimRight(validatedBase, "/") + qoderChatPath + qoderChatQuery

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	buildRequest := func(acceptValue string) (*http.Request, error) {
		upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
		upstreamReq, reqErr := http.NewRequestWithContext(upstreamCtx, http.MethodPost, targetURL, bytes.NewReader([]byte(encodedBody)))
		releaseUpstreamCtx()
		if reqErr != nil {
			return nil, fmt.Errorf("build qoder upstream request: %w", reqErr)
		}
		upstreamReq = upstreamReq.WithContext(WithHTTPUpstreamProfile(upstreamReq.Context(), HTTPUpstreamProfileOpenAI))
		if hErr := applyQoderChatHeaders(upstreamReq, creds, securityToken, meta, []byte(encodedBody), targetURL, acceptValue); hErr != nil {
			return nil, hErr
		}
		return upstreamReq, nil
	}
	// 4) 发送（Qoder 无 401 刷新重试路径：dt- 失效只能报错重新登录）。
	// accept 恒为 text/event-stream：实测上游 agent_chat_generation 端点只接受
	// SSE accept，application/json 会直接 500 Internal Server Error（即使请求体
	// stream:true 也不行）；非流式客户端由下方「读全流聚合为 CC JSON」路径满足。
	upstreamReq, err := buildRequest("text/event-stream")
	if err != nil {
		return nil, err
	}
	resp, err := s.doOpenAIUpstream(upstreamReq, proxyURL, account)
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	// 5) >=400 错误路径：读 body 后回卷，原样返回交由上层 failover/透传。
	if resp.StatusCode >= 400 {
		workbuddyReadAndRewindBody(resp)
		return resp, nil
	}
	// 6) 成功路径。sentModel 为实际出站 key，用于把响应 model 从上游占位值
	// "auto" 回写为客户端可识别的真实模型。
	//
	// HTTP 200 + SSE 流内业务错误帧（110 今日额度已用尽等）是 Qoder 上游的主
	// 要拒绝形态——此前只透传 CC error 帧不落账号状态，导致账号被限额后面板
	// 仍显示「正常」、调度器持续选用。此处两条消费路径均接入分级落状态
	// （流式经 reader 回调；非流式在聚合失败/成功帧内检查）。
	if clientStream {
		resp.Body = io.NopCloser(newQoderSSEReaderWithErrorHook(resp.Body, modelKey,
			func(code, message string) {
				// 回调运行于下游读流 goroutine（脱离入站 ctx 取消链），落状态
				// 使用分离 context，避免客户端提前断开时丢标记。
				healCtx, releaseHealCtx := detachUpstreamContext(ctx)
				defer releaseHealCtx()
				s.markQoderAccountFromBusinessError(healCtx, c, account, code, message)
			}))
		resp.Header = resp.Header.Clone()
		resp.Header.Set("Content-Type", "text/event-stream")
		return resp, nil
	}
	// 非流式：读取全流（有界）聚合为单个 CC JSON 响应，构造假 resp 返回。
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, workbuddyAggregateReadLimit))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read qoder upstream stream: %w", readErr)
	}
	aggregated, usage, aggErr := aggregateQoderSSE(bytes.NewReader(raw), modelKey)
	if aggErr != nil {
		// 空流/信封/业务错误：先按错误码分级落账号状态（与流式路径同口径），
		// 再交由上层错误处理链（对齐 workbuddy 聚合路径语义）。
		var env *errQoderUpstreamEnvelope
		if errors.As(aggErr, &env) {
			healCtx, releaseHealCtx := detachUpstreamContext(ctx)
			s.markQoderAccountFromBusinessError(healCtx, c, account, env.Code, env.Message)
			releaseHealCtx()
		}
		return nil, aggErr
	}
	_ = usage
	resp.Body = io.NopCloser(bytes.NewReader(aggregated))
	resp.Header = resp.Header.Clone()
	resp.Header.Set("Content-Type", "application/json")
	resp.StatusCode = http.StatusOK
	resp.Status = "200 OK"
	resp.ContentLength = int64(len(aggregated))
	return resp, nil
}

// qoderResolveUpstreamModelKey 从 CC body 提取 model 并映射为 Qoder 模型 key。
// 优先级：凭据模型映射（GetMappedModel）→ 原始 model 值 → 缺省 "auto"。
// 最后统一过 normalizeQoderModelKey 别名归一：映射结果若还是展示名
// （qwen3.8-flash 等），换成官方 key（qfmodel），避免上游静默回落（credits=0）
// 与「模型不一致」告警。
func qoderResolveUpstreamModelKey(account *Account, ccBody []byte) string {
	model := gjson.GetBytes(ccBody, "model").String()
	model = strings.TrimSpace(account.GetMappedModel(model))
	if model == "" {
		return "auto"
	}
	return normalizeQoderModelKey(model)
}

// qoderResolveUserType 解析 aliyun_user_type：个人号固定 personal_standard
// （官方形态；未来企业号接入再按凭据扩展）。
func qoderResolveUserType(creds QoderCredentials) string {
	return "personal_standard"
}

// exchangeQoderJobToken 用 PAT/刷新令牌换取 securityOauthToken 并写回凭据。
// 并发控制：按账号 ID 的锁 + 锁内双检（对齐 refreshWorkbuddyToken 模式）。
func (s *OpenAIGatewayService) exchangeQoderJobToken(ctx context.Context, account *Account) error {
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
	resp, err := QoderExchangeJobToken(qoderGatewayTransport(s.httpUpstream), endpoints, seed, token)
	if err != nil {
		return err
	}
	// 写回：先更新内存凭据（请求路径立即可用），再持久化到账号库。
	next := shallowCopyMap(account.Credentials)
	next["access_token"] = resp.SecurityOauthToken
	if rt := strings.TrimSpace(resp.RefreshToken); rt != "" {
		next["refresh_token"] = rt
	}
	// jobToken 交换响应无 expires_in：保留既有过期语义（无过期信息不主动刷新）。
	if resp.ID != "" {
		if _, exists := next["uid"]; !exists || strings.TrimSpace(qoderCredentialValueString(next["uid"])) == "" {
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

// qoderGatewayTransport 把网关 HTTPUpstream 适配为 jobToken 交换所需的
// http.RoundTripper（复用既有上游传输层与 TLS profile；交换是低频一次性调用）。
func qoderGatewayTransport(upstream HTTPUpstream) http.RoundTripper {
	return &qoderUpstreamRoundTripper{upstream: upstream}
}

// qoderUpstreamRoundTripper 经网关 HTTPUpstream 执行请求的 RoundTripper 适配器。
type qoderUpstreamRoundTripper struct {
	upstream HTTPUpstream
}

// RoundTrip 实现 http.RoundTripper（并发槽位复用 OpenAI profile 缺省值 1）。
func (t *qoderUpstreamRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.upstream.Do(req, "", 0, 1)
}

// qoderJobTokenExpiresAt 交换成功后的过期时间估算（官方未返回 expires_in；
// 保守按 24h 写入 expires_at，供观测与未来刷新策略使用）。
func qoderJobTokenExpiresAt(now time.Time) int64 {
	skew := int64(24 * time.Hour / time.Second)
	if skew >= int64(qoderJobTokenExpiresInMax/time.Second) {
		return 0
	}
	return now.Unix() + skew
}
