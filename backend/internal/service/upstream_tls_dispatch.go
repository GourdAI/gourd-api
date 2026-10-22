package service

import (
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/config"
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

// platformDefaultTLSFor 报告某账号是否适用「平台级默认开启 TLS 指纹」。
//
// 适用平台：WorkBuddy / Trae / Qoder。这三家的共同点是 HTTP 头层已完整伪装成
// 官方桌面客户端（WorkBuddy 的 X-CodeBuddy-Request、Trae 的 X-Market-Client-Id /
// User-Agent: Trae/<ver>、Qoder 的 cosy-clienttype 一整套），传输层若仍发 Go 标准库
// ClientHello 就是两层身份自相矛盾，是最明确的 JA3/JA4 自动化标记。
//
// **必须只有一个实现**：聊天转发（openai_plugin_transport.go）、账号测试
// （account_test_service.go）、以及三家 credits/活动服务（traeDo/qoderDo/workbuddyDo）
// 若各自维护一份判定，就会出现「真实请求带指纹、测试请求不带」这类身份不一致
// ——那正是本次改造要消除的形态。新增平台只在此处加一行分支。
//
// cfg 为 nil（单测常见）时仍返回 true：平台默认语义不依赖配置存在性。
// 账号 Extra 显式 enable_tls_fingerprint=false 由 resolvePlatformDefaultTLSProfile
// 处理（优先级更高），本函数只回答「该平台的默认值是什么」。
func platformDefaultTLSFor(account *Account, cfg *config.Config) bool {
	if account == nil {
		return false
	}
	switch account.Platform {
	case PlatformWorkbuddy:
		if cfg == nil {
			return true
		}
		return cfg.Gateway.Workbuddy.TLSFingerprintEnabled
	case PlatformTrae:
		if cfg == nil {
			return true
		}
		return cfg.Gateway.Trae.TLSFingerprintEnabled
	case PlatformQoder:
		if cfg == nil {
			return true
		}
		return cfg.Gateway.Qoder.TLSFingerprintEnabled
	default:
		// 其它平台保持零行为变更（仍为 opt-in）。
		return false
	}
}

// resolvePlatformDefaultTLSProfile 在「平台级默认开启」语义下解析 TLS Profile：
//
//   - 账号 Extra 显式设置 enable_tls_fingerprint=false → 返回 nil（尊重管理员意愿）；
//   - 账号 Extra 显式 true → 同 ResolveTLSProfile；
//   - 未表态 → 不询问账号开关，直接按平台解析（ResolvePlatformProfile）。
//
// platformDefault 为 false 时完全退化为 resolveAccountTLSProfile（即历史行为），
// 这是保证其它平台零行为变更的阀。
func resolvePlatformDefaultTLSProfile(
	profileService *TLSFingerprintProfileService,
	account *Account,
	platformDefault bool,
) *tlsfingerprint.Profile {
	if profileService == nil || account == nil {
		return nil
	}
	if !platformDefault {
		return profileService.ResolveTLSProfile(account)
	}
	if enabled, explicit := account.tlsFingerprintExtraState(); explicit {
		if !enabled {
			return nil // 显式关闭优先于平台默认
		}
		return profileService.ResolveTLSProfile(account)
	}
	return profileService.ResolvePlatformProfile(account)
}

// doUpstreamWithPlatformDefaultTLS 在 doUpstreamWithOptionalTLSConcurrency 之上
// 支持「平台级默认开启」：platformDefault=true 时，账号未表态也走 DoWithTLS。
//
// 与 doUpstreamWithOptionalTLSConcurrency 的唯一差别就是 profile 的解析语义；
// 并发度、nil 降级、Do/DoWithTLS 分派的逻辑完全一致（直接复用，不另建一套）。
func doUpstreamWithPlatformDefaultTLS(
	httpUpstream HTTPUpstream,
	profileService *TLSFingerprintProfileService,
	req *http.Request,
	proxyURL string,
	account *Account,
	accountConcurrency int,
	platformDefault bool,
) (*http.Response, error) {
	if httpUpstream == nil || req == nil || account == nil {
		return nil, errNilUpstreamRequest
	}
	if profile := resolvePlatformDefaultTLSProfile(profileService, account, platformDefault); profile != nil {
		return httpUpstream.DoWithTLS(req, proxyURL, account.ID, accountConcurrency, profile)
	}
	return httpUpstream.Do(req, proxyURL, account.ID, accountConcurrency)
}
