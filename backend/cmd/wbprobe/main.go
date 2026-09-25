package main

// 临时差分探针（跑完可删）：用本地库导出的真实凭据，直连 WorkBuddy 上游
// /v2/chat/completions，逐个变量复现「单条正常、agent 请求 400」。
//
// 用法:
//
//	go run ./cmd/wbprobe <case>
//	# case: all | s0 | s1 | s2 | s3 | s4 | s5 | s6 | s7 | s8 | agentloop
//
// 凭据文件查找顺序（Linux/Windows 通用）：
//  1. 环境变量 WB_ACCOUNTS_FILE 指定的路径
//  2. $WB_ACCOUNTS_FILE 同目录约定：./wb_accounts.json
//  3. os.TempDir()/wb_accounts.json
//
// 每条 case 都会把上游 **原始响应体**（含 code/msg）打印出来，不再依赖网关兜底文案。
// 所有模型固定 deepseek-v4.1-flash（WorkBuddy 上游仅有该系列，勿用其它模型测试）。

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	baseURL       = "https://copilot.tencent.com"
	chatPath      = "/v2/chat/completions"
	refreshPath   = "/v2/plugin/auth/token/refresh"
	clientVersion = "5.5.4"
	cliVersion    = "2.137.1"
	originCN      = "https://www.codebuddy.cn"

	// 唯一允许使用的测试模型（上游账号只有 deepseek 系列）。
	testModel = "deepseek-v4.1-flash"

	// WB_ACCOUNTS_KEY 选择使用哪个账号的凭据（默认 "1"）。
	defaultAcctKey = "1"
)

type creds struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterprise_id"`
	Realm        string `json:"realm"`
	ExpiresAt    int64  `json:"expires_at"`
}

// accountsFile 按优先级定位凭据文件。
func accountsFile() string {
	if p := strings.TrimSpace(os.Getenv("WB_ACCOUNTS_FILE")); p != "" {
		return p
	}
	if p, err := filepath.Abs("wb_accounts.json"); err == nil {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(os.TempDir(), "wb_accounts.json")
}

func loadCreds() creds {
	path := accountsFile()
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取凭据失败 %s: %v\n", path, err)
		os.Exit(2)
	}
	var all map[string]map[string]any
	if err := json.Unmarshal(raw, &all); err != nil {
		fmt.Fprintf(os.Stderr, "解析凭据失败 %s: %v\n", path, err)
		os.Exit(2)
	}
	key := strings.TrimSpace(os.Getenv("WB_ACCOUNTS_KEY"))
	if key == "" {
		key = defaultAcctKey
	}
	m, ok := all[key]
	if !ok {
		keys := make([]string, 0, len(all))
		for k := range all {
			keys = append(keys, k)
		}
		fmt.Fprintf(os.Stderr, "凭据文件无账号 %q（可用: %v）\n", key, keys)
		os.Exit(2)
	}
	pick := func(k string) string { s, _ := m[k].(string); return s }
	var exp float64
	switch v := m["expires_at"].(type) {
	case float64:
		exp = v
	case string:
		_, _ = fmt.Sscanf(v, "%d", &exp)
	}
	c := creds{
		AccessToken:  pick("access_token"),
		RefreshToken: pick("refresh_token"),
		UID:          pick("uid"),
		EnterpriseID: pick("enterprise_id"),
		Realm:        pick("realm"),
		ExpiresAt:    int64(exp),
	}
	if c.UID == "" {
		fmt.Fprintln(os.Stderr, "凭据缺少 uid")
		os.Exit(2)
	}
	if c.Realm == "" {
		c.Realm = "cn"
	}
	if c.EnterpriseID == "" {
		c.EnterpriseID = pick("enterpriseId")
	}
	fmt.Fprintf(os.Stderr, "[creds] file=%s acct=%s uid=%s… realm=%s ent=%s token_len=%d\n",
		path, key, firstN(c.UID, 8), c.Realm, orNone(c.EnterpriseID), len(c.AccessToken))
	return c
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func hexID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18])
}

func randID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// applyHeaders 与 sub2api 生产出站头保持一致的指纹集合。
func applyHeaders(req *http.Request, c creds) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	if strings.EqualFold(c.Realm, "global") {
		req.Header.Set("Origin", "https://www.workbuddy.ai")
	} else {
		req.Header.Set("Origin", originCN)
	}
	req.Header.Set("Referer", originCN+"/")
	req.Header.Set("User-Agent", "WorkBuddy/"+clientVersion+" WorkBuddy/"+clientVersion+" CLI/"+cliVersion)
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("X-Machine-ID", hexID(c.UID, "machine"))
	req.Header.Set("X-Session-ID", hexID(c.UID, "session"))
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("X-User-Id", c.UID)
	if c.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", c.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	req.Header.Set("X-No-Department-Info", "1")
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", clientVersion)
	req.Header.Set("X-Product", "WorkBuddy")
	msg := randID()
	req.Header.Set("X-Conversation-Request-ID", msg)
	req.Header.Set("X-Conversation-Message-ID", msg)
	req.Header.Set("X-Request-ID", msg)
	req.Header.Set("X-Root-Request-ID", msg)
	req.Header.Set("X-Trace-ID", msg)
	req.Header.Set("X-B3-TraceId", msg)
	req.Header.Set("X-B3-SpanId", msg[:16])
	req.Header.Set("X-B3-Sampled", "1")
}

var httpc = &http.Client{Timeout: 180 * time.Second}

// refreshOnce 刷新 access token（就地更新 c）；失败返回错误。
func refreshOnce(c *creds) error {
	req, _ := http.NewRequest(http.MethodPost, baseURL+refreshPath, nil)
	applyHeaders(req, *c)
	req.Header.Set("X-Refresh-Token", c.RefreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken string `json:"accessToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Code != 0 || env.Data.AccessToken == "" {
		return fmt.Errorf("refresh failed: http=%d body=%s", resp.StatusCode, snippet(string(body), 300))
	}
	c.AccessToken = env.Data.AccessToken
	fmt.Fprintln(os.Stderr, "[token refreshed]")
	return nil
}

// doRaw 发送并返回 (status, 响应体摘要)。非 200 时打印原始 body（关键：拿到真实 code/msg）。
func doRaw(c creds, body []byte) (int, string) {
	req, err := http.NewRequest(http.MethodPost, baseURL+chatPath, bytes.NewReader(body))
	if err != nil {
		panic(err)
	}
	applyHeaders(req, c)
	resp, err := httpc.Do(req)
	if err != nil {
		return -1, "transport: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	all, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	snip := string(all)
	if resp.StatusCode == 200 {
		frames := strings.Count(snip, "data: ")
		var (
			content strings.Builder
			errFrm  string
			toolN   int
		)
		for _, line := range strings.Split(snip, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			if payload == "[DONE]" {
				continue
			}
			if strings.Contains(payload, `"error"`) && errFrm == "" {
				errFrm = snippet(payload, 200)
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
						//nolint:revive // 探针需要观察工具帧
						ToolCalls []struct {
							Function struct {
								Name string `json:"name"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(payload), &chunk) == nil && len(chunk.Choices) > 0 {
				content.WriteString(chunk.Choices[0].Delta.Content)
				toolN += len(chunk.Choices[0].Delta.ToolCalls)
			}
		}
		out := snippet(content.String(), 60)
		return 200, fmt.Sprintf("sse frames=%d tools=%d err_frame=%q content=%q", frames, toolN, errFrm, out)
	}
	return resp.StatusCode, "RAW_BODY=" + snippet(strings.ReplaceAll(snip, "\n", " "), 900)
}

func prepare(c creds, obj map[string]any) []byte {
	b, _ := json.Marshal(obj)
	return service.PrepareWorkbuddyBody(b, service.WorkbuddyCredentials{Realm: c.Realm, UID: c.UID})
}

// prepareNone 绕过整条变换管线，只补 stream:true（用于隔离「我们注入的字段是否元凶」）。
func prepareNone(obj map[string]any) []byte {
	obj["stream"] = true
	b, _ := json.Marshal(obj)
	return b
}

func report(label string, status int, summary string, body []byte) {
	fmt.Printf("%-34s | bytes=%-6d => %d\n    %s\n", label, len(body), status, summary)
}

func snippet(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---------------------------------------------------------------------------
// 请求体构造器（全部 deepseek-v4.1-flash）
// ---------------------------------------------------------------------------

// longSystemClaudeCode 真实 Claude Code 形态的超长 system（含已知指纹模板句）。
func longSystemClaudeCode() string {
	parts := []string{
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are an interactive agent that helps users with software engineering tasks.",
		"# System\n - All text you output outside of tool use is displayed to the user.",
		"- Tools are executed in a user-selected permission mode.",
		"# Doing tasks\n - The user will primarily request you to perform software engineering tasks.",
		"- Read files before editing. Never assume file contents.",
		"# Function update\n gitStatus: This is the optional git status output.",
		"Main branch (you will usually use this for PRs): main",
		"# Workspace\n Primary working directory: /root/project",
		"Is a git repository: true",
		"Platform: linux",
		"Shell: bash",
		"OS Version: Linux 6.1.0",
		"# Feedback\n To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"# Environment\n You are powered by the model named deepseek-v4.1-flash.",
		"# Tone\n Keep responses short. Use markdown. Do not use emojis.",
		"# Language\n Always respond in the language the user writes in.",
		strings.Repeat("Additional guidance line for body size padding. ", 60),
	}
	return strings.Join(parts, "\n")
}

// ccTools 8 个带完整 parameters 的 function 工具（Claude Code 形态）。
func ccTools() []any {
	names := []string{"Bash", "Read", "Write", "Edit", "Glob", "Grep", "Task", "TodoWrite"}
	out := make([]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"type": "function", "function": map[string]any{
			"name":        n,
			"description": "Claude Code tool " + n + " for agentic software engineering.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cmd":  map[string]any{"type": "string", "description": "argument one"},
					"path": map[string]any{"type": "string", "description": "argument two"},
				},
				"required": []string{"cmd"},
			},
		}})
	}
	return out
}

// codexToolsNoParams Codex 桥产物形态：function 工具**不带 parameters**，custom 工具降级后带。
func codexToolsNoParams() []any {
	return []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "shell", "description": "runs a shell command"}},
		map[string]any{"type": "function", "function": map[string]any{
			"name": "update_plan", "description": "update the plan",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"plan": map[string]any{"type": "array"}}, "required": []string{"plan"}},
		}},
	}
}

// agentHistory 完整配对的两轮工具历史（assistant.tool_calls + role:tool 结果）。
func agentHistory() []any {
	return []any{
		map[string]any{"role": "user", "content": "看下当前目录有什么文件"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "call_a1", "type": "function", "function": map[string]any{"name": "Bash", "arguments": `{"cmd":"ls -la"}`}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call_a1", "content": "total 3\n-rw-r--r-- 1 root root 12 a.txt\n-rw-r--r-- 1 root root 34 b.md"},
		map[string]any{"role": "assistant", "content": "有两个文件：a.txt 与 b.md。"},
		map[string]any{"role": "user", "content": "把 a.txt 的内容读出来"},
	}
}

func baseBody() map[string]any {
	return map[string]any{
		"model": testModel,
		"messages": []any{
			map[string]any{"role": "system", "content": "You are helpful."},
			map[string]any{"role": "user", "content": "只回复OK"},
		},
		"max_tokens": 512,
	}
}

// bodySimple S0 基线：单条短消息。
func bodySimple() map[string]any { return baseBody() }

// bodyAgentFull S1 完整 agent 形态（长 system + 8 工具 + 两轮工具历史）。
func bodyAgentFull() map[string]any {
	b := map[string]any{
		"model": testModel,
		"messages": append([]any{
			map[string]any{"role": "system", "content": longSystemClaudeCode()},
		}, agentHistory()...),
		"tools":       ccTools(),
		"tool_choice": "auto",
		"max_tokens":  4096,
	}
	return b
}

// bodyLongSystemOnly S2 只保留超长 system（无 tools、无工具历史）——隔离体积/系统提示因素。
func bodyLongSystemOnly() map[string]any {
	return map[string]any{
		"model": testModel,
		"messages": []any{
			map[string]any{"role": "system", "content": longSystemClaudeCode()},
			map[string]any{"role": "user", "content": "只回复OK"},
		},
		"max_tokens": 512,
	}
}

// bodyToolsNoSys S3 短 system + 工具声明 + 工具历史（隔离 tools/历史因素）。
func bodyToolsNoSys() map[string]any {
	return map[string]any{
		"model":       testModel,
		"messages":    agentHistory(),
		"tools":       ccTools(),
		"tool_choice": "auto",
		"max_tokens":  4096,
	}
}

// bodyCodexShape S4 Codex 桥真实产物：function 缺 parameters + 长 instructions + 工具历史。
func bodyCodexShape() map[string]any {
	return map[string]any{
		"model": testModel,
		"messages": append([]any{
			map[string]any{"role": "system", "content": "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. " + strings.Repeat("padding guidance. ", 60)},
		}, agentHistory()...),
		"tools":               codexToolsNoParams(),
		"parallel_tool_calls": true,
		"max_tokens":          4096,
	}
}

// bodyNoInject S5 完整 agent 形态但**绕过整条管线**（不注入 thinking/effort/prompt_cache_key、不做脱敏、不翻译）。
func bodyNoInject() map[string]any {
	b := bodyAgentFull()
	b["reasoning_effort"] = "high"
	return b
}

// bodyNoThinking S6 完整 agent 形态 + 管线，但显式关思考（thinking disabled）——隔离 thinking/effort。
func bodyNoThinking() map[string]any {
	b := bodyAgentFull()
	b["thinking"] = map[string]any{"type": "disabled"}
	return b
}

// bodyNoCacheKey S7 完整 agent 形态 + 管线，但显式带一个客户端自己的 prompt_cache_key（验证注入键是否敏感）。
func bodyNoCacheKey() map[string]any {
	b := bodyAgentFull()
	b["prompt_cache_key"] = "probe-fixed-key-0001"
	return b
}

// bodyRawTools S8 完整 agent 形态 + 管线，但**保留 CC 透传杂项**（store/service_tier/n）——隔离透传字段。
func bodyRawTools() map[string]any {
	b := bodyAgentFull()
	b["store"] = true
	b["service_tier"] = "auto"
	b["n"] = 1
	return b
}

// runCases 执行 case 矩阵。
type probeCase struct {
	name     string
	build    func() map[string]any
	skipPrep bool // true = 绕过 PrepareWorkbuddyBody
}

func allCases() []probeCase {
	return []probeCase{
		{"S0 单条基线", bodySimple, false},
		{"S1 完整agent形态", bodyAgentFull, false},
		{"S2 超长system无tools", bodyLongSystemOnly, false},
		{"S3 tools+历史短system", bodyToolsNoSys, false},
		{"S4 Codex缺parameters", bodyCodexShape, false},
		{"S5 绕过整条管线", bodyNoInject, true},
		{"S6 thinking disabled", bodyNoThinking, false},
		{"S7 固定cache_key", bodyNoCacheKey, false},
		{"S8 透传杂项字段", bodyRawTools, false},
	}
}

func main() {
	if len(mime.TypeByExtension(".json")) == 0 {
		_ = mime.AddExtensionType(".json", "application/json")
	}
	c := loadCreds()
	if c.ExpiresAt > 0 && time.Now().Add(time.Minute).Unix() >= c.ExpiresAt {
		if err := refreshOnce(&c); err != nil {
			fmt.Fprintf(os.Stderr, "刷新 token 失败: %v\n", err)
			os.Exit(2)
		}
	}

	args := os.Args[1:]
	sel := "all"
	if len(args) > 0 {
		sel = args[0]
	}
	fmt.Printf("== wbprobe  model=%s  upstream=%s%s ==\n", testModel, baseURL, chatPath)

	matched := 0
	for _, tc := range allCases() {
		if sel != "all" && !strings.Contains(tc.name, sel) && !strings.HasPrefix(strings.ToLower(tc.name), strings.ToLower(sel)) {
			continue
		}
		matched++
		obj := tc.build()
		var body []byte
		if tc.skipPrep {
			body = prepareNone(obj)
		} else {
			body = prepare(c, obj)
		}
		st, sn := doRaw(c, body)
		report(tc.name, st, sn, body)
		if st != 200 {
			// 打印脱敏后的出站体，便于人工核对差异
			fmt.Printf("    OUT=%s\n", snippet(strings.ReplaceAll(string(body), "\n", " "), 700))
		}
		time.Sleep(400 * time.Millisecond)
	}

	if sel == "burst" {
		// 频率假设：同一份脱敏后的完整 agent body 连发 N 次，看上游会不会从
		// 200 变成 400（参考实现经验：真正的频率惩罚应是 429+6004/11140，
		// 若 400 再现则说明拦截与请求次数量相关，而非内容）。
		fmt.Println("--- burst: 同一 agent body 串行连发 25 次（无 sleep）---")
		body := prepare(c, bodyAgentFull())
		fails := 0
		for i := 1; i <= 25; i++ {
			st, sn := doRaw(c, body)
			if st != 200 {
				fails++
				fmt.Printf("B%-2d => %d  %s\n", i, st, snippet(sn, 300))
			} else {
				fmt.Printf("B%-2d => 200\n", i)
			}
		}
		fmt.Printf("burst serial: fails=%d/25\n", fails)

		fmt.Println("--- burst-conc: 8 并发 x 2 波 ---")
		for wave := 1; wave <= 2; wave++ {
			var wg sync.WaitGroup
			codes := make([]int, 8)
			for g := 0; g < 8; g++ {
				wg.Add(1)
				go func(idx int) {
					defer wg.Done()
					codes[idx], _ = doRaw(c, body)
				}(g)
			}
			wg.Wait()
			fmt.Printf("wave%d codes=%v\n", wave, codes)
		}
		matched++
	}

	if sel == "agentloop" {
		matched++
		fmt.Println("--- agentloop: 6 轮增长历史（完整 agent 形态）---")
		for i := 0; i < 6; i++ {
			b := bodyAgentFull()
			msgs := b["messages"].([]any)
			for j := 0; j < i; j++ {
				msgs = append(msgs,
					map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": fmt.Sprintf("call_x%d", j), "type": "function", "function": map[string]any{"name": "Bash", "arguments": `{"cmd":"echo hi"}`}}}},
					map[string]any{"role": "tool", "tool_call_id": fmt.Sprintf("call_x%d", j), "content": "hi"})
			}
			msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("第%d步，只回复OK", i+1)})
			b["messages"] = msgs
			body := prepare(c, b)
			st, sn := doRaw(c, body)
			report(fmt.Sprintf("L%d 轮次#%d", i+1, i+1), st, sn, body)
			if st != 200 {
				fmt.Printf("    OUT=%s\n", snippet(strings.ReplaceAll(string(body), "\n", " "), 700))
			}
			time.Sleep(300 * time.Millisecond)
		}
	}

	if matched == 0 && sel != "all" {
		names := make([]string, 0)
		for _, tc := range allCases() {
			names = append(names, tc.name)
		}
		fmt.Printf("未匹配 case %q；可选: all / agentloop / S0..S8（可用序号或名称片段）\n%v\n", sel, names)
	}
	_ = strings.TrimSpace
	_ = fmt.Sprint
}
