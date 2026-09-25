package main

// 临时探针（跑完可删）：复现「单条消息正常、连续多轮就 400」。
//
// 做法：打本地 sub2api 网关 /v1/responses（WorkBuddy 账号），第一轮只发一条 user，
// 然后把**网关真实返回的 output item 原样回填**再发第二轮、第三轮……
// 对回填内容做差分，定位到底是哪一类 item 让上游/网关炸掉：
//
//	V1 keepAll    —— 全部 item 原样回填（含 reasoning 的 encrypted_content，Codex 真实形态）
//	V2 dropReas   —— 丢弃 reasoning item，只回填 function_call / message
//	V3 stripEnc   —— 保留 reasoning 但删掉 encrypted_content
//	V4 msgPlain   —— assistant message 用纯字符串 content（OpenAI CC 形态）而非 output_text 数组
//
// 模型固定 deepseek-v4.1-flash（WorkBuddy 上游只有该系列，禁止 gpt 系列）。
//
// 用法:
//
//	go run ./cmd/wbloop [baseURL] [apiKey] [rounds]
//
// 缺省 baseURL=http://127.0.0.1:8080，apiKey 取环境变量 SUB2API_KEY。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const model = "deepseek-v4.1-flash"

// codexInstructions 真实 Codex CLI instructions（取自 .tmp-research/wbtest/08_real_codex.json）。
const codexInstructions = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. " +
	"Codex CLI is an open source project led by OpenAI. You are expected to be precise, safe, and helpful.\n\n" +
	"Your capabilities:\n- Receive user prompts and other context provided by the harness.\n" +
	"- Emit function calls to run terminal commands and apply patches.\n\n" +
	"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues\n\n" +
	"Main branch (you will usually use this for PRs)"

// toolFakeOutput 伪造的工具执行结果（探针不真跑 shell，只为推进轮次）。
const toolFakeOutput = "total 8\ndrwxr-xr-x 2 u g 4096 Sep 21 15:00 .\n-rw-r--r-- 1 u g 100 Sep 21 15:00 a.txt\n--- a.txt ---\nhello workbuddy"

type variant struct {
	name     string
	dropReas bool
	stripEnc bool
	msgPlain bool
}

var variants = []variant{
	{"V1 keepAll", false, false, false},
	{"V2 dropReasoning", true, false, false},
	{"V3 stripEncrypted", false, true, false},
	{"V4 msgPlainString", false, false, true},
}

func main() {
	base := "http://127.0.0.1:8080"
	key := strings.TrimSpace(os.Getenv("SUB2API_KEY"))
	rounds := 6
	args := os.Args[1:]
	if len(args) > 0 {
		base = args[0]
	}
	if len(args) > 1 {
		key = args[1]
	}
	if len(args) > 2 {
		if n, err := strconv.Atoi(args[2]); err == nil {
			rounds = n
		}
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "缺少 api key：第二个参数或环境变量 SUB2API_KEY")
		os.Exit(2)
	}
	fmt.Printf("== wbloop  base=%s  model=%s  rounds=%d ==\n", base, model, rounds)
	for _, v := range variants {
		fmt.Printf("\n---------- %s ----------\n", v.name)
		runVariant(base, key, v, rounds)
	}
}

func runVariant(base, key string, v variant, rounds int) {
	input := []map[string]any{
		{"type": "message", "role": "user", "content": []map[string]any{
			{"type": "input_text", "text": "列出当前目录文件，然后读取 a.txt 并总结它的内容"},
		}},
	}
	for i := 1; i <= rounds; i++ {
		body := buildBody(input)
		status, raw, err := post(base, key, body)
		if err != nil {
			fmt.Printf("R%d TRANSPORT_ERR %v\n", i, err)
			return
		}
		items, failFrame := parseSSE(raw)
		if status != 200 {
			fmt.Printf("R%d | HTTP %d | req_bytes=%d | %s\n", i, status, len(body), trim(string(raw), 600))
			return
		}
		if failFrame != "" {
			fmt.Printf("R%d | HTTP 200 但 SSE 出错帧 | req_bytes=%d | items=%d\n    FAIL=%s\n", i, len(body), len(items), trim(failFrame, 600))
			return
		}
		kinds := make([]string, 0, len(items))
		for _, it := range items {
			kinds = append(kinds, fmt.Sprint(it["type"]))
		}
		fmt.Printf("R%d | HTTP 200 | req_bytes=%-6d | output=[%s]\n", i, len(body), strings.Join(kinds, ","))
		if len(items) == 0 {
			fmt.Println("    无 output item，停止（避免空转）")
			return
		}
		next := append([]map[string]any{}, items...)
		// 回填策略差分
		filtered := make([]map[string]any, 0, len(next))
		nCalls := 0
		for _, it := range next {
			t, _ := it["type"].(string)
			if t == "reasoning" {
				if v.dropReas {
					continue
				}
				if v.stripEnc {
					delete(it, "encrypted_content")
				}
			}
			if t == "message" && v.msgPlain {
				if arr, ok := it["content"].([]any); ok && len(arr) > 0 {
					if m, ok := arr[0].(map[string]any); ok {
						it["content"] = fmt.Sprint(m["text"])
					}
				}
			}
			filtered = append(filtered, it)
			if t == "function_call" {
				nCalls++
				// 每个 function_call 必须配一个 function_call_output，否则轮次无法推进
				cid, _ := it["call_id"].(string)
				filtered = append(filtered, map[string]any{
					"type": "function_call_output", "call_id": cid, "output": toolFakeOutput,
				})
			}
		}
		if len(filtered) == 0 {
			fmt.Println("    回填集为空，停止")
			return
		}
		input = append(input, filtered...)
		time.Sleep(250 * time.Millisecond)
	}
	fmt.Println("    跑满所有轮次，未出现失败")
}

func buildBody(input []map[string]any) []byte {
	b := map[string]any{
		"model":        model,
		"instructions": codexInstructions,
		"input":        input,
		"stream":       true,
		"reasoning":    map[string]any{"summary": "auto", "effort": "medium"},
		"include":      []string{"reasoning.encrypted_content"},
		"tools": []any{
			map[string]any{
				"type": "namespace", "name": "update_plan", "description": "Plan toolset",
				"tools": []any{map[string]any{
					"type": "function", "name": "update_plan", "description": "Update the plan",
					"strict": false,
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"plan": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						},
						"required": []string{"plan"},
					},
				}},
			},
			map[string]any{
				"type": "function", "name": "shell", "description": "Runs a shell command and returns its output",
				"strict": false,
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"workdir":    map[string]any{"type": "string"},
						"timeout_ms": map[string]any{"type": "number"},
					},
					"required": []string{"command"},
				},
			},
			map[string]any{"type": "custom", "name": "apply_patch", "description": "Use the `apply_patch` tool to edit files."},
			map[string]any{"type": "web_search"},
		},
		"max_output_tokens":   4096,
		"parallel_tool_calls": false,
		"store":               false,
	}
	out, _ := json.Marshal(b)
	return out
}

func post(base, key string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, base+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 180 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	all, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, all, nil
}

// parseSSE 从 Responses SSE 流里收集 output item（优先 output_item.done 的完整体），
// 并返回出错帧原文（response.failed / error 事件）。
func parseSSE(raw []byte) ([]map[string]any, string) {
	var (
		items   []map[string]any
		failFrm string
		flushed = map[string]bool{}
	)
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if p == "" || p == "[DONE]" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(p), &ev) != nil {
			continue
		}
		t, _ := ev["type"].(string)
		if t == "response.failed" || t == "response.incomplete" || strings.Contains(p, `"error":`) {
			if failFrm == "" {
				failFrm = p
			}
		}
		switch t {
		case "response.output_item.done":
			if it, ok := ev["item"].(map[string]any); ok {
				id, _ := it["id"].(string)
				if id != "" && flushed[id] {
					continue
				}
				flushed[id] = true
				items = append(items, normalizeItem(it))
			}
		case "response.completed", "response.output_items", "response.created":
			// completed 事件带完整 response.output —— 用它兜底补齐（有些实现不发 output_item.done）
			respObj, _ := ev["response"].(map[string]any)
			if respObj == nil {
				continue
			}
			arr, _ := respObj["output"].([]any)
			for _, a := range arr {
				if it, ok := a.(map[string]any); ok {
					id, _ := it["id"].(string)
					if id != "" && flushed[id] {
						continue
					}
					flushed[id] = true
					items = append(items, normalizeItem(it))
				}
			}
		}
	}
	return items, failFrm
}

// normalizeItem 保留 Codex 回传形态需要的键，剥掉网关自加的展示性字段。
func normalizeItem(it map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range it {
		switch k {
		case "id", "status", "summary", "output_index", "type":
			if k == "type" {
				out[k] = v
			}
			continue
		default:
			out[k] = v
		}
	}
	if t, _ := it["type"].(string); t != "" {
		out["type"] = t
	}
	// reasoning 的 summary 是数组形态 [{type:summary_text,text:...}]，回传需要保留
	if t, _ := it["type"].(string); t == "reasoning" {
		if s, ok := it["summary"]; ok {
			out["summary"] = s
		}
	}
	return out
}

func trim(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
