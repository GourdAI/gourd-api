package service

import (
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// errNilUpstreamRequest 是出站请求参数缺失时的哨兵错误。
//
// 语义说明：doUpstreamWithOptionalTLS 的三参数 nil 分支（httpUpstream/req/account）
// 在真实调用路径上均不可达（调用方已保证非 nil），仅在误用时触发。该错误会被上层
// 当作上游失败处理（可能触发 failover），因此它代表「编程错误」而非可恢复的运行时
// 错误，不应在业务逻辑中据其做降级重试。
var errNilUpstreamRequest = errors.New("upstream request is nil")

// doUpstreamWithOptionalTLS 是「按账号 TLS 指纹开关自动选择 Do/DoWithTLS」的共享入口。
//
// 防封加固：Gemini/Antigravity/Grok/OpenCode 等平台的主链路统一经本函数出站，
// 当账号显式启用 TLS 指纹（Extra.enable_tls_fingerprint=true）时走 DoWithTLS，
// 否则保持原 httpUpstream.Do 行为（零行为变更）。
//
// profileService 为 nil（未注入）时自动降级为普通 Do。
//
// 已知不经过本入口的路径（评估记录）：
//   - 插件宿主进程内的出站请求：openai_plugin_transport 中 pluginManager 接管时，
//     请求交由插件进程自己发出，不经本函数；
//   - OpenAI WS 直连拨号：coderws.Dial 路径（如 openai_ws_client.go）自行建立
//     连接，不经本函数；
//   - 辅助/探针路径：quota 检查、模型同步、b64 回填、cn_provider 等低频后台请求。
//
// 以上均为有意保留的现状（辅助路径或独立进程），如需全链收口可作为后续加固项。
func doUpstreamWithOptionalTLS(
	httpUpstream HTTPUpstream,
	profileService *TLSFingerprintProfileService,
	req *http.Request,
	proxyURL string,
	account *Account,
) (*http.Response, error) {
	if httpUpstream == nil || req == nil || account == nil {
		return nil, errNilUpstreamRequest
	}
	if profile := resolveAccountTLSProfile(profileService, account); profile != nil {
		return httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, profile)
	}
	return httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
}

// resolveAccountTLSProfile 解析账号的 TLS Profile；未启用/未注入时返回 nil。
func resolveAccountTLSProfile(profileService *TLSFingerprintProfileService, account *Account) *tlsfingerprint.Profile {
	if profileService == nil || account == nil {
		return nil
	}
	return profileService.ResolveTLSProfile(account)
}
