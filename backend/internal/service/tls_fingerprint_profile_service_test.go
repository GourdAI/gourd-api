//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// tlsFPStubRepo 返回固定模板列表的仓储桩（cache 传 nil，服务会直接落 DB 填充 localCache）。
type tlsFPStubRepo struct {
	profiles []*model.TLSFingerprintProfile
}

func (r *tlsFPStubRepo) List(ctx context.Context) ([]*model.TLSFingerprintProfile, error) {
	return r.profiles, nil
}

func (r *tlsFPStubRepo) GetByID(ctx context.Context, id int64) (*model.TLSFingerprintProfile, error) {
	for _, p := range r.profiles {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, nil
}

func (r *tlsFPStubRepo) Create(ctx context.Context, p *model.TLSFingerprintProfile) (*model.TLSFingerprintProfile, error) {
	return p, nil
}

func (r *tlsFPStubRepo) Update(ctx context.Context, p *model.TLSFingerprintProfile) (*model.TLSFingerprintProfile, error) {
	return p, nil
}

func (r *tlsFPStubRepo) Delete(ctx context.Context, id int64) error { return nil }

// nodeCipherTemplate 是一个「看起来像 Claude 时代默认值」的手工模板，用于验证显式绑定优先。
func nodeCipherTemplate() *model.TLSFingerprintProfile {
	return &model.TLSFingerprintProfile{
		ID:           77,
		Name:         "Manual Node Template",
		CipherSuites: []uint16{0x1301, 0x1302, 0x1303},
	}
}

// TestResolveTLSProfile_OpenAIUsesCodexRustlsDefault 核心回归：OpenAI 账号启用指纹但未绑定
// 模板时，必须拿到 rustls 形状（历史上这里是 Claude Code/Node.js 形状）。
func TestResolveTLSProfile_OpenAIUsesCodexRustlsDefault(t *testing.T) {
	svc := NewTLSFingerprintProfileService(&tlsFPStubRepo{}, nil)
	account := &Account{
		ID:       1,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"enable_tls_fingerprint": true},
	}

	profile := svc.ResolveTLSProfile(account)
	require.NotNil(t, profile)
	require.Equal(t, tlsfingerprint.ProfileNameCodexRustls, profile.Name,
		"openai must resolve to the codex/rustls default, not the claude-code node default")
	require.NotEmpty(t, profile.CipherSuites, "codex profile must carry explicit rustls suites")
	require.Equal(t, uint16(0x1302), profile.CipherSuites[0], "rustls prefers AES-256 first")
	require.Equal(t, []string{"http/1.1"}, profile.ALPNProtocols)
	require.False(t, profile.EnableGREASE, "rustls has no GREASE")
}

// TestResolveTLSProfile_OtherPlatformsKeepNodeDefault 保证除 OpenAI 外所有平台零行为变更。
func TestResolveTLSProfile_OtherPlatformsKeepNodeDefault(t *testing.T) {
	svc := NewTLSFingerprintProfileService(&tlsFPStubRepo{}, nil)

	platforms := []string{
		PlatformAnthropic, PlatformGemini, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax,
		PlatformOpenCodeGo, PlatformWorkbuddy, PlatformQoder, PlatformTrae,
		PlatformComposite, "kiro", "",
	}
	for _, platform := range platforms {
		account := &Account{
			ID:       2,
			Platform: platform,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{"enable_tls_fingerprint": true},
		}
		profile := svc.ResolveTLSProfile(account)
		require.NotNil(t, profile, "platform=%s", platform)
		require.Equal(t, tlsfingerprint.ProfileNameNodeDefault, profile.Name,
			"platform=%s must keep the historic node default", platform)
		require.Empty(t, profile.CipherSuites,
			"platform=%s must leave fields empty so dialer applies the built-in Node.js defaults", platform)
	}
}

// TestResolveTLSProfile_ExplicitBindingBeatsPlatformDefault 显式绑定模板优先于平台回落，
// 否则本次改动会变成「强制规定 OpenAI 只能用 rustls」，剥夺管理员的选择权。
func TestResolveTLSProfile_ExplicitBindingBeatsPlatformDefault(t *testing.T) {
	svc := NewTLSFingerprintProfileService(&tlsFPStubRepo{profiles: []*model.TLSFingerprintProfile{nodeCipherTemplate()}}, nil)

	account := &Account{
		ID:       3,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"enable_tls_fingerprint":     true,
			"tls_fingerprint_profile_id": float64(77),
		},
	}

	profile := svc.ResolveTLSProfile(account)
	require.NotNil(t, profile)
	require.Equal(t, "Manual Node Template", profile.Name,
		"an explicitly bound template must win over the platform default")
	require.Equal(t, []uint16{0x1301, 0x1302, 0x1303}, profile.CipherSuites)
}

// TestResolveTLSProfile_RandomStillPicksTemplate 随机（-1）在有模板时仍从模板池选，
// 不受平台回落影响；无模板时才落到平台默认。
func TestResolveTLSProfile_RandomStillPicksTemplate(t *testing.T) {
	withTemplates := NewTLSFingerprintProfileService(&tlsFPStubRepo{profiles: []*model.TLSFingerprintProfile{nodeCipherTemplate()}}, nil)
	account := &Account{
		ID:       4,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"enable_tls_fingerprint":     true,
			"tls_fingerprint_profile_id": float64(-1),
		},
	}
	profile := withTemplates.ResolveTLSProfile(account)
	require.NotNil(t, profile)
	require.Equal(t, "Manual Node Template", profile.Name, "random must still draw from the template pool")

	// 模板表为空时（初始部署状态），-1 历史上会穿透成 Node 默认；现在必须按平台回落，
	// 否则「选随机 = 拿到 Claude Code 指纹」这个原始 bug 在 OpenAI 账号上依然存在。
	empty := NewTLSFingerprintProfileService(&tlsFPStubRepo{}, nil)
	profile = empty.ResolveTLSProfile(account)
	require.NotNil(t, profile)
	require.Equal(t, tlsfingerprint.ProfileNameCodexRustls, profile.Name,
		"random with an empty template table must fall back to the platform default")
}

// TestResolveTLSProfile_MissingTemplateIDFallsBackToPlatform 绑定了不存在的模板 ID 时，
// 按平台回落（历史上会静默变成 Node 默认）。
func TestResolveTLSProfile_MissingTemplateIDFallsBackToPlatform(t *testing.T) {
	svc := NewTLSFingerprintProfileService(&tlsFPStubRepo{}, nil)
	account := &Account{
		ID:       5,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"enable_tls_fingerprint":     true,
			"tls_fingerprint_profile_id": float64(9999),
		},
	}

	profile := svc.ResolveTLSProfile(account)
	require.NotNil(t, profile)
	require.Equal(t, tlsfingerprint.ProfileNameCodexRustls, profile.Name)
}

// TestResolveTLSProfile_DisabledReturnsNil 未启用时仍不伪装（行为不变）。
func TestResolveTLSProfile_DisabledReturnsNil(t *testing.T) {
	svc := NewTLSFingerprintProfileService(&tlsFPStubRepo{}, nil)
	require.Nil(t, svc.ResolveTLSProfile(&Account{ID: 6, Platform: PlatformOpenAI}))
	require.Nil(t, svc.ResolveTLSProfile(nil))
}
