package service

// qoder_headers.go Qoder chat 出站请求头构造：COSY 会话封装 + 签名头 + 设备指纹。
//
// 头族对齐官方客户端 agent_chat_generation 请求（全小写头名，与 Go net/http 的
// 规范化头名等价传输）；签名链：payloadB64 = base64(json({cosyVersion, ideVersion,
// info, requestId, version})) → md5(payloadB64\n cosyKey\n date\n body\n pathSig)。
//
// 会话封装（每次出站新建，与官方客户端每请求新会话一致）：
//   - tempKey（16 字节）→ RSA PKCS1v15 加密 → base64 = cosyKey（cosy-key 头）；
//   - 身份 JSON → AES-128-CBC（key=tempKey，IV=key 前 16 字节）→ base64 = info（进签名 payload）。

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// applyQoderChatHeaders 构造 Qoder chat 请求的全套 COSY 头。
// params:
//   - creds：账号凭据（uid / token 参与指纹派生）；
//   - securityToken：Authorization 用的 securityOauthToken（dt- 设备令牌或交换产物）；
//   - meta：BuildQoderBody 产物元数据（requestId / modelKey）；
//   - body：编码后的请求体（签名输入）；
//   - targetURL：完整 chat URL（pathSig 取其 path 去掉 /algo 前缀）；
//   - acceptValue：恒 text/event-stream（上游 agent_chat_generation 只接受 SSE
//     accept，application/json 会直接 500；非流式由读全流聚合满足）。
//
// 返回错误仅可能是会话加密失败（RSA 公钥常量损坏）。
func applyQoderChatHeaders(req *http.Request, creds QoderCredentials, securityToken string, meta *QoderBodyMeta, body []byte, targetURL, acceptValue string) error {
	if meta == nil {
		return fmt.Errorf("qoder chat headers: nil body meta")
	}
	// 会话封装：临时密钥 → RSA 包裹为 cosyKey；身份 JSON → AES 为 info。
	identity := QoderAuthIdentity{
		Name:               orElseQoder(creds.Nickname, "qoder"),
		AID:                "",
		UID:                qoderFingerprintSeed(creds.UID, securityToken),
		YXUID:              "",
		OrganizationID:     "",
		OrganizationName:   "",
		UserType:           qoderResolveUserType(creds),
		SecurityOauthToken: securityToken,
		RefreshToken:       creds.RefreshToken,
	}
	session, err := QoderNewSession(identity)
	if err != nil {
		return fmt.Errorf("qoder chat headers: build session: %w", err)
	}
	// 签名链。
	date := qoderUnixSeconds()
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return fmt.Errorf("qoder chat headers: parse target url: %w", err)
	}
	pathSig := qoderPathSig(parsed.Path)
	_, authorization := QoderSignRequest(session.Info, meta.RequestID, session.CosyKey, date, string(body), pathSig)

	// 机器指纹：uid（缺省 cred:+token）稳定派生。
	seed := qoderFingerprintSeed(creds.UID, securityToken)
	machineID, machineType, machineToken := QoderDeriveMachineID(seed)

	// COSY 全套头（官方客户端形态）。
	req.Header.Set("cosy-data-policy", "agree")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("cosy-machinetype", machineType)
	req.Header.Set("cosy-clienttype", qoderCosyClientType)
	req.Header.Set("cosy-date", date)
	req.Header.Set("cosy-user", identity.UID)
	req.Header.Set("cosy-key", session.CosyKey)
	req.Header.Set("cache-control", "no-cache")
	req.Header.Set("accept", acceptValue)
	req.Header.Set("authorization", authorization)
	req.Header.Set("cosy-version", qoderCosyVersion)
	req.Header.Set("cosy-machineid", machineID)
	req.Header.Set("cosy-machinetoken", machineToken)
	req.Header.Set("login-version", "v2")
	req.Header.Set("user-agent", "Go-http-client/2.0")
	req.Header.Set("cosy-scene", "assistant")
	req.Header.Set("cosy-business-product", "ide")
	req.Header.Set("cosy-business-type", "agent")
	// 模型路由头。
	req.Header.Set("x-model-key", meta.ModelKey)
	req.Header.Set("x-model-source", "system")
	return nil
}

// orElseQoder 返回第一个非空字符串（缺省值兜底）。
func orElseQoder(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
