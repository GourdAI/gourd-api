package service

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

func (s *OpenAIGatewayService) SetPluginManager(manager *PluginManager) {
	s.pluginManager = manager
}

// SetTLSFingerprintProfileService 注入 TLS 指纹模板服务（可选依赖）。
// 注入后，启用了 TLS 指纹的账号的出站请求会走 DoWithTLS 路径。
func (s *OpenAIGatewayService) SetTLSFingerprintProfileService(svc *TLSFingerprintProfileService) {
	s.tlsFPProfileService = svc
}

// resolveOpenAIUpstreamTLSProfile 解析当前账号应使用的 TLS 指纹 Profile。
// 返回 nil 表示未启用或服务未注入（调用方应回退到普通 Do 路径）。
//
// 平台默认开启：WorkBuddy / Trae / Qoder 在 HTTP 层已完整伪装成桌面客户端，传输层若仍发
// Go 标准库 ClientHello 就是两层身份矛盾（JA3/JA4 直接可见）。因此这三家默认开启，
// 受 gateway.<platform>.tls_fingerprint_enabled 控制；账号 Extra 显式
// enable_tls_fingerprint=false 仍然优先。其它平台语义完全不变（仍为 opt-in）。
//
// 路径说明：Trae 聊天（trae_client.go sendTraeUpstreamRequest）与 Qoder 聊天
// （qoder_client.go sendQoderUpstreamRequest）都经本函数出站，因此在这里接平台默认
// 就同时覆盖两家聊天主链路，不会出现「辅助面带指纹、主链路不带」的缺口。
func (s *OpenAIGatewayService) resolveOpenAIUpstreamTLSProfile(account *Account) *tlsfingerprint.Profile {
	if s == nil || s.tlsFPProfileService == nil {
		return nil
	}
	return resolvePlatformDefaultTLSProfile(s.tlsFPProfileService, account, platformDefaultTLSFor(account, s.cfg))
}

// workbuddyPlatformDefaultTLS 已由 platformDefaultTLSFor 统一承担（三平台同源）。
//
// 保留本方法只为兼容既有测试引用；新代码一律直接用 platformDefaultTLSFor，
// 避免再次出现「某路径自己维护一份平台判定」的身份不一致。
//
// Deprecated: 迁移到 platformDefaultTLSFor(account, cfg)。
func (s *OpenAIGatewayService) workbuddyPlatformDefaultTLS(account *Account) bool {
	if s == nil {
		return false
	}
	return platformDefaultTLSFor(account, s.cfg)
}

// doOpenAIUpstream 只在 OpenAI OAuth 能力绑定已启用时把真实请求交给插件。
// 插件返回标准 http.Response，响应解析、错误映射、SSE 和计费仍由现有核心链处理。
//
// 防封加固：当账号启用 TLS 指纹（或所属平台默认开启，如 WorkBuddy/Trae/Qoder）
// 时，插件未接管的前提下走 DoWithTLS（OpenAI/Grok/OpenCode 等经本函数的平台
// 共享此路径）。
func (s *OpenAIGatewayService) doOpenAIUpstream(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if s == nil || account == nil {
		return nil, errNilUpstreamRequest
	}
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	if profile := s.resolveOpenAIUpstreamTLSProfile(account); profile != nil {
		return s.httpUpstream.DoWithTLS(request, proxyURL, account.ID, account.Concurrency, profile)
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}

// doOpenAIAccountTestUpstream 让 OpenAI OAuth 账号测试与真实转发使用同一插件路径。
// API Key 和未命中插件的账号保持各自原有的 HTTPUpstream 行为。
func (s *AccountTestService) doOpenAIAccountTestUpstream(
	request *http.Request,
	proxyURL string,
	account *Account,
	useTLSFallback bool,
) (*http.Response, error) {
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	if useTLSFallback {
		return s.httpUpstream.DoWithTLS(
			request,
			proxyURL,
			account.ID,
			account.Concurrency,
			s.resolveTLSProfileForAccount(account),
		)
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}
