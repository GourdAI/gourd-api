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
//   - 无账号上下文的出站（登录/换票阶段账号尚未入库）：本函数拿不到
//     *Account，只能走普通 Do。
//
// 辅助/探针路径的收口进展（防封加固续轮）：Trae/Qoder/WorkBuddy 三家的
// 签到/余额/换票/批量任务出站已改为经本入口（含 Concurrency 变体）；未注入
// profileService 时仍为 nil 降级（零行为变更）。
// 同类形态仍未收口的点（保持现状，需要时按同一模式接）：
//
//	cn_provider_balance_service.go:155 / cn_provider_quota_service.go:201 /
//	grok_quota_service.go:177,395 / grok_observed_models.go:118 /
//	ollama_cloud_usage.go:917 / openai_images_b64_backfill.go:126 /
//	antigravity_gateway_retry.go:52 / openai_codex_models_service.go:1868；
//
// 以及 qoder_client.go:345（jobToken 交换的 RoundTripper 适配层，因接口签名丢失
// account 与账号代理，属单独议题）。
func doUpstreamWithOptionalTLS(
	httpUpstream HTTPUpstream,
	profileService *TLSFingerprintProfileService,
	req *http.Request,
	proxyURL string,
	account *Account,
) (*http.Response, error) {
	concurrency := 0
	if account != nil {
		concurrency = account.Concurrency
	}
	return doUpstreamWithOptionalTLSConcurrency(httpUpstream, profileService, req, proxyURL, account, concurrency)
}

// doUpstreamWithOptionalTLSConcurrency 与 doUpstreamWithOptionalTLS 同语义，
// 但由调用方显式给出连接池并发度。
//
// 为何需要单独一个入参：积分/签到/任务类服务（Trae/Qoder/WorkBuddy）拿不到
// 聊天链那样的账号并发预设，历史上统一传 maxInt(account.Concurrency, 1)；直接复用
// 上面的入口会把并发度从 1 改成 0，从而改变连接池大小（resolvePoolSettings 行为）。
// 防封收口不得顺带改掉连接池语义，故保留调用方原口径。
func doUpstreamWithOptionalTLSConcurrency(
	httpUpstream HTTPUpstream,
	profileService *TLSFingerprintProfileService,
	req *http.Request,
	proxyURL string,
	account *Account,
	accountConcurrency int,
) (*http.Response, error) {
	if httpUpstream == nil || req == nil || account == nil {
		return nil, errNilUpstreamRequest
	}
	if profile := resolveAccountTLSProfile(profileService, account); profile != nil {
		return httpUpstream.DoWithTLS(req, proxyURL, account.ID, accountConcurrency, profile)
	}
	return httpUpstream.Do(req, proxyURL, account.ID, accountConcurrency)
}

// resolveAccountTLSProfile 解析账号的 TLS Profile；未启用/未注入时返回 nil。
func resolveAccountTLSProfile(profileService *TLSFingerprintProfileService, account *Account) *tlsfingerprint.Profile {
	if profileService == nil || account == nil {
		return nil
	}
	return profileService.ResolveTLSProfile(account)
}
