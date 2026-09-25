package service

// qoder_sse.go Qoder 上游 SSE 流的解析、规范化与聚合。
//
// 上游帧形态（与 OpenAI 标准流不同）：
//   data:{"headers":{...},"body":"<内层 JSON 字符串>","statusCodeValue":200}
//
//   - statusCodeValue != 200 → 信封错误（body 字符串为详情），转结构化错误帧；
//   - 内层 JSON 为 OpenAI chunk 形态：choices[0].delta.{role,content,
//     reasoning_content,tool_calls}；usage{prompt_tokens,completion_tokens} 可能在
//     最后帧与 choices 同帧（需合并进该帧后再透传）；
//   - 业务错误形态 {"code":"115",...}：code 非 "0" 且非空 → 错误帧；
//   - data: [DONE] 结束；空流（0 有效帧）→ 显式错误。
//
// 两种消费形态与 workbuddy_sse.go 对齐：
//   - 客户端要流式：newQoderSSEReader 包装为规范化标准 CC SSE 流；
//   - 客户端要非流式：aggregateQoderSSE 读取全流聚合为单个 CC JSON。
//
// 响应模型回写（sentModel）：上游对任何请求都恒报 model:"auto"（协议占位，
// 实测发 qfmodel 也回 auto），而客户端按响应 model 判定"实际用了哪个模型"。
// 两种消费形态都把出站官方 key 回写进响应，口径一致；sentModel 为空时退回
// 既有占位行为（缺省 "qoder" / 透传上游值）。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// errQoderEmptyStream 上游返回 200 但没有有效 SSE 数据帧（空流/只有注释/[DONE]）。
var errQoderEmptyStream = errors.New("qoder upstream stream contained no valid data events")

// errQoderUpstreamEnvelope 信封级错误（statusCodeValue != 200 / 业务 code 非 0）。
// 结构化保留上游 code 与详情，由调用方做 failover 判定与错误透传。
type errQoderUpstreamEnvelope struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *errQoderUpstreamEnvelope) Error() string {
	return fmt.Sprintf("qoder upstream error: HTTP %d code=%s msg=%s", e.StatusCode, e.Code, workbuddyTruncateForError(e.Message))
}

// isQoderUpstreamEnvelopeError 报告错误是否为上游信封错误。
func isQoderUpstreamEnvelopeError(err error) bool {
	var env *errQoderUpstreamEnvelope
	return errors.As(err, &env)
}

// qoderEnvelope 信封帧解析形态。
type qoderEnvelope struct {
	Headers         json.RawMessage `json:"headers"`
	Body            string          `json:"body"`
	StatusCodeValue int             `json:"statusCodeValue"`
}

// qoderSSEData 返回数据帧的归一化中间形态。
type qoderSSEData struct {
	Envelope    *qoderEnvelope  // 信封（恒非 nil）
	Inner       json.RawMessage // 内层 JSON（body 反序列化后的原始字节）
	InnerObject map[string]any  // 内层对象（choices/usage/code 解析用）
	BusinessErr *qoderBizError  // 业务错误（code 非 "0" 非空）
}

// qoderBizError 上游业务错误（{"code":"115",...} 形态）。
type qoderBizError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail"`
}

// qoderInnerBusinessCode 从内层对象提取业务错误码。上游同时存在字符串型
// （{"code":"110"}）与数字型（{"code":110}）两种 JSON 形态，二者语义等价，
// 必须都识别——只认字符串会让数字型错误帧被当成正常内容帧透传，既不转 CC
// error 帧也不触发落状态回调（账号被限额却仍显示「正常」的原始故障形态之一）。
// 返回 "" 表示无码或 code "0"（非业务错误）。json.Number 分支覆盖调用方
// 启用 UseNumber() 的解码器。
func qoderInnerBusinessCode(obj map[string]any) string {
	switch v := obj["code"].(type) {
	case string:
		if c := strings.TrimSpace(v); c != "" && c != "0" {
			return c
		}
	case float64:
		if c := strconv.FormatInt(int64(v), 10); c != "0" {
			return c
		}
	case json.Number:
		if c := strings.TrimSpace(v.String()); c != "" && c != "0" {
			return c
		}
	}
	return ""
}

// qoderParseDataFrame 解析一帧上游 data 载荷：信封 → 内层 JSON → 业务错误判定。
// 解析失败的帧返回 nil data（调用方按无效帧计数处理）。
func qoderParseDataFrame(payload string) *qoderSSEData {
	var env qoderEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		return nil
	}
	data := &qoderSSEData{Envelope: &env}
	if env.StatusCodeValue != 0 && env.StatusCodeValue != 200 {
		data.BusinessErr = &qoderBizError{
			Code:    fmt.Sprintf("%d", env.StatusCodeValue),
			Message: env.Body,
		}
		return data
	}
	inner := strings.TrimSpace(env.Body)
	if inner == "" {
		return data
	}
	if err := json.Unmarshal([]byte(inner), &data.InnerObject); err != nil {
		// 内层非 JSON：保留原始字节供错误详情透传。
		data.Inner = json.RawMessage(inner)
		return data
	}
	data.Inner, _ = json.Marshal(data.InnerObject)
	// 业务错误：{"code":"115",...} / {"code":115,...} —— code 非 "0" 且非空即错误。
	if code := qoderInnerBusinessCode(data.InnerObject); code != "" {
		msg, _ := data.InnerObject["message"].(string)
		detail, _ := data.InnerObject["detail"].(string)
		biz := &qoderBizError{Code: code, Message: msg, Detail: detail}
		if biz.Message == "" {
			// 某些错误形态只带 message/detail 其一；退化为整对象文本。
			if raw, err := json.Marshal(data.InnerObject); err == nil {
				biz.Message = string(raw)
			}
		}
		data.BusinessErr = biz
	}
	return data
}

// qoderResponseModel 计算回写给客户端的响应 model：仅当实际出站模型为 Qoder
// 官方 key 时用它覆盖上游占位值（上游对任何请求都恒报 model:"auto"，而客户端
// 按响应 model 判定"实际用了哪个模型"）。发非官方 key 时不改写，保留上游
// "auto" 声明，使「模型不一致」审计仍能发现历史坏配置（与
// upstreamModelMismatchForAccount 的不豁免口径一致）。sentModel 为空且上游也无
// 声明时回落占位 "qoder"。
func qoderResponseModel(sentModel string, upstreamDeclared any) string {
	if sent := strings.ToLower(strings.TrimSpace(sentModel)); isQoderModelCode(sent) {
		return sent
	}
	if declared, ok := upstreamDeclared.(string); ok && strings.TrimSpace(declared) != "" {
		return declared
	}
	return "qoder"
}

// qoderFrameToCCChunk 把一帧内层 OpenAI chunk 归一化为标准 CC 流式帧（顶层补
// id/object/created/model、usage 同帧合并、错误帧原样透传）。
// sentModel 为本请求实际出站模型 key，非空时覆盖上游恒为 "auto" 的占位声明。
func qoderFrameToCCChunk(inner map[string]any, firstID string, sentModel string) map[string]any {
	out := make(map[string]any, len(inner)+4)
	for k, v := range inner {
		out[k] = v
	}
	if _, ok := out["id"].(string); !ok || out["id"] == "" {
		out["id"] = firstID
	}
	if _, ok := out["object"]; !ok {
		out["object"] = "chat.completion.chunk"
	}
	if _, ok := out["created"]; !ok {
		out["created"] = time.Now().Unix()
	}
	if model := qoderResponseModel(sentModel, out["model"]); model != "" {
		out["model"] = model
	}
	// choices 缺失/非数组时不强造（usage-only 尾帧合法）。
	if chs, ok := out["choices"].([]any); ok {
		nchs := make([]any, 0, len(chs))
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			nc := map[string]any{}
			if idx, has := c["index"]; has {
				nc["index"] = idx
			}
			delta := map[string]any{}
			if d, ok := c["delta"].(map[string]any); ok {
				for _, key := range []string{"role", "content", "reasoning_content", "refusal", "tool_calls"} {
					if v, has := d[key]; has && v != nil {
						delta[key] = v
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
	return out
}

// newQoderSSEReader 包装上游 SSE 为「规范化后的标准 CC SSE 流」：
//   - 信封解析、内层 delta 重建、usage 同帧合并、错误帧转 CC error 形态；
//   - 保证恰好一个 `data: [DONE]`；空流补一帧 error 再 [DONE]。
//
// 实现为拉取式（无后台 goroutine），风格与 workbuddySSEReader 一致。
func newQoderSSEReader(r io.Reader, sentModel string) io.Reader {
	return newQoderSSEReaderWithErrorHook(r, sentModel, nil)
}

// qoderSSEBusinessErrorHook 是流式路径业务错误帧的观察回调（code/message 为
// 上游原始业务错误）。回调在归一化帧写入前调用，供网关落账号状态（额度类
// 错误自动避让等）；不得阻断/修改帧内容本身。
//
// 上游可能只发一个错误帧后直接关闭（无 [DONE]）：回调可能被触发一次或
// 多次（帧重发防御），落状态操作需幂等（SetTempUnschedulable/SetError 均为
// 幂等 upsert，见仓库实现）。
type qoderSSEBusinessErrorHook func(code, message string)

// newQoderSSEReaderWithErrorHook 同 newQoderSSEReader，额外注册业务错误帧观察
// 回调（nil 时行为与旧版完全一致：仅透传 CC error 帧，不落状态）。
func newQoderSSEReaderWithErrorHook(r io.Reader, sentModel string, hook qoderSSEBusinessErrorHook) io.Reader {
	if r == nil {
		r = strings.NewReader("")
	}
	return &qoderSSEReader{
		br:        bufio.NewReaderSize(r, 64*1024),
		sentModel: strings.TrimSpace(sentModel),
		onBizErr:  hook,
	}
}

// qoderSSEReader 拉取式规范化 SSE 读取器。
type qoderSSEReader struct {
	br          *bufio.Reader
	pending     bytes.Buffer
	firstID     string
	sentModel   string
	validFrames int
	finished    bool
	// onBizErr 业务错误帧观察回调（可选；见 qoderSSEBusinessErrorHook）。
	onBizErr qoderSSEBusinessErrorHook
}

// Read 实现 io.Reader。
func (r *qoderSSEReader) Read(p []byte) (int, error) {
	for r.pending.Len() == 0 && !r.finished {
		r.pump()
	}
	if r.pending.Len() > 0 {
		return r.pending.Read(p)
	}
	return 0, io.EOF
}

// pump 读一行上游输入并产出规范化帧。
func (r *qoderSSEReader) pump() {
	line, err := r.br.ReadString('\n')
	trimmed := strings.TrimRight(line, "\r\n")
	if payload, ok := extractOpenAISSEDataLine(trimmed); ok {
		payload = strings.TrimSpace(payload)
		switch {
		case payload == "[DONE]":
			r.finish()
			return
		case payload != "":
			r.writeFrame(payload)
		}
	}
	if err != nil {
		if err != io.EOF {
			logger.L().Warn("qoder sse normalizer: read upstream stream error", zap.Error(err))
		}
		r.finish()
	}
}

// writeFrame 解析一帧并写入规范化输出。
func (r *qoderSSEReader) writeFrame(payload string) {
	data := qoderParseDataFrame(payload)
	if data == nil {
		// 非 JSON 帧：原样透传（不二次错误化），但不计入有效帧。
		r.pending.WriteString("data: " + payload + "\n\n")
		return
	}
	r.validFrames++
	if data.BusinessErr != nil {
		// 信封/业务错误 → 先通知观察回调（账号状态处置），再转 CC error 帧
		// （客户端可识别形态）。回调失败不得影响帧透传。
		if r.onBizErr != nil {
			r.onBizErr(data.BusinessErr.Code, data.BusinessErr.Message)
		}
		errFrame := map[string]any{
			"error": map[string]any{
				"message": data.BusinessErr.Message,
				"type":    "upstream_error",
				"code":    data.BusinessErr.Code,
			},
		}
		raw, _ := json.Marshal(errFrame)
		r.pending.WriteString("data: " + string(raw) + "\n\n")
		return
	}
	if data.InnerObject == nil {
		return // 无内层载荷的 200 信封：无内容可透传
	}
	if r.firstID == "" {
		if v, ok := data.InnerObject["id"].(string); ok && v != "" {
			r.firstID = v
		}
	}
	chunk := qoderFrameToCCChunk(data.InnerObject, r.firstID, r.sentModel)
	raw, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	r.pending.WriteString("data: " + string(raw) + "\n\n")
}

// finish 终止产出：空流先补一帧 error（warn），再保证恰好一个 [DONE]。
func (r *qoderSSEReader) finish() {
	if r.finished {
		return
	}
	if r.validFrames == 0 {
		logger.L().Warn("qoder sse normalizer: upstream stream contained no valid data events")
		r.pending.WriteString("data: " + workbuddyEmptyStreamErrorFrame + "\n\n")
	}
	r.pending.WriteString("data: [DONE]\n\n")
	r.finished = true
}

// aggregateQoderSSE 读取完整上游流，聚合 delta.content/reasoning_content/tool_calls
// 为单个 OpenAI chat.completion 响应，返回 (JSON 字节, usage 指针, err)。
// usage 与 choices 同帧到达时同样合并；空流返回 errQoderEmptyStream；
// 信封/业务错误帧中断聚合并返回结构化信封错误。
func aggregateQoderSSE(r io.Reader, sentModel string) ([]byte, *OpenAIUsage, error) {
	if r == nil {
		return nil, nil, errQoderEmptyStream
	}
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model    string
		created      float64
		content      strings.Builder
		reasoning    strings.Builder
		role         = "assistant"
		finishReason = "stop"
		usage        map[string]any
		validEvents  int
		sawDone      bool
		toolCalls    = map[int]map[string]any{}
		toolOrder    []int
		toolSeq      int
		idIndex      = map[string]int{}
	)
	appendContent := func(txt string) {
		if txt == "" {
			return
		}
		content.WriteString(txt)
	}
	nextToolIndex := func() int {
		for {
			idx := toolSeq
			toolSeq++
			if _, used := toolCalls[idx]; !used {
				return idx
			}
		}
	}
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
					idx = mid
				} else {
					idx = nextToolIndex()
				}
			} else if len(toolOrder) > 0 {
				idx = toolOrder[len(toolOrder)-1]
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
	handleInner := func(inner map[string]any) error {
		// 业务错误优先：code 非 "0" 非空即中断（字符串/数字两种形态同口径）。
		if code := qoderInnerBusinessCode(inner); code != "" {
			msg, _ := inner["message"].(string)
			return &errQoderUpstreamEnvelope{StatusCode: 200, Code: code, Message: msg}
		}
		if v, ok := inner["id"].(string); ok && id == "" && v != "" {
			id = v
		}
		if v, ok := inner["model"].(string); ok && model == "" && v != "" {
			model = v
		}
		if v, ok := inner["created"].(float64); ok && created == 0 {
			created = v
		}
		if u, ok := inner["usage"].(map[string]any); ok {
			usage = u
		}
		chs, _ := inner["choices"].([]any)
		for _, ci := range chs {
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
		}
		return nil
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
				sawDone = true
				break
			}
			if payload != "" {
				if data := qoderParseDataFrame(payload); data != nil {
					validEvents++
					if data.BusinessErr != nil {
						return nil, nil, &errQoderUpstreamEnvelope{
							StatusCode: data.Envelope.StatusCodeValue,
							Code:       data.BusinessErr.Code,
							Message:    data.BusinessErr.Message,
						}
					}
					if data.InnerObject != nil {
						if hErr := handleInner(data.InnerObject); hErr != nil {
							return nil, nil, hErr
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
		return nil, nil, errQoderEmptyStream
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	if model = qoderResponseModel(sentModel, model); model == "" {
		model = "qoder"
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts := func(arr []int) {
			for i := 1; i < len(arr); i++ {
				for j := i; j > 0 && arr[j] < arr[j-1]; j-- {
					arr[j], arr[j-1] = arr[j-1], arr[j]
				}
			}
		}
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
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
		resp["usage"] = ensureWorkbuddyUsageTotal(usage)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal qoder aggregated response: %w", err)
	}
	var usagePtr *OpenAIUsage
	if parsed, ok := extractOpenAIUsageFromJSONBytes(out); ok {
		usagePtr = &parsed
	}
	return out, usagePtr, nil
}

// qoderIsEmptyStreamError 报告错误是否为「上游空流」。
func qoderIsEmptyStreamError(err error) bool {
	return errors.Is(err, errQoderEmptyStream)
}
