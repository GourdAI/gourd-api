package service

// workbuddy_sse.go WorkBuddy 上游 SSE 流的规范化与聚合
// （移植自 workbuddy2api/internal/upstream/sse.go + truncation.go）。
//
// 上游只支持流式（stream:true），网关两种消费形态：
//   - 客户端要流式：newWorkbuddySSEReader 把上游 SSE 包装为「规范化后的标准 SSE 流」
//     （逐帧白名单重建、tool_call name 收敛、id 续传、恰好一个 [DONE]）；
//   - 客户端要非流式：aggregateWorkbuddySSE 读取全流聚合为单个 CC JSON 响应
//     （content/reasoning_content/tool_calls 按 index 合并、usage 补 total、
//     截断的 tool_call arguments 丢弃）。
//
// 行解析统一复用仓库既有 extractOpenAISSEDataLine（兼容 `data: xxx` 与 `data:xxx`），
// 与 sub2api 其他 SSE 路径风格一致。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// errWorkbuddyEmptyStream 上游返回 200 但没有有效 SSE 数据帧（空流/只有注释/[DONE]）。
// 用哨兵错误替代裸 fmt.Errorf：调用方（非流式聚合路径）需要区分「上游空流」与
// 普通读错误——空流是上游缺陷，应记 502 观测。
var errWorkbuddyEmptyStream = errors.New("workbuddy upstream stream contained no valid data events")

// workbuddyEmptyStreamErrorFrame 网关本地空流兜底帧（绕过白名单原样写出，
// 保留 error 字段供客户端识别；与 workbuddy2api 同文案）。
const workbuddyEmptyStreamErrorFrame = `{"error":{"message":"empty upstream stream","type":"upstream_error","code":"upstream_parse"}}`

// newWorkbuddySSEReader 包装上游 SSE 为「规范化后的标准 SSE 流」：
//   - 逐帧 normalizeWorkbuddyFrame（白名单重建：id/object/created/model/
//     system_fingerprint/service_tier/choices(delta 白名单/finish_reason 空转 null)/usage；
//     error 帧原样透传；stripWorkbuddyToolCallNames 收敛 tool_call name；id 续传首帧真实 id）；
//   - 吞掉注释行与空行，再自产 "\n\n" 帧分隔；
//   - 保证恰好一个 `data: [DONE]`；
//   - 上游空流时输出一帧 error + [DONE] 并记 warn（不 panic）。
//
// 实现为拉取式（无后台 goroutine）：消费方读多少、上游就消费多少，客户端断连
// 场景下不存在写阻塞的泄漏 goroutine；上游流自然终止（EOF）时读侧收到 [DONE] 后 EOF。
func newWorkbuddySSEReader(r io.Reader) io.Reader {
	if r == nil {
		r = strings.NewReader("")
	}
	return &workbuddySSEReader{
		br:       bufio.NewReaderSize(r, 64*1024),
		toolSeen: map[int]bool{},
	}
}

// workbuddySSEReader 拉取式规范化 SSE 读取器。
type workbuddySSEReader struct {
	br          *bufio.Reader
	pending     bytes.Buffer // 已产出、待消费的规范化帧字节
	toolSeen    map[int]bool // delta.tool_calls 已发过首片的 index（name 收敛）
	firstID     string       // 首帧真实 id（后续帧缺失/空串时续传）
	validFrames int          // 有效帧计数（JSON 解析成功或 error 帧）
	finished    bool         // 已向 pending 写入 [DONE]，不再产出
}

// Read 实现 io.Reader：无待消费字节时逐行推进上游，直到产出至少一帧或流程终止。
func (r *workbuddySSEReader) Read(p []byte) (int, error) {
	for r.pending.Len() == 0 && !r.finished {
		r.pump()
	}
	if r.pending.Len() > 0 {
		return r.pending.Read(p)
	}
	return 0, io.EOF
}

// pump 读一行上游输入并产出规范化帧（或终止流程）。帧分隔由本读取器自产 "\n\n"，
// 注释行/event 行/空行一律吞掉。
func (r *workbuddySSEReader) pump() {
	line, err := r.br.ReadString('\n')
	trimmed := strings.TrimRight(line, "\r\n")
	if payload, ok := extractOpenAISSEDataLine(trimmed); ok {
		payload = strings.TrimSpace(payload)
		switch {
		case payload == "[DONE]":
			// 上游显式结束：停止读取，[DONE] 统一由 finish 写出（保证恰好一个）。
			r.finish()
			return
		case payload != "":
			r.writeFrame(payload)
		}
	}
	if err != nil {
		if err != io.EOF {
			logger.L().Warn("workbuddy sse normalizer: read upstream stream error", zap.Error(err))
		}
		r.finish()
	}
}

// writeFrame 把 payload 按规范白名单重建后暂存。仅 JSON 解析成功（或上游 error 帧）
// 计入有效帧；JSON 解析失败照常降级原样写出，但不计数。
func (r *workbuddySSEReader) writeFrame(payload string) {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		// 非 JSON 帧：原样透传（不二次错误化）。
		r.pending.WriteString("data: " + payload + "\n\n")
		return
	}
	// 上游错误帧透传（error-passthrough）：带 error 键的帧原样写出，不走白名单——
	// 白名单会剥掉 error 字段，客户端就看不到上游 code/msg/requestId。
	if _, hasErr := obj["error"]; hasErr {
		r.validFrames++
		r.pending.WriteString("data: " + payload + "\n\n")
		return
	}
	// 先按 index 收敛 tool_calls name（每 index 仅首片保留，后续分片删 name 键），再规范化透传。
	stripWorkbuddyToolCallNames(obj, r.toolSeen)
	// id 续传：首帧非空真实 id 缓存；后续帧缺 id / 空 id 一律用缓存值
	// （同一条 SSE 消息所有帧共用一个真实 id，后台按 id 归并）。
	if r.firstID == "" {
		if v, ok := obj["id"].(string); ok && v != "" {
			r.firstID = v
		}
	} else if v, ok := obj["id"].(string); !ok || v == "" {
		obj["id"] = r.firstID
	}
	if raw, err := json.Marshal(normalizeWorkbuddyFrame(obj)); err == nil {
		payload = string(raw)
	}
	r.validFrames++
	r.pending.WriteString("data: " + payload + "\n\n")
}

// finish 终止产出：空流先补一帧 error（记录 warn），再保证恰好一个 [DONE]。
func (r *workbuddySSEReader) finish() {
	if r.finished {
		return
	}
	if r.validFrames == 0 {
		logger.L().Warn("workbuddy sse normalizer: upstream stream contained no valid data events")
		r.pending.WriteString("data: " + workbuddyEmptyStreamErrorFrame + "\n\n")
	}
	r.pending.WriteString("data: [DONE]\n\n")
	r.finished = true
}

// stripWorkbuddyToolCallNames 收敛流式 tool_calls 的 name 语义为「每个 index 只出现一次」：
// 首片保留 function.name，同一 index 后续分片里的 name 键一律删除（无论上游是空串
// 还是重复非空串）。这是 OpenAI 官方流的真实形态——首帧带 name，后续帧只带
// arguments 片段、不再出现 name 键——两类消费模型在该形态下同时正确：
//   - 累加型（官方 `name += tc_function?.name || ""`）：后续分片 name 键缺失 → 追加
//     空串，累积 name 保持唯一，不再拼成 Bash×帧数；
//   - 覆盖型（`name ?? state.name`）：后续分片 name 键缺失 → 保留首帧 name，
//     键缺失是比空串更安全的形态（`??` 对空串会误判为重设并清空工具名）。
//
// seen 记录每个 index 是否已发过首片（与 name 是否非空无关）；删除是幂等的。
// 只动 function.name 键，id/type/arguments 原样透传。
func stripWorkbuddyToolCallNames(obj map[string]any, seen map[int]bool) {
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		tcs, _ := delta["tool_calls"].([]any)
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			idx := 0
			if v, ok := tc["index"].(float64); ok {
				idx = int(v)
			}
			if seen[idx] {
				// 已发过首片：删除本分片的 name 键（存在即删，幂等）。
				if fn, _ := tc["function"].(map[string]any); fn != nil {
					delete(fn, "name")
				}
				continue
			}
			// 首现：保留 name 键原样（空 name 也照发），随后分片统一删除。
			seen[idx] = true
		}
	}
}

// normalizeWorkbuddyFrame 以 OpenAI 流式规范白名单重建帧：仅保留标准字段，
// 剔除上游噪声（finish_reason:"" → null、空 content/refusal、空 tool_calls 列表、
// 空占位 function_call、顶层未知字段），空 delta 键一律省略，
// usage 缺失 → null，保证任意标准客户端按规范解析。
func normalizeWorkbuddyFrame(obj map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	if _, ok := out["object"]; !ok {
		out["object"] = "chat.completion.chunk"
	}
	if _, ok := out["id"]; !ok {
		out["id"] = "chatcmpl-wb2api"
	}
	if chs, ok := obj["choices"].([]any); ok {
		nchs := make([]any, 0, len(chs))
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			nc := map[string]any{}
			if idx, ok := c["index"]; ok {
				nc["index"] = idx
			}
			delta := map[string]any{}
			if d, ok := c["delta"].(map[string]any); ok {
				if v, ok := d["role"].(string); ok && v != "" {
					delta["role"] = v
				}
				if v, ok := d["content"].(string); ok && v != "" {
					delta["content"] = v
				}
				if v, ok := d["reasoning_content"].(string); ok && v != "" {
					delta["reasoning_content"] = v
				}
				if v, ok := d["refusal"].(string); ok && v != "" {
					delta["refusal"] = v
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					delta["tool_calls"] = tcs
				}
				if fc, ok := d["function_call"]; ok && fc != nil {
					// 空占位 function_call（name/arguments 全空）视为噪声剔除。
					keep := false
					if fcm, ok2 := fc.(map[string]any); ok2 {
						n, _ := fcm["name"].(string)
						a, _ := fcm["arguments"].(string)
						keep = n != "" || a != ""
					} else {
						keep = true
					}
					if keep {
						delta["function_call"] = fc
					}
				}
			}
			nc["delta"] = delta
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				nc["finish_reason"] = fr
			} else {
				nc["finish_reason"] = nil
			}
			nchs = append(nchs, nc)
		}
		out["choices"] = nchs
	}
	if u, ok := obj["usage"].(map[string]any); ok {
		// usage 帧：把上游非标准的 prompt_cache_hit_tokens 映射为标准嵌套字段，
		// 使计费解析（openAIUsageFromGJSON）能识别缓存命中（见 workbuddyUsageWithCacheReadMapping）。
		out["usage"] = workbuddyUsageWithCacheReadMapping(u)
	} else if u, ok := obj["usage"]; ok {
		out["usage"] = u
	} else {
		out["usage"] = nil
	}
	return out
}

// workbuddyUsageWithCacheReadMapping 返回 usage 的副本：上游非标准的 prompt_cache_hit_tokens
// 在缺少标准嵌套字段时映射为 prompt_tokens_details.cached_tokens。
//
// 背景：既有计费解析（openAIUsageFromGJSON → openAICacheReadTokensFromUsage）只认
// input_tokens_details/prompt_tokens_details.cached_tokens 与 cache_read_input_tokens
// 等标准字段；不映射时缓存命中部分会按全价输入 token 计费（用户多扣费）。
// 语义等价：前缀缓存命中 = cache read。已有 prompt_tokens_details 或命中为 0 时原样
// 返回（零改动，不覆盖上游标准字段）。
func workbuddyUsageWithCacheReadMapping(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	if _, has := u["prompt_tokens_details"]; has {
		return u
	}
	hit := workbuddyJSONInt(u["prompt_cache_hit_tokens"])
	if hit <= 0 {
		return u
	}
	out := make(map[string]any, len(u)+1)
	for k, v := range u {
		out[k] = v
	}
	out["prompt_tokens_details"] = map[string]any{"cached_tokens": hit}
	return out
}

// ---------------------------------------------------------------------------
// 聚合（非流式路径）
// ---------------------------------------------------------------------------

// aggregateWorkbuddySSE 读取完整 SSE 流，聚合 delta.content 为单个 OpenAI
// chat.completion 响应，返回 (JSON 字节, usage 指针, err)。
//
// 分片/半行由 bufio.Reader.ReadString 处理；遇到 "data: [DONE]" 结束。
// tool_calls 以流式 delta 到达（按 index 合并：首片带 id/type/name，后续只带
// arguments 片段；缺 index 时按「id 优先、最近槽位兜底」归位，防多调用数据污染）。
// 流被截断时（finish_reason=="length" 或未收到 [DONE]）丢弃 arguments 残缺的
// tool_call——脏参数会被客户端解析成非法 JSON 卡死会话。
// usage 保留上游原字段并 ensureUsageTotal 补 total；空流返回 errWorkbuddyEmptyStream。
func aggregateWorkbuddySSE(r io.Reader) ([]byte, *OpenAIUsage, error) {
	if r == nil {
		return nil, nil, errWorkbuddyEmptyStream
	}
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model     string
		created       float64
		content       strings.Builder
		reasoning     strings.Builder
		role          = "assistant"
		finishReason  = "stop"
		usage         map[string]any
		gotAnyContent bool
		validEvents   int
		sawDone       bool // 上游显式发过 data: [DONE]（正常收尾）
		toolCalls     = map[int]map[string]any{}
		toolOrder     []int
		// toolSeq 缺 index 的 tool_call 的分配序号源：跨帧延续「最近分配」槽位，同帧内递增。
		toolSeq int
		// idIndex id → 已分配的 index：跨帧持续，供缺 index 时按 id 归位既有调用。
		idIndex = map[string]int{}
	)
	// appendContent 是「已取到正文」（gotAnyContent latch）的唯一写入点：delta 与
	// message 两路 content 都必须经此并入（空串不算「已取到正文」、不占 latch 名额）。
	appendContent := func(txt string) {
		if txt == "" {
			return
		}
		content.WriteString(txt)
		gotAnyContent = true
	}
	// nextToolIndex 分配下一个不冲突的缺 index 序号：从 toolSeq 起递增跳过既有 index。
	nextToolIndex := func() int {
		for {
			idx := toolSeq
			toolSeq++
			if _, used := toolCalls[idx]; !used {
				return idx
			}
		}
	}
	// mergeToolCallsChunk 把一段 tool_calls 数组按 index 合并进累计表（delta 与 message 共用）。
	mergeToolCallsChunk := func(tcs []any) {
		for _, tc := range tcs {
			call, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			idx := -1
			if v, ok := call["index"].(float64); ok {
				idx = int(v)
			} else if cid, _ := call["id"].(string); cid != "" {
				if mid, seen := idIndex[cid]; seen {
					idx = mid // 该 id 已归位过：延续既有调用（跨帧有效）
				} else {
					idx = nextToolIndex()
				}
			} else if len(toolOrder) > 0 {
				idx = toolOrder[len(toolOrder)-1] // 无 id 碎片：延续最近调用
			} else {
				idx = nextToolIndex()
			}
			merged, seen := toolCalls[idx]
			if !seen {
				merged = map[string]any{"index": idx}
				toolCalls[idx] = merged
				toolOrder = append(toolOrder, idx)
			}
			if cid, _ := call["id"].(string); cid != "" {
				idIndex[cid] = idx
			}
			if cid, _ := merged["id"].(string); cid != "" {
				idIndex[cid] = idx
			}
			mergeWorkbuddyToolCallDelta(merged, call)
		}
	}
	// mergeMessageFields 把非 delta 的完整 message 内容并入聚合（整条下发，content 只取一次）。
	mergeMessageFields := func(msg map[string]any) {
		if r2, ok := msg["role"].(string); ok && r2 != "" {
			role = r2
		}
		if txt, ok := msg["content"].(string); ok {
			appendContent(txt)
		}
		if rc, ok := msg["reasoning_content"].(string); ok {
			reasoning.WriteString(rc)
		}
		if tcs, ok := msg["tool_calls"].([]any); ok {
			mergeToolCallsChunk(tcs)
		}
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, nil, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if payload, ok := extractOpenAISSEDataLine(trimmed); ok {
			payload = strings.TrimSpace(payload)
			if payload == "[DONE]" {
				// 上游显式结束：停止读取，DONE 之后的任何数据一律忽略。
				sawDone = true
				break
			}
			if payload != "" {
				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					// 有效事件计数：仅 JSON 解析成功的数据帧计入。
					validEvents++
					if v, ok := chunk["id"].(string); ok && id == "" {
						id = v
					}
					if v, ok := chunk["model"].(string); ok && model == "" {
						model = v
					}
					if v, ok := chunk["created"].(float64); ok && created == 0 {
						created = v
					}
					if u, ok := chunk["usage"].(map[string]any); ok {
						usage = u
					}
					if ch, ok := chunk["choices"].([]any); ok {
						for _, ci := range ch {
							c, _ := ci.(map[string]any)
							if c == nil {
								continue
							}
							if fr, ok := c["finish_reason"].(string); ok && fr != "" {
								finishReason = fr
							}
							if delta, ok := c["delta"].(map[string]any); ok {
								if r2, ok := delta["role"].(string); ok && r2 != "" {
									role = r2
								}
								if txt, ok := delta["content"].(string); ok {
									appendContent(txt)
								}
								if rc, ok := delta["reasoning_content"].(string); ok {
									reasoning.WriteString(rc)
								}
								if tcs, ok := delta["tool_calls"].([]any); ok {
									mergeToolCallsChunk(tcs)
								}
							}
							// 有的上游把完整消息放在 message 里（非 delta）：整条并入，
							// delta 已取过正文（gotAnyContent）则跳过（避免重复拼接）。
							if msg, ok := c["message"].(map[string]any); ok && !gotAnyContent {
								mergeMessageFields(msg)
							}
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if validEvents == 0 {
		// 上游返回 200 但没有任何有效数据事件（空流/只有 [DONE]/只有注释行）：
		// 不合成空 content 的假成功响应，直接报错，由调用方映射为 502 upstream_parse。
		return nil, nil, errWorkbuddyEmptyStream
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		// 流被截断时 tool_call 的 arguments 是残缺 JSON（解析失败），不把脏参数
		// 交给客户端。截断的两个来源：finish_reason=="length"（模型因 max_tokens
		// 提前中止）；上游连接中断（EOF 收尾但未发 [DONE]，sawDone=false）。
		// 完整参数原样保留；空参数（无参工具）不是截断，同样保留。
		if finishReason == "length" || !sawDone {
			calls = dropTruncatedWorkbuddyToolCalls(calls)
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		// OpenAI 非流式 usage 必含 total_tokens。上游若只发 prompt_tokens +
		// completion_tokens（部分上游末帧缺 total），网关合成补齐；已有 total
		// 或二者缺一不补（不臆造：单边有值无法合成可信的 total）。
		// 同时把 prompt_cache_hit_tokens 映射为标准嵌套字段，供上层计费解析识别缓存命中。
		resp["usage"] = ensureWorkbuddyUsageTotal(workbuddyUsageWithCacheReadMapping(usage))
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal workbuddy aggregated response: %w", err)
	}
	// usage 提取复用 sub2api 既有解析（prompt_tokens/completion_tokens/缓存字段映射）；
	// prompt_cache_hit_tokens 已在写回 resp 时映射为标准嵌套字段
	// （workbuddyUsageWithCacheReadMapping），这里解析即含缓存命中（CacheReadInputTokens）。
	var usagePtr *OpenAIUsage
	if parsed, ok := extractOpenAIUsageFromJSONBytes(out); ok {
		usagePtr = &parsed
	}
	return out, usagePtr, nil
}

// ensureWorkbuddyUsageTotal 在 usage 缺 total_tokens 但 prompt_tokens/completion_tokens
// 都在时补齐 total = prompt + completion（通过新 map 合并，不修改原上游 map）。
// 任一缺失或已有 total 时原样返回。
func ensureWorkbuddyUsageTotal(u map[string]any) map[string]any {
	if _, ok := u["total_tokens"]; ok {
		return u
	}
	pt, pok := workbuddyJSONNumber(u["prompt_tokens"])
	ct, cok := workbuddyJSONNumber(u["completion_tokens"])
	if !pok || !cok {
		return u
	}
	out := make(map[string]any, len(u)+1)
	for k, v := range u {
		out[k] = v
	}
	out["total_tokens"] = pt + ct
	return out
}

// workbuddyJSONNumber 把 JSON number（float64/int64/int 均可）归一为 float64；
// 非数字返回 ok=false。与 payload 侧的类型 switch 语义不同：那边是翻译请求别名
// 字段（非数字拒绝整个请求），这边是聚合响应求和（非数字只跳过合成）。
func workbuddyJSONNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}

// workbuddyJSONInt 同 workbuddyJSONNumber 但取整数形态（仅取用场景），非数字返回 0。
func workbuddyJSONInt(v any) int {
	if n, ok := workbuddyJSONNumber(v); ok {
		return int(n)
	}
	return 0
}

// mergeWorkbuddyToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeWorkbuddyToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// workbuddyIsTruncatedArguments 判定工具参数字符串是否因分片丢失而残缺
// （区别于「该工具本就无参数」）：
//   - 空串 / 纯空白 → false（合法无参工具）；
//   - 非空但 JSON 解析失败 → true（截断）；
//   - 能解析（含 null/标量/数组等任何合法 JSON）→ false。
func workbuddyIsTruncatedArguments(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	var v any
	return json.Unmarshal([]byte(trimmed), &v) != nil
}

// dropTruncatedWorkbuddyToolCalls 过滤出 arguments 完整的 tool_call（返回新 slice）。
// 只依据 workbuddyIsTruncatedArguments 判定，不改动任何保留的调用（正例零改动）。
func dropTruncatedWorkbuddyToolCalls(calls []map[string]any) []map[string]any {
	kept := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			kept = append(kept, call)
			continue
		}
		args, _ := fn["arguments"].(string)
		if workbuddyIsTruncatedArguments(args) {
			continue
		}
		kept = append(kept, call)
	}
	return kept
}
