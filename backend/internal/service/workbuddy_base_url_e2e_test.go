//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// workbuddy_base_url_e2e_test.go 端到端链路断言：存量裸域账号出站实际打到 www。
//
// 背景（2026-09 国际版 404 事件）：credentials.base_url 曾可能入库裸域
// https://workbuddy.ai（缺 www），上游边缘对 POST 无条件 301 → Go 客户端跟随
// 重定向把 POST 改写为 GET 并丢弃请求体 → 上游返回 "404 page not found"。
// workbuddy.go 的读取侧归一化（normalizeWorkbuddyStoredBaseURL）保证出站前
// 已改写为 https://www.workbuddy.ai；本文件锁定从「发送管线」到「实际 URL」的链路。

// workbuddyCaptureUpstream 记录出站请求 URL 但返回预置 SSE 响应（不触网）。
type workbuddyCaptureUpstream struct {
	mu   sync.Mutex
	urls []string
}

func (u *workbuddyCaptureUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	u.mu.Lock()
	u.urls = append(u.urls, req.URL.String())
	u.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(workbuddyTestSSEBody)),
		Request:    req,
	}, nil
}

func (u *workbuddyCaptureUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func (u *workbuddyCaptureUpstream) snapshotURLs() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.urls...)
}

func TestSendWorkbuddyUpstreamRequestNormalizesBareDomainEndToEnd(t *testing.T) {
	t.Parallel()
	upstream := &workbuddyCaptureUpstream{}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := &Account{ID: 730, Platform: PlatformWorkbuddy, Credentials: map[string]any{
		"access_token": "at-730",
		"uid":          "u-730",
		"realm":        "global",
		"base_url":     "https://workbuddy.ai",
	}}

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendWorkbuddyUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"ping"}]}`), true)
	require.NoError(t, err)
	defer resp.Body.Close()

	urls := upstream.snapshotURLs()
	require.Len(t, urls, 1)
	require.Equal(t, "https://www.workbuddy.ai"+workbuddyChatPath, urls[0],
		"存量裸域必须归一化为带 www 出站（防 301→POST 改写 GET→404）")
}

func TestSendWorkbuddyUpstreamRequestKeepsWwwDomainVerbatim(t *testing.T) {
	t.Parallel()
	upstream := &workbuddyCaptureUpstream{}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := &Account{ID: 731, Platform: PlatformWorkbuddy, Credentials: map[string]any{
		"access_token": "at-731",
		"uid":          "u-731",
		"realm":        "global",
		"base_url":     "https://www.workbuddy.ai",
	}}

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendWorkbuddyUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"ping"}]}`), true)
	require.NoError(t, err)
	defer resp.Body.Close()

	urls := upstream.snapshotURLs()
	require.Len(t, urls, 1)
	require.Equal(t, "https://www.workbuddy.ai"+workbuddyChatPath, urls[0], "已带 www 的存量恒不改写")
}

func TestSendWorkbuddyUpstreamRequestKeepsCustomRelayVerbatim(t *testing.T) {
	t.Parallel()
	upstream := &workbuddyCaptureUpstream{}
	svc := newWorkbuddyTestService(upstream, &workbuddyTestAccountRepo{})
	account := &Account{ID: 732, Platform: PlatformWorkbuddy, Credentials: map[string]any{
		"access_token": "at-732",
		"uid":          "u-732",
		"realm":        "global",
		"base_url":     "https://relay.example.com",
	}}

	c := newWorkbuddyTestGinContext()
	resp, err := svc.sendWorkbuddyUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"ping"}]}`), true)
	require.NoError(t, err)
	defer resp.Body.Close()

	urls := upstream.snapshotURLs()
	require.Len(t, urls, 1)
	require.Equal(t, "https://relay.example.com"+workbuddyChatPath, urls[0],
		"自定义中转保持原样（归一化仅精确命中裸域）")
}
