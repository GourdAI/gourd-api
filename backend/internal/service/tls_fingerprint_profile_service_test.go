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

// TestResolveTLSProfile_OtherPlatformsKeepNodeDefault 保证除 OpenAI 与三平台（WorkBuddy/Trae/Qoder）
// 外所有平台零行为变更。
//
// 三平台已从本用例移出：它们拿的是 NodeOpenSSLH2Profile（Node/OpenSSL 形状 + ALPN h2）。
// 移出原因不是「让测试通过」：带指纹的出站只能跑 HTTP/1.1，而账号隔离下连接上限被夹成
// account.Concurrency（线上常见 1），同账号并发会串行排队——实测 12 并发 105ms→779ms。
// 声明 h2 后传输层改用 http2.Transport，实测回到 104ms。见下方
// TestResolveTLSProfile_ThreePlatformsUseHTTP2CapableDefault。
func TestResolveTLSProfile_OtherPlatformsKeepNodeDefault(t *testing.T) {
	svc := NewTLSFingerprintProfileService(&tlsFPStubRepo{}, nil)

	platforms := []string{
		PlatformAnthropic, PlatformGemini, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax,
		PlatformOpenCodeGo,
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
		// 这些平台的 ALPN 必须仍为空（= 回落 http/1.1）：一旦误升 h2，
		// 它们的 JA4 协议段会变，而官方客户端形状本仓并未采集。
		require.Empty(t, profile.ALPNProtocols, "platform=%s must not advertise h2", platform)
	}
}

// TestResolveTLSProfile_ThreePlatformsUseHTTP2CapableDefault 钉住三平台拿「带 h2 的 Node 形状」。
//
// 必须同时断言两件事：形状仍是 Node/OpenSSL（CipherSuites 为空 ⇒ JA3 不变），
// 且 ALPN 声明 h2（⇒ 传输层走 http2.Transport，恢复多路复用）。
// 反向护栏：把 defaultTLSProfileForPlatform 里三平台分支改回 NodeDefaultProfile，
// 本用例必须 FAIL。
func TestResolveTLSProfile_ThreePlatformsUseHTTP2CapableDefault(t *testing.T) {
	svc := NewTLSFingerprintProfileService(&tlsFPStubRepo{}, nil)

	for _, platform := range []string{PlatformWorkbuddy, PlatformTrae, PlatformQoder} {
		for _, resolve := range []struct {
			name string
			fn   func(*Account) *tlsfingerprint.Profile
		}{
			{"ResolveTLSProfile", svc.ResolveTLSProfile},
			{"ResolvePlatformProfile", svc.ResolvePlatformProfile},
		} {
			account := &Account{
				ID:       7,
				Platform: platform,
				Type:     AccountTypeAPIKey,
				Extra:    map[string]any{"enable_tls_fingerprint": true},
			}
			profile := resolve.fn(account)
			require.NotNil(t, profile, "%s platform=%s", resolve.name, platform)
			require.Equal(t, tlsfingerprint.ProfileNameNodeOpenSSLH2, profile.Name,
				"%s platform=%s 必须拿带 h2 的内置默认", resolve.name, platform)
			require.Contains(t, profile.ALPNProtocols, "h2",
				"%s platform=%s 必须声明 h2，否则传输层只能 h1 并与小连接上限叠加成串行排队", resolve.name, platform)
			require.Contains(t, profile.ALPNProtocols, "http/1.1",
				"%s platform=%s 必须保留 http/1.1 声明以匹配真实 Chromium 的 ALPN 列表形状（注意：它不是可用性兜底——实测 http2.Transport 在协商到 h1 时直接硬失败，不回落）", resolve.name, platform)
			// 形状不变：除 ALPN 外必须与 NodeDefaultProfile 字节级一致
			require.Empty(t, profile.CipherSuites, "%s platform=%s", resolve.name, platform)
			require.Empty(t, profile.Extensions, "%s platform=%s", resolve.name, platform)
			require.False(t, profile.EnableGREASE, "%s platform=%s", resolve.name, platform)
		}
	}

	// OpenAI 必须仍是 rustls 形状，不得被三平台分支误抢。
	openai := &Account{ID: 8, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Extra: map[string]any{"enable_tls_fingerprint": true}}
	require.Equal(t, tlsfingerprint.ProfileNameCodexRustls, svc.ResolveTLSProfile(openai).Name)
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

// multiTemplateService 构造一个含三个模板的服务，用于验证「随机档」的选取稳定性。
// 单模板夹具抓不到“每请求重随机”这个缺陷（永远只有一个可能结果），故必须多模板。
func multiTemplateService() *TLSFingerprintProfileService {
	return NewTLSFingerprintProfileService(&tlsFPStubRepo{profiles: []*model.TLSFingerprintProfile{
		{ID: 11, Name: "Template Eleven", CipherSuites: []uint16{0x1301}},
		{ID: 22, Name: "Template TwentyTwo", CipherSuites: []uint16{0x1302}},
		{ID: 33, Name: "Template ThirtyThree", CipherSuites: []uint16{0x1303}},
	}}, nil)
}

func randomProfileAccount(id int64, platform string) *Account {
	return &Account{
		ID:       id,
		Platform: platform,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"enable_tls_fingerprint":     true,
			"tls_fingerprint_profile_id": float64(-1),
		},
	}
}

// TestResolveTLSProfile_RandomIsStablePerAccount 防封加固修正（C2）：「随机模板」
// 不得每次出站重抛。原实现 profiles[rand.IntN(len)] 会让同一账号在不同请求里交出
// 不同 ClientHello，形状高频漂移本身就是风控特征，也与本包“形状自洽”纪律直接矛盾。
// 正确语义：同账号恒定。
func TestResolveTLSProfile_RandomIsStablePerAccount(t *testing.T) {
	svc := multiTemplateService()
	account := randomProfileAccount(4242, PlatformAnthropic)

	first := svc.ResolveTLSProfile(account)
	require.NotNil(t, first)
	require.Contains(t, []string{"Template Eleven", "Template TwentyTwo", "Template ThirtyThree"}, first.Name)

	for i := 0; i < 200; i++ {
		got := svc.ResolveTLSProfile(account)
		require.NotNil(t, got)
		require.Equal(t, first.Name, got.Name,
			"random tier must be stable per account across resolutions (iteration %d)", i)
	}
}

// TestResolveTLSProfile_RandomSpreadsAcrossAccounts 另一面：同账号恒定不能答成
// “所有账号拿同一个模板”，那就退化成了固定档，跨账号形状完全一致反而是号池特征。
// 20 个账号里至少应命中 2 个不同模板（三个模板的确定性散列分布）。
func TestResolveTLSProfile_RandomSpreadsAcrossAccounts(t *testing.T) {
	svc := multiTemplateService()
	names := make(map[string]struct{}, 3)
	for id := int64(1); id <= 20; id++ {
		profile := svc.ResolveTLSProfile(randomProfileAccount(id, PlatformAnthropic))
		require.NotNil(t, profile)
		names[profile.Name] = struct{}{}
	}
	require.GreaterOrEqual(t, len(names), 2, "random tier must vary across accounts, not collapse to one template")
}

// TestResolveTLSProfile_RandomFallsBackWhenTemplateDeleted 取锁间隙模板被删完时，
// 必须按平台回落而不是 panic，且不能吞掉账号的“已启用指纹”意图。
func TestResolveTLSProfile_RandomFallsBackWhenTemplateDeleted(t *testing.T) {
	svc := multiTemplateService()
	account := randomProfileAccount(9, PlatformOpenAI)
	require.NotNil(t, svc.ResolveTLSProfile(account))

	// 模板表清空（等效于所有模板被删）
	svc.setLocalCache(nil)
	profile := svc.ResolveTLSProfile(account)
	require.NotNil(t, profile)
	require.Equal(t, tlsfingerprint.ProfileNameCodexRustls, profile.Name,
		"empty template table with random tier must fall back to the platform default")
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
