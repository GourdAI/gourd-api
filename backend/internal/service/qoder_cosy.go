package service

// qoder_cosy.go Qoder COSY 签名层：base64 变体编码（Encode/Decode）、Legacy 签名、
// chat 请求签名、设备指纹派生、会话封装（RSA+AES）与 jobToken 交换。
//
// 协议要点（逆向自 Qoder 官方客户端）：
//   - Encode：标准 base64 密文做「三段重排」（尾段前移），再按自定义字母表逐字符
//     映射（标准 base64 字母表第 i 位 → customAlphabet 第 i 位，'=' → '$'）；
//   - Legacy 签名（jobToken 交换）：md5(AppCode + "&" + SecretB64 + "&" + date)，
//     date 为 HTTP GMT 日期；
//   - 会话：随机 tempKey 经内嵌 RSA 公钥（PKCS1v15）加密为 cosyKey；身份 JSON 以
//     AES-128-CBC（key=tempKey，IV=key 前 16 字节，PKCS7）加密为 info；
//   - chat 签名：md5(payloadB64 + "\n" + cosyKey + "\n" + date + "\n" + body + "\n" + pathSig)，
//     pathSig 为 URL path 去掉前缀 "/algo"，Authorization = "Bearer COSY.<payloadB64>.<sig>"。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// COSY 协议常量。
const (
	// qoderCosyAppCode / qoderCosySecretB64 Legacy 签名三要素之一
	// （SecretB64 = base64("war, war never changes")）。
	qoderCosyAppCode   = "cosy"
	qoderCosySecretB64 = "d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw=="

	// qoderCosyVersion COSY 协议版本（chat 签名 payload 与请求头共用）。
	qoderCosyVersion = "1.0.10"

	// qoderCosyClientType 官方客户端类型号。
	qoderCosyClientType = "5"
)

// qoderCosyStdAlphabet / qoderCosyCustomAlphabet Encode 的字母表映射对：
// 标准字母表第 i 位映射到自定义字母表第 i 位（长度必须一致）。
const (
	qoderCosyStdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	qoderCosyCustomAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"

	// qoderCosyStdPad / qoderCosyCustomPad base64 padding 字符的映射对。
	qoderCosyStdPad    = '='
	qoderCosyCustomPad = '$'
)

// qoderRSA.PublicKeyPEM 是内嵌的会话加密 RSA 公钥（官方客户端固定值）。
const qoderRSAPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----
`

// qoderCosyReorder 三段重排：设 n=len(s)，a=n/3，返回 s[n-a:] + s[a:n-a] + s[:a]
// （尾段前移、中段居中、首段殿后）。Encode 与 Decode 的核心变换。
func qoderCosyReorder(s string) (string, error) {
	n := len(s)
	if n == 0 {
		return "", nil
	}
	a := n / 3
	if a == 0 {
		// n < 3：三段退化为整段，重排为恒等（std[n:] + std[0:n] + std[:0] = s）。
		return s, nil
	}
	var b strings.Builder
	b.Grow(n)
	b.WriteString(s[n-a:])
	b.WriteString(s[a : n-a])
	b.WriteString(s[:a])
	return b.String(), nil
}

// qoderCosyUnreorder 三段重排的逆变换：由 r = s[n-a:]+s[a:n-a]+s[:a] 还原 s。
func qoderCosyUnreorder(r string) (string, error) {
	n := len(r)
	if n == 0 {
		return "", nil
	}
	a := n / 3
	if a == 0 {
		return r, nil
	}
	tail := r[:a]     // 即 s[n-a:]
	mid := r[a : n-a] // 即 s[a:n-a]
	head := r[n-a:]   // 即 s[:a]
	return head + mid + tail, nil
}

// qoderCosyMapLetters 按字母表映射逐字符变换（from→to）。
func qoderCosyMapLetters(s string, from, to string, fromPad, toPad byte) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == fromPad {
			b.WriteByte(toPad)
			continue
		}
		idx := strings.IndexByte(from, c)
		if idx < 0 {
			// 不在字母表内的字符原样保留（防御：正常编码输出不会出现）。
			b.WriteByte(c)
			continue
		}
		b.WriteByte(to[idx])
	}
	return b.String()
}

// QoderCosyEncode 把任意字节串编码为 COSY 请求体形态：
// 标准 base64 → 三段重排 → 自定义字母表映射（'=' → '$'）。
func QoderCosyEncode(data []byte) string {
	std := base64.StdEncoding.EncodeToString(data)
	reordered, _ := qoderCosyReorder(std)
	return qoderCosyMapLetters(reordered, qoderCosyStdAlphabet, qoderCosyCustomAlphabet, qoderCosyStdPad, qoderCosyCustomPad)
}

// QoderCosyDecode 是 QoderCosyEncode 的严格互逆：自定义字母表 → 标准字母表
// （'$' → '='）→ 逆三段重排 → 标准 base64 解码。编码损坏（字母表外字符、
// 长度/填充非法）返回错误，绝不静默产出脏数据。
func QoderCosyDecode(encoded string) ([]byte, error) {
	std := qoderCosyMapLetters(encoded, qoderCosyCustomAlphabet, qoderCosyStdAlphabet, qoderCosyCustomPad, qoderCosyStdPad)
	unreordered, err := qoderCosyUnreorder(std)
	if err != nil {
		return nil, fmt.Errorf("cosy decode reorder: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(unreordered)
	if err != nil {
		return nil, fmt.Errorf("cosy decode base64: %w", err)
	}
	return raw, nil
}

// qoderLegacySignature 计算 jobToken 交换的 Legacy 签名：
// md5(AppCode + "&" + SecretB64 + "&" + date)，date 为 UTC "Mon, 02 Jan 2006 15:04:05 GMT"。
func qoderLegacySignature(date string) string {
	sum := md5.Sum([]byte(qoderCosyAppCode + "&" + qoderCosySecretB64 + "&" + date))
	return hex.EncodeToString(sum[:])
}

// qoderPathSig 计算 chat 签名的 pathSig：URL path 去掉前缀 "/algo"
// （/algo/api/v2/... → /api/v2/...；无前缀时原样返回）。
func qoderPathSig(urlPath string) string {
	return strings.TrimPrefix(urlPath, "/algo")
}

// BuildQoderPayloadB64 构造 chat 签名 payload 的 base64：
// base64(json({cosyVersion, ideVersion:"", info:<会话info>, requestId, version:"v1"}))。
func BuildQoderPayloadB64(info, requestID string) string {
	payload := map[string]string{
		"cosyVersion": qoderCosyVersion,
		"ideVersion":  "",
		"info":        info,
		"requestId":   requestID,
		"version":     "v1",
	}
	raw, _ := json.Marshal(payload)
	return base64.StdEncoding.EncodeToString(raw)
}

// QoderComposeBearer 组装 COSY Authorization 头：
// "Bearer COSY." + payloadB64 + "." + md5(payloadB64 + "\n" + cosyKey + "\n" + date + "\n" + body + "\n" + pathSig)。
func QoderComposeBearer(payloadB64, cosyKey, date, body, pathSig string) string {
	sum := md5.Sum([]byte(payloadB64 + "\n" + cosyKey + "\n" + date + "\n" + body + "\n" + pathSig))
	return "Bearer COSY." + payloadB64 + "." + hex.EncodeToString(sum[:])
}

// QoderSignRequest 组合签名请求（BuildQoderPayloadB64 + QoderComposeBearer 一步到位，
// 返回 payloadB64 与 Authorization 头值；供客户端与单测复用）。
func QoderSignRequest(info, requestID, cosyKey, date, body, pathSig string) (payloadB64, authorization string) {
	payloadB64 = BuildQoderPayloadB64(info, requestID)
	authorization = QoderComposeBearer(payloadB64, cosyKey, date, body, pathSig)
	return payloadB64, authorization
}

// QoderAuthIdentity 是 COSY 会话封装的身份信息（AES 加密前的 JSON 载荷）。
type QoderAuthIdentity struct {
	Name               string `json:"name"`
	AID                string `json:"aid"`
	UID                string `json:"uid"`
	YXUID              string `json:"yx_uid"`
	OrganizationID     string `json:"organization_id"`
	OrganizationName   string `json:"organization_name"`
	UserType           string `json:"user_type"`
	SecurityOauthToken string `json:"security_oauth_token"`
	RefreshToken       string `json:"refresh_token"`
}

// QoderSessionContext 是一次会话封装的产物：cosyKey（RSA 包裹的 tempKey）与
// info（AES 加密的身份 JSON），分别用于 cosy-key 头与签名 payload。
type QoderSessionContext struct {
	CosyKey string
	Info    string
}

// qoderParseRSAPublicKey 解析内嵌 PEM 公钥（进程级只解析一次，失败即协议常量损坏）。
func qoderParseRSAPublicKey() (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(qoderRSAPublicKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("qoder cosy: invalid embedded RSA public key PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("qoder cosy: parse RSA public key: %w", err)
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("qoder cosy: embedded key is not RSA public key")
	}
	return pub, nil
}

// qoderAES128CBCEncrypt AES-128-CBC + PKCS7 加密（key 即 IV 源：IV = key 前 16 字节）。
func qoderAES128CBCEncrypt(key, plaintext []byte) ([]byte, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("qoder cosy: AES key must be 16 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("qoder cosy: new AES cipher: %w", err)
	}
	// PKCS7 padding 到块大小。
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := make([]byte, len(plaintext)+pad)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	iv := key[:aes.BlockSize]
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)
	return ciphertext, nil
}

// qoderAES128CBCDecrypt 是 qoderAES128CBCEncrypt 的逆（PKCS7 去填充，
// 填充非法时报错，供 Decode 路径与单测使用）。
func qoderAES128CBCDecrypt(key, ciphertext []byte) ([]byte, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("qoder cosy: AES key must be 16 bytes, got %d", len(key))
	}
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("qoder cosy: invalid AES ciphertext length %d", len(ciphertext))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("qoder cosy: new AES cipher: %w", err)
	}
	iv := key[:aes.BlockSize]
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, ciphertext)
	pad := int(plaintext[len(plaintext)-1])
	if pad <= 0 || pad > aes.BlockSize || pad > len(plaintext) {
		return nil, fmt.Errorf("qoder cosy: invalid PKCS7 padding")
	}
	for _, b := range plaintext[len(plaintext)-pad:] {
		if int(b) != pad {
			return nil, fmt.Errorf("qoder cosy: corrupted PKCS7 padding")
		}
	}
	return plaintext[:len(plaintext)-pad], nil
}

// QoderNewSession 构造 COSY 会话：
//   - 随机 16 字节 tempKey（hex 编码截 16 字符作为 AES 密钥源）；
//   - tempKey 经 RSA PKCS1v15 加密后 base64 = cosyKey；
//   - 身份 JSON 以 AES-128-CBC 加密后 base64 = info。
func QoderNewSession(identity QoderAuthIdentity) (*QoderSessionContext, error) {
	// tempKey：16 随机字节 hex（32 字符）截前 16 字符——AES-128 需要 16 字节密钥。
	raw := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return nil, fmt.Errorf("qoder cosy: generate temp key: %w", err)
	}
	tempKey := []byte(hex.EncodeToString(raw)[:16])

	pub, err := qoderParseRSAPublicKey()
	if err != nil {
		return nil, err
	}
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, pub, tempKey)
	if err != nil {
		return nil, fmt.Errorf("qoder cosy: RSA wrap temp key: %w", err)
	}
	cosyKey := base64.StdEncoding.EncodeToString(wrapped)

	identityJSON, err := json.Marshal(identity)
	if err != nil {
		return nil, fmt.Errorf("qoder cosy: marshal identity: %w", err)
	}
	encrypted, err := qoderAES128CBCEncrypt(tempKey, identityJSON)
	if err != nil {
		return nil, err
	}
	return &QoderSessionContext{
		CosyKey: cosyKey,
		Info:    base64.StdEncoding.EncodeToString(encrypted),
	}, nil
}

// QoderDeriveMachineID 按账号稳定派生设备指纹三件套：
// seed = uid（为空回落 "cred:" + token）；machineId = md5("machine:"+seed) 32 hex；
// machineType = md5("machinetype:"+seed) 截 18 位；machineToken =
// base64.RawURLEncoding(sha512("machinetoken:"+seed)) 截 43 位。
// 派生规则对齐官方客户端：同账号恒同指纹（防多号被设备漂移关联风控）。
func QoderDeriveMachineID(seed string) (machineID, machineType, machineToken string) {
	midSum := md5.Sum([]byte("machine:" + seed))
	machineID = hex.EncodeToString(midSum[:])
	mtSum := md5.Sum([]byte("machinetype:" + seed))
	machineType = hex.EncodeToString(mtSum[:])
	if len(machineType) > 18 {
		machineType = machineType[:18]
	}
	tokSum := sha512.Sum512([]byte("machinetoken:" + seed))
	machineToken = base64.RawURLEncoding.EncodeToString(tokSum[:])
	if len(machineToken) > 43 {
		machineToken = machineToken[:43]
	}
	return machineID, machineType, machineToken
}

// qoderFingerprintSeed 返回指纹派生种子：uid 优先，为空回落 "cred:" + token。
func qoderFingerprintSeed(uid, token string) string {
	if uid = strings.TrimSpace(uid); uid != "" {
		return uid
	}
	return "cred:" + strings.TrimSpace(token)
}

// qoderJobTokenInner 请求 payload 的内层对象（PAT / 刷新令牌二选一）。
type qoderJobTokenInner struct {
	PersonalToken      string         `json:"personalToken"`
	SecurityOauthToken string         `json:"securityOauthToken"`
	RefreshToken       string         `json:"refreshToken"`
	NeedRefresh        bool           `json:"needRefresh"`
	AuthInfo           map[string]any `json:"authInfo"`
}

// qoderJobTokenResponse jobToken 交换响应（身份信息子集）。
type qoderJobTokenResponse struct {
	Name               string `json:"name"`
	ID                 string `json:"id"`
	UserType           string `json:"userType"`
	SecurityOauthToken string `json:"securityOauthToken"`
	RefreshToken       string `json:"refreshToken"`
}

// QoderExchangeJobToken 用 PAT（pt-）/刷新令牌向 jobToken 端点换取
// securityOauthToken。请求体为 Encode(json({payload:<string(json(inner))>,
// encodeVersion:"1"}))，头为 Legacy 签名全家桶；设备指纹按 uid（缺省回落
// "cred:"+token）稳定派生。transport 恒为非 nil（网关/测试服务各自注入带
// TLS profile 的传输实现）。
func QoderExchangeJobToken(transport http.RoundTripper, endpoints qoderEndpoints, uid, token string) (*qoderJobTokenResponse, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("qoder jobToken exchange: empty token")
	}
	if transport == nil {
		return nil, fmt.Errorf("qoder jobToken exchange: nil transport")
	}
	inner := qoderJobTokenInner{AuthInfo: map[string]any{}}
	if strings.HasPrefix(token, "drt-") {
		inner.RefreshToken = token
		inner.NeedRefresh = true
	} else {
		inner.PersonalToken = token
	}
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		return nil, fmt.Errorf("qoder jobToken exchange: marshal inner: %w", err)
	}
	envelope := map[string]string{
		"payload":       string(innerJSON),
		"encodeVersion": "1",
	}
	envelopeJSON, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("qoder jobToken exchange: marshal envelope: %w", err)
	}
	body := QoderCosyEncode(envelopeJSON)

	date := time.Now().UTC().Format(http.TimeFormat) // "Mon, 02 Jan 2006 15:04:05 GMT"
	req, err := http.NewRequest(http.MethodPost, endpoints.JobTokenURL, strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("qoder jobToken exchange: build request: %w", err)
	}
	applyQoderJobTokenHeaders(req, date, qoderFingerprintSeed(uid, token))
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("qoder jobToken exchange: transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, workbuddyUpstreamErrorBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("qoder jobToken exchange: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("qoder jobToken exchange: HTTP %d: %s", resp.StatusCode, workbuddyTruncateForError(string(raw)))
	}
	// 响应体为 Encode 编码的信封：先 Decode 再 json。
	decoded, derr := QoderCosyDecode(strings.TrimSpace(string(raw)))
	if derr != nil {
		// 兼容上游偶发明文 JSON 响应（非 Encode 形态）。
		var plain qoderJobTokenResponse
		if perr := json.Unmarshal(raw, &plain); perr != nil || strings.TrimSpace(plain.SecurityOauthToken) == "" {
			return nil, fmt.Errorf("qoder jobToken exchange: decode response: %w", derr)
		}
		return &plain, nil
	}
	var out qoderJobTokenResponse
	if err := json.Unmarshal(decoded, &out); err != nil {
		return nil, fmt.Errorf("qoder jobToken exchange: parse response: %w", err)
	}
	if strings.TrimSpace(out.SecurityOauthToken) == "" {
		return nil, fmt.Errorf("qoder jobToken exchange: no securityOauthToken in response: %s", workbuddyTruncateForError(string(decoded)))
	}
	return &out, nil
}

// applyQoderJobTokenHeaders 设置 jobToken 交换的全套请求头（Legacy 签名族 +
// 按种子稳定派生的设备指纹三件套）。
func applyQoderJobTokenHeaders(req *http.Request, date, seed string) {
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	req.Header.Set("accept-encoding", "identity")
	req.Header.Set("appcode", qoderCosyAppCode)
	req.Header.Set("cosy-version", qoderCosyVersion)
	req.Header.Set("cosy-clienttype", qoderCosyClientType)
	req.Header.Set("login-version", "v2")
	req.Header.Set("user-agent", "Go-http-client/2.0")
	req.Header.Set("date", date)
	req.Header.Set("signature", qoderLegacySignature(date))
	machineID, machineType, machineToken := QoderDeriveMachineID(seed)
	req.Header.Set("cosy-machineid", machineID)
	req.Header.Set("cosy-machinetype", machineType)
	req.Header.Set("cosy-machinetoken", machineToken)
}

// qoderUnixSeconds 当前 unix 秒字符串（chat 签名 cosy-date 与 date 头共用）。
func qoderUnixSeconds() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}
