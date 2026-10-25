package service

// workbuddy_jwt.go WorkBuddy 会话令牌（JWT）的只读解析：仅在需要补齐过期时刻时使用。
//
// 纪律：**不做签名校验**。这里的 token 来自用户自己粘贴或自家换密接口，用途只是
// 读取 exp 声明以补齐账号凭据的 expires_at，不是鉴权凭证，因此按 base64url 解
// payload 即可，签名段忽略。手法与 trae_jwt.go 完全一致（那里的纪律说明同样适用）。
//
// 【为什么必须有这一层（2026-10-24 全量 401 事件根因之一）】
// 前端 buildWorkbuddyCredentials（credentialsBuilder.ts）组装凭据时**从不写入
// expires_at**（Qoder 的同名函数写，WorkBuddy 漏了）。而 workbuddyTokenNeedsRefresh
// 的判据是「ExpiresAt <= 0 → 不主动刷新」（见 workbuddy_client.go 注释：无过期信息时
// 避免把有效 token 刷掉）。两者叠加的后果是：手工粘贴 token 的账号 expires_at 恒为 0
// ⇒ 预刷新判据恒假 ⇒ **永不预刷新，只能等撞 401**。
//
// 实测证据（2026-10-24，本机真实 CN 账号）：
//   expires_at(raw)=<nil> -> parsed=0
//   needsRefresh(now)=false
//   needsRefresh(+1y)=false
//
// 而 WorkBuddy 的 access_token 是标准 JWT，exp 声明真实存在（实测 exp=2026-11-13、
// refresh_token exp=2026-11-18，仅比 access 多 5 天）。因此「从 token 自身读出 exp」
// 是唯一不需要用户重新手工填写的补救路径：存量账号在第一次请求时即可获得有效过期时刻。

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
)

// workbuddyJWTClaims 只承载本文件需要的声明（exp 是标准 claim；iss/aud 供排障参考）。
type workbuddyJWTClaims struct {
	Exp int64  `json:"exp"`
	Iat int64  `json:"iat"`
	Iss string `json:"iss"`
	Sub string `json:"sub"`
}

// workbuddyParseJWTClaims 解析 JWT payload；段数不足/base64 失败/JSON 非法时返回 false。
func workbuddyParseJWTClaims(token string) (workbuddyJWTClaims, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return workbuddyJWTClaims{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 部分签发实现给出带 padding 的 base64url。
		if raw, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return workbuddyJWTClaims{}, false
		}
	}
	var claims workbuddyJWTClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return workbuddyJWTClaims{}, false
	}
	return claims, true
}

// workbuddyJWTExpiry 取 JWT 的 exp（epoch 秒）；缺失/非法/非正一律返回 0。
//
// 返回 0 的语义必须保持「未知」而不是「已过期」：调用方（workbuddyResolveCredentialsExpiry）
// 只在拿到正数时才回填，否则保留库里的原值，避免把「解析不出」误当成「已过期」而触发
// 一次无意义的上游刷新。
func workbuddyJWTExpiry(token string) int64 {
	claims, ok := workbuddyParseJWTClaims(token)
	if !ok {
		return 0
	}
	exp := claims.Exp
	// 兼容毫秒级 epoch（极少数签发实现给的是 13 位）：超过 1e11 视为毫秒。
	if exp > 1e11 {
		exp /= 1000
	}
	if exp <= 0 {
		return 0
	}
	return exp
}

// workbuddyJWTSubject 取 JWT 的 sub 声明（账号 uid）；缺失返回 ""。
// 与 exp 同源、同一趟解析口径：WorkBuddy 的 uid 就是 access_token 的 sub
// （实测 sub=661ecfa0-1a94-4120-b515-fe0736a21394 与凭据 uid 逐字一致），
// 因此 uid 缺失时也能由 token 自证，不必让用户手工补填。
func workbuddyJWTSubject(token string) string {
	claims, ok := workbuddyParseJWTClaims(token)
	if !ok {
		return ""
	}
	return strings.TrimSpace(claims.Sub)
}

// workbuddyJWTExpiryString 返回 exp 的十进制字符串形式（供需要字符串凭据值的场景）。
func workbuddyJWTExpiryString(token string) string {
	exp := workbuddyJWTExpiry(token)
	if exp <= 0 {
		return ""
	}
	return strconv.FormatInt(exp, 10)
}
