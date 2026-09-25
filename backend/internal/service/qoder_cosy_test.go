//go:build unit

package service

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// qoder_cosy_test.go COSY 签名层单测：Encode/Decode 往返、Legacy 签名已知向量、
// chat 请求签名确定性、指纹派生确定性、AES/RSA 会话封装。

func TestQoderCosyEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()
	inputs := [][]byte{
		[]byte(""),
		[]byte("hi"),
		[]byte("hello, cosy world"),
		[]byte(`{"payload":"x","encodeVersion":"1"}`),
		bytesRepeatQoder([]byte("abc123-_/+中文测试"), 50),
	}
	for _, in := range inputs {
		encoded := QoderCosyEncode(in)
		// 编码形态断言：只用自定义字母表与 '$'，不含标准 base64 字符 '='、'+'、'/'。
		require.NotContains(t, encoded, "=")
		require.NotContains(t, encoded, "+")
		require.NotContains(t, encoded, "/")
		for _, c := range encoded {
			require.True(t, strings.ContainsRune(qoderCosyCustomAlphabet, c) || c == '$',
				"编码输出含字母表外字符 %q", c)
		}
		decoded, err := QoderCosyDecode(encoded)
		require.NoError(t, err)
		require.Equal(t, in, decoded, "Encode/Decode 必须严格互逆")
	}
}

func TestQoderCosyDecodeRejectsCorruptInput(t *testing.T) {
	t.Parallel()
	// 非法 base64 长度/填充：解码必须报错而非静默产出脏数据。
	_, err := QoderCosyDecode("~~~~")
	require.Error(t, err)
}

func TestQoderLegacySignatureKnownVector(t *testing.T) {
	t.Parallel()
	// 已知向量：date 取任意固定串时结果稳定且等于 md5(AppCode&Secret&date) hex。
	date := "Mon, 02 Jan 2006 15:04:05 GMT"
	got := qoderLegacySignature(date)
	want := md5.Sum([]byte("cosy&d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw==&" + date))
	require.Equal(t, hex.EncodeToString(want[:]), got)
	// 确定性：同输入恒同输出。
	require.Equal(t, got, qoderLegacySignature(date))
	// 32 位 hex 格式。
	require.Len(t, got, 32)
	_, err := hex.DecodeString(got)
	require.NoError(t, err)
}

func TestQoderSignRequestDeterministic(t *testing.T) {
	t.Parallel()
	payload1, auth1 := QoderSignRequest("info-1", "req-1", "key-1", "1700000000", `{"a":1}`, "/api/v2/x")
	payload2, auth2 := QoderSignRequest("info-1", "req-1", "key-1", "1700000000", `{"a":1}`, "/api/v2/x")
	require.Equal(t, payload1, payload2)
	require.Equal(t, auth1, auth2)
	// 形态：Bearer COSY.<payloadB64>.<32hex>。
	require.True(t, strings.HasPrefix(auth1, "Bearer COSY."), "Authorization 形态: %s", auth1)
	rest := strings.TrimPrefix(auth1, "Bearer COSY.")
	parts := strings.Split(rest, ".")
	require.Len(t, parts, 2)
	_, err := base64.StdEncoding.DecodeString(parts[0])
	require.NoError(t, err, "payload 段必须是合法 base64")
	require.Len(t, parts[1], 32)
	// payload 解开后包含 cosyVersion/info/requestId/version。
	raw, err := base64.StdEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	var payload map[string]string
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.Equal(t, "1.0.10", payload["cosyVersion"])
	require.Equal(t, "req-1", payload["requestId"])
	require.Equal(t, "info-1", payload["info"])
	require.Equal(t, "v1", payload["version"])
	// 不同输入产生不同签名。
	_, auth3 := QoderSignRequest("info-2", "req-1", "key-1", "1700000000", `{"a":1}`, "/api/v2/x")
	require.NotEqual(t, auth1, auth3)
}

func TestQoderDeriveMachineIDDeterministic(t *testing.T) {
	t.Parallel()
	a, b, c := QoderDeriveMachineID("u-1")
	a2, b2, c2 := QoderDeriveMachineID("u-1")
	require.Equal(t, a, a2)
	require.Equal(t, b, b2)
	require.Equal(t, c, c2)
	// 格式：machineId 32 hex；machineType 18 hex；machineToken 43 位 URL-safe base64。
	require.Len(t, a, 32)
	_, err := hex.DecodeString(a)
	require.NoError(t, err)
	require.Len(t, b, 18)
	require.Len(t, c, 43)
	require.NotContains(t, c, "+")
	require.NotContains(t, c, "/")
	require.NotContains(t, c, "=")
	// 不同 seed 派生不同指纹。
	d, e, f := QoderDeriveMachineID("u-2")
	require.NotEqual(t, a, d)
	require.NotEqual(t, b, e)
	require.NotEqual(t, c, f)
}

func TestQoderFingerprintSeedFallback(t *testing.T) {
	t.Parallel()
	require.Equal(t, "u-1", qoderFingerprintSeed("u-1", "tok"))
	require.Equal(t, "cred:tok", qoderFingerprintSeed("", "tok"))
	require.Equal(t, "cred:tok", qoderFingerprintSeed("  ", " tok "))
}

func TestQoderNewSessionRSAAndAES(t *testing.T) {
	t.Parallel()
	identity := QoderAuthIdentity{
		Name:               "tester",
		UID:                "u-9",
		UserType:           "personal_standard",
		SecurityOauthToken: "sec-tok",
		RefreshToken:       "drt-x",
	}
	session, err := QoderNewSession(identity)
	require.NoError(t, err)
	require.NotEmpty(t, session.CosyKey)
	require.NotEmpty(t, session.Info)

	// cosyKey：合法 base64（RSA 加密产物）。
	cosyKeyRaw, err := base64.StdEncoding.DecodeString(session.CosyKey)
	require.NoError(t, err)
	require.NotEmpty(t, cosyKeyRaw)

	// info 是 AES 加密的 base64；RSA 私钥不在手，无法端到端解出 tempKey，
	// 但 info 的可解性由「同 tempKey 才能解」保证——此处验证 base64 形态与非常量性。
	infoRaw, err := base64.StdEncoding.DecodeString(session.Info)
	require.NoError(t, err)
	require.NotEmpty(t, infoRaw)
	require.Equal(t, 0, len(infoRaw)%16, "AES-CBC 密文长度必须是块大小的整数倍")
	session2, err := QoderNewSession(identity)
	require.NoError(t, err)
	require.NotEqual(t, session.CosyKey, session2.CosyKey, "每次会话 tempKey 必须随机")
	require.NotEqual(t, session.Info, session2.Info)
}

func TestQoderAESRoundTrip(t *testing.T) {
	t.Parallel()
	key := []byte("0123456789abcdef")
	plaintext := []byte(`{"name":"n","uid":"u"}`)
	encrypted, err := qoderAES128CBCEncrypt(key, plaintext)
	require.NoError(t, err)
	decrypted, err := qoderAES128CBCDecrypt(key, encrypted)
	require.NoError(t, err)
	require.Equal(t, plaintext, decrypted)
	// 错误密钥解不开（padding 校验大概率失败）。
	_, err = qoderAES128CBCDecrypt([]byte("fedcba9876543210"), encrypted)
	require.Error(t, err)
}

func TestQoderPathSigStripsAlgoPrefix(t *testing.T) {
	t.Parallel()
	require.Equal(t, "/api/v2/service/pro/sse/agent_chat_generation", qoderPathSig("/algo/api/v2/service/pro/sse/agent_chat_generation"))
	require.Equal(t, "/api/v1/x", qoderPathSig("/api/v1/x"))
}

// bytesRepeatQoder 测试辅助：重复拼接字节串。
func bytesRepeatQoder(b []byte, n int) []byte {
	out := make([]byte, 0, len(b)*n)
	for i := 0; i < n; i++ {
		out = append(out, b...)
	}
	return out
}
