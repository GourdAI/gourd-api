package repository

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/mod/semver"
	"golang.org/x/net/http2"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/servertiming"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

// 默认配置常量
// 这些值在配置文件未指定时作为回退默认值使用
const (
	// directProxyKey: 无代理时的缓存键标识
	directProxyKey = "direct"
	// defaultMaxIdleConns: 默认最大空闲连接总数
	// HTTP/2 场景下，单连接可多路复用，240 足以支撑高并发
	defaultMaxIdleConns = 240
	// defaultMaxIdleConnsPerHost: 默认每主机最大空闲连接数
	defaultMaxIdleConnsPerHost = 120
	// defaultMaxConnsPerHost: 默认每主机最大连接数（含活跃连接）
	// 达到上限后新请求会等待，而非无限创建连接
	defaultMaxConnsPerHost = 240
	// defaultIdleConnTimeout: 默认空闲连接超时时间（90秒）
	// 超时后连接会被关闭，释放系统资源（建议小于上游 LB 超时）
	defaultIdleConnTimeout = 90 * time.Second
	// defaultResponseHeaderTimeout: 默认等待响应头超时时间（5分钟）
	// LLM 请求可能排队较久，需要较长超时
	defaultResponseHeaderTimeout = 300 * time.Second
	// defaultUpstreamDialTimeout: 默认 TCP/DNS 建连超时（10秒）
	// Transport 不设置 DialContext 时会退化为零值 net.Dialer（无超时），建连阶段
	// 只能依赖内核默认 TCP 重传（Linux 约 130 秒）。ResponseHeaderTimeout 只约束
	// 连接建立之后等待响应头的阶段，覆盖不到 DNS 解析与 TCP 握手。
	// 上游域名被解析到 443 不可达的 IP 时（DNS 污染/路由异常），单个账号就要卡满
	// 内核超时；而多账号故障转移是串行的，一次请求会阻塞数分钟且不写中间错误。
	defaultUpstreamDialTimeout = 10 * time.Second
	// defaultUpstreamDialKeepAlive: TCP keepalive 探测间隔，与 Go 默认值保持一致
	defaultUpstreamDialKeepAlive = 30 * time.Second
	// defaultUpstreamTLSHandshakeTimeout: TLS 握手超时（10秒）
	// 与建连超时同量级，避免 TCP 已连通但对端不推进握手时无限等待
	defaultUpstreamTLSHandshakeTimeout = 10 * time.Second
	// defaultMaxUpstreamClients: 默认最大客户端缓存数量
	// 超出后会淘汰最久未使用的客户端
	defaultMaxUpstreamClients = 5000
	// defaultClientIdleTTLSeconds: 默认客户端空闲回收阈值（15分钟）
	defaultClientIdleTTLSeconds = 900
	// OpenAI HTTP/2 代理回退策略默认值
	defaultOpenAIHTTP2FallbackErrorThreshold = 2
	defaultOpenAIHTTP2FallbackWindow         = 60 * time.Second
	defaultOpenAIHTTP2FallbackTTL            = 10 * time.Minute
	// 长流 HTTP/2 连接健康探测：池化连接被代理/NAT
	// 静默掐断会成为“死连接”（两端都以为存活），请求落上去会挂到 TCP 重传超时
	// （分钟级）。Go 的 http2.Transport 默认 ReadIdleTimeout=0（不发健康 PING），
	// 无法检测。启用主动 PING 探测：连接空闲 ReadIdleTimeout 后发 PING，PingTimeout
	// 内无响应即判定死连接并关闭，从源头避免请求挂在死连接上。
	longStreamHTTP2ReadIdleTimeout = 10 * time.Second
	longStreamHTTP2PingTimeout     = 5 * time.Second

	// The Grok CLI proxy rejects requests that do not identify a supported
	// client version. Host/env/version pins live in package xai so service,
	// billing, and transport layers advertise the same identity.
	grokCLIProxyHost       = xai.CLIProxyHost
	grokOfficialAPIHost    = "api.x.ai"
	grokCLIStableVersion   = xai.CLIClientVersion // preferred pin (not the minimum floor)
	grokCLIVersionOverride = xai.CLIVersionEnv
	grokFallbackBodyLimit  = 64 << 10
)

const (
	upstreamProtocolModeDefault          = "default"
	upstreamProtocolModeLongStreamH2     = "long_stream_h2"
	upstreamProtocolModeOpenAIH1         = "openai_h1"
	upstreamProtocolModeOpenAIH2         = "openai_h2"
	upstreamProtocolModeOpenAIH1Fallback = "openai_h1_fallback"
	upstreamProtocolModeGrok             = "grok"
)

var errUpstreamClientLimitReached = errors.New("upstream client cache limit reached")

// poolSettings 连接池配置参数
// 封装 Transport 所需的各项连接池参数
type poolSettings struct {
	maxIdleConns          int           // 最大空闲连接总数
	maxIdleConnsPerHost   int           // 每主机最大空闲连接数
	maxConnsPerHost       int           // 每主机最大连接数（含活跃）
	idleConnTimeout       time.Duration // 空闲连接超时时间
	responseHeaderTimeout time.Duration // 等待响应头超时时间
}

type openAIHTTP2Settings struct {
	enabled                   bool
	allowProxyFallbackToHTTP1 bool
	fallbackErrorThreshold    int
	fallbackWindow            time.Duration
	fallbackTTL               time.Duration
}

// upstreamClientEntry 上游客户端缓存条目
// 记录客户端实例及其元数据，用于连接池管理和淘汰策略
type upstreamClientEntry struct {
	client       *http.Client // HTTP 客户端实例
	proxyKey     string       // 代理标识（用于检测代理变更）
	poolKey      string       // 连接池配置标识（用于检测配置变更）
	protocolMode string       // 协议模式（default/long_stream_h2/openai_h1/openai_h2/openai_h1_fallback）
	lastUsed     int64        // 最后使用时间戳（纳秒），用于 LRU 淘汰
	inFlight     int64        // 当前进行中的请求数，>0 时不可淘汰
}

type openAIHTTP2FallbackState struct {
	mu            sync.Mutex
	windowStart   time.Time
	errorCount    int
	fallbackUntil time.Time
}

// httpUpstreamService 通用 HTTP 上游服务
// 用于向任意 HTTP API（Claude、OpenAI 等）发送请求，支持可选代理
//
// 架构设计：
// - 根据隔离策略（proxy/account/account_proxy）缓存客户端实例
// - 每个客户端拥有独立的 Transport 连接池
// - 支持 LRU + 空闲时间双重淘汰策略
//
// 性能优化：
// 1. 根据隔离策略缓存客户端实例，避免频繁创建 http.Client
// 2. 复用 Transport 连接池，减少 TCP 握手和 TLS 协商开销
// 3. 支持账号级隔离与空闲回收，降低连接层关联风险
// 4. 达到最大连接数后等待可用连接，而非无限创建
// 5. 仅回收空闲客户端，避免中断活跃请求
// 6. HTTP/2 多路复用，连接上限不等于并发请求上限
// 7. 代理变更时清空旧连接池，避免复用错误代理
// 8. 账号并发数与连接池上限对应（账号隔离策略下）
type httpUpstreamService struct {
	cfg     *config.Config                  // 全局配置
	mu      sync.RWMutex                    // 保护 clients map 的读写锁
	clients map[string]*upstreamClientEntry // 客户端缓存池，key 由隔离策略决定
	// OpenAI 走 HTTP/HTTPS 代理时的 H2->H1 回退状态（key=标准化 proxyKey）
	openAIHTTP2Fallbacks sync.Map
	// pacer 账号级出站节奏控制（最小间隔+抖动）；nil 表示禁用。
	pacer *accountPacer
	// gate 进程级（跨账号）出站闸门；nil 表示禁用。
	// 与 pacer 互补：pacer 只管同一账号，管不到同一出口 IP 上的聚合流量与
	// 无账号上下文的出站（换票/OAuth/探针常传 accountID=0）。
	gate *upstreamGlobalGate
}

// NewHTTPUpstream 创建通用 HTTP 上游服务
// 使用配置中的连接池参数构建 Transport
//
// 参数:
//   - cfg: 全局配置，包含连接池参数和隔离策略
//
// 返回:
//   - service.HTTPUpstream 接口实现
func NewHTTPUpstream(cfg *config.Config) service.HTTPUpstream {
	svc := &httpUpstreamService{
		cfg:     cfg,
		clients: make(map[string]*upstreamClientEntry),
	}
	if cfg != nil {
		svc.pacer = newAccountPacer(
			time.Duration(cfg.Gateway.AccountMinRequestIntervalMs)*time.Millisecond,
			time.Duration(cfg.Gateway.AccountRequestJitterMs)*time.Millisecond,
		)
		gate := newUpstreamGlobalGate(
			time.Duration(cfg.Gateway.UpstreamMinIntervalMs)*time.Millisecond,
			time.Duration(cfg.Gateway.UpstreamRequestJitterMs)*time.Millisecond,
			cfg.Gateway.UpstreamMaxConcurrentRequests,
		)
		if gate != nil {
			// 排队上限单独赋值：不改构造签名，避开已把该构造器写为 3 参的全部调用点。
			gate.maxWait = time.Duration(cfg.Gateway.UpstreamGateWaitTimeoutMs) * time.Millisecond
		}
		svc.gate = gate
	}
	return svc
}

// acquireUpstreamGate 占用一个进程级出站名额（并发槽位 + 最小间隔）。
// 未启用时返回 no-op 释放函数（恒非 nil），调用方可无条件调用。
//
// 释放时机约定（与 anti-api 的 Semaphore(1) 关键差异）：并发槽位必须等到
// **响应体关闭**才释放，否则流式（SSE）请求在拿到响应头后就放闸，
// 「同时在途上游流数」这个约束会形同虚设。调用方须把 release 同时挂到
// 三条出口：请求出错、客户端获取失败、以及 wrapTrackedBody 的 onClose。
func (s *httpUpstreamService) acquireUpstreamGate(req *http.Request) (func(), error) {
	if s == nil || s.gate == nil {
		return func() {}, nil
	}
	ctx := context.Background()
	if req != nil {
		ctx = req.Context()
	}
	return s.gate.acquire(ctx)
}

// paceAccount 在出站前应用账号级节奏控制。未启用或账号缺失时零开销放行。
func (s *httpUpstreamService) paceAccount(ctx context.Context, accountID int64) error {
	if s == nil || s.pacer == nil {
		return nil
	}
	return s.pacer.wait(ctx, accountID)
}

// Do 执行 HTTP 请求
// 根据隔离策略获取或创建客户端，并跟踪请求生命周期
//
// 参数:
//   - req: HTTP 请求对象
//   - proxyURL: 代理地址，空字符串表示直连
//   - accountID: 账户 ID，用于账户级隔离
//   - accountConcurrency: 账户并发限制，用于动态调整连接池大小
//
// 返回:
//   - *http.Response: HTTP 响应（Body 已包装，关闭时自动更新计数）
//   - error: 请求错误
//
// 注意:
//   - 调用方必须关闭 resp.Body，否则会导致 inFlight 计数泄漏
//   - inFlight > 0 的客户端不会被淘汰，确保活跃请求不被中断
func (s *httpUpstreamService) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	applyGrokCLIProxyHeaders(req)
	if err := s.validateRequestHost(req); err != nil {
		return nil, err
	}
	// 账号级出站节奏控制（默认禁用；启用后同一账号的请求按最小间隔+抖动排开）。
	if req != nil {
		if err := s.paceAccount(req.Context(), accountID); err != nil {
			return nil, err
		}
	}
	// 进程级出站闸门（默认禁用）。排在账号节奏之后：账号节奏可能长时间等待，
	// 若先占全局并发槽位再等账号间隔，槽位会被空耗，反而放大排队。
	releaseGate, err := s.acquireUpstreamGate(req)
	if err != nil {
		return nil, err
	}
	profile := service.HTTPUpstreamProfileDefault
	if req != nil {
		profile = service.HTTPUpstreamProfileFromContext(req.Context())
	}

	// 获取或创建对应的客户端，并标记请求占用
	entry, err := s.acquireClientWithProfile(proxyURL, accountID, accountConcurrency, profile)
	if err != nil {
		releaseGate()
		return nil, err
	}

	// 执行请求
	client := s.httpClientForUpstreamRequest(entry.client, req)
	client = httpClientWithGrokAccessDeniedFallback(client)
	resp, err := servertiming.Do(client, req)
	if err != nil {
		s.recordOpenAIHTTP2Failure(profile, entry.protocolMode, entry.proxyKey, err)
		// 请求失败，立即减少计数
		atomic.AddInt64(&entry.inFlight, -1)
		atomic.StoreInt64(&entry.lastUsed, time.Now().UnixNano())
		releaseGate()
		return nil, err
	}
	s.recordOpenAIHTTP2Success(profile, entry.protocolMode, entry.proxyKey)

	// 如果上游返回了压缩内容，解压后再交给业务层
	decompressResponseBody(resp)

	// 包装响应体，在关闭时自动减少计数并更新时间戳
	// 这确保了流式响应（如 SSE）在完全读取前不会被淘汰
	// 全局闸门槽位同样在此释放 —— 流式响应必须在整个流读完后才放行下一个出站。
	resp.Body = wrapTrackedBody(resp.Body, func() {
		atomic.AddInt64(&entry.inFlight, -1)
		atomic.StoreInt64(&entry.lastUsed, time.Now().UnixNano())
		releaseGate()
	})
	// 防御性兜底。注意：走标准库时此分支不可达 —— net/http 的 Client.Do 在 err==nil
	// 前提下保证 resp.Body 非 nil（自定义 RoundTripper 返回 nil Body 时标准库会填入
	// io.NopCloser("")，见 net/http/client.go）。保留只做一次 nil 判断，为测试桩与
	// 未来可能注入的自定义 transport 兜底。
	if resp.Body == nil {
		releaseGate()
	}

	return resp, nil
}

// DoWithTLS 执行带 TLS 指纹伪装的 HTTP 请求
//
// profile 为 nil 时不启用 TLS 指纹，行为与 Do 方法相同。
// profile 非 nil 时使用指定的 Profile 进行 TLS 指纹伪装。
func (s *httpUpstreamService) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	if profile == nil {
		return s.Do(req, proxyURL, accountID, accountConcurrency)
	}
	// Plain HTTP has no TLS handshake to fingerprint. Reuse the normal transport
	// so a configured HTTP or SOCKS proxy is not bypassed.
	if req != nil && req.URL != nil && strings.EqualFold(req.URL.Scheme, "http") {
		return s.Do(req, proxyURL, accountID, accountConcurrency)
	}
	applyGrokCLIProxyHeaders(req)
	upstreamProfile := service.HTTPUpstreamProfileDefault
	if req != nil {
		upstreamProfile = service.HTTPUpstreamProfileFromContext(req.Context())
	}

	targetHost := ""
	if req != nil && req.URL != nil {
		targetHost = req.URL.Host
	}
	proxyInfo := "direct"
	if proxyURL != "" {
		proxyInfo = proxyURL
	}
	slog.Debug("tls_fingerprint_enabled", "account_id", accountID, "target", targetHost, "proxy", proxyInfo, "profile", profile.Name)

	if err := s.validateRequestHost(req); err != nil {
		return nil, err
	}
	// 账号级出站节奏控制（默认禁用；与 Do 共享同一套账号状态）。
	// 放在主机校验之后：校验失败的请求不应占用账号节奏槽位。
	if req != nil {
		if err := s.paceAccount(req.Context(), accountID); err != nil {
			return nil, err
		}
	}
	// 进程级出站闸门（默认禁用；与 Do 同语义，槽位到响应体关闭才释放）。
	// 此处不会与 Do 重复占用：profile==nil 与 plain-HTTP 两个分支都在本函数开头
	// 就转发给 Do 并直接 return，走不到这里。
	releaseGate, err := s.acquireUpstreamGate(req)
	if err != nil {
		return nil, err
	}

	entry, err := s.acquireClientWithTLS(proxyURL, accountID, accountConcurrency, profile, upstreamProfile)
	if err != nil {
		releaseGate()
		slog.Debug("tls_fingerprint_acquire_client_failed", "account_id", accountID, "error", err)
		return nil, err
	}

	client := s.httpClientForUpstreamRequest(entry.client, req)
	client = httpClientWithGrokAccessDeniedFallback(client)
	resp, err := servertiming.Do(client, req)
	if err != nil {
		atomic.AddInt64(&entry.inFlight, -1)
		atomic.StoreInt64(&entry.lastUsed, time.Now().UnixNano())
		releaseGate()
		slog.Debug("tls_fingerprint_request_failed", "account_id", accountID, "error", err)
		return nil, err
	}

	decompressResponseBody(resp)

	resp.Body = wrapTrackedBody(resp.Body, func() {
		atomic.AddInt64(&entry.inFlight, -1)
		atomic.StoreInt64(&entry.lastUsed, time.Now().UnixNano())
		releaseGate()
	})
	// 与 Do 同形的防御性兜底（标准库路径不可达，见 Do 中的注释）。
	if resp.Body == nil {
		releaseGate()
	}

	return resp, nil
}

// httpClientForUpstreamRequest 按请求上下文的标记派生客户端：禁用重定向，或对重定向的每一跳做主机校验。
// 派生的克隆与缓存客户端共享 Transport；未打标记时原样返回。
func (s *httpUpstreamService) httpClientForUpstreamRequest(client *http.Client, req *http.Request) *http.Client {
	if client == nil || req == nil {
		return client
	}
	ctx := req.Context()
	switch {
	case service.HTTPUpstreamRedirectsDisabled(ctx):
		clone := *client
		clone.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		return &clone
	case service.HTTPUpstreamPublicHostsOnly(ctx) && client.CheckRedirect == nil:
		clone := *client
		clone.CheckRedirect = s.redirectChecker
		return &clone
	default:
		return client
	}
}

// grokAccessDeniedFallbackTransport preserves the subscription CLI proxy as
// the primary OAuth route, but retries a replayable request against api.x.ai
// when the proxy returns its compatibility-specific 403 "Access denied".
// Trial subscriptions can hit this boundary while the same OAuth credential
// remains valid on the official API. Other entitlement failures stay on the
// original response so account scheduling semantics do not change.
type grokAccessDeniedFallbackTransport struct {
	base http.RoundTripper
}

func httpClientWithGrokAccessDeniedFallback(client *http.Client) *http.Client {
	if client == nil {
		return nil
	}
	clone := *client
	base := clone.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clone.Transport = &grokAccessDeniedFallbackTransport{base: base}
	return &clone
}

func (t *grokAccessDeniedFallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || !isGrokCLIAccessDeniedFallbackCandidate(req, resp) {
		return resp, err
	}

	body, ok := bufferSmallResponseBody(resp, grokFallbackBodyLimit)
	if !ok || !isGrokCLICompatibilityAccessDenied(body) {
		return resp, nil
	}

	fallbackReq, err := newGrokOfficialAPIFallbackRequest(req)
	if err != nil {
		return resp, nil
	}
	fallbackResp, fallbackErr := t.base.RoundTrip(fallbackReq)
	if fallbackErr != nil {
		slog.Debug("grok_cli_access_denied_api_fallback_failed", "path", req.URL.EscapedPath(), "error", fallbackErr)
		return resp, nil
	}
	if fallbackResp.StatusCode < http.StatusOK || fallbackResp.StatusCode >= http.StatusMultipleChoices {
		if fallbackResp.Body != nil {
			_ = fallbackResp.Body.Close()
		}
		return resp, nil
	}

	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	slog.Warn("grok_cli_access_denied_api_fallback_succeeded", "method", req.Method, "path", req.URL.EscapedPath())
	return fallbackResp, nil
}

func isGrokCLICompatibilityAccessDenied(body []byte) bool {
	lower := bytes.ToLower(body)
	if bytes.Contains(lower, []byte("access denied")) {
		return true
	}
	var payload struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || !strings.EqualFold(strings.TrimSpace(payload.Code), "permission_denied") {
		return false
	}
	const chatEndpointDeniedPrefix = "access to the chat endpoint is denied. please ensure you're using the correct credentials. if you believe this is a mistake, please"
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(payload.Error)), chatEndpointDeniedPrefix)
}

func isGrokCLIAccessDeniedFallbackCandidate(req *http.Request, resp *http.Response) bool {
	return req != nil && req.URL != nil && req.GetBody != nil && resp != nil &&
		resp.StatusCode == http.StatusForbidden &&
		strings.EqualFold(strings.TrimSpace(req.URL.Hostname()), grokCLIProxyHost) &&
		strings.EqualFold(strings.TrimSpace(req.Header.Get("X-XAI-Token-Auth")), "xai-grok-cli") &&
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(req.Header.Get("Authorization"))), "bearer ")
}

func newGrokOfficialAPIFallbackRequest(req *http.Request) (*http.Request, error) {
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	fallbackReq := req.Clone(req.Context())
	fallbackReq.Body = body
	fallbackReq.URL = cloneURL(req.URL)
	fallbackReq.URL.Scheme = "https"
	fallbackReq.URL.Host = grokOfficialAPIHost
	fallbackReq.Host = ""
	fallbackReq.RequestURI = ""
	fallbackReq.Header = req.Header.Clone()
	for _, header := range []string{
		"X-XAI-Token-Auth",
		"X-Grok-Client-Version",
		"X-Grok-Client-Surface",
		"X-UserID",
		"X-Email",
		"User-Agent",
	} {
		fallbackReq.Header.Del(header)
	}
	return fallbackReq, nil
}

func cloneURL(value *url.URL) *url.URL {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func bufferSmallResponseBody(resp *http.Response, limit int64) ([]byte, bool) {
	if resp == nil || resp.Body == nil || limit <= 0 {
		return nil, false
	}
	original := resp.Body
	body, err := io.ReadAll(io.LimitReader(original, limit+1))
	if err != nil || int64(len(body)) > limit {
		resp.Body = &prefixedReadCloser{
			Reader: io.MultiReader(bytes.NewReader(body), original),
			Closer: original,
		}
		return nil, false
	}
	_ = original.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return body, true
}

type prefixedReadCloser struct {
	io.Reader
	io.Closer
}

// applyGrokCLIProxyHeaders applies the official Grok Build client identity at
// the final shared transport boundary. Keying this behavior to the exact CLI
// proxy host keeps direct api.x.ai traffic unchanged and automatically covers
// Responses, Chat Completions, media, quota probes, and account tests.
//
// Operator overrides must be >= CLIClientVersion (the preferred pin). Package
// xai.IsSupportedCLIVersion uses a lower floor (CLIStableVersion) for general
// validation; transport is stricter so we never silently advertise an older pin
// than the binary default.
func applyGrokCLIProxyHeaders(req *http.Request) {
	if req == nil || req.URL == nil || !strings.EqualFold(strings.TrimSpace(req.URL.Hostname()), grokCLIProxyHost) {
		return
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	version := strings.TrimSpace(os.Getenv(grokCLIVersionOverride))
	if !isSupportedGrokCLIVersion(version) {
		version = grokCLIStableVersion
	}
	req.Header.Set("X-XAI-Token-Auth", xai.CLITokenAuth)
	req.Header.Set("x-grok-client-version", version)
	req.Header.Set("x-grok-client-identifier", xai.CLIClientIdentifier)
	req.Header.Set("User-Agent", xai.CLIUserAgent(version))
}

func isSupportedGrokCLIVersion(version string) bool {
	canonical := "v" + version
	minimum := "v" + xai.CLIClientVersion
	return semver.IsValid(canonical) &&
		semver.Canonical(canonical) == canonical &&
		semver.Compare(canonical, minimum) >= 0
}

// acquireClientWithTLS 获取或创建带 TLS 指纹的客户端
func (s *httpUpstreamService) acquireClientWithTLS(proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile, upstreamProfile service.HTTPUpstreamProfile) (*upstreamClientEntry, error) {
	return s.getClientEntryWithTLS(proxyURL, accountID, accountConcurrency, profile, upstreamProfile, true, true)
}

// getClientEntryWithTLS 获取或创建带 TLS 指纹的客户端条目
// TLS 指纹客户端使用独立的缓存键，与普通客户端隔离
func (s *httpUpstreamService) getClientEntryWithTLS(proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile, upstreamProfile service.HTTPUpstreamProfile, markInFlight bool, enforceLimit bool) (*upstreamClientEntry, error) {
	isolation := s.getIsolationMode()
	proxyKey, parsedProxy, err := normalizeProxyURL(proxyURL)
	if err != nil {
		return nil, err
	}
	settings := s.resolvePoolSettings(isolation, accountConcurrency)
	settings = s.applyProfilePoolSettings(settings, upstreamProfile)
	// TLS 指纹客户端使用独立的缓存键，加 "tls:" 前缀。
	//
	// 防封加固修正：键中必须包含 profile 的形状摘要。此前只按
	// (isolation, proxy, accountID) 组键，而 profile 是每次调用现解析的，于是
	// 「把账号从模板 A 改绑到 B」时缓存命中的仍是按 A 建好的 Transport，
	// 新模板要等该条目空闲被淘汰（默认 900s）才生效 —— 表现为改了没生效、
	// 也不报错，且这期间实际交出的 ClientHello 与面板显示不一致（chimera）。
	// profile 为 nil 时 CacheKey() 返回空串，键形态与本改动前的 "tls:<base>" 等价语义
	// （多一段 "|fp:" 后缀，不影响与普通客户端的隔离）。
	cacheKey := "tls:" + buildCacheKey(isolation, proxyKey, accountID, upstreamProtocolModeDefault) + "|fp:" + profile.CacheKey()
	poolKey := buildPoolKey(settings, upstreamProtocolModeDefault) + ":tls"

	now := time.Now()
	nowUnix := now.UnixNano()

	// 读锁快速路径
	s.mu.RLock()
	if entry, ok := s.clients[cacheKey]; ok && s.shouldReuseEntry(entry, isolation, proxyKey, poolKey) {
		atomic.StoreInt64(&entry.lastUsed, nowUnix)
		if markInFlight {
			atomic.AddInt64(&entry.inFlight, 1)
		}
		s.mu.RUnlock()
		slog.Debug("tls_fingerprint_reusing_client", "account_id", accountID, "cache_key", cacheKey)
		return entry, nil
	}
	s.mu.RUnlock()

	// 写锁慢路径
	s.mu.Lock()
	if entry, ok := s.clients[cacheKey]; ok {
		if s.shouldReuseEntry(entry, isolation, proxyKey, poolKey) {
			atomic.StoreInt64(&entry.lastUsed, nowUnix)
			if markInFlight {
				atomic.AddInt64(&entry.inFlight, 1)
			}
			s.mu.Unlock()
			slog.Debug("tls_fingerprint_reusing_client", "account_id", accountID, "cache_key", cacheKey)
			return entry, nil
		}
		slog.Debug("tls_fingerprint_evicting_stale_client",
			"account_id", accountID,
			"cache_key", cacheKey,
			"proxy_changed", entry.proxyKey != proxyKey,
			"pool_changed", entry.poolKey != poolKey)
		s.removeClientLocked(cacheKey, entry)
	}

	// 超出缓存上限时尝试淘汰
	if enforceLimit && s.maxUpstreamClients() > 0 {
		s.evictIdleLocked(now)
		if len(s.clients) >= s.maxUpstreamClients() {
			if !s.evictOldestIdleLocked() {
				s.mu.Unlock()
				return nil, errUpstreamClientLimitReached
			}
		}
	}

	// 创建带 TLS 指纹的 Transport
	slog.Debug("tls_fingerprint_creating_new_client", "account_id", accountID, "cache_key", cacheKey, "proxy", proxyKey)
	transport, err := buildUpstreamRoundTripperWithTLSFingerprint(settings, parsedProxy, profile, upstreamProfile)
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("build TLS fingerprint transport: %w", err)
	}

	client := &http.Client{Transport: transport}
	if s.shouldValidateResolvedIP() {
		client.CheckRedirect = s.redirectChecker
	}

	entry := &upstreamClientEntry{
		client:   client,
		proxyKey: proxyKey,
		poolKey:  poolKey,
	}
	atomic.StoreInt64(&entry.lastUsed, nowUnix)
	if markInFlight {
		atomic.StoreInt64(&entry.inFlight, 1)
	}
	s.clients[cacheKey] = entry

	s.evictIdleLocked(now)
	s.evictOverLimitLocked()
	s.mu.Unlock()
	return entry, nil
}

func (s *httpUpstreamService) shouldValidateResolvedIP() bool {
	if s.cfg == nil {
		return false
	}
	if !s.cfg.Security.URLAllowlist.Enabled {
		return false
	}
	return !s.cfg.Security.URLAllowlist.AllowPrivateHosts
}

// validateRequestHost 校验请求主机的解析结果不落在回环、私网、链路本地或未指定地址。
// 是否全局启用由 security.url_allowlist 决定；带 WithHTTPUpstreamPublicHostsOnly 标记的请求无论配置如何都校验。
func (s *httpUpstreamService) validateRequestHost(req *http.Request) error {
	publicHostsOnly := req != nil && service.HTTPUpstreamPublicHostsOnly(req.Context())
	if !s.shouldValidateResolvedIP() && !publicHostsOnly {
		return nil
	}
	if req == nil || req.URL == nil {
		return errors.New("request url is nil")
	}
	host := strings.TrimSpace(req.URL.Hostname())
	if host == "" {
		return errors.New("request host is empty")
	}
	if err := urlvalidator.ValidateResolvedIP(host); err != nil {
		return err
	}
	return nil
}

func (s *httpUpstreamService) redirectChecker(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return s.validateRequestHost(req)
}

// acquireClient 获取或创建客户端，并标记为进行中请求
// 用于请求路径，避免在获取后被淘汰
func (s *httpUpstreamService) acquireClient(proxyURL string, accountID int64, accountConcurrency int) (*upstreamClientEntry, error) {
	return s.acquireClientWithProfile(proxyURL, accountID, accountConcurrency, service.HTTPUpstreamProfileDefault)
}

// acquireClientWithProfile 获取或创建客户端，并按请求 profile 选择协议策略。
func (s *httpUpstreamService) acquireClientWithProfile(proxyURL string, accountID int64, accountConcurrency int, profile service.HTTPUpstreamProfile) (*upstreamClientEntry, error) {
	return s.getClientEntry(proxyURL, accountID, accountConcurrency, profile, true, true)
}

// getOrCreateClient 获取或创建客户端
// 根据隔离策略和参数决定缓存键，处理代理变更和配置变更
//
// 参数:
//   - proxyURL: 代理地址
//   - accountID: 账户 ID
//   - accountConcurrency: 账户并发限制
//
// 返回:
//   - *upstreamClientEntry: 客户端缓存条目
//
// 隔离策略说明:
//   - proxy: 按代理地址隔离，同一代理共享客户端
//   - account: 按账户隔离，同一账户共享客户端（代理变更时重建）
//   - account_proxy: 按账户+代理组合隔离，最细粒度
func (s *httpUpstreamService) getOrCreateClient(proxyURL string, accountID int64, accountConcurrency int) (*upstreamClientEntry, error) {
	return s.getClientEntry(proxyURL, accountID, accountConcurrency, service.HTTPUpstreamProfileDefault, false, false)
}

// getClientEntry 获取或创建客户端条目
// markInFlight=true 时会标记进行中请求，用于请求路径防止被淘汰
// enforceLimit=true 时会限制客户端数量，超限且无法淘汰时返回错误
func (s *httpUpstreamService) getClientEntry(proxyURL string, accountID int64, accountConcurrency int, profile service.HTTPUpstreamProfile, markInFlight bool, enforceLimit bool) (*upstreamClientEntry, error) {
	// 获取隔离模式
	isolation := s.getIsolationMode()
	// 标准化代理 URL 并解析
	proxyKey, parsedProxy, err := normalizeProxyURL(proxyURL)
	if err != nil {
		return nil, err
	}
	// 根据请求 profile（例如 OpenAI）选择协议模式
	protocolMode := s.resolveProtocolMode(profile, proxyKey, parsedProxy)
	settings := s.resolvePoolSettings(isolation, accountConcurrency)
	settings = s.applyProfilePoolSettings(settings, profile)
	// 构建缓存键（根据隔离策略不同）
	cacheKey := buildCacheKey(isolation, proxyKey, accountID, protocolMode)
	// 构建连接池配置键（用于检测配置变更）
	poolKey := buildPoolKey(settings, protocolMode)

	now := time.Now()
	nowUnix := now.UnixNano()

	// 读锁快速路径：命中缓存直接返回，减少锁竞争
	s.mu.RLock()
	if entry, ok := s.clients[cacheKey]; ok && s.shouldReuseEntry(entry, isolation, proxyKey, poolKey) {
		atomic.StoreInt64(&entry.lastUsed, nowUnix)
		if markInFlight {
			atomic.AddInt64(&entry.inFlight, 1)
		}
		s.mu.RUnlock()
		return entry, nil
	}
	s.mu.RUnlock()

	// 写锁慢路径：创建或重建客户端
	s.mu.Lock()
	if entry, ok := s.clients[cacheKey]; ok {
		if s.shouldReuseEntry(entry, isolation, proxyKey, poolKey) {
			atomic.StoreInt64(&entry.lastUsed, nowUnix)
			if markInFlight {
				atomic.AddInt64(&entry.inFlight, 1)
			}
			s.mu.Unlock()
			return entry, nil
		}
		s.removeClientLocked(cacheKey, entry)
	}

	// 超出缓存上限时尝试淘汰，无法淘汰则拒绝新建
	if enforceLimit && s.maxUpstreamClients() > 0 {
		s.evictIdleLocked(now)
		if len(s.clients) >= s.maxUpstreamClients() {
			if !s.evictOldestIdleLocked() {
				s.mu.Unlock()
				return nil, errUpstreamClientLimitReached
			}
		}
	}

	// 缓存未命中或需要重建，创建新客户端
	transport, err := buildUpstreamTransport(settings, parsedProxy, protocolMode)
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("build transport: %w", err)
	}
	client := &http.Client{Transport: transport}
	if s.shouldValidateResolvedIP() {
		client.CheckRedirect = s.redirectChecker
	}
	entry := &upstreamClientEntry{
		client:       client,
		proxyKey:     proxyKey,
		poolKey:      poolKey,
		protocolMode: protocolMode,
	}
	atomic.StoreInt64(&entry.lastUsed, nowUnix)
	if markInFlight {
		atomic.StoreInt64(&entry.inFlight, 1)
	}
	s.clients[cacheKey] = entry

	// 执行淘汰策略：先淘汰空闲超时的，再淘汰超出数量限制的
	s.evictIdleLocked(now)
	s.evictOverLimitLocked()
	s.mu.Unlock()
	return entry, nil
}

// shouldReuseEntry 判断缓存条目是否可复用
// 若代理或连接池配置发生变化，则需要重建客户端
func (s *httpUpstreamService) shouldReuseEntry(entry *upstreamClientEntry, isolation, proxyKey, poolKey string) bool {
	if entry == nil {
		return false
	}
	if isolation == config.ConnectionPoolIsolationAccount && entry.proxyKey != proxyKey {
		return false
	}
	if entry.poolKey != poolKey {
		return false
	}
	return true
}

// removeClientLocked 移除客户端（需持有锁）
// 从缓存中删除并关闭空闲连接
//
// 参数:
//   - key: 缓存键
//   - entry: 客户端条目
func (s *httpUpstreamService) removeClientLocked(key string, entry *upstreamClientEntry) {
	delete(s.clients, key)
	if entry != nil && entry.client != nil {
		// 关闭空闲连接，释放系统资源
		// 注意：这不会中断活跃连接
		entry.client.CloseIdleConnections()
	}
}

// evictIdleLocked 淘汰空闲超时的客户端（需持有锁）
// 遍历所有客户端，移除超过 TTL 且无活跃请求的条目
//
// 参数:
//   - now: 当前时间
func (s *httpUpstreamService) evictIdleLocked(now time.Time) {
	ttl := s.clientIdleTTL()
	if ttl <= 0 {
		return
	}
	// 计算淘汰截止时间
	cutoff := now.Add(-ttl).UnixNano()
	for key, entry := range s.clients {
		// 跳过有活跃请求的客户端
		if atomic.LoadInt64(&entry.inFlight) != 0 {
			continue
		}
		// 淘汰超时的空闲客户端
		if atomic.LoadInt64(&entry.lastUsed) <= cutoff {
			s.removeClientLocked(key, entry)
		}
	}
}

// evictOldestIdleLocked 淘汰最久未使用且无活跃请求的客户端（需持有锁）
func (s *httpUpstreamService) evictOldestIdleLocked() bool {
	var (
		oldestKey   string
		oldestEntry *upstreamClientEntry
		oldestTime  int64
	)
	// 查找最久未使用且无活跃请求的客户端
	for key, entry := range s.clients {
		// 跳过有活跃请求的客户端
		if atomic.LoadInt64(&entry.inFlight) != 0 {
			continue
		}
		lastUsed := atomic.LoadInt64(&entry.lastUsed)
		if oldestEntry == nil || lastUsed < oldestTime {
			oldestKey = key
			oldestEntry = entry
			oldestTime = lastUsed
		}
	}
	// 所有客户端都有活跃请求，无法淘汰
	if oldestEntry == nil {
		return false
	}
	s.removeClientLocked(oldestKey, oldestEntry)
	return true
}

// evictOverLimitLocked 淘汰超出数量限制的客户端（需持有锁）
// 使用 LRU 策略，优先淘汰最久未使用且无活跃请求的客户端
func (s *httpUpstreamService) evictOverLimitLocked() bool {
	maxClients := s.maxUpstreamClients()
	if maxClients <= 0 {
		return false
	}
	evicted := false
	// 循环淘汰直到满足数量限制
	for len(s.clients) > maxClients {
		if !s.evictOldestIdleLocked() {
			return evicted
		}
		evicted = true
	}
	return evicted
}

// getIsolationMode 获取连接池隔离模式
// 从配置中读取，无效值回退到 account_proxy 模式
//
// 返回:
//   - string: 隔离模式（proxy/account/account_proxy）
func (s *httpUpstreamService) getIsolationMode() string {
	if s.cfg == nil {
		return config.ConnectionPoolIsolationAccountProxy
	}
	mode := strings.ToLower(strings.TrimSpace(s.cfg.Gateway.ConnectionPoolIsolation))
	if mode == "" {
		return config.ConnectionPoolIsolationAccountProxy
	}
	switch mode {
	case config.ConnectionPoolIsolationProxy, config.ConnectionPoolIsolationAccount, config.ConnectionPoolIsolationAccountProxy:
		return mode
	default:
		return config.ConnectionPoolIsolationAccountProxy
	}
}

// maxUpstreamClients 获取最大客户端缓存数量
// 从配置中读取，无效值使用默认值
func (s *httpUpstreamService) maxUpstreamClients() int {
	if s.cfg == nil {
		return defaultMaxUpstreamClients
	}
	if s.cfg.Gateway.MaxUpstreamClients > 0 {
		return s.cfg.Gateway.MaxUpstreamClients
	}
	return defaultMaxUpstreamClients
}

// clientIdleTTL 获取客户端空闲回收阈值
// 从配置中读取，无效值使用默认值
func (s *httpUpstreamService) clientIdleTTL() time.Duration {
	if s.cfg == nil {
		return time.Duration(defaultClientIdleTTLSeconds) * time.Second
	}
	if s.cfg.Gateway.ClientIdleTTLSeconds > 0 {
		return time.Duration(s.cfg.Gateway.ClientIdleTTLSeconds) * time.Second
	}
	return time.Duration(defaultClientIdleTTLSeconds) * time.Second
}

// resolvePoolSettings 解析连接池配置
// 根据隔离策略和账户并发数动态调整连接池参数
//
// 参数:
//   - isolation: 隔离模式
//   - accountConcurrency: 账户并发限制
//
// 返回:
//   - poolSettings: 连接池配置
//
// 说明:
//   - 账户隔离模式下，连接池大小与账户并发数对应
//   - 这确保了单账户不会占用过多连接资源
func (s *httpUpstreamService) resolvePoolSettings(isolation string, accountConcurrency int) poolSettings {
	settings := defaultPoolSettings(s.cfg)
	// 账户隔离模式下，根据账户并发数调整连接池大小
	if (isolation == config.ConnectionPoolIsolationAccount || isolation == config.ConnectionPoolIsolationAccountProxy) && accountConcurrency > 0 {
		settings.maxIdleConns = accountConcurrency
		settings.maxIdleConnsPerHost = accountConcurrency
		settings.maxConnsPerHost = accountConcurrency
	}
	return settings
}

func (s *httpUpstreamService) applyProfilePoolSettings(settings poolSettings, profile service.HTTPUpstreamProfile) poolSettings {
	switch profile {
	case service.HTTPUpstreamProfileOpenAI:
		settings.responseHeaderTimeout = 0
		if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIResponseHeaderTimeout > 0 {
			settings.responseHeaderTimeout = time.Duration(s.cfg.Gateway.OpenAIResponseHeaderTimeout) * time.Second
		}
	case service.HTTPUpstreamProfileGrok:
		// Grok can stall before its first byte under capacity pressure. Keep the
		// generic 600s gateway timeout from turning one request into a 10-minute
		// resource hold; streaming after headers is unaffected.
		settings.responseHeaderTimeout = 120 * time.Second
		if s != nil && s.cfg != nil {
			settings.responseHeaderTimeout = time.Duration(s.cfg.Gateway.GrokResponseHeaderTimeout) * time.Second
		}
	}
	return settings
}

// buildPoolKey 构建连接池配置键，用于检测连接池配置变更。
func buildPoolKey(settings poolSettings, protocolMode string) string {
	base := fmt.Sprintf(
		"idle:%d|idle_host:%d|max:%d|idle_timeout:%s|header_timeout:%s",
		settings.maxIdleConns,
		settings.maxIdleConnsPerHost,
		settings.maxConnsPerHost,
		settings.idleConnTimeout,
		settings.responseHeaderTimeout,
	)
	if protocolMode == "" || protocolMode == upstreamProtocolModeDefault {
		return base
	}
	return base + "|proto:" + protocolMode
}

// buildCacheKey 构建客户端缓存键
// 根据隔离策略决定缓存键的组成
//
// 参数:
//   - isolation: 隔离模式
//   - proxyKey: 代理标识
//   - accountID: 账户 ID
//
// 返回:
//   - string: 缓存键
//
// 缓存键格式:
//   - proxy 模式: "proxy:{proxyKey}"
//   - account 模式: "account:{accountID}"
//   - account_proxy 模式: "account:{accountID}|proxy:{proxyKey}"
func buildCacheKey(isolation, proxyKey string, accountID int64, protocolMode string) string {
	var base string
	switch isolation {
	case config.ConnectionPoolIsolationAccount:
		base = fmt.Sprintf("account:%d", accountID)
	case config.ConnectionPoolIsolationAccountProxy:
		base = fmt.Sprintf("account:%d|proxy:%s", accountID, proxyKey)
	default:
		base = fmt.Sprintf("proxy:%s", proxyKey)
	}
	if protocolMode != "" && protocolMode != upstreamProtocolModeDefault {
		base += "|proto:" + protocolMode
	}
	return base
}

func (s *httpUpstreamService) resolveOpenAIHTTP2Settings() openAIHTTP2Settings {
	settings := openAIHTTP2Settings{
		enabled:                   false,
		allowProxyFallbackToHTTP1: true,
		fallbackErrorThreshold:    defaultOpenAIHTTP2FallbackErrorThreshold,
		fallbackWindow:            defaultOpenAIHTTP2FallbackWindow,
		fallbackTTL:               defaultOpenAIHTTP2FallbackTTL,
	}
	if s == nil || s.cfg == nil {
		return settings
	}
	cfg := s.cfg.Gateway.OpenAIHTTP2
	settings.enabled = cfg.Enabled
	settings.allowProxyFallbackToHTTP1 = cfg.AllowProxyFallbackToHTTP1
	if cfg.FallbackErrorThreshold > 0 {
		settings.fallbackErrorThreshold = cfg.FallbackErrorThreshold
	}
	if cfg.FallbackWindowSeconds > 0 {
		settings.fallbackWindow = time.Duration(cfg.FallbackWindowSeconds) * time.Second
	}
	if cfg.FallbackTTLSeconds > 0 {
		settings.fallbackTTL = time.Duration(cfg.FallbackTTLSeconds) * time.Second
	}
	return settings
}

func (s *httpUpstreamService) resolveProtocolMode(profile service.HTTPUpstreamProfile, proxyKey string, parsedProxy *url.URL) string {
	if profile == service.HTTPUpstreamProfileLongStream {
		return upstreamProtocolModeLongStreamH2
	}
	if profile == service.HTTPUpstreamProfileGrok {
		return upstreamProtocolModeGrok
	}
	if profile != service.HTTPUpstreamProfileOpenAI {
		return upstreamProtocolModeDefault
	}
	settings := s.resolveOpenAIHTTP2Settings()
	if !settings.enabled {
		return upstreamProtocolModeOpenAIH1
	}
	if parsedProxy == nil {
		return upstreamProtocolModeOpenAIH2
	}
	scheme := strings.ToLower(parsedProxy.Scheme)
	if scheme != "http" && scheme != "https" {
		return upstreamProtocolModeOpenAIH2
	}
	if settings.allowProxyFallbackToHTTP1 && s.isOpenAIHTTP2FallbackActive(proxyKey) {
		return upstreamProtocolModeOpenAIH1Fallback
	}
	return upstreamProtocolModeOpenAIH2
}

func (s *httpUpstreamService) isOpenAIHTTP2FallbackActive(proxyKey string) bool {
	raw, ok := s.openAIHTTP2Fallbacks.Load(proxyKey)
	if !ok {
		return false
	}
	state, ok := raw.(*openAIHTTP2FallbackState)
	if !ok || state == nil {
		return false
	}
	return state.isFallbackActive(time.Now())
}

func (s *httpUpstreamService) getOrCreateOpenAIHTTP2FallbackState(proxyKey string) *openAIHTTP2FallbackState {
	state := &openAIHTTP2FallbackState{}
	actual, _ := s.openAIHTTP2Fallbacks.LoadOrStore(proxyKey, state)
	cached, ok := actual.(*openAIHTTP2FallbackState)
	if !ok || cached == nil {
		return state
	}
	return cached
}

func isHTTPProxyKey(proxyKey string) bool {
	return strings.HasPrefix(proxyKey, "http://") || strings.HasPrefix(proxyKey, "https://")
}

func isOpenAIHTTP2CompatibilityError(err error) bool {
	if err == nil {
		return false
	}
	if isUpstreamTimeoutError(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	if msg == "" {
		return false
	}
	markers := []string{
		"alpn",
		"no application protocol",
		"protocol error",
		"stream error",
		"goaway",
		"refused_stream",
		"frame too large",
	}
	for _, marker := range markers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func isUpstreamTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	if msg == "" {
		return false
	}
	timeoutMarkers := []string{
		"timeout awaiting response headers",
		"i/o timeout",
		"context deadline exceeded",
		"client.timeout exceeded while awaiting headers",
		"tls handshake timeout",
	}
	for _, marker := range timeoutMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func (s *httpUpstreamService) recordOpenAIHTTP2Failure(profile service.HTTPUpstreamProfile, protocolMode, proxyKey string, err error) {
	if profile != service.HTTPUpstreamProfileOpenAI || protocolMode != upstreamProtocolModeOpenAIH2 {
		return
	}
	settings := s.resolveOpenAIHTTP2Settings()
	if !settings.enabled || !settings.allowProxyFallbackToHTTP1 {
		return
	}
	if !isHTTPProxyKey(proxyKey) || !isOpenAIHTTP2CompatibilityError(err) {
		return
	}
	state := s.getOrCreateOpenAIHTTP2FallbackState(proxyKey)
	activated, until := state.recordFailure(time.Now(), settings.fallbackErrorThreshold, settings.fallbackWindow, settings.fallbackTTL)
	if activated {
		slog.Warn("openai_http2_proxy_fallback_activated",
			"proxy", proxyKey,
			"fallback_until", until.Format(time.RFC3339))
	}
}

func (s *httpUpstreamService) recordOpenAIHTTP2Success(profile service.HTTPUpstreamProfile, protocolMode, proxyKey string) {
	if profile != service.HTTPUpstreamProfileOpenAI || protocolMode != upstreamProtocolModeOpenAIH2 {
		return
	}
	if !isHTTPProxyKey(proxyKey) {
		return
	}
	raw, ok := s.openAIHTTP2Fallbacks.Load(proxyKey)
	if !ok {
		return
	}
	state, ok := raw.(*openAIHTTP2FallbackState)
	if !ok || state == nil {
		return
	}
	state.resetErrorWindow()
}

func (s *openAIHTTP2FallbackState) isFallbackActive(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fallbackUntil.IsZero() {
		return false
	}
	if now.Before(s.fallbackUntil) {
		return true
	}
	s.fallbackUntil = time.Time{}
	return false
}

func (s *openAIHTTP2FallbackState) resetErrorWindow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.windowStart = time.Time{}
	s.errorCount = 0
}

func (s *openAIHTTP2FallbackState) recordFailure(now time.Time, threshold int, window, ttl time.Duration) (bool, time.Time) {
	if threshold <= 0 {
		threshold = defaultOpenAIHTTP2FallbackErrorThreshold
	}
	if window <= 0 {
		window = defaultOpenAIHTTP2FallbackWindow
	}
	if ttl <= 0 {
		ttl = defaultOpenAIHTTP2FallbackTTL
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.fallbackUntil.IsZero() && now.Before(s.fallbackUntil) {
		return false, s.fallbackUntil
	}
	if !s.fallbackUntil.IsZero() && !now.Before(s.fallbackUntil) {
		s.fallbackUntil = time.Time{}
	}

	if s.windowStart.IsZero() || now.Sub(s.windowStart) > window {
		s.windowStart = now
		s.errorCount = 0
	}
	s.errorCount++
	if s.errorCount < threshold {
		return false, time.Time{}
	}

	s.fallbackUntil = now.Add(ttl)
	s.windowStart = time.Time{}
	s.errorCount = 0
	return true, s.fallbackUntil
}

// normalizeProxyURL 标准化代理 URL
// 处理空值和解析错误，返回标准化的键和解析后的 URL
//
// 参数:
//   - raw: 原始代理 URL 字符串
//
// 返回:
//   - string: 标准化的代理键（空返回 "direct"）
//   - *url.URL: 解析后的 URL（空返回 nil）
//   - error: 非空代理 URL 解析失败时返回错误（禁止回退到直连）
func normalizeProxyURL(raw string) (string, *url.URL, error) {
	_, parsed, err := proxyurl.Parse(raw)
	if err != nil {
		return "", nil, err
	}
	if parsed == nil {
		return directProxyKey, nil, nil
	}
	// 规范化：小写 scheme/host，去除路径和查询参数
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = ""
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.ForceQuery = false
	if hostname := parsed.Hostname(); hostname != "" {
		port := parsed.Port()
		if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
			port = ""
		}
		hostname = strings.ToLower(hostname)
		if port != "" {
			parsed.Host = net.JoinHostPort(hostname, port)
		} else {
			parsed.Host = hostname
		}
	}
	return parsed.String(), parsed, nil
}

// defaultPoolSettings 获取默认连接池配置
// 从全局配置中读取，无效值使用常量默认值
//
// 参数:
//   - cfg: 全局配置
//
// 返回:
//   - poolSettings: 连接池配置
func defaultPoolSettings(cfg *config.Config) poolSettings {
	maxIdleConns := defaultMaxIdleConns
	maxIdleConnsPerHost := defaultMaxIdleConnsPerHost
	maxConnsPerHost := defaultMaxConnsPerHost
	idleConnTimeout := defaultIdleConnTimeout
	responseHeaderTimeout := defaultResponseHeaderTimeout

	if cfg != nil {
		if cfg.Gateway.MaxIdleConns > 0 {
			maxIdleConns = cfg.Gateway.MaxIdleConns
		}
		if cfg.Gateway.MaxIdleConnsPerHost > 0 {
			maxIdleConnsPerHost = cfg.Gateway.MaxIdleConnsPerHost
		}
		if cfg.Gateway.MaxConnsPerHost >= 0 {
			maxConnsPerHost = cfg.Gateway.MaxConnsPerHost
		}
		if cfg.Gateway.IdleConnTimeoutSeconds > 0 {
			idleConnTimeout = time.Duration(cfg.Gateway.IdleConnTimeoutSeconds) * time.Second
		}
		if cfg.Gateway.ResponseHeaderTimeout >= 0 {
			responseHeaderTimeout = time.Duration(cfg.Gateway.ResponseHeaderTimeout) * time.Second
		}
	}

	return poolSettings{
		maxIdleConns:          maxIdleConns,
		maxIdleConnsPerHost:   maxIdleConnsPerHost,
		maxConnsPerHost:       maxConnsPerHost,
		idleConnTimeout:       idleConnTimeout,
		responseHeaderTimeout: responseHeaderTimeout,
	}
}

// newUpstreamDialer 构建上游 Transport 的 TCP dialer。
//
// 必须显式提供：http.Transport 的 DialContext 为 nil 时使用零值 net.Dialer，
// 建连没有任何超时上限，只能等内核 TCP 重传耗尽（Linux 约 130 秒）。
func newUpstreamDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   defaultUpstreamDialTimeout,
		KeepAlive: defaultUpstreamDialKeepAlive,
	}
}

// buildUpstreamTransport 构建上游请求的 Transport
// 使用配置文件中的连接池参数，支持生产环境调优
//
// 参数:
//   - settings: 连接池配置
//   - proxyURL: 代理 URL（nil 表示直连）
//
// 返回:
//   - *http.Transport: 配置好的 Transport 实例
//   - error: 代理配置错误
//
// Transport 参数说明:
//   - DialContext: DNS 解析 + TCP 建连超时（不设置则无上限，退化为内核默认重传）
//   - TLSHandshakeTimeout: TLS 握手超时
//   - MaxIdleConns: 所有主机的最大空闲连接总数
//   - MaxIdleConnsPerHost: 每主机最大空闲连接数（影响连接复用率）
//   - MaxConnsPerHost: 每主机最大连接数（达到后新请求等待）
//   - IdleConnTimeout: 空闲连接超时（超时后关闭）
//   - ResponseHeaderTimeout: 等待响应头超时（不影响流式传输）
func buildUpstreamTransport(settings poolSettings, proxyURL *url.URL, protocolMode string) (*http.Transport, error) {
	transport := &http.Transport{
		DialContext:           newUpstreamDialer().DialContext,
		TLSHandshakeTimeout:   defaultUpstreamTLSHandshakeTimeout,
		MaxIdleConns:          settings.maxIdleConns,
		MaxIdleConnsPerHost:   settings.maxIdleConnsPerHost,
		MaxConnsPerHost:       settings.maxConnsPerHost,
		IdleConnTimeout:       settings.idleConnTimeout,
		ResponseHeaderTimeout: settings.responseHeaderTimeout,
	}
	switch protocolMode {
	case upstreamProtocolModeLongStreamH2, upstreamProtocolModeOpenAIH2:
		transport.ForceAttemptHTTP2 = true
		// 显式配置 http2 并启用 PING 健康探测，剔除代理/NAT 静默掐断的死连接，
		// 避免请求挂在死连接上直到 TCP 重传超时（分钟级）。
		if _, err := enableHTTP2KeepAlive(transport); err != nil {
			return nil, err
		}
	case upstreamProtocolModeOpenAIH1:
		transport.ForceAttemptHTTP2 = false
		transport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	case upstreamProtocolModeOpenAIH1Fallback:
		// 显式禁用 HTTP/2，确保代理不兼容场景回退到 HTTP/1.1。
		transport.ForceAttemptHTTP2 = false
		transport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	}
	if err := proxyutil.ConfigureTransportProxy(transport, proxyURL); err != nil {
		return nil, err
	}
	return transport, nil
}

// enableHTTP2KeepAlive 在 http.Transport 上显式配置 HTTP/2 并启用连接健康探测。
// Go 默认惰性配置 http2 且 ReadIdleTimeout=0（不发健康 PING），无法检测被代理/NAT
// 静默掐断的死连接。此处主动设置 ReadIdleTimeout/PingTimeout，让死连接被提前 PING
// 出并关闭，请求得以重建连接而非挂到 TCP 重传超时。返回底层 *http2.Transport 便于测试。
func enableHTTP2KeepAlive(transport *http.Transport) (*http2.Transport, error) {
	h2, err := http2.ConfigureTransports(transport)
	if err != nil {
		return nil, err
	}
	if h2 != nil {
		h2.ReadIdleTimeout = longStreamHTTP2ReadIdleTimeout
		h2.PingTimeout = longStreamHTTP2PingTimeout
	}
	return h2, nil
}

// fingerprintNegotiatesHTTP2 报告该指纹 Profile 是否应当跑 HTTP/2。
//
// 判据就是 Profile 自己声明的 ALPN：包含 "h2" 就走 http2.Transport，否则保持 HTTP/1.1。
// 这样「是否多路复用」由指纹形状单一来源决定，不会出现「ALPN 声称 h2 但传输层只会 h1」
// 这种自相矛盾的 chimera（那正是本仓反复强调要避免的形态）。
func fingerprintNegotiatesHTTP2(profile *tlsfingerprint.Profile) bool {
	if profile == nil {
		return false
	}
	for _, p := range profile.ALPNProtocols {
		if strings.EqualFold(strings.TrimSpace(p), "h2") {
			return true
		}
	}
	return false
}

// buildUpstreamTransportWithTLSFingerprint 构建带 TLS 指纹伪装的 Transport
// 使用 utls 库模拟 Claude CLI 的 TLS 指纹
//
// 参数:
//   - settings: 连接池配置
//   - proxyURL: 代理 URL（nil 表示直连）
//   - profile: TLS 指纹配置
//
// 返回:
//   - *http.Transport: 配置好的 Transport 实例
//   - error: 配置错误
//
// 代理类型处理:
//   - nil/空: 直连，使用 TLSFingerprintDialer
//   - http/https: HTTP 代理，使用 HTTPProxyDialer（CONNECT 隧道 + utls 握手）
//   - socks5: SOCKS5 代理，使用 SOCKS5ProxyDialer（SOCKS5 隧道 + utls 握手）
//
// HTTP/2 不在本函数处理，见 buildUpstreamRoundTripperWithTLSFingerprint（它才是调用方
// 应用的入口）。本函数永远返回 *http.Transport（签名与行为与改动前一致），以免
// 触碰既有代理路径测试的类型假设。
//
// 为何 h2 必须换 transport 实现而不能只设 ForceAttemptHTTP2：
//
//	http.Transport 在 DialTLSContext 路径上要求连接能断言成
//	`ConnectionState() tls.ConnectionState`（crypto/tls 类型）才会走 h2
//	（Go 源码 net/http/transport.go:1892 与 :2059 的 tlsState.NegotiatedProtocol=="h2"），
//	而 utls 的 ConnectionState 是它自己的 struct（utls common.go:239），断言必失败
//	⇒ 永不启用 h2。实测：ForceAttemptHTTP2=true + ALPN 声明 h2，仍拿到 HTTP/1.1。
//
// 不这样修的性能后果（真实上游 copilot.tencent.com，12 并发，同机同网实测）：
//
//	改造前 普通/h2/池1 = 105ms；改造后 指纹/h1/池1 = 779ms（7.4×）；本修复后 = 104ms。
//
// 根因链：指纹 transport 只能 h1，而 resolvePoolSettings 在账号隔离下把
// maxConnsPerHost 夹成 account.Concurrency（线上常见 1~3）。h2 单连接可复用所以从前
// 无害；h1 下同账号并发请求全部串行排队，且每个排队请求再付一次完整 utls 握手。
func buildUpstreamTransportWithTLSFingerprint(settings poolSettings, proxyURL *url.URL, profile *tlsfingerprint.Profile) (*http.Transport, error) {
	transport := &http.Transport{
		MaxIdleConns:          settings.maxIdleConns,
		MaxIdleConnsPerHost:   settings.maxIdleConnsPerHost,
		MaxConnsPerHost:       settings.maxConnsPerHost,
		IdleConnTimeout:       settings.idleConnTimeout,
		ResponseHeaderTimeout: settings.responseHeaderTimeout,
		// 禁用默认的 TLS，我们使用自定义的 DialTLSContext
		ForceAttemptHTTP2: false,
	}

	// 根据代理类型选择合适的 TLS 指纹 Dialer
	if proxyURL == nil {
		// 直连：使用 TLSFingerprintDialer
		slog.Debug("tls_fingerprint_transport_direct")
		dialer := tlsfingerprint.NewDialer(profile, nil)
		transport.DialTLSContext = dialer.DialTLSContext
	} else {
		scheme := strings.ToLower(proxyURL.Scheme)
		switch scheme {
		case "socks5", "socks5h":
			// SOCKS5 代理：使用 SOCKS5ProxyDialer
			slog.Debug("tls_fingerprint_transport_socks5", "proxy", proxyURL.Host)
			socks5Dialer := tlsfingerprint.NewSOCKS5ProxyDialer(profile, proxyURL)
			transport.DialTLSContext = socks5Dialer.DialTLSContext
		case "https":
			// The fingerprint dialer emits a plaintext CONNECT preface and cannot
			// establish TLS to an HTTPS proxy. Keep proxy routing via net/http.
			return buildUpstreamTransport(settings, proxyURL, upstreamProtocolModeDefault)
		case "http":
			// HTTP/HTTPS 代理：使用 HTTPProxyDialer（CONNECT 隧道）
			slog.Debug("tls_fingerprint_transport_http_connect", "proxy", proxyURL.Host)
			httpDialer := tlsfingerprint.NewHTTPProxyDialer(profile, proxyURL)
			transport.DialTLSContext = httpDialer.DialTLSContext
		default:
			// 未知代理类型，回退到普通代理配置（无 TLS 指纹）
			slog.Debug("tls_fingerprint_transport_unknown_scheme_fallback", "scheme", scheme)
			if err := proxyutil.ConfigureTransportProxy(transport, proxyURL); err != nil {
				return nil, err
			}
		}
	}

	return transport, nil
}

// buildUpstreamRoundTripperWithTLSFingerprint 是带指纹出站的**推荐入口**：在
// 「http2.Transport（单连接多路复用）」与「http.Transport（HTTP/1.1）」之间分派。
//
// 为何需要单独一个入口：指纹 transport 只能跑 h1（原因见下方
// buildFingerprintHTTP2Transport 的说明），而账号隔离下 maxConnsPerHost 被
// resolvePoolSettings 夹成 account.Concurrency（线上常见 1~3）。h1 + 小连接上限
// = 同账号并发请求串行排队，且每个排队请求再付一次完整 utls 握手。实测
// copilot.tencent.com 12 并发：改造前（无指纹、h2）105ms → 改造后（指纹、h1）779ms
// → 本入口（指纹、h2）104ms。这就是 WorkBuddy/Trae/Qoder 「接口变慢」的根因。
//
// 分派判据是三个条件的**合取**，各自职责不同（不是冗余）：
//
//   - profile 的 ALPN 含 h2 —— 回答「能不能」：传输层协议必须与 ClientHello 里声明的
//     ALPN 自洽，否则就是本仓反复避免的 chimera。由指纹形状单一来源决定。
//   - upstreamProfile == OpenAI —— 回答「要不要」：http2.Transport **没有**
//     ResponseHeaderTimeout 对应字段，切过去会丢掉该超时。
//     注意实际覆盖面比「聊天主链路」大得多：三平台（WorkBuddy/Trae/Qoder）的
//     **全部** 出站都注入 HTTPUpstreamProfileOpenAI —— 除聊天外还包括签到/余额/
//     活跃上报/换票/模型列表（workbuddy_tasks_service.go:589、
//     workbuddy_credits_service.go:508/667、trae_credits_service.go:803、
//     qoder_campaign_service.go:864），因此这些辅助面也会被切到 h2。
//     该超时丢失可接受，因为：聊天路径该值本就是 0（applyProfilePoolSettings 对
//     OpenAI profile 置 0，默认 gateway.openai_response_header_timeout=0）；而辅助面
//     请求全部自带 context.WithTimeout 兜底（workbuddyBillingTimeout /
//     traeBillingTimeout / qoderCampaignTimeout 等），不会无限挂住。
//     保留本条件的真正作用是：其它平台（OpenAI/Claude/Grok 等）的 Default/Grok 画像
//     仍走 http.Transport，ResponseHeaderTimeout 语义零变更。
//     【若将来把 gateway.openai_response_header_timeout 配成非 0】三平台出站会静默
//     失去该超时，需同步给 http2.Transport 补一层 per-request context 超时。
//   - 直连或 http CONNECT 代理 —— 见下方「代理范围」。
//
// 代理范围：仅直连与 http CONNECT 代理启用 h2；socks5/https 代理保持原
// http.Transport 路径，避免一次改动同时触碰代理隧道语义。
func buildUpstreamRoundTripperWithTLSFingerprint(
	settings poolSettings,
	proxyURL *url.URL,
	profile *tlsfingerprint.Profile,
	upstreamProfile service.HTTPUpstreamProfile,
) (http.RoundTripper, error) {
	if upstreamProfile == service.HTTPUpstreamProfileOpenAI &&
		fingerprintNegotiatesHTTP2(profile) &&
		(proxyURL == nil || strings.EqualFold(proxyURL.Scheme, "http")) {
		return buildFingerprintHTTP2Transport(settings, proxyURL, profile)
	}
	return buildUpstreamTransportWithTLSFingerprint(settings, proxyURL, profile)
}

// buildFingerprintHTTP2Transport 用「utls 指纹拨号 + http2.Transport」构造可多路复用的传输层。
//
// 存在的理由见 buildUpstreamTransportWithTLSFingerprint 的 HTTP/2 说明：标准库
// http.Transport 在自定义 DialTLSContext 路径上拿不到 crypto/tls 的连接状态，因此
// 永不升级到 h2（Go 源码 transport.go:1892 的 connectionStater 接口与 :2109 的
// pconn.conn.(*tls.Conn) **具体类型**断言；utls 的 Conn.ConnectionState 返回它自己的
// ConnectionState（utls conn.go:1622），包一层方法也满不了 2109 那道断言），
// 所以只能改用 http2.Transport。
//
// ⚠ 重要约束：http2.Transport **不会**根据 ALPN 协商结果回落到 HTTP/1.1，它把拿到的
// 任何连接都当 h2 用。实测（httptest 起一个 NextProtos=["http/1.1"] 的 TLS 服务，客户端
// 声明 [h2, http/1.1]）：RoundTrip 直接失败，报 "http2: failed reading the frame payload:
// http2: frame too large, note that the frame header looked like an HTTP/1.1 header"。
// ⇒ 若三平台账号的 base_url 被配成只支持 HTTP/1.1 的服务（中转/自建网关，
// validateUpstreamBaseURL 不限制协议能力），该账号请求会**全量硬失败**而不是退回 h1。
// 已实测三家官方共 19 个域名 ALPN 均协商到 h2，默认配置下不触发；但自定义 base_url
// 场景需靠「账号级显式关闭指纹」或改回不带 h2 的模板逃生（三态修复已使前者真正可用）。
//
// 实测（copilot.tencent.com，12 并发，池上限 1）：HTTP/1.1 指纹路径 779ms，本路径 104ms
// 且协商结果 HTTP/2.0；反向对照：ALPN 只声明 http/1.1 时 http2.Transport 直接报错
// （"received record with version 5454"）⇒ 证明 h2 是真实协商结果，不是自说自话。
//
// 字段映射说明（诚实口径）：
//   - IdleConnTimeout / ReadIdleTimeout / PingTimeout 直接对应；后两项沿用长流 h2 的
//     既有常量，用于剔除被代理/NAT 静默掐断的死连接（与 enableHTTP2KeepAlive 同口径）。
//   - maxIdleConnsPerHost / maxConnsPerHost **无法 1:1 映射**：h2 单连接多路复用，
//     连接数上限不再等价于并发上限。这里不拿它们去限制 h2 并发，否则会把账号并发
//     从「请求数」错解成「连接数」；真正的请求并发由调度层 tryAcquireAccountSlot
//     （openai_account_scheduler.go:574，以 account.Concurrency 为额）控制，与本层无关。
//   - ResponseHeaderTimeout 在 http2.Transport 上无对应字段。三平台的全部出站（聊天
//     与签到/余额/活跃上报/换票/模型列表）都注入 HTTPUpstreamProfileOpenAI，其
//     responseHeaderTimeout 默认被 applyProfilePoolSettings 置 0（无超时），因此默认配置下
//     不丢语义；辅助面另有 context.WithTimeout 兜底。仅当
//     gateway.openai_response_header_timeout 被配成非 0 时，该超时会被静默丢弃（见
//     buildUpstreamRoundTripperWithTLSFingerprint 的告警）。
func buildFingerprintHTTP2Transport(settings poolSettings, proxyURL *url.URL, profile *tlsfingerprint.Profile) (http.RoundTripper, error) {
	// 适配签名：本仓指纹拨号器是 http.Transport 形态的
	// DialTLSContext(ctx, network, addr)，而 http2.Transport 多一个 *tls.Config 形参
	// （标准库用它传 NextProtos/ServerName）。这里刻意**不转发** cfg：
	// ServerName 由拨号器从 addr 自行推导，ALPN 由 Profile.ALPNProtocols 写进
	// ClientHello（见 tlsfingerprint.buildClientHelloSpecFromProfile）。若把 cfg 的
	// NextProtos 拿去覆盖，反而会绕过指纹声明，造出「ClientHello 与 ALPN 不一致」。
	dialTLSContext := func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
		if proxyURL == nil {
			return tlsfingerprint.NewDialer(profile, nil).DialTLSContext(ctx, network, addr)
		}
		return tlsfingerprint.NewHTTPProxyDialer(profile, proxyURL).DialTLSContext(ctx, network, addr)
	}
	tr := &http2.Transport{
		DialTLSContext:  dialTLSContext,
		IdleConnTimeout: settings.idleConnTimeout,
		ReadIdleTimeout: longStreamHTTP2ReadIdleTimeout,
		PingTimeout:     longStreamHTTP2PingTimeout,
	}
	slog.Debug("tls_fingerprint_transport_http2",
		"profile", profile.Name,
		"alpn", strings.Join(profile.ALPNProtocols, ","),
		"proxy", proxyURL != nil)
	return tr, nil
}

// trackedBody 带跟踪功能的响应体包装器
// 在 Close 或「读到结束」时执行回调，用于递减请求计数并归还进程级出站闸门槽位。
type trackedBody struct {
	io.ReadCloser // 原始响应体
	once          sync.Once
	onClose       func() // 结束时的回调函数（幂等，至多执行一次）
}

// Close 关闭响应体并执行回调。
// 与 Read 的结束判定共用同一个 once，因此 Close 与读完叠加也不会重复递减。
func (b *trackedBody) Close() error {
	err := b.ReadCloser.Close()
	b.fire()
	return err
}

// Read 读取响应体；只要读到结束信号（io.EOF 或任何读错误）就触发回调。
//
// WHY：回调承担「递减 entry.inFlight」与「归还闸门槽位」两件事，而原实现只认 Close。
// 一旦下游把 resp.Body 再包一层且不转发 Close，回调就永远不执行 —— 本仓三家聊天出站
// 的流式成功路径正是这种形态：trae_client.go:204、qoder_client.go:225、
// workbuddy_client.go:191 都写 `resp.Body = io.NopCloser(<归一化 reader>)`，而这些
// 归一化 reader 的构造器形参只有 io.Reader（trae_sse.go:471、qoder_sse.go:286、
// workbuddy_sse.go:64），Closer 在类型层就已丢失；上层 defer resp.Body.Close()
// （trae_gateway.go:68、workbuddy_gateway.go:103）关的是 NopCloser，等于 no-op。
// 结果每个流式请求净泄漏一个槽位且永不回收，启用 upstream_max_concurrent_requests
// 后表现为实例级出站全部阻塞。
//
// 这里把契约从「必须 Close」放宽为「Close 或读完」：EOF 是响应体已完整消费的确定
// 信号，此刻放行下一个出站是准确的；非 EOF 的读错误同样意味着本次事务结束（连接层
// 错误不可恢复），不释放就会把异常场景变成永久泄漏。两者都走 once，
// 因此与后续的 Close 叠加也不会重复递减。
//
// 仍未覆盖的形态（实测确认，不是推理）：调用方**既不 Close 也不读完**就丢弃响应体。
// 典型场景是客户端中途断开：三家归一化 reader 停止被读，既不会到 EOF（上游还在发），
// 也不会触发真正的 Close（外层是 io.NopCloser）——槽位仍会泄漏一个。本层无法修它：
// 没有任何信号送到 trackedBody。两道可用的兼容措施：
//   - 首选：启用 gateway.upstream_gate_wait_timeout_ms，把「永久阻塞」降级为可观察失败；
//   - 彻底：让三家流式路径改为**包住**原 ReadCloser（仓内范式 openai_gateway_forward.go:1094），
//     而不是用 io.NopCloser 丢弃它。那属跨模块改动，本次未做。
//
// inFlight 语义变化需留意：现在 EOF 也会递减它（原先只在 Close），而 inFlight>0 是
// 「活跃请求不被淘汰」的保护（evictIdleLocked）。这段窗口内连接可能正被 Transport
// 归还复用，但它已不再持有响应体；且淘汰还额外要求 lastUsed 超过空闲 TTL，而 EOF 同时
// 刷新了 lastUsed，因此不会中断进行中的请求。换来的好处是顺带修掉了历史遗留的
// inFlight 泄漏（调用方漏 Close 时计数永久偏高）。
//
// 语义变化需留意：inFlight 会在 EOF 时提前递减（早于 Close），而 inFlight>0 是
// 「活跃请求不被淘汰」的保护。这段窗口内连接可能正在被 Transport 归还复用，但它已
// 不再持有响应体；且淘汰逻辑只对空闲条目动手，因此不会中断已完成的请求。换来的是
// 同时修掉了历史遗留的 inFlight 泄漏（调用方漏 Close 时计数永久偏高）。
func (b *trackedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.fire()
	}
	return n, err
}

// fire 至多执行一次回调。
func (b *trackedBody) fire() {
	if b.onClose != nil {
		b.once.Do(b.onClose)
	}
}

// wrapTrackedBody 包装响应体以跟踪「本次出站结束」事件
// 用于递减 inFlight 计数并归还进程级闸门槽位。
//
// 结束信号包含 Close、读到 io.EOF 以及任何读错误 —— 见 trackedBody.Read 的注释：
// 只认 Close 会被下游「包一层但不转发 Close」的写法永久卡住。
//
// 参数:
//   - body: 原始响应体
//   - onClose: 结束时的回调函数（实现内部保证至多执行一次）
//
// 返回:
//   - io.ReadCloser: 包装后的响应体
func wrapTrackedBody(body io.ReadCloser, onClose func()) io.ReadCloser {
	if body == nil {
		return body
	}
	return &trackedBody{ReadCloser: body, onClose: onClose}
}

// decompressResponseBody 根据 Content-Encoding 解压响应体。
// 当请求显式设置了 accept-encoding 时，Go 的 Transport 不会自动解压，需要手动处理。
// 解压成功后会删除 Content-Encoding 和 Content-Length header（长度已不准确）。
func decompressResponseBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	ce := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if ce == "" {
		return
	}

	originalBody := resp.Body
	var reader io.Reader
	switch ce {
	case "gzip":
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return // 解压失败，保持原样
		}
		reader = gr
	case "br":
		reader = brotli.NewReader(resp.Body)
	case "deflate":
		reader = flate.NewReader(resp.Body)
	case "zstd":
		bufferedBody := bufio.NewReader(resp.Body)
		resp.Body = &decompressedBody{reader: bufferedBody, closer: originalBody}

		headerBytes, _ := bufferedBody.Peek(zstd.HeaderMaxSize)
		var header zstd.Header
		if err := header.Decode(headerBytes); err != nil {
			slog.Warn("zstd_decompress_failed", "error", err)
			return
		}

		zr, err := zstd.NewReader(bufferedBody)
		if err != nil {
			slog.Warn("zstd_decompress_failed", "error", err)
			return
		}
		reader = &zstdResponseReader{ReadCloser: zr.IOReadCloser()}
	default:
		return
	}

	resp.Body = &decompressedBody{reader: reader, closer: originalBody}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length") // 解压后长度不确定
	resp.ContentLength = -1
}

type zstdResponseReader struct {
	io.ReadCloser
	warnOnce sync.Once
}

func (r *zstdResponseReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.warnOnce.Do(func() {
			slog.Warn("zstd_decompress_failed", "error", err)
		})
	}
	return n, err
}

// decompressedBody 组合解压 reader 和原始 body 的 close。
type decompressedBody struct {
	reader io.Reader
	closer io.Closer
}

func (d *decompressedBody) Read(p []byte) (int, error) {
	return d.reader.Read(p)
}

func (d *decompressedBody) Close() error {
	// 如果 reader 本身也是 Closer（如 gzip.Reader），先关闭它
	if rc, ok := d.reader.(io.Closer); ok {
		_ = rc.Close()
	}
	return d.closer.Close()
}
