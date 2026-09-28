//go:build unit

package service

// trae_login_device_reuse_test.go 覆盖「设备号复用」与「粘贴截断检测」两组修复。
//
// 背景（为什么值得单测锁住）：Trae 官方授权页对每账号设备数有硬上限（页面文案
// 「每个账号最多可在 3 台设备上保持登录状态」，错误码 20401 / DEVICE_LIMIT_REACHED，
// 取证自授权页生产 bundle）。而浏览器登录每次都会 mint 新 device_id + 新 EC 密钥对，
// 于是"重登/重试"会不断消耗设备槽，占满后该账号再也登录不进来，且上游没有可用的
// 服务端解绑端点。设备号复用是唯一的缓解手段，必须被测试固定住。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 设备号校验与归一（复用入参的入口守门员）
// ---------------------------------------------------------------------------

func TestTraeSanitizeLoginDeviceID(t *testing.T) {
	t.Parallel()

	// 合法：8~24 位纯数字。
	require.Equal(t, "1234567890123456", traeSanitizeLoginDeviceID(" 1234567890123456 "))

	// 非法一律返回空串（由调用方回落新生成）：含字母、过短、过长。
	require.Empty(t, traeSanitizeLoginDeviceID("abc4567890123456"))
	require.Empty(t, traeSanitizeLoginDeviceID("1234567"))
	require.Empty(t, traeSanitizeLoginDeviceID(strings.Repeat("1", 25)))
	require.Empty(t, traeSanitizeLoginDeviceID(""))
	require.Empty(t, traeSanitizeLoginDeviceID("   "))
	// 带连字符的 UUID 形态最危险：授权页会直接判非法设备号，用户侧只看到"网络错误"。
	require.Empty(t, traeSanitizeLoginDeviceID("b5f0e0f2-9944-42ca-857e-608eeb98c8e8"))
}

func TestTraeSanitizeLoginMachineID(t *testing.T) {
	t.Parallel()

	// UUID v4 形态原样接受（大小写归一为小写）。
	const uuidForm = "b5f0e0f2-9944-42ca-857e-608eeb98c8e8"
	require.Equal(t, uuidForm, traeSanitizeLoginMachineID(uuidForm))
	require.Equal(t, uuidForm, traeSanitizeLoginMachineID(strings.ToUpper(uuidForm)))

	// 32hex（本服务落库形态：去掉连字符）必须还原为 UUID 形态，
	// 否则授权 URL 与 DeviceInfo 会拿到与首次登录不同形态的机器码。
	// （这同时锁住一个坑：uuid.Parse 也接受 32 位无连字符 hex，先跑它就会原样返回。）
	compact := strings.ReplaceAll(uuidForm, "-", "")
	require.Equal(t, uuidForm, traeSanitizeLoginMachineID(compact))

	// 非法输入回落空串（触发新生成，而不是把脏值送进上游）。
	require.Empty(t, traeSanitizeLoginMachineID(""))
	require.Empty(t, traeSanitizeLoginMachineID("zzzzzzzz-9944-42ca-857e-608eeb98c8e8"))
	require.Empty(t, traeSanitizeLoginMachineID("short"))
	require.Empty(t, traeSanitizeLoginMachineID("gggggggggggggggggggggggggggggggg"))
}

// ---------------------------------------------------------------------------
// StartLogin 的设备号复用行为
// ---------------------------------------------------------------------------

func newTraeReuseTestService(t *testing.T) *TraeLoginService {
	t.Helper()
	upstream := &traeLoginUpstream{handler: func(req *http.Request) (*http.Response, error) {
		// 所有上游调用都返回 500：本组测试只关心授权 URL 与回显，不关心 guidance 结果。
		return httptest.NewRecorder().Result(), nil
	}}
	cfg := &config.Config{Security: config.SecurityConfig{
		URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
	}}
	oauth := &TraeOAuthService{httpUpstream: upstream, cfg: cfg}
	return NewTraeLoginService(oauth)
}

func TestTraeStartLoginEchoesAndReusesDeviceIdentity(t *testing.T) {
	t.Parallel()
	svc := newTraeReuseTestService(t)

	first, err := svc.StartLoginWithOptions(context.Background(), TraeLoginStartOptions{Realm: "cn"})
	require.NoError(t, err)
	require.NotEmpty(t, first.DeviceID)
	require.NotEmpty(t, first.MachineID)
	// 回显的设备号必须是上游可接受的形态（数字设备号 + UUID 机器码）。
	require.True(t, traeIsNumericID(first.DeviceID), "device_id=%s", first.DeviceID)
	require.Len(t, first.MachineID, 36)

	second, err := svc.StartLoginWithOptions(context.Background(), TraeLoginStartOptions{
		Realm: "cn", DeviceID: first.DeviceID, MachineID: first.MachineID,
	})
	require.NoError(t, err)
	// **同设备重试**：不传才生成新值，传了必须逐字沿用（每多一个设备号就离 20401 更近一步）。
	require.Equal(t, first.DeviceID, second.DeviceID)
	require.Equal(t, first.MachineID, second.MachineID)

	// 授权 URL 里的 device_id / machine_id 必须与回显一致（回显不是事后拼的装饰值）。
	parsed, err := url.Parse(second.LoginURL)
	require.NoError(t, err)
	q := parsed.Query()
	require.Equal(t, first.DeviceID, q.Get("device_id"))
	require.Equal(t, first.MachineID, q.Get("machine_id"))
	require.Equal(t, first.DeviceID, q.Get("x_device_id"))
}

func TestTraeStartLoginRejectsDirtyDeviceIdentityAndRegenerates(t *testing.T) {
	t.Parallel()
	svc := newTraeReuseTestService(t)

	// 用户手填的旧形态脏值（hex 设备号）不得被送进授权 URL —— 那会让授权页直接
	// 渲染"网络错误"，而用户完全看不出是自己填的设备号不合法。
	result, err := svc.StartLoginWithOptions(context.Background(), TraeLoginStartOptions{
		Realm:     "cn",
		DeviceID:  "deadbeefcafe1234",
		MachineID: "not-a-uuid",
	})
	require.NoError(t, err)
	require.True(t, traeIsNumericID(result.DeviceID), "device_id=%s", result.DeviceID)
	require.NotEqual(t, "deadbeefcafe1234", result.DeviceID)
	require.NotEqual(t, "not-a-uuid", result.MachineID)
	require.Len(t, result.MachineID, 36)
}

func TestTraeStartLoginAcceptsCompactHexMachineID(t *testing.T) {
	t.Parallel()
	svc := newTraeReuseTestService(t)

	// 账号里存的 machine_id 是 32hex（落库时去掉了连字符），复用时必须能识别并还原。
	result, err := svc.StartLoginWithOptions(context.Background(), TraeLoginStartOptions{
		Realm:     "cn",
		MachineID: "b5f0e0f2994442ca857e608eeb98c8e8",
	})
	require.NoError(t, err)
	require.Equal(t, "b5f0e0f2-9944-42ca-857e-608eeb98c8e8", result.MachineID)
}

// ---------------------------------------------------------------------------
// 粘贴截断检测（最高频用户错误，报错语义必须准）
// ---------------------------------------------------------------------------

func TestTraeCallbackTruncationHint(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   string
		hit  bool
	}{
		{"完整链接", "http://127.0.0.1:49682/authorize?isRedirect=true&authCodeInfo=%7B%22AuthCode%22%3A%22C%22%7D", false},
		{"省略号截断", "http://127.0.0.1:49682/authorize?isRedirect=true&authCodeInfo=…", true},
		{"三个点截断", "http://127.0.0.1:49682/authorize?authCodeInfo={...}", true},
		{"中文引号包裹", "“http://127.0.0.1:49682/authorize?code=x”", true},
		{"尾随参数名被切", "http://127.0.0.1:49682/authorize?code=x&userInfo=", true},
		{"尾部与号", "http://127.0.0.1:49682/authorize?code=x&", true},
		{"空输入不判定", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hint := traeCallbackTruncationHint(tc.in)
			if tc.hit {
				require.NotEmpty(t, hint, "应识别为截断/包裹：%s", tc.in)
				// 提示必须给可操作指引，不能只说"格式错误"。
				require.Contains(t, hint, "Ctrl+A")
			} else {
				require.Empty(t, hint, "不应误判正常输入：%s (got %q)", tc.in, hint)
			}
		})
	}
}

func TestTraeSubmitCallbackReportsTruncationNotMissingCode(t *testing.T) {
	t.Parallel()
	svc := newTraeReuseTestService(t)

	_, err := svc.StartLogin(context.Background(), "cn", "", nil)
	require.NoError(t, err)
	svc.mu.Lock()
	var loginID string
	for id := range svc.pending {
		loginID = id
	}
	svc.mu.Unlock()
	require.NotEmpty(t, loginID)

	// 用户只拷到一半（authCodeInfo 是最长参数，最容易被切）。注意被截断的 JSON 会让
	// traeParseCallbackURL **解析成功**（解码失败静默降级成"无授权码"），所以必须
	// 在 NO_CODE 分支上也升级措词：报 TRUNCATED 而不是误导性的 NO_CODE——后者会让人
	// 以为登录失败而反复重登，每次重登又多占一个设备槽。
	// 两个形态都是真实截断尾部：停在参数名后、停在不完整转义符中间。
	for _, truncated := range []string{
		"http://127.0.0.1:49682/authorize?isRedirect=true&authCodeInfo=",
		"http://127.0.0.1:49682/authorize?isRedirect=true&authCodeInfo=%7B%22AuthCode%22%3A%22ab%",
	} {
		svc.mu.Lock()
		svc.pending[loginID] = &traeLoginPending{
			Realm: "cn", ClientID: traeCNClientID, LoginTraceID: "t1",
			CodeVerifier: "v", DeviceID: "1234567890123456", Deadline: time.Now().Add(time.Minute),
		}
		svc.mu.Unlock()
		_, err = svc.SubmitCallback(context.Background(), loginID, truncated)
		require.Error(t, err, truncated)
		require.Contains(t, err.Error(), "TRAE_LOGIN_CALLBACK_TRUNCATED", truncated)
	}

	// 同理：真的没有授权码、但也没露出截断痕迹时，仍应报 NO_CODE（不得误升级）。
	svc.mu.Lock()
	svc.pending[loginID] = &traeLoginPending{
		Realm: "cn", ClientID: traeCNClientID, LoginTraceID: "t2",
		CodeVerifier: "v", DeviceID: "1234567890123456", Deadline: time.Now().Add(time.Minute),
	}
	svc.mu.Unlock()
	_, err = svc.SubmitCallback(context.Background(), loginID,
		"http://127.0.0.1:49682/authorize?isRedirect=true&scope=trae")
	require.Error(t, err)
	require.Contains(t, err.Error(), "TRAE_LOGIN_NO_CODE")
	require.NotContains(t, err.Error(), "TRUNCATED")
}
