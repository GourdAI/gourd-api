//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// tlsDispatchUpstreamStub 记录 Do/DoWithTLS 的分派情况。
type tlsDispatchUpstreamStub struct {
	doCalls      int
	doTLSCalls   int
	lastProfile  *tlsfingerprint.Profile
	lastProxyURL string
	lastAccount  int64
}

func (u *tlsDispatchUpstreamStub) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	u.doCalls++
	u.lastProxyURL = proxyURL
	u.lastAccount = accountID
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

func (u *tlsDispatchUpstreamStub) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.doTLSCalls++
	u.lastProfile = profile
	u.lastProxyURL = proxyURL
	u.lastAccount = accountID
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

func tlsDispatchRequest() *http.Request {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.com/v1", nil)
	return req
}

// tlsDispatchProfileRepo 是 TLS 模板仓储的空实现（无模板 → 走内置默认 profile）。
type tlsDispatchProfileRepo struct{}

func (r *tlsDispatchProfileRepo) List(ctx context.Context) ([]*model.TLSFingerprintProfile, error) {
	return nil, nil
}

func (r *tlsDispatchProfileRepo) GetByID(ctx context.Context, id int64) (*model.TLSFingerprintProfile, error) {
	return nil, nil
}

func (r *tlsDispatchProfileRepo) Create(ctx context.Context, profile *model.TLSFingerprintProfile) (*model.TLSFingerprintProfile, error) {
	return profile, nil
}

func (r *tlsDispatchProfileRepo) Update(ctx context.Context, profile *model.TLSFingerprintProfile) (*model.TLSFingerprintProfile, error) {
	return profile, nil
}

func (r *tlsDispatchProfileRepo) Delete(ctx context.Context, id int64) error {
	return nil
}

func newTLSDispatchProfileService() *TLSFingerprintProfileService {
	return NewTLSFingerprintProfileService(&tlsDispatchProfileRepo{}, nil)
}

// TestDoUpstreamWithOptionalTLS_NoProfileService TLS 服务未注入时走普通 Do。
func TestDoUpstreamWithOptionalTLS_NoProfileService(t *testing.T) {
	upstream := &tlsDispatchUpstreamStub{}
	account := &Account{ID: 1, Platform: PlatformGemini, Type: AccountTypeOAuth}

	_, err := doUpstreamWithOptionalTLS(upstream, nil, tlsDispatchRequest(), "", account)
	require.NoError(t, err)
	require.Equal(t, 1, upstream.doCalls)
	require.Equal(t, 0, upstream.doTLSCalls)
}

// TestDoUpstreamWithOptionalTLS_Disabled TLS 未启用时走普通 Do。
func TestDoUpstreamWithOptionalTLS_Disabled(t *testing.T) {
	upstream := &tlsDispatchUpstreamStub{}
	profileService := newTLSDispatchProfileService()
	account := &Account{ID: 1, Platform: PlatformGemini, Type: AccountTypeOAuth}

	_, err := doUpstreamWithOptionalTLS(upstream, profileService, tlsDispatchRequest(), "", account)
	require.NoError(t, err)
	require.Equal(t, 1, upstream.doCalls, "account without enable_tls_fingerprint must use plain Do")
	require.Equal(t, 0, upstream.doTLSCalls)
}

// TestDoUpstreamWithOptionalTLS_Enabled TLS 启用时走 DoWithTLS。
func TestDoUpstreamWithOptionalTLS_Enabled(t *testing.T) {
	upstream := &tlsDispatchUpstreamStub{}
	profileService := newTLSDispatchProfileService()
	account := &Account{
		ID:       42,
		Platform: PlatformGemini,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"enable_tls_fingerprint": true},
	}

	_, err := doUpstreamWithOptionalTLS(upstream, profileService, tlsDispatchRequest(), "http://proxy", account)
	require.NoError(t, err)
	require.Equal(t, 0, upstream.doCalls)
	require.Equal(t, 1, upstream.doTLSCalls, "enabled TLS fingerprint must use DoWithTLS")
	require.NotNil(t, upstream.lastProfile)
	require.Equal(t, "http://proxy", upstream.lastProxyURL)
	require.Equal(t, int64(42), upstream.lastAccount)
}

// TestDoUpstreamWithOptionalTLS_NonAnthropicPlatformEligible
// 防封加固后非 Anthropic 平台同样可启用（此前被 IsAnthropicOAuthOrSetupToken 拒绝）。
func TestDoUpstreamWithOptionalTLS_NonAnthropicPlatformEligible(t *testing.T) {
	upstream := &tlsDispatchUpstreamStub{}
	profileService := newTLSDispatchProfileService()

	platforms := []string{PlatformGemini, PlatformOpenAI, PlatformGrok, PlatformAntigravity, PlatformWorkbuddy, PlatformOpenCodeGo, PlatformQoder}
	for _, platform := range platforms {
		account := &Account{
			ID:       7,
			Platform: platform,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{"enable_tls_fingerprint": true},
		}
		_, err := doUpstreamWithOptionalTLS(upstream, profileService, tlsDispatchRequest(), "", account)
		require.NoError(t, err, "platform=%s", platform)
	}
	require.Equal(t, 0, upstream.doCalls)
	require.Equal(t, len(platforms), upstream.doTLSCalls, "all subscription platforms must be eligible after the hardening")
}

// TestDoUpstreamWithOptionalTLS_NilInputs 参数缺失时返回错误而非 panic。
func TestDoUpstreamWithOptionalTLS_NilInputs(t *testing.T) {
	_, err := doUpstreamWithOptionalTLS(nil, nil, tlsDispatchRequest(), "", &Account{ID: 1})
	require.ErrorIs(t, err, errNilUpstreamRequest)

	_, err = doUpstreamWithOptionalTLS(&tlsDispatchUpstreamStub{}, nil, nil, "", &Account{ID: 1})
	require.ErrorIs(t, err, errNilUpstreamRequest)

	_, err = doUpstreamWithOptionalTLS(&tlsDispatchUpstreamStub{}, nil, tlsDispatchRequest(), "", nil)
	require.ErrorIs(t, err, errNilUpstreamRequest)
}

// TestResolveAccountTLSProfile_NilSafety 解析器对 nil 输入安全。
func TestResolveAccountTLSProfile_NilSafety(t *testing.T) {
	require.Nil(t, resolveAccountTLSProfile(nil, &Account{ID: 1}))
	profileService := newTLSDispatchProfileService()
	require.Nil(t, resolveAccountTLSProfile(profileService, nil))
}

// TestAccountIsTLSFingerprintEnabled_AllPlatforms 账号级开关对所有平台生效（防封加固）。
func TestAccountIsTLSFingerprintEnabled_AllPlatforms(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformGemini, PlatformOpenAI, PlatformGrok, PlatformAntigravity} {
		enabled := &Account{Platform: platform, Type: AccountTypeOAuth, Extra: map[string]any{"enable_tls_fingerprint": true}}
		require.True(t, enabled.IsTLSFingerprintEnabled(), "platform=%s", platform)

		disabled := &Account{Platform: platform, Type: AccountTypeOAuth}
		require.False(t, disabled.IsTLSFingerprintEnabled(), "platform=%s", platform)

		explicitOff := &Account{Platform: platform, Type: AccountTypeOAuth, Extra: map[string]any{"enable_tls_fingerprint": false}}
		require.False(t, explicitOff.IsTLSFingerprintEnabled(), "platform=%s", platform)
	}
}

// TestDoUpstreamWithOptionalTLS_ExplicitFalse 显式关闭（Extra=false）时走普通 Do。
func TestDoUpstreamWithOptionalTLS_ExplicitFalse(t *testing.T) {
	upstream := &tlsDispatchUpstreamStub{}
	profileService := newTLSDispatchProfileService()
	account := &Account{
		ID:       1,
		Platform: PlatformGemini,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"enable_tls_fingerprint": false},
	}

	_, err := doUpstreamWithOptionalTLS(upstream, profileService, tlsDispatchRequest(), "", account)
	require.NoError(t, err)
	require.Equal(t, 1, upstream.doCalls, "explicit enable_tls_fingerprint=false must use plain Do")
	require.Equal(t, 0, upstream.doTLSCalls)
}

// TestDoUpstreamWithOptionalTLS_NonBoolValue Extra 为非 bool 值（"true" 字符串）时走普通 Do。
// IsTLSFingerprintEnabled 只认 bool 类型，字符串不会被当作启用。
func TestDoUpstreamWithOptionalTLS_NonBoolValue(t *testing.T) {
	upstream := &tlsDispatchUpstreamStub{}
	profileService := newTLSDispatchProfileService()
	account := &Account{
		ID:       1,
		Platform: PlatformGemini,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"enable_tls_fingerprint": "true"},
	}

	_, err := doUpstreamWithOptionalTLS(upstream, profileService, tlsDispatchRequest(), "", account)
	require.NoError(t, err)
	require.Equal(t, 1, upstream.doCalls, "non-bool enable_tls_fingerprint must use plain Do")
	require.Equal(t, 0, upstream.doTLSCalls)
}
