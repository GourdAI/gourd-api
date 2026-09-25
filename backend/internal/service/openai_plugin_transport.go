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
func (s *OpenAIGatewayService) resolveOpenAIUpstreamTLSProfile(account *Account) *tlsfingerprint.Profile {
	if s == nil || s.tlsFPProfileService == nil {
		return nil
	}
	return s.tlsFPProfileService.ResolveTLSProfile(account)
}

// doOpenAIUpstream 只在 OpenAI OAuth 能力绑定已启用时把真实请求交给插件。
// 插件返回标准 http.Response，响应解析、错误映射、SSE 和计费仍由现有核心链处理。
//
// 防封加固：当账号启用 TLS 指纹时，插件未接管的前提下走 DoWithTLS
// （OpenAI/Grok/OpenCode 等经本函数的平台共享此路径）。
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
