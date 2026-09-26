package service

// trae_jwt.go Trae 会话令牌（JWT）的只读解析：仅在需要补齐 uid / 过期时刻时使用。
//
// 纪律：**不做签名校验**。这里的 token 来自用户自己粘贴或自家换票接口，用途只是
// 读取 exp 与 data.id 两个声明以补齐账号凭据（避免让用户手填 uid），不是鉴权凭证，
// 因此按 base64url 解 payload 即可，签名段忽略。

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
)

// traeJWTClaims 解析 Trae JWT 的 payload 段（非标准 claim 名，故用 map 承载）。
type traeJWTClaims struct {
	Data map[string]any `json:"data"`
	Exp  int64          `json:"exp"`
	Iat  int64          `json:"iat"`
}

// traeParseJWTClaims 解析 JWT payload；段数不足/base64 失败/JSON 非法时返回 false。
func traeParseJWTClaims(token string) (traeJWTClaims, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return traeJWTClaims{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 部分实现给出带 padding 的 base64url。
		if raw, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return traeJWTClaims{}, false
		}
	}
	var claims traeJWTClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return traeJWTClaims{}, false
	}
	return claims, true
}

// traeJWTClaimString 取 JWT payload 里的字符串声明（支持一层嵌套，如 data.id）。
func traeJWTClaimString(token string, keys ...string) string {
	claims, ok := traeParseJWTClaims(token)
	if !ok {
		return ""
	}
	switch len(keys) {
	case 1:
		if v, ok := claims.Data[keys[0]]; ok {
			return traeClaimToString(v)
		}
	case 2:
		if nested, ok := claims.Data[keys[0]].(map[string]any); ok {
			if v, ok := nested[keys[1]]; ok {
				return traeClaimToString(v)
			}
		}
	}
	return ""
}

// traeJWTExpiry 取 JWT 的 exp（epoch 秒），缺失/非法返回 0。
func traeJWTExpiry(token string) int64 {
	claims, ok := traeParseJWTClaims(token)
	if !ok {
		return 0
	}
	return traeNormalizeEpochSeconds(claims.Exp)
}

// traeClaimToString 把 JSON 声明值转为字符串（数字声明在 Go 里是 float64，需去尾零）。
func traeClaimToString(v any) string {
	switch val := v.(type) {
	case string:
		return strings.TrimSpace(val)
	case float64:
		// 数字型 uid 去掉科学计数与尾零，保持与上游 x-uid 实值一致。
		return strings.TrimSuffix(strconv.FormatFloat(val, 'f', -1, 64), ".0")
	case json.Number:
		return val.String()
	default:
		return ""
	}
}
