package service

// workbuddy_oauth_service.go WorkBuddy 设备授权登录服务（后台「生成授权 URL →
// 浏览器登录 → 轮询回填凭据」三步式流程，对齐 OpenAI/Grok 渠道的 OAuth 体验）。
//
// 上游设备流语义（移植自 workbuddy2api/cmd/login 的 runURL / runPoll，
// 与 internal/upstream doJSON 家族的信封口径一致）：
//
//  1. POST {base}/v2/plugin/auth/state?platform=CLI（body 为空对象 {}）
//     → {code,msg,data:{state,authUrl}}：state 由上游服务端签发（无 PKCE），
//     authUrl 交给用户在浏览器完成登录；
//  2. GET {base}/v2/plugin/auth/token?state={state}
//     → {code,msg,data:{accessToken,refreshToken,expiresIn,domain}}：登录未完成时
//     业务 code != 0（实测 msg 形如 "login ing"），调用方按 pending 继续轮询；
//  3. GET {base}/v2/plugin/login/account?state={state}（Authorization: Bearer {at}）
//     → {code,msg,data:{uid,enterpriseId,nickname}}：补全账号展示信息，
//     best-effort（失败不阻断——凭据本身已可用，缺失字段留空）。
//
// realm 双域（对齐账号侧 GetWorkbuddyRealm 与 workbuddy_headers.go 的 origin 取值）：
//   - cn     → base https://copilot.tencent.com，Origin/Referer https://www.codebuddy.cn
//   - global → base https://www.workbuddy.ai，Origin/Referer https://www.workbuddy.ai
//
// 出站使用本服务独立的 http.Client（可按 proxyID 解析的代理配置），与
// GrokOAuthService 的「独立 client + 代理」模式一致：设备流无账号上下文、低频，
// 且必须保持固定的 CLI 客户端指纹（与 chat 出站的桌面端 UA 不同），
// 因此不复用聊天网关的 HTTPUpstream。
//
// 状态不落库：登录会话（state → token 的绑定）由上游保管，本服务只做无状态转发，
// 多实例部署天然一致，也避免了本地 state 过期/丢失导致的前端轮询中断。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
)

const (
	// 设备流三个上游端点（相对 base 的路径）。
	workbuddyOAuthStatePath   = "/v2/plugin/auth/state"
	workbuddyOAuthTokenPath   = "/v2/plugin/auth/token"
	workbuddyOAuthAccountPath = "/v2/plugin/login/account"

	// workbuddyOAuthPlatform 是 auth/state 的 platform 查询参数，设备流固定 CLI。
	workbuddyOAuthPlatform = "CLI"

	// workbuddyOAuthUserAgent 是设备流出站 UA，对齐官方 CLI 内置登录实现
	// （workbuddy2api/cmd/login 的 clientUA）。登录端点只认 CLI 指纹，
	// 与 chat 出站的桌面端 UA（workbuddyUAFor）不可混用。
	workbuddyOAuthUserAgent = "CLI/2.63.2 CodeBuddy/2.63.2"

	// workbuddyOAuthTimeout 单次设备流上游调用的总时长上限：前端轮询间隔远大于该值，
	// 30s 既覆盖上游抖动，又不会让单次请求把前端轮询串行堵死。
	workbuddyOAuthTimeout = 30 * time.Second

	// WorkBuddyOAuthStatusPending / WorkBuddyOAuthStatusCompleted 是轮询结果的两种状态：
	// pending = 用户尚未在浏览器完成登录（前端继续轮询）；
	// completed = 凭据已可回填（Token 非空）。
	WorkBuddyOAuthStatusPending   = "pending"
	WorkBuddyOAuthStatusCompleted = "completed"
)

// WorkBuddyOAuthService 负责 WorkBuddy 设备授权登录的服务端编排：
// 生成授权 URL（上游签发 state）与轮询兑换凭据（前端反复调用直到 completed）。
type WorkBuddyOAuthService struct {
	proxyRepo ProxyRepository

	// baseURLOverride 仅供单测注入（httptest 假上游）；生产恒为空，
	// 由 workbuddyOAuthEndpointsFor 按 realm 返回官方域。
	baseURLOverride string
}

// NewWorkBuddyOAuthService 构造设备授权服务；proxyRepo 可为 nil（表示不支持代理选择）。
func NewWorkBuddyOAuthService(proxyRepo ProxyRepository) *WorkBuddyOAuthService {
	return &WorkBuddyOAuthService{proxyRepo: proxyRepo}
}

// NewWorkBuddyOAuthServiceWithBaseURL 构造指向自定义上游 base 的设备授权服务。
// 生产链路恒用 NewWorkBuddyOAuthService（官方双域由 realm 决定）；本构造函数供单测
// 注入 httptest 假上游，也为未来接入自建/镜像域预留入口（空 base 等价于默认构造）。
func NewWorkBuddyOAuthServiceWithBaseURL(proxyRepo ProxyRepository, baseURL string) *WorkBuddyOAuthService {
	return &WorkBuddyOAuthService{proxyRepo: proxyRepo, baseURLOverride: baseURL}
}

// WorkBuddyAuthURLResult 是授权 URL 生成结果（直接回给后台前端展示/跳转）。
type WorkBuddyAuthURLResult struct {
	AuthURL string `json:"auth_url"`
	State   string `json:"state"`
	Realm   string `json:"realm"`
}

// WorkBuddyOAuthTokenInfo 是设备流兑换出的完整凭据与账号展示信息。
// 字段命名与 workbuddy2api 落盘的 auth 文件一致（前端据此预填账号表单）。
type WorkBuddyOAuthTokenInfo struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"` // = now + expiresIn（expiresIn 缺失/异常时不设，保留 0）
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterprise_id"`
	Nickname     string `json:"nickname"`
}

// WorkBuddyOAuthExchangeResult 是单次轮询结果：pending 时 Token 为 nil，
// completed 时 Token 非空。前端按 Status 决定是否继续轮询。
type WorkBuddyOAuthExchangeResult struct {
	Status string                   `json:"status"`
	Token  *WorkBuddyOAuthTokenInfo `json:"token,omitempty"`
}

// workbuddyOAuthEnvelope 是上游 {code,msg,data} 业务信封（与 workbuddy_client.go
// 的 workbuddyRefreshResponse 同一口径；data 延迟解析以兼容 pending 时空 data）。
type workbuddyOAuthEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// workbuddyLoginAccount 是 login/account 端点的展示信息子集。
type workbuddyLoginAccount struct {
	UID          string
	EnterpriseID string
	Nickname     string
}

// normalizeWorkbuddyOAuthRealm 归一化 realm 入参：仅接受 "cn"/"global"
// （大小写不敏感、容忍首尾空白），其余（含空）一律回落 "cn"——
// 对齐账号侧 GetWorkbuddyRealm 的缺省语义（无法判定时按国内版处理）。
func normalizeWorkbuddyOAuthRealm(realm string) string {
	if strings.EqualFold(strings.TrimSpace(realm), "global") {
		return "global"
	}
	return "cn"
}

// workbuddyOAuthEndpoints 按 realm 返回上游 base 与 Origin/Referer origin。
// origin 取值与 workbuddy_headers.go 的 workbuddyOriginFor 保持一致：
// CN 登录页在 codebuddy.cn（与 base 跨域），global 则同域。
func workbuddyOAuthEndpoints(realm string) (base, origin string) {
	if realm == "global" {
		return DefaultWorkbuddyGlobalBaseURL, workbuddyOriginGlobal
	}
	return DefaultWorkbuddyBaseURL, workbuddyOriginCN
}

// workbuddyOAuthEndpointsFor 返回本次调用使用的 base 与 origin：base 优先取测试注入的
// override（生产为空 → 官方默认域），origin 恒按 realm 计算（测试同样据此断言双域头）。
func (s *WorkBuddyOAuthService) workbuddyOAuthEndpointsFor(realm string) (base, origin string) {
	base, origin = workbuddyOAuthEndpoints(realm)
	if s != nil && strings.TrimSpace(s.baseURLOverride) != "" {
		base = strings.TrimRight(strings.TrimSpace(s.baseURLOverride), "/")
	}
	return base, origin
}

// workbuddyOAuthProxyURL 解析 proxyID 对应的代理 URL：nil → 直连（空串）；
// 仓库缺失/未找到 → 400（调用方配置错误）；查询失败 → 503（基础设施抖动）。
func (s *WorkBuddyOAuthService) workbuddyOAuthProxyURL(ctx context.Context, proxyID *int64) (string, error) {
	if proxyID == nil {
		return "", nil
	}
	if s == nil || s.proxyRepo == nil {
		return "", infraerrors.New(http.StatusBadRequest, "WORKBUDDY_OAUTH_PROXY_NOT_AVAILABLE", "proxy repository is not available")
	}
	proxy, err := s.proxyRepo.GetByID(ctx, *proxyID)
	if err != nil {
		if errors.Is(err, ErrProxyNotFound) {
			return "", infraerrors.New(http.StatusBadRequest, "WORKBUDDY_OAUTH_PROXY_NOT_FOUND", "configured proxy was not found")
		}
		return "", infraerrors.New(http.StatusServiceUnavailable, "WORKBUDDY_OAUTH_PROXY_LOOKUP_FAILED", "proxy lookup is temporarily unavailable")
	}
	if proxy == nil {
		return "", infraerrors.New(http.StatusBadRequest, "WORKBUDDY_OAUTH_PROXY_NOT_FOUND", "configured proxy was not found")
	}
	return proxy.URL(), nil
}

// workbuddyOAuthHTTPClient 构造设备流专用 http.Client：按 proxyURL（空 = 直连）
// 经 proxyutil 统一配置传输层代理（http/https/socks5/socks5h）。
// 每次调用新建 transport（OAuth 登录是低频操作，无需连接池复用）。
func workbuddyOAuthHTTPClient(proxyURL string) (*http.Client, error) {
	transport := &http.Transport{}
	if strings.TrimSpace(proxyURL) != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, infraerrors.Newf(http.StatusBadRequest, "WORKBUDDY_OAUTH_INVALID_PROXY", "invalid proxy url: %v", err)
		}
		if err := proxyutil.ConfigureTransportProxy(transport, parsed); err != nil {
			return nil, infraerrors.Newf(http.StatusBadRequest, "WORKBUDDY_OAUTH_UNSUPPORTED_PROXY", "unsupported proxy: %v", err)
		}
	}
	return &http.Client{Timeout: workbuddyOAuthTimeout, Transport: transport}, nil
}

// applyWorkbuddyOAuthHeaders 设置设备流通用请求头（对齐 workbuddy2api/cmd/login 的
// commonHeaders）：Origin/Referer 随 realm 变化，UA 固定 CLI 指纹。
func applyWorkbuddyOAuthHeaders(req *http.Request, origin string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", workbuddyOAuthUserAgent)
}

// workbuddyOAuthRequest 发送一次设备流上游请求，返回 HTTP 状态码与原始响应体。
// 只把传输层问题（建连/读体）转成错误；HTTP 状态与业务信封交由调用方判定——
// 设备流对「未完成」与「失败」的区分依赖 status + code 组合（见 ExchangeState）。
// accessToken 非空时附加 Authorization: Bearer（login/account 端点需要）。
func workbuddyOAuthRequest(ctx context.Context, client *http.Client, method, targetURL, origin string, body []byte, accessToken string) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, targetURL, reader)
	if err != nil {
		return 0, nil, infraerrors.Newf(http.StatusBadGateway, "WORKBUDDY_OAUTH_REQUEST_FAILED", "build workbuddy oauth request failed: %v", err)
	}
	applyWorkbuddyOAuthHeaders(req, origin)
	if token := strings.TrimSpace(accessToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, infraerrors.Newf(http.StatusBadGateway, "WORKBUDDY_OAUTH_UPSTREAM_UNREACHABLE", "workbuddy oauth upstream request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, workbuddyUpstreamErrorBodyLimit))
	if err != nil {
		return resp.StatusCode, nil, infraerrors.Newf(http.StatusBadGateway, "WORKBUDDY_OAUTH_UPSTREAM_READ_FAILED", "read workbuddy oauth upstream body failed: %v", err)
	}
	return resp.StatusCode, raw, nil
}

// GenerateAuthURL 向上游申请设备授权会话，返回授权 URL 与 state。
//
// state 由上游签发（无 PKCE、无需本地状态），因此本方法无副作用、可重复调用；
// 用户每次点「生成授权 URL」都会拿到新的 state 与登录会话。
//
// 错误语义（与 ExchangeState 的 pending 语义不同）：state 签发失败是硬错误——
// 没有 state 就没有后续流程，重试只会持续失败，直接上抛由前端提示。
func (s *WorkBuddyOAuthService) GenerateAuthURL(ctx context.Context, realm string, proxyID *int64) (*WorkBuddyAuthURLResult, error) {
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "WORKBUDDY_OAUTH_SERVICE_UNAVAILABLE", "workbuddy oauth service is not configured")
	}
	realm = normalizeWorkbuddyOAuthRealm(realm)
	proxyURL, err := s.workbuddyOAuthProxyURL(ctx, proxyID)
	if err != nil {
		return nil, err
	}
	client, err := workbuddyOAuthHTTPClient(proxyURL)
	if err != nil {
		return nil, err
	}
	base, origin := s.workbuddyOAuthEndpointsFor(realm)
	targetURL := base + workbuddyOAuthStatePath + "?" + url.Values{"platform": {workbuddyOAuthPlatform}}.Encode()
	// 设备流约定：state 端点请求体为空 JSON 对象（上游按 Content-Type 校验请求形状）。
	status, raw, err := workbuddyOAuthRequest(ctx, client, http.MethodPost, targetURL, origin, []byte("{}"), "")
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, infraerrors.Newf(http.StatusBadGateway, "WORKBUDDY_OAUTH_STATE_HTTP_ERROR",
			"workbuddy auth state failed: HTTP %d: %s", status, workbuddyTruncateForError(string(raw)))
	}
	var env workbuddyOAuthEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "WORKBUDDY_OAUTH_STATE_PARSE_FAILED",
			"parse workbuddy auth state response failed: %v", err)
	}
	if env.Code != 0 {
		return nil, infraerrors.Newf(http.StatusBadGateway, "WORKBUDDY_OAUTH_STATE_REJECTED",
			"workbuddy auth state rejected: code=%d msg=%s", env.Code, strings.TrimSpace(env.Msg))
	}
	var data struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "WORKBUDDY_OAUTH_STATE_PARSE_FAILED",
			"parse workbuddy auth state data failed: %v", err)
	}
	// 缺任一字段都无法继续（没有 URL 用户点不开、没有 state 无法轮询）。
	if strings.TrimSpace(data.State) == "" || strings.TrimSpace(data.AuthURL) == "" {
		return nil, infraerrors.New(http.StatusBadGateway, "WORKBUDDY_OAUTH_STATE_INVALID_RESPONSE",
			"workbuddy auth state response missing state or authUrl")
	}
	return &WorkBuddyAuthURLResult{
		AuthURL: strings.TrimSpace(data.AuthURL),
		State:   strings.TrimSpace(data.State),
		Realm:   realm,
	}, nil
}

// ExchangeState 单次轮询 state 的登录结果（前端会反复调用直到 completed）。
//
// 状态判定（对齐 workbuddy2api/cmd/login 的 doJSON + runPoll 实测语义）：
//   - 传输层错误 / 读体失败 → error（上游不可达，继续轮询无意义）；
//   - HTTP >= 500 → error（上游故障，硬错误）；
//   - HTTP 4xx 且能解析出业务信封 code != 0 → pending（上游用「HTTP 拒绝 + 信封 code」
//     表达业务态，如登录未完成；对齐 login/main.go 把 4xx 视为「未完成」的处理）；
//   - HTTP 4xx 且 code == 0 → error（无法判定为业务态的异常响应）；
//   - JSON 解析失败 → error（响应形状损坏，重试同样失败）；
//   - 2xx 且 code != 0 → pending（登录未完成，实测 msg 形如 "login ing"）；
//   - 2xx 且 code == 0 但 accessToken 为空 → pending（上游边界态，多轮一次即可拿到，
//     报错反而中断前端轮询体验）。
//
// 成功（completed）时先拉取 login/account 补全 uid/enterpriseId/nickname
// （best-effort，失败仅留空），再按 expiresIn 计算 expires_at。
func (s *WorkBuddyOAuthService) ExchangeState(ctx context.Context, state, realm string, proxyID *int64) (*WorkBuddyOAuthExchangeResult, error) {
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "WORKBUDDY_OAUTH_SERVICE_UNAVAILABLE", "workbuddy oauth service is not configured")
	}
	state = strings.TrimSpace(state)
	if state == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "WORKBUDDY_OAUTH_STATE_REQUIRED", "state is required")
	}
	realm = normalizeWorkbuddyOAuthRealm(realm)
	proxyURL, err := s.workbuddyOAuthProxyURL(ctx, proxyID)
	if err != nil {
		return nil, err
	}
	client, err := workbuddyOAuthHTTPClient(proxyURL)
	if err != nil {
		return nil, err
	}
	base, origin := s.workbuddyOAuthEndpointsFor(realm)
	targetURL := base + workbuddyOAuthTokenPath + "?state=" + url.QueryEscape(state)
	status, raw, err := workbuddyOAuthRequest(ctx, client, http.MethodGet, targetURL, origin, nil, "")
	if err != nil {
		return nil, err
	}
	if status >= 500 {
		return nil, infraerrors.Newf(http.StatusBadGateway, "WORKBUDDY_OAUTH_TOKEN_HTTP_ERROR",
			"workbuddy auth token failed: HTTP %d: %s", status, workbuddyTruncateForError(string(raw)))
	}
	var env workbuddyOAuthEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "WORKBUDDY_OAUTH_TOKEN_PARSE_FAILED",
			"parse workbuddy auth token response failed: %v", err)
	}
	if status >= 400 {
		if env.Code == 0 {
			return nil, infraerrors.Newf(http.StatusBadGateway, "WORKBUDDY_OAUTH_TOKEN_HTTP_ERROR",
				"workbuddy auth token failed: HTTP %d: %s", status, workbuddyTruncateForError(string(raw)))
		}
		return &WorkBuddyOAuthExchangeResult{Status: WorkBuddyOAuthStatusPending}, nil
	}
	if env.Code != 0 {
		return &WorkBuddyOAuthExchangeResult{Status: WorkBuddyOAuthStatusPending}, nil
	}
	var data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "WORKBUDDY_OAUTH_TOKEN_PARSE_FAILED",
			"parse workbuddy auth token data failed: %v", err)
	}
	if strings.TrimSpace(data.AccessToken) == "" {
		return &WorkBuddyOAuthExchangeResult{Status: WorkBuddyOAuthStatusPending}, nil
	}
	account := s.fetchWorkbuddyLoginAccount(ctx, client, base, origin, state, data.AccessToken)
	info := &WorkBuddyOAuthTokenInfo{
		AccessToken:  strings.TrimSpace(data.AccessToken),
		RefreshToken: strings.TrimSpace(data.RefreshToken),
		Domain:       strings.TrimSpace(data.Domain),
		Realm:        realm,
		UID:          account.UID,
		EnterpriseID: account.EnterpriseID,
		Nickname:     account.Nickname,
	}
	// expires_at 仅在 expiresIn 为正且未超量级（10 年）时写入：缺失/异常时保留 0
	// 表示「无过期信息」，与 workbuddy_client.go 的 preserveExpiry 语义一致
	// （workbuddyTokenNeedsRefresh 对 ExpiresAt <= 0 不主动刷新，避免脏数据把
	// 有效 token 判成过期）。
	if data.ExpiresIn > 0 && time.Duration(data.ExpiresIn)*time.Second < workbuddyRefreshExpiresInMax {
		info.ExpiresAt = time.Now().Add(time.Duration(data.ExpiresIn) * time.Second).Unix()
	}
	return &WorkBuddyOAuthExchangeResult{Status: WorkBuddyOAuthStatusCompleted, Token: info}, nil
}

// fetchWorkbuddyLoginAccount 拉取账号展示信息（uid/enterpriseId/nickname）。
//
// best-effort：任何失败（网络/解析/业务 code）都返回零值，绝不阻断凭据回填——
// 对齐 workbuddy2api/cmd/login runPoll 的「拿不到就用空值」语义（凭据本身已可用，
// 缺失的展示字段可以后续刷新或人工补录）。
func (s *WorkBuddyOAuthService) fetchWorkbuddyLoginAccount(ctx context.Context, client *http.Client, base, origin, state, accessToken string) workbuddyLoginAccount {
	targetURL := base + workbuddyOAuthAccountPath + "?state=" + url.QueryEscape(state)
	status, raw, err := workbuddyOAuthRequest(ctx, client, http.MethodGet, targetURL, origin, nil, accessToken)
	if err != nil || status >= 400 {
		return workbuddyLoginAccount{}
	}
	var env workbuddyOAuthEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Code != 0 {
		return workbuddyLoginAccount{}
	}
	var data struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return workbuddyLoginAccount{}
	}
	return workbuddyLoginAccount{
		UID:          strings.TrimSpace(data.UID),
		EnterpriseID: strings.TrimSpace(data.EnterpriseID),
		Nickname:     strings.TrimSpace(data.Nickname),
	}
}
