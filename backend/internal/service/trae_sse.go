package service

// trae_sse.go Trae 自定义 SSE 事件流 → OpenAI Chat Completions 流的归一层。
//
// 上游形态（实测，两条通道一致）：`event:` + `data:` 成对，**从不输出 `data: [DONE]`**。
//   - event: output        data: {response|content, reasoning_content|reasoning,
//     tool_calls, finish_reason}   —— 主增量帧
//   - event: done          data: {finish_reason}                                   —— 结束帧
//   - event: token_usage   data: {completion_tokens, ...}                          —— 计费读数
//   - event: error         data: {code, message, extra}                            —— 流内业务错误
//     （HTTP 仍是 200，故必须在此处识别并转成 CC error，否则客户端拿到"成功但空回复"）
//   - event: request_wait_in_queue / queue_begin / queue_end / progress_notice /
//     extra_info / metadata / timing_cost                                        —— 观测类，忽略
//
// 字段改名兼容：新版把 response→content、reasoning_content→reasoning，两套都读。
// data: 后的空格不可假定（参考实现对 `line[5:]` 原样取），解析时统一 TrimSpace。
//
// 归一产物恒为 OpenAI CC chunk。正常结束（上游给出 done 帧，或至少一个有效帧后
// EOF）由 normalizer 补一个 `data: [DONE]`；但**上游在流内给出 event:error 时不补
// [DONE]**——否则该次失败会被上层的截断检测当成正常结束（记成功、按 0 token 出账、
// 不换号也不落账号状态）。错误同时通过 error hook 上报给调用方做分级处置。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
)

// traeEmptyStreamErrorFrame 上游一个有效帧都没给出时的兜底错误帧。
const traeEmptyStreamErrorFrame = `{"error":{"message":"empty upstream stream","type":"upstream_error","code":"upstream_parse"}}`

// errTraeEmptyStream 空流标记（上层据此判定可 failover）。
var errTraeEmptyStream = errors.New("trae: empty upstream stream")

// traeSSEEvent 一条解析后的上游 SSE 事件。
type traeSSEEvent struct {
	event string
	data  json.RawMessage
}

// traeSSEScanner 把原始字节流切分为 (event, data) 对。多行 data 按 SSE 规范以换行
// 拼接；未闭合的尾部事件在 EOF 时丢弃（对齐"没有 done 帧即截断"的语义）。
type traeSSEScanner struct {
	scanner *bufio.Scanner
	pending traeSSEEvent
	hasData bool
}

func newTraeSSEScanner(r io.Reader) *traeSSEScanner {
	scanner := bufio.NewScanner(r)
	// 单帧上限 4MB：Trae 的 token_usage/metadata 帧很小，但历史见过整段上下文
	// 塞进单帧的实现，留出余量避免误判截断。
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &traeSSEScanner{scanner: scanner}
}

// next 返回下一条事件；无更多事件返回 false。
func (s *traeSSEScanner) next() (*traeSSEEvent, bool) {
	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		switch {
		case line == "":
			// 空行为事件分隔符：派发已攒到的事件。
			if s.hasData {
				event := s.pending
				s.pending = traeSSEEvent{}
				s.hasData = false
				data := event.data
				return &traeSSEEvent{event: event.event, data: data}, true
			}
			s.pending = traeSSEEvent{}
		case strings.HasPrefix(line, "event:"):
			s.pending.event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(line[len("data:"):])
			if payload == "" {
				continue
			}
			if s.hasData {
				s.pending.data = append(append(s.pending.data, '\n'), []byte(payload)...)
				continue
			}
			s.pending.data = json.RawMessage(payload)
			s.hasData = true
		default:
			// 非 SSE 行（裸 JSON / 心跳注释）：裸 JSON 当作无事件名的 data 处理。
			if strings.HasPrefix(line, "{") && !s.hasData {
				s.pending.data = json.RawMessage(line)
				s.hasData = true
			}
		}
	}
	if s.hasData {
		event := s.pending
		s.hasData = false
		return &traeSSEEvent{event: event.event, data: event.data}, true
	}
	return nil, false
}

// traeStreamMeta 归一化所需的响应级元数据（id/model/created 在帧间续传）。
type traeStreamMeta struct {
	ID      string
	Model   string
	Created int64
}

// traeFrameState 单次流转换的累积状态。
type traeFrameState struct {
	meta       traeStreamMeta
	sawOutput  bool
	doneIssued bool
	// bizErr 上游 event:error 的原始载荷（HTTP 仍为 200）；非 nil 时收尾不得补 [DONE]。
	bizErr *traeStreamError
	usage  map[string]any
}

// normalizeTraeEvent 把一条上游事件翻译为零至多个 OpenAI CC chunk（map 形态）。
// 返回 error 仅用于 event:error 帧（上层据此向客户端下发错误）。
func (st *traeFrameState) normalizeTraeEvent(ev *traeSSEEvent) ([]map[string]any, error) {
	if ev == nil || len(ev.data) == 0 {
		return nil, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(ev.data, &obj); err != nil {
		return nil, nil // 非 JSON 帧：忽略（心跳/观测文本）
	}
	switch strings.ToLower(strings.TrimSpace(ev.event)) {
	case "error":
		streamErr := traeUpstreamErrorFromObject(obj)
		if typed, ok := streamErr.(*traeStreamError); ok {
			st.bizErr = typed
		}
		return nil, streamErr
	case "token_usage", "usage":
		if usage := traeExtractUsageObject(obj); usage != nil {
			st.usage = usage
		}
		return st.chunksWithUsage(obj), nil
	case "done", "complete", "finished":
		frames := st.chunksWithUsage(obj)
		st.doneIssued = true
		return frames, nil
	case "output", "message", "chunk", "":
		return st.chunksWithUsage(obj), nil
	default:
		// queue/progress/metadata/timing 等观测事件：丢弃但顺带保留 usage。
		if usage := traeExtractUsageObject(obj); usage != nil {
			st.usage = usage
		}
		return nil, nil
	}
}

// chunksWithUsage 从上游 output/done 帧构造 CC chunk：正文取 response|content，
// 思维链取 reasoning_content|reasoning，工具调用透传 delta，finish_reason 归一。
func (st *traeFrameState) chunksWithUsage(obj map[string]any) []map[string]any {
	content := traeFirstStringOrEmpty(obj, "response", "content", "text")
	reasoning := traeFirstStringOrEmpty(obj, "reasoning_content", "reasoning", "thinking")
	finishReason := strings.TrimSpace(traeFirstStringOrEmpty(obj, "finish_reason"))
	toolCalls := traeExtractToolCalls(obj)
	if usage := traeExtractUsageObject(obj); usage != nil {
		st.usage = usage
	}
	if st.meta.ID == "" {
		st.meta.ID = traeFirstStringOrEmpty(obj, "id", "message_id", "request_id")
	}
	if st.meta.Model == "" {
		st.meta.Model = traeFirstStringOrEmpty(obj, "model", "model_name", "config_name")
	}
	if st.meta.Created == 0 {
		st.meta.Created = time.Now().Unix()
	}
	if content == "" && reasoning == "" && finishReason == "" && len(toolCalls) == 0 && st.usage == nil {
		return nil
	}
	if content != "" || reasoning != "" || len(toolCalls) > 0 {
		st.sawOutput = true
	}
	delta := map[string]any{}
	if content != "" {
		delta["content"] = content
	}
	if reasoning != "" {
		delta["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		delta["tool_calls"] = toolCalls
	}
	chunk := map[string]any{
		"id":      traeChunkID(st.meta),
		"object":  "chat.completion.chunk",
		"created": st.meta.Created,
		"model":   traeChunkModel(st.meta),
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": traeFinishReasonOrNull(finishReason),
		}},
	}
	if st.usage != nil && (finishReason != "" || strings.EqualFold(traeEventNameOf(obj), "token_usage")) {
		chunk["usage"] = st.usage
	}
	return []map[string]any{chunk}
}

// traeEventNameOf 从已解析对象里取事件名（兼容部分通道把 event 名放进 data 的形态）。
func traeEventNameOf(obj map[string]any) string {
	if v, ok := obj["event"].(string); ok {
		return v
	}
	return ""
}

// traeChunkID id 缺失时回落到稳定占位（CC 客户端要求 id 非空）。
func traeChunkID(meta traeStreamMeta) string {
	if meta.ID != "" {
		return meta.ID
	}
	return "chatcmpl-trae-" + strconv.FormatInt(meta.Created, 10)
}

// traeChunkModel model 缺失时回落到 "trae"（与上游未声明模型时的既有占位一致）。
func traeChunkModel(meta traeStreamMeta) string {
	if meta.Model != "" {
		return meta.Model
	}
	return "trae"
}

// traeFinishReasonOrNull 空 finish_reason 输出 null（CC 规范要求增量帧为 null）。
func traeFinishReasonOrNull(reason string) any {
	if reason == "" {
		return nil
	}
	return reason
}

// traeFirstString 返回第一个存在且为非空字符串的键值。
func traeFirstString(obj map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if v, ok := obj[key].(string); ok && strings.TrimSpace(v) != "" {
			return v, true
		}
	}
	return nil, false
}

// traeFirstStringOrEmpty 取字符串键值（缺失/非字符串返回空串）。
func traeFirstStringOrEmpty(obj map[string]any, keys ...string) string {
	if v, ok := traeFirstString(obj, keys...); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// traeExtractToolCalls 提取帧内工具调用增量。上游两种形态都要兼容：
//   - tool_calls: [{index,id,type,function:{name,arguments}}]  （OpenAI 形态）
//   - function_call: {name,arguments}                          （Trae 首帧/续帧合并形态）
//
// 续帧只带 arguments（无 name），按 index 累加，故此处必须原样保留 index。
func traeExtractToolCalls(obj map[string]any) []any {
	if calls, ok := obj["tool_calls"].([]any); ok && len(calls) > 0 {
		out := make([]any, 0, len(calls))
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, normalizeTraeToolCallDelta(call))
		}
		if len(out) > 0 {
			return out
		}
	}
	if fc, ok := obj["function_call"].(map[string]any); ok {
		name, _ := fc["name"].(string)
		args, _ := fc["arguments"].(string)
		if strings.TrimSpace(name) == "" && strings.TrimSpace(args) == "" {
			return nil
		}
		return []any{map[string]any{
			"index": 0,
			"id":    traeFirstStringOrEmpty(obj, "id"),
			"type":  "function",
			"function": map[string]any{
				"name":      name,
				"arguments": args,
			},
		}}
	}
	return nil
}

// normalizeTraeToolCallDelta 补齐 tool_call 增量的 index/type 字段，并把上游的
// function_call 形态收敛为 OpenAI 的 function 字段。
func normalizeTraeToolCallDelta(call map[string]any) map[string]any {
	out := map[string]any{}
	if fn, ok := call["function"].(map[string]any); ok {
		out["function"] = fn
	} else if fc, ok := call["function_call"].(map[string]any); ok {
		out["function"] = fc
	}
	if idx, ok := call["index"]; ok {
		out["index"] = idx
	} else {
		out["index"] = 0
	}
	if id, ok := call["id"].(string); ok && strings.TrimSpace(id) != "" {
		out["id"] = id
	}
	if typ, ok := call["type"].(string); ok && strings.TrimSpace(typ) != "" {
		out["type"] = typ
	} else {
		out["type"] = "function"
	}
	return out
}

// traeExtractUsageObject 从任意帧里取 usage 对象（上游放在 token_usage 帧或
// output 帧的 usage / token_usage 字段）。
func traeExtractUsageObject(obj map[string]any) map[string]any {
	for _, key := range []string{"usage", "token_usage"} {
		if usage, ok := obj[key].(map[string]any); ok && len(usage) > 0 {
			return traeEnsureUsageTotal(usage)
		}
	}
	// 事件本身就是 usage 帧：data 顶层即计数字段。
	if traeLooksLikeUsage(obj) {
		return traeEnsureUsageTotal(obj)
	}
	return nil
}

// traeLooksLikeUsage 报告对象是否形如 usage（含 completion_tokens/prompt_tokens）。
func traeLooksLikeUsage(obj map[string]any) bool {
	if len(obj) == 0 {
		return false
	}
	for _, key := range []string{"completion_tokens", "prompt_tokens", "total_tokens"} {
		if _, ok := obj[key]; ok {
			return true
		}
	}
	return false
}

// traeEnsureUsageTotal 补全 total_tokens（上游常只给 prompt/completion）。
func traeEnsureUsageTotal(usage map[string]any) map[string]any {
	out := make(map[string]any, len(usage)+1)
	for k, v := range usage {
		out[k] = v
	}
	if _, ok := out["total_tokens"]; !ok {
		total := traeJSONInt(usage["prompt_tokens"]) + traeJSONInt(usage["completion_tokens"])
		if total > 0 {
			out["total_tokens"] = total
		}
	}
	return out
}

// traeJSONInt 宽松取整数（float64/int/json.Number/数字字符串）。
func traeJSONInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
		if f, err := n.Float64(); err == nil {
			return int64(f)
		}
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
			return i
		}
	}
	return 0
}

// traeUpstreamErrorFromObject event:error 帧 → 带业务码的错误对象。
func traeUpstreamErrorFromObject(obj map[string]any) error {
	message := traeFirstStringOrEmpty(obj, "message", "msg", "error_message")
	code := traeFirstStringOrEmpty(obj, "code", "error_code")
	if code == "" {
		if n := traeJSONInt(obj["code"]); n != 0 {
			code = strconv.FormatInt(n, 10)
		}
	}
	if message == "" {
		message = "trae upstream returned an error"
	}
	return &traeStreamError{Code: code, Message: message}
}

// traeStreamError 流内业务错误（HTTP 200 但 event:error）。code 供上层判定
// 是否可 failover（1005=套餐额度耗尽、1001=令牌失效）。
type traeStreamError struct {
	Code    string
	Message string
}

func (e *traeStreamError) Error() string {
	if e == nil {
		return "trae stream error"
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Message + " (code=" + e.Code + ")"
}

// traeSSEBusinessErrorHook 是流式路径业务错误帧的观察回调（code/message 为
// 上游 event:error 的原始值，HTTP 仍是 200）。
//
// 用途：把「流内失败」落回账号状态（限额→临时不可调度、会话失效→标记错误），
// 口径与 qoder_sse.go 的同名机制一致。回调可能在下游 Read() 调用栈内同步触发，
// 实现方必须自行收敛 ctx 并保证幂等（上游重发错误帧时会多次触发）。
type traeSSEBusinessErrorHook func(code, message string)

// traeNewSSEReader 把上游 Trae SSE 包装为规范化 OpenAI CC SSE 流（io.Reader）。
func traeNewSSEReader(r io.Reader, meta traeStreamMeta) io.Reader {
	return traeNewSSEReaderWithErrorHook(r, meta, nil)
}

// traeNewSSEReaderWithErrorHook 同 traeNewSSEReader，额外注册业务错误帧观察回调。
func traeNewSSEReaderWithErrorHook(r io.Reader, meta traeStreamMeta, hook traeSSEBusinessErrorHook) io.Reader {
	if r == nil {
		r = bytes.NewReader(nil)
	}
	return &traeSSEReader{scanner: newTraeSSEScanner(r), state: &traeFrameState{meta: meta}, onBizErr: hook}
}

type traeSSEReader struct {
	scanner  *traeSSEScanner
	state    *traeFrameState
	onBizErr traeSSEBusinessErrorHook
	pending  bytes.Buffer // 已产出、待消费的规范化帧字节
	finished bool
	errSeen  bool // 错误帧已上报，避免同一错误重入回调
}

func (rd *traeSSEReader) Read(p []byte) (int, error) {
	for rd.pending.Len() == 0 && !rd.finished {
		ev, ok := rd.scanner.next()
		if !ok {
			rd.finish()
			if rd.pending.Len() == 0 {
				return 0, io.EOF
			}
			break
		}
		frames, err := rd.state.normalizeTraeEvent(ev)
		if err != nil {
			// 错误帧统一由 finish() 产出（那里能同时决定是否补 [DONE]），此处只上报不写，
			// 否则会与 finish() 重复写出一条语义相同的 error。
			rd.reportBusinessError(err)
			rd.finish()
			break
		}
		for _, frame := range frames {
			if raw, merr := json.Marshal(frame); merr == nil {
				rd.writeFrame(string(raw))
			}
		}
	}
	return rd.pending.Read(p)
}

// reportBusinessError 向观察者上报一次流内业务错误（同一错误只报一次）。
func (rd *traeSSEReader) reportBusinessError(err error) {
	if rd.onBizErr == nil || rd.errSeen {
		return
	}
	rd.errSeen = true
	var typed *traeStreamError
	if errors.As(err, &typed) {
		rd.onBizErr(typed.Code, typed.Message)
		return
	}
	rd.onBizErr("", err.Error())
}

func (rd *traeSSEReader) writeFrame(payload string) {
	rd.pending.WriteString("data: ")
	rd.pending.WriteString(payload)
	rd.pending.WriteString("\n\n")
}

// finish 收尾：补 finish_reason 帧与 [DONE]；一个有效帧都没给出时下发错误帧。
//
// 上游给出流内 error 时**不补 [DONE]**：否则「HTTP 200 + 错误帧」会被上层的截断
// 检测判为正常结束（见 openai_raw_stream_truncation.go 认 [DONE] 即 sawDone），
// 失败会被记成成功、按 0 token 出账，账号也不换号不落状态。
func (rd *traeSSEReader) finish() {
	if rd.finished {
		return
	}
	rd.finished = true
	if rd.state.bizErr != nil {
		// 流内业务错误（含「已有正文后出错」与「首帧即错」两种）：只下发错误帧，
		// **不补 [DONE]**，上层据此按截断/失败处理并可换号。
		rd.writeFrame(traeCCErrorFrame(rd.state.bizErr))
		return
	}
	if rd.state.validFrames() == 0 {
		rd.writeFrame(traeEmptyStreamErrorFrame)
		return
	}
	if !rd.state.doneIssued && rd.state.sawOutput {
		frame := map[string]any{
			"id":      traeChunkID(rd.state.meta),
			"object":  "chat.completion.chunk",
			"created": rd.state.meta.Created,
			"model":   traeChunkModel(rd.state.meta),
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			}},
		}
		if rd.state.usage != nil {
			frame["usage"] = rd.state.usage
		}
		if raw, err := json.Marshal(frame); err == nil {
			rd.writeFrame(string(raw))
		}
	}
	rd.pending.WriteString("data: [DONE]\n\n")
}

// validFrames 报告是否产出过任何有效内容帧。
func (st *traeFrameState) validFrames() int {
	if st.sawOutput || st.doneIssued || st.usage != nil {
		return 1
	}
	return 0
}

// traeCCErrorFrame 把内部错误转成 CC 规范的错误 chunk。
func traeCCErrorFrame(err error) string {
	code := ""
	var se *traeStreamError
	if errors.As(err, &se) {
		code = se.Code
	}
	payload := map[string]any{
		"error": map[string]any{
			"message": err.Error(),
			"type":    "upstream_error",
			"code":    traeOr(code, "upstream_error"),
		},
	}
	raw, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return traeEmptyStreamErrorFrame
	}
	return string(raw)
}

func traeOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// traeAggregateSSE 读取上游全流并聚合为单个 OpenAI CC JSON 响应（非流式入站用）。
// 上游只有 SSE，故非流式必须由本函数合成，否则会把 SSE 文本当 JSON 回给客户端。
func traeAggregateSSE(r io.Reader, meta traeStreamMeta) ([]byte, error) {
	if r == nil {
		return nil, errTraeEmptyStream
	}
	scanner := newTraeSSEScanner(r)
	state := &traeFrameState{meta: meta}
	var content strings.Builder
	var reasoning strings.Builder
	toolCalls := make(map[int]map[string]any)
	toolOrder := make([]int, 0, 4)
	finishReason := ""
	var streamErr error

	for {
		ev, ok := scanner.next()
		if !ok {
			break
		}
		if ev.data == nil {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(ev.data, &obj); err != nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(ev.event))
		if name == "error" {
			streamErr = traeUpstreamErrorFromObject(obj)
			break
		}
		if usage := traeExtractUsageObject(obj); usage != nil {
			state.usage = usage
		}
		if name != "" && name != "output" && name != "message" && name != "chunk" && name != "done" && name != "token_usage" && name != "usage" {
			continue
		}
		if text := traeFirstStringOrEmpty(obj, "response", "content", "text"); text != "" {
			content.WriteString(text)
			state.sawOutput = true
		}
		if chain := traeFirstStringOrEmpty(obj, "reasoning_content", "reasoning", "thinking"); chain != "" {
			reasoning.WriteString(chain)
			state.sawOutput = true
		}
		for _, raw := range traeExtractToolCalls(obj) {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			idx := int(traeJSONInt(call["index"]))
			existing, seen := toolCalls[idx]
			if !seen {
				toolOrder = append(toolOrder, idx)
				existing = map[string]any{
					"index":    idx,
					"id":       call["id"],
					"type":     "function",
					"function": map[string]any{"name": "", "arguments": ""},
				}
				toolCalls[idx] = existing
			}
			if id, ok := call["id"].(string); ok && strings.TrimSpace(id) != "" {
				existing["id"] = id
			}
			fn, _ := existing["function"].(map[string]any)
			if delta, ok := call["function"].(map[string]any); ok {
				if name, ok := delta["name"].(string); ok && name != "" {
					fn["name"] = name
				}
				if args, ok := delta["arguments"].(string); ok && args != "" {
					fn["arguments"] = traeJSONInt0String(fn["arguments"]) + args
				}
			}
			state.sawOutput = true
		}
		if reason := traeFirstStringOrEmpty(obj, "finish_reason"); reason != "" {
			finishReason = reason
			state.doneIssued = true
		}
	}
	if streamErr != nil {
		return nil, streamErr
	}
	if !state.sawOutput && state.usage == nil {
		return nil, errTraeEmptyStream
	}
	if finishReason == "" {
		finishReason = "stop"
	}
	if len(toolOrder) > 0 && (finishReason == "stop" || finishReason == "") {
		finishReason = "tool_calls"
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		calls := make([]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	if message["content"] == "" && len(toolOrder) == 0 {
		message["content"] = nil
	}
	response := map[string]any{
		"id":      traeChunkID(state.meta),
		"object":  "chat.completion",
		"created": traeOrZeroInt64(state.meta.Created, time.Now().Unix()),
		"model":   traeChunkModel(state.meta),
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if state.usage != nil {
		response["usage"] = state.usage
	}
	raw, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// traeJSONInt0String 取字符串值（缺失按空串，用于 arguments 增量累加）。
func traeJSONInt0String(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// traeOrZeroInt64 零值兜底。
func traeOrZeroInt64(value, fallback int64) int64 {
	if value == 0 {
		return fallback
	}
	return value
}
