// account_tls_fingerprint_state.go TLS 指纹「三态回显」。
//
// 背景缺陷（本次修复的根因）：WorkBuddy/Trae/Qoder 三家的 TLS 指纹是**平台级默认开启**
// （config: gateway.<platform>.tls_fingerprint_enabled，默认 true），而管理台账号弹窗里
// 那个「TLS 指纹模拟」开关读写的是**账号级** Extra.enable_tls_fingerprint。两者语义不
// 对齐造成一个真实陷阱：
//
//  1. 回显侧：旧实现只在 IsTLSFingerprintEnabled()（= 显式 true）时下发字段，于是
//     「未表态」与「显式关闭」在 API 响应里长得一模一样，后端**永远不会**把该开关显示
//     成 ON —— 即使出站实际正在伪装。
//  2. 写入侧：前端关闭时执行 delete extra.enable_tls_fingerprint（删键 = 退回未表态 =
//     跟随平台默认 = 开），因此管理员把开关拨到 OFF，出站**仍然带指纹**。
//
// 修法：DTO 同时给出「管理员表态（三态）」与「真实生效值」，让 UI 能显示 ON 并说明来源；
// 前端改为显式写 false（而非删键），使 OFF 真正生效。
package dto

import (
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// TLS 指纹表态枚举（对外契约，前端按此渲染三态文案）。
const (
	// TLSFingerprintStateExplicitOn 管理员在账号上显式开启。
	TLSFingerprintStateExplicitOn = "explicit_on"
	// TLSFingerprintStateExplicitOff 管理员在账号上显式关闭（压过平台默认）。
	TLSFingerprintStateExplicitOff = "explicit_off"
	// TLSFingerprintStateInherit 账号未表态，是否伪装由平台级默认决定。
	TLSFingerprintStateInherit = "inherit"
)

// tlsFingerprintPlatformCfg 保存平台级默认配置，用于计算「真实生效值」。
//
// 用包级注入而不是给每个 mapper 函数加参数：AccountFromService* 有 20+ 调用点，改签名
// 会波及全部 handler；且这些 mapper 是纯投影函数，把配置塞进参数链没有额外收益。
//
// 未注入（nil，含单测）时的语义由 service.platformDefaultTLSFor 定义：三平台仍返回 true
// （平台默认开启不依赖配置存在性）。本包刻意与之保持一致，**不做任何兜底改写** ——
// 回显的唯一职责就是「跟出站实际行为一致」，哪怕那个默认值看起来保守。
var tlsFingerprintPlatformCfg *config.Config

// SetTLSFingerprintPlatformConfig 注入平台级 TLS 指纹默认配置。
//
// 必须在服务启动早期调用一次（见 cmd/server/main.go）。注意：本处传入的 cfg 与 wire 图里的
// *config.Config 是**两个不同实例但内容相同** —— 两者都由 config.LoadForBootstrap() 从同一份
// viper 全局状态构建。配置仅在启动时加载一次（无运行时热更新），因此回显判定与出站实际
// 判定读到的是同一组值。若将来引入配置热更新，必须改为从 wire 注入同一实例。
func SetTLSFingerprintPlatformConfig(cfg *config.Config) {
	tlsFingerprintPlatformCfg = cfg
}

// TLSFingerprintState 计算账号 TLS 指纹的表态、平台默认与真实生效值。
//
// 判定顺序必须与出站链路 resolvePlatformDefaultTLSProfile 完全一致：
//  1. Extra 显式表态 → 以表态为准（显式 false 压过平台默认）；
//  2. 未表态 → 跟随平台级默认（仅 WorkBuddy/Trae/Qoder 为 true）。
//
// 平台默认值通过 service.PlatformDefaultTLSFingerprintEnabled 取，绝不在本包重写
// 平台 switch —— 否则新增默认开启平台时又会只改一处，回显与行为再次分叉。
//
// 为何连 platformDefault 一起返回：UI 需要知道「若取消覆盖会回到什么状态」，才能提供
// 「恢复跟随平台默认」并立即渲染出正确开关。仅靠 state+effective 推不出该值（账号已显式
// 表态时 effective 等于表态、不再反映平台默认）。
func TLSFingerprintState(account *service.Account) (state string, platformDefault bool, effective bool) {
	platformDefault = service.PlatformDefaultTLSFingerprintEnabled(account, tlsFingerprintPlatformCfg)
	if account == nil {
		return TLSFingerprintStateInherit, platformDefault, false
	}
	enabled, explicit := account.TLSFingerprintExtraState()
	if explicit {
		if enabled {
			return TLSFingerprintStateExplicitOn, platformDefault, true
		}
		return TLSFingerprintStateExplicitOff, platformDefault, false
	}
	return TLSFingerprintStateInherit, platformDefault, platformDefault
}

// tlsFingerprintDefaultProbePlatforms 需要向新建表单预告平台默认值的平台。
//
// 只列出「平台级默认可能为 true」的那几家：其余平台默认必为 false（opt-in），
// 预先查询没有意义，且避免把完整平台清单再抄一份到本包。
var tlsFingerprintDefaultProbePlatforms = []string{
	service.PlatformWorkbuddy,
	service.PlatformTrae,
	service.PlatformQoder,
}

// TLSFingerprintPlatformDefaults 返回各平台的 TLS 指纹默认开关。
//
// 存在的理由：新建账号弹窗没有已存在的账号对象，拿不到回显字段，但需要知道
// 「若该平台默认开启，则默认开关应显示为 ON」——否则新建的 WorkBuddy 账号在表单里
// 显示 OFF、创建完却实际在伪装，与本次修的编辑弹窗是同一个缺陷。
//
// 必须经 service.PlatformDefaultTLSFingerprintEnabled 取（传一个只填 Platform 的临时
// 账号），不能在前端写死平台名单。
func TLSFingerprintPlatformDefaults() map[string]bool {
	out := make(map[string]bool, len(tlsFingerprintDefaultProbePlatforms))
	for _, platform := range tlsFingerprintDefaultProbePlatforms {
		out[platform] = service.PlatformDefaultTLSFingerprintEnabled(
			&service.Account{Platform: platform},
			tlsFingerprintPlatformCfg,
		)
	}
	return out
}
