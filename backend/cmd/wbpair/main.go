package main

// 临时探针（跑完即删）：验证「assistant.tool_calls[].function.arguments 是非法 JSON」
// 是否就是 WorkBuddy 上游 400 code=11148 tool_call_sequence_broken 的直接触发条件。
//
// 背景：sub2api 的指纹脱敏层（workbuddy_payload.go:81-85 sanitizeWorkbuddyToolCalls）
// 在 tool 配对修复（:63-67）**之后**执行，会把 arguments 字符串里的
// "x-anthropic-billing-header: ..." / "cc_xxx=..." 整段删掉，连带吃掉闭合引号与花括号，
// 产出非法 JSON（已由单测坐实）。上游 Go struct 解析不出 tool_calls 时，
// 对应的 role:tool 结果就成了「多出来的」，于是判 11148。
//
// 全部使用 deepseek-v4.1-flash（WorkBuddy 上游只有该模型，禁止 gpt 系列）。
// 每个 case 的 messages 都保证 tool_call 与 tool 结果**数量与 id 完全配对**，
// 唯一变量是 arguments 是否合法 JSON。
//
// 用法: go run ./cmd/wbpair [case关键字]

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	upstream = "https://copilot.tencent.com"
	chatPath = "/v2/chat/completions"
	testMode = "deepseek-v4.1-flash"
)

// toolCall 构造一条 assistant 工具调用消息
func asst(calls ...[2]string) map[string]any { // {id, arguments}
	tcs := make([]any, 0, len(calls))
	for _, c := range calls {
		tcs = append(tcs, map[string]any{
			"id": c[0], "type": "function",
			"function": map[string]any{"name": "Bash", "arguments": c[1]},
		})
	}
	return map[string]any{"role": "assistant", "tool_calls": tcs}
}

func tool(id, out string) map[string]any {
	return map[string]any{"role": "tool", "tool_call_id": id, "content": out}
}

func body(msgs ...map[string]any) map[string]any {
	return map[string]any{
		"model":      testMode,
		"messages":   msgs,
		"max_tokens": 64,
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "Bash", "description": "run a command",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{
				"command": map[string]any{"type": "string"},
			}, "required": []string{"command"}},
		}}},
	}
}

type probeCase struct {
	name string
	body map[string]any
}

func allCases() []probeCase {
	// 合法 arguments（对照组）
	okA := `{"command":"ls -la"}`
	okB := `{"command":"cat a.txt"}`
	// 非法 arguments —— 三种由 sanitize 实测产出的坏形态
	badTrunc := `{"command":"export ` // 正则吃尾（trailing_kv 实测产物）
	badHdr := `{"command":"grep `     // header 剥离实测产物
	badNum := `{"n":11-128,"cmd":"ls"}` // 数字插连字符实测产物
	return []probeCase{
		{"P1 并行2调用-合法(对照)", body(
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "run two commands"},
			asst([2]string{"call_a", okA}, [2]string{"call_b", okB}),
			tool("call_a", "file list"), tool("call_b", "file body"),
			map[string]any{"role": "user", "content": "thanks, summarize"},
		)},
		{"P2 并行2调用-a非法", body(
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "run two commands"},
			asst([2]string{"call_a", badTrunc}, [2]string{"call_b", okB}),
			tool("call_a", "file list"), tool("call_b", "file body"),
			map[string]any{"role": "user", "content": "thanks, summarize"},
		)},
		{"P3 并行2调用-双非法", body(
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "run two commands"},
			asst([2]string{"call_a", badHdr}, [2]string{"call_b", badNum}),
			tool("call_a", "file list"), tool("call_b", "file body"),
			map[string]any{"role": "user", "content": "thanks, summarize"},
		)},
		{"P4 单调用-非法", body(
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "run one command"},
			asst([2]string{"call_a", badTrunc}),
			tool("call_a", "file list"),
			map[string]any{"role": "user", "content": "thanks, summarize"},
		)},
		// ---- 不变式探测：上游到底校什么（数量对称 / id 对称 / 紧邻）----
		{"Q1 缺一个结果(2调1果)", body(
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "run two commands"},
			asst([2]string{"call_a", okA}, [2]string{"call_b", okB}),
			tool("call_a", "file list"),
			map[string]any{"role": "user", "content": "thanks, summarize"},
		)},
		{"Q2 多余结果(孤儿tool)", body(
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "run one command"},
			asst([2]string{"call_a", okA}),
			tool("call_a", "file list"),
			tool("call_zz", "orphan"),
			map[string]any{"role": "user", "content": "thanks, summarize"},
		)},
		{"Q3 结果无id键", body(
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "run one command"},
			asst([2]string{"call_a", okA}),
			map[string]any{"role": "tool", "content": "file list"},
			map[string]any{"role": "user", "content": "thanks, summarize"},
		)},
		{"Q4 结果被user隔开", body(
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "run one command"},
			asst([2]string{"call_a", okA}),
			map[string]any{"role": "user", "content": "approved"},
			tool("call_a", "file list"),
			map[string]any{"role": "user", "content": "thanks, summarize"},
		)},
		{"Q5 结果乱序(b先a后)", body(
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "run two commands"},
			asst([2]string{"call_a", okA}, [2]string{"call_b", okB}),
			tool("call_b", "file body"), tool("call_a", "file list"),
			map[string]any{"role": "user", "content": "thanks, summarize"},
		)},
		{"Q6 assistant无content键", body(
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "run one command"},
			asst([2]string{"call_a", okA}),
			tool("call_a", "file list"),
			map[string]any{"role": "user", "content": "thanks, summarize"},
		)},
		{"Q7 重复call_id(2调同id)", body(
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "run two commands"},
			asst([2]string{"call_a", okA}, [2]string{"call_a", okB}),
			tool("call_a", "file list"),
			map[string]any{"role": "user", "content": "thanks, summarize"},
		)},
		{"Q8 两轮工具历史", body(
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "run one"},
			asst([2]string{"call_a", okA}), tool("call_a", "file list"),
			map[string]any{"role": "assistant", "content": "done, now?"},
			map[string]any{"role": "user", "content": "run another"},
			asst([2]string{"call_b", okB}), tool("call_b", "file body"),
			map[string]any{"role": "user", "content": "thanks, summarize"},
		)},
		// Q9 = normalizeChatMessagesWithToolOutputMedia 在「客户端回填两个同 call_id 结果」
		// 时的实际产物：2 个同 id tool_calls + 2 条同 id tool 结果（同内容被复制）。
		{"Q9 同id双调双果", body(
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "run two commands"},
			asst([2]string{"call_a", okA}, [2]string{"call_a", okB}),
			tool("call_a", "file list"), tool("call_a", "file list"),
			map[string]any{"role": "user", "content": "thanks, summarize"},
		)},
		// Q10 = 并行两工具中一个是 custom(apply_patch) 降级形态
		{"Q10 custom降级并行", body(
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "run two"},
			asst([2]string{"call_a", okA}, [2]string{"call_b", `{"input":"*** Begin Patch"}`}),
			tool("call_a", "file list"), tool("call_b", "ok"),
			map[string]any{"role": "user", "content": "thanks, summarize"},
		)},
		// Q11 = 上一轮工具调用后，本轮又带了一个新的悬空 tool_call（无结果）
		{"Q11 末尾悬空调用", body(
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "run one"},
			asst([2]string{"call_a", okA}), tool("call_a", "file list"),
			asst([2]string{"call_b", okB}),
		)},
	}
}

func main() {
	c := loadCreds()
	if c.AccessToken == "" {
		fmt.Fprintln(os.Stderr, "无凭据")
		os.Exit(2)
	}
	sel := "all"
	if a := os.Args[1:]; len(a) > 0 {
		sel = a[0]
	}
	fmt.Printf("== wbpair  model=%s  ==\n", testMode)
	for _, tc := range allCases() {
		if sel != "all" && !strings.Contains(strings.ToLower(tc.name), strings.ToLower(sel)) {
			continue
		}
		obj := tc.body
		obj["stream"] = true
		raw, _ := json.Marshal(obj)
		st, sn := post(c, raw)
		fmt.Printf("%-28s | bytes=%-5d => %d\n    %s\n", tc.name, len(raw), st, sn)
		time.Sleep(400 * time.Millisecond)
	}
}

// ---------------------------------------------------------------- 复用 wbprobe 逻辑

type creds struct {
	AccessToken string
	Realm       string
	UID         string
}

func loadCreds() creds {
	path := os.TempDir() + string(os.PathSeparator) + "wb_accounts.json"
	if p := strings.TrimSpace(os.Getenv("WB_ACCOUNTS_FILE")); p != "" {
		path = p
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读凭据失败 %s: %v\n", path, err)
		os.Exit(2)
	}
	var doc map[string]map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		os.Exit(2)
	}
	key := strings.TrimSpace(os.Getenv("WB_ACCOUNTS_KEY"))
	if key == "" {
		key = "1"
	}
	acct, ok := doc[key]
	if !ok {
		for k, v := range doc {
			fmt.Printf("[creds] 可用账号=%s\n", k)
			acct, ok = v, true
			break
		}
	}
	if !ok {
		os.Exit(2)
	}
	pick := func(k string) string { s, _ := acct[k].(string); return s }
	return creds{AccessToken: pick("access_token"), Realm: pick("realm"), UID: pick("uid")}
}

func post(c creds, bodyRaw []byte) (int, string) {
	req, err := http.NewRequest(http.MethodPost, upstream+chatPath, strings.NewReader(string(bodyRaw)))
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.Realm != "" {
		req.Header.Set("X-Realm", c.Realm)
	}
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return 0, "TRANSPORT " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	all, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	snip := strings.ReplaceAll(strings.TrimSpace(string(all)), "\n", " ")
	if len([]rune(snip)) > 500 {
		snip = string([]rune(snip)[:500]) + "…"
	}
	return resp.StatusCode, snip
}
