package service

// qoder_device_token.go Qoder 设备授权流的令牌续期（drt- → 新 dt-/drt- pair）。
//
// 【为什么必须单独存在（2026-11 线上签到 TOKEN_EXPIRE 的根因）】
// Qoder 有**两套互不兼容**的凭据家族，续期端点不同，必须按 refresh_token 前缀路由：
//
//   - 设备授权登录家族：access=dt-（设备令牌）、refresh=drt-（设备刷新令牌）。
//     续期走 openapi 域 POST /api/v1/deviceToken/refresh（明文 JSON，无 COSY 签名），
//     请求体 {"refresh_token":"drt-..."}，响应携新 device_token + **轮换后的** refresh_token。
//   - PAT 家族：pt- 经 jobToken 端点（center 域 /algo/api/v3/user/jobToken，COSY Encode
//     + Legacy 签名）换出 securityOauthToken，见 qoder_cosy.go:QoderExchangeJobToken。
//
// 本仓库此前的 401 自愈（ensureQoderJobToken）是「PAT 优先、回落把 refresh_token 塞进
// jobToken 端点」，于是设备流账号（线上 4 个账号全部是 dt-/drt-、无 pt-）撞 401 后
// 把 drt- 送进 PAT 端点，上游恒定回 HTTP 401 "personal token is invalid"，签到/积分/
// 领取从此永久 TOKEN_EXPIRE —— 11-07 起 3 个账号连续签到失败即此形态。
//
// 端点与响应形态经 2026-11-07 对 CN 域 openapi.qoder.com.cn 实测取证：
//
//	POST /api/v1/deviceToken/refresh  {"refresh_token":"drt-..."}
//	200 {"device_token":"dt-...","refresh_token":"drt-...","token_type":"Bearer",
//	     "expires_at":"2026-12-07T17:35:18Z","refresh_token_expires_at":"2027-11-02T17:35:18Z"}
//
// 注意两点（写回凭据时必须遵守，否则下次续期必败）：
//  1. 刷新路径用 **device_token** 键回传访问令牌（poll 路径用 token），两者都要兼容；
//  2. refresh_token 每次都会**轮换**，必须与新 access_token 成对写回。

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

const (
	// qoderDeviceRefreshTokenPrefix 设备流刷新令牌前缀（凭据家族判据，不是装饰）。
	qoderDeviceRefreshTokenPrefix = "drt-"

	// qoderDeviceTokenRefreshTimeout 单次设备刷新调用的总时长上限。
	qoderDeviceTokenRefreshTimeout = 20 * time.Second
)

// qoderDeviceTokenResponse 是 /api/v1/deviceToken/refresh 的响应。
// 访问令牌在刷新路径回传为 device_token，poll 路径为 token，故两键都收。
type qoderDeviceTokenResponse struct {
	Token                 string `json:"token"`
	DeviceToken           string `json:"device_token"`
	RefreshToken          string `json:"refresh_token"`
	TokenType             string `json:"token_type"`
	ExpiresAt             string `json:"expires_at"`
	RefreshTokenExpiresAt string `json:"refresh_token_expires_at"`
}

// AccessToken 返回新访问令牌（兼容 device_token / token 两种键）。
func (r *qoderDeviceTokenResponse) AccessToken() string {
	if r == nil {
		return ""
	}
	if t := strings.TrimSpace(r.Token); t != "" {
		return t
	}
	return strings.TrimSpace(r.DeviceToken)
}

// ExpiresAtUnix 把 RFC3339 的 expires_at 折算为 unix 秒；缺失或解析失败返回 0。
// dt- 是不透明令牌（非 JWT，解不出 exp），这个字段只用于观测与将来的主动刷新判据，
// 不参与本次 401 触发式自愈，故解析失败不视为错误。
func (r *qoderDeviceTokenResponse) ExpiresAtUnix() int64 {
	if r == nil {
		return 0
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(r.ExpiresAt))
	if err != nil {
		return 0
	}
	return t.Unix()
}

// QoderRefreshDeviceToken 用设备流 drt- 刷新令牌换取新的 dt-/drt- pair。
//
// 与 QoderExchangeJobToken 对称：transport 由调用方注入（复用网关 HTTPUpstream），
// endpoints.DeviceTokenRefreshURL 由 realm 解析（openapi 域，不受 base_url 中转覆盖）。
// 纯明文 JSON，无 COSY 签名；请求头 UA 与 Qoder 其它域保持同一约定值。
//
// transport 为 nil 时回落到 http.DefaultTransport（与包内其它降级路径同口径）。
func QoderRefreshDeviceToken(ctx context.Context, transport http.RoundTripper, endpoints qoderEndpoints, refreshToken string) (*qoderDeviceTokenResponse, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, fmt.Errorf("qoder device token refresh: empty refresh token")
	}
	if strings.TrimSpace(endpoints.DeviceTokenRefreshURL) == "" {
		return nil, fmt.Errorf("qoder device token refresh: empty endpoint")
	}
	if transport == nil {
		transport = http.DefaultTransport
	}

	body, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if err != nil {
		return nil, fmt.Errorf("qoder device token refresh: marshal body: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, qoderDeviceTokenRefreshTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoints.DeviceTokenRefreshURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("qoder device token refresh: build request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", qoderUpstreamUserAgent)

	resp, err := transport.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("qoder device token refresh: transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, workbuddyUpstreamErrorBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("qoder device token refresh: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("qoder device token refresh: HTTP %d: %s",
			resp.StatusCode, workbuddyTruncateForError(string(raw)))
	}
	var out qoderDeviceTokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("qoder device token refresh: parse response: %w", err)
	}
	// 必须成对拿到新令牌：只写 access_token 而不写轮换后的 refresh_token，
	// 会让账号在下一次 401 时用已经作废的旧 drt- 刷新，永久救不回来。
	if out.AccessToken() == "" || strings.TrimSpace(out.RefreshToken) == "" {
		return nil, fmt.Errorf("qoder device token refresh: incomplete token pair in response: %s",
			workbuddyTruncateForError(string(raw)))
	}
	return &out, nil
}
