package service

// workbuddy_payload.go WorkBuddy 出站请求体变换管线
// （移植自 workbuddy2api/internal/upstream/payload.go + tool_pairing.go + thinking.go +
// cache_key.go + sanitize.go，合并为单 pass 实现）。
//
// 管线顺序（与上游实现保持一致的语义顺序）：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. max_completion_tokens → max_tokens 翻译（上游只认 max_tokens）
//  3. stream_options.include_usage=true 补全（上游据此在末帧返回 usage）
//  4. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101）
//  5. developer → system 角色归一（上游 role 白名单不含 developer，命中即 400 code=11128）
//  6. tool 配对修复（先 repack 再 cleanup，缺配对会让上游对之后每条消息返 400）
//  7. deepseek 思维链注入（thinking.type=enabled + 默认档 effort + 多轮 reasoning_content 回填）
//  8. prompt_cache_key 注入（按账号隔离的稳定前缀缓存键，费用降 ~17×）
//  9. global realm 的 ensureConsoleSystem（首条非 system 时前置兜底 system）
//  10. 指纹脱敏 sanitize（剥离上游内容审核黑名单模板句，可开关）
//
// 每个子步骤都对不合规输入保持宽容：无法解析的 body 原样返回、别名 0/负值不翻译、
// 非法 tool_choice 删除、未知模型零改动，绝不把坏 body 二次错误化。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
)

// PrepareWorkbuddyBody 改写发往 WorkBuddy 上游的 chat 请求体，默认开启指纹脱敏
// （与 workbuddy2api 默认 SanitizeFingerprints=true 对齐）。
// creds 提供 realm（global 走 ensureConsoleSystem）与 uid（prompt_cache_key 隔离段）。
func PrepareWorkbuddyBody(body []byte, creds WorkbuddyCredentials) []byte {
	return PrepareWorkbuddyBodyOpt(body, creds, true)
}

// PrepareWorkbuddyBodyOpt 单 pass 改写；sanitize=false 时行为还原为「仅协议兼容变换」
// （强制 stream + 字段翻译/归一 + tool 配对 + thinking + cache key + console system），
// 不做指纹脱敏。保留开关函数签名以对齐上游三层封装的语义锚点。
func PrepareWorkbuddyBodyOpt(src []byte, creds WorkbuddyCredentials, sanitize bool) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	// 1) 强制 stream:true（上游拒绝非流式）。
	obj["stream"] = true
	// 2) max_completion_tokens → max_tokens 翻译。
	translateWorkbuddyMaxCompletionTokens(obj)
	// 3) stream_options 仅当 body 未显式带时补 {include_usage: true}（显式带则不覆盖）。
	if _, has := obj["stream_options"]; !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	// 4) tool_choice 归一化；5) developer → system。
	normalizeWorkbuddyToolChoice(obj)
	normalizeWorkbuddyRoles(obj)
	// 6) tool 配对两步：先重排再清理（所有模型一律执行，独立于 sanitize 开关——
	// 这是「让请求通过」的安全网）。无改动时两步返回原 slice，回写等于零操作；
	// 任一步重排/删除都必须落到 obj（不能只在「最后一步改动」时回写）。
	if msgs, ok := obj["messages"].([]any); ok {
		msgs, _ = repackWorkbuddyToolResultBlocks(msgs)
		msgs, _ = cleanupWorkbuddyOrphanToolCalls(msgs)
		obj["messages"] = msgs
	}
	// 7) DeepSeek 思维链：注入 thinking.type=enabled + 缺档补默认档，再做多轮
	// reasoning_content 回填。sub2api 暂无模型目录缓存：默认档回落硬编码 "high"，
	// 支持档未知 → normalizeWorkbuddyReasoningEffort 透传（不降级）。
	injectWorkbuddyThinking(obj, "")
	normalizeWorkbuddyReasoningEffort(obj, nil)
	backfillWorkbuddyReasoningContent(obj)
	// 8) prompt_cache_key 注入（按账号隔离，uid 必须参与）。
	injectWorkbuddyPromptCacheKey(obj, creds)
	// 9) global realm 兜底 system 注入（防 console 域上游 code 11-128）。
	if workbuddyRealmIsGlobal(creds.Realm) {
		ensureWorkbuddyConsoleSystem(obj)
	}
	// 10) 指纹脱敏（可开关；协议兼容变换与脱敏解耦）。
	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeWorkbuddyMessages(msgs)
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// workbuddyRealmIsGlobal 报告归一化 realm 是否为 global（大小写/空白宽容）。
func workbuddyRealmIsGlobal(realm string) bool {
	return strings.EqualFold(strings.TrimSpace(realm), "global")
}

// translateWorkbuddyMaxCompletionTokens 把 OpenAI 别名 max_completion_tokens 翻译为
// 上游认的 max_tokens。
// 规则（对齐上游语义）：
//   - 显式 max_tokens 存在 → 原样保留（显式优先，别名只删不译）；
//   - 别名值为 0/null/负数/非数值 → 不翻译（0/null 语义是「未设置」；
//     负数是非法值，翻译等于把垃圾搬进 max_tokens）；
//   - 无论翻译与否，别名一律删除（上游 Go struct 宽松但留着徒增噪音）。
func translateWorkbuddyMaxCompletionTokens(obj map[string]any) {
	alias, has := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens")
	if !has {
		return
	}
	if _, explicit := obj["max_tokens"]; explicit {
		return // 显式 max_tokens 优先：别名只删不译
	}
	// json.Unmarshal 数字 → float64（整数去整后回写，避免科学计数法/小数尾巴进上游 body）；
	// 其他数值类型防御性兼容（手构造 map 的调用方）。
	switch v := alias.(type) {
	case float64:
		if v > 0 && v == float64(int64(v)) {
			obj["max_tokens"] = int64(v)
		}
	case int64:
		if v > 0 {
			obj["max_tokens"] = v
		}
	case int:
		if v > 0 {
			obj["max_tokens"] = int64(v)
		}
	}
}

// workbuddyEffortRank 档位从低到高。
var workbuddyEffortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// normalizeWorkbuddyReasoningEffort 按模型 supportedEfforts 降级 reasoning_effort
// （snake/camel 双字段兼容）。
//   - 请求档位模型支持 → 原样透传
//   - 请求档位不支持 → 改为 ≤请求档位的最高支持档（降级）
//   - 支持档全部高于请求档 → 取最低支持档（偏离最小）
//   - 未知模型/未知档位/未携带字段/efforts 为 nil → 一律透传
//
// sub2api 暂无模型目录缓存：调用方传 nil（透传不降级），保留完整实现供后续接入。
func normalizeWorkbuddyReasoningEffort(obj map[string]any, efforts map[string][]string) {
	if len(efforts) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := workbuddyEffortRank[reqStr]
	if !known {
		return
	}
	// 在 ≤请求档位的支持档里选最高档；命中且与请求不同才改写。
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := workbuddyEffortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if !strings.EqualFold(best, reqStr) {
			obj[key] = best
			slog.Warn("workbuddy reasoning_effort downgraded", "model", model, "from", reqStr, "to", best)
		}
		return
	}
	// 支持档全部高于请求档：取最低支持档。
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := workbuddyEffortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest != "" {
		obj[key] = lowest
		slog.Warn("workbuddy reasoning_effort floored", "model", model, "from", reqStr, "to", lowest)
	}
}

// normalizeWorkbuddyRoles 把 messages 里的 developer 角色归一为 system。
// 上游对 messages 的 role 字段做白名单校验，developer 不在白名单内，命中即
// HTTP 400 code=11128；developer 是 OpenAI 新规范里 system 的别名（Codex/Cursor
// 等新客户端用它承载 system 级指令），改写不丢语义。
// 此归一化是「协议兼容」（补上游 role 白名单），与 sanitize 开关有意解耦：
// 即使 sanitize=false 也照常归一。只认 developer 一个值，其余 role 原样保留。
func normalizeWorkbuddyRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			slog.Debug("workbuddy role normalized developer->system", "index", i)
		}
	}
}

// ensureWorkbuddyConsoleSystem global realm 兜底 system 注入：首条消息非 system 时
// 在 messages 最前补一条 fallback system（防 console 域上游 code 11-128）。
// 仅对 global 请求调用（CN 现状不动；首条已是 system 也不重复注入）。
// body 无 messages / messages 为空时零改动（与坏 body 宽容语义一致）。
func ensureWorkbuddyConsoleSystem(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	if first, ok := msgs[0].(map[string]any); ok {
		if role, _ := first["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
			return // 首条已是 system：不注入
		}
	}
	obj["messages"] = append([]any{map[string]any{"role": "system", "content": "You are a helpful assistant."}}, msgs...)
}

// normalizeWorkbuddyToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none"            → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}   → 同上
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeWorkbuddyToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// repackWorkbuddyToolResultBlocks 把插在 assistant.tool_calls 与其 tool 结果之间的
// 非 tool 消息挪到整组之后，保证同一批 tool_call 的结果在 wire 上连续。
//
// 背景：Codex 的 image_resize_notice 特性会把通知消息插在并行工具结果中间，
// OpenAI 兼容协议要求 tool 结果紧跟 assistant，中间插任何消息都算配对断裂，
// 上游判 11148（tool_call_sequence_broken）并顶死整条会话。这里只调顺序、不改内容。
// 结果顺序保持不变（同批 tool_call 的原相对顺序 = 结果顺序）；无插入消息时零改动
// 零分配（返回原 slice）。
func repackWorkbuddyToolResultBlocks(messages []any) ([]any, bool) {
	if len(messages) < 3 {
		return messages, false
	}
	out := make([]any, 0, len(messages))
	changed := false
	i := 0
	for i < len(messages) {
		m, ok := messages[i].(map[string]any)
		if !ok || m["role"] != "assistant" {
			out = append(out, messages[i])
			i++
			continue
		}
		tcs, hasCalls := m["tool_calls"].([]any)
		if !hasCalls || len(tcs) == 0 {
			out = append(out, messages[i])
			i++
			continue
		}
		want := map[string]bool{}
		for _, tci := range tcs {
			if tc, ok := tci.(map[string]any); ok {
				if id, _ := tc["id"].(string); id != "" {
					want[id] = true
				}
			}
		}
		// 收集紧随其后（允许被其他消息打断）的同批 tool 结果，按原相对顺序。
		out = append(out, messages[i])
		i++
		var results []any
		var between []any
		sawNonTool := false
		for i < len(messages) {
			mm, ok := messages[i].(map[string]any)
			if !ok {
				break
			}
			role, _ := mm["role"].(string)
			if role == "tool" {
				id, _ := mm["tool_call_id"].(string)
				if !want[id] {
					break
				}
				results = append(results, messages[i])
				if sawNonTool {
					changed = true
				}
				i++
				continue
			}
			if len(results) == 0 {
				break // assistant 后没有结果：交由 cleanupWorkbuddyOrphanToolCalls 处理
			}
			// 下一组 assistant.tool_calls 是新的组头，绝不能当插入物吞掉：
			// 一旦被收进 between，它自己那批结果也就永远得不到重排。必须 break 交还外层循环。
			if role == "assistant" {
				if next, _ := mm["tool_calls"].([]any); len(next) > 0 {
					break
				}
			}
			// 同批结果尚未收齐时，中间消息视为插入物，暂存待后移。
			between = append(between, messages[i])
			sawNonTool = true
			i++
		}
		out = append(out, results...)
		out = append(out, between...)
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// cleanupWorkbuddyOrphanToolCalls 剔除无法配对的 tool_call 与 tool 结果（所有模型，
// 独立于 sanitize 开关）。语义：
//   - 收集全线 role:tool 消息的 tool_call_id（结果集）与 assistant.tool_calls[].id（调用集）；
//   - assistant.tool_calls 按 keepCalls 对称裁剪：只留有结果配对的调用（部分保留
//     不会留下无结果的 tool_call），过滤后为空才删掉整个 tool_calls 键；
//     同一条 assistant 内重复的 tool_call id 只保留首次出现（同 id 去重）；
//   - role:tool 每个 id 只保留首条结果（去同 id 多结果），且只在对应 tool_call
//     被保留时才保留，否则删除整条消息；
//   - 无任何工具流量 → 原 slice 原样返回，changed=false（零分配零改动）。
//
// 这是「让请求通过」的安全网：工具执行失败时客户端会把 assistant 的 tool_calls
// 持久化进会话历史却写不回结果消息，坏历史被每次请求原样重放会让上游对之后每条
// 用户消息都返 400，整条会话报废；宁可丢一轮工具上下文，也好过整条会话死亡。
// 只要存在合法配对就整段保留这些字段，绝不吞掉正确配对。
//
// 去重必要性（上游实测）：同一 assistant 内两个相同 tool_call id、或同一 id 有
// 两条结果，上游一律返 400 code=11148 tool_call_sequence_broken（「工具调用记录
// 不完整」）。旧版网关 completed 换发 item id 污染过的客户端历史正是这种形态，
// 去重后调用与结果恢复一一对应，会话自愈。
func cleanupWorkbuddyOrphanToolCalls(messages []any) ([]any, bool) {
	if len(messages) == 0 {
		return messages, false
	}
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	hasTraffic := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch msg["role"] {
		case "tool":
			if id, ok := msg["tool_call_id"].(string); ok && id != "" {
				resultIDs[id] = true
				hasTraffic = true
			}
		case "assistant":
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, tci := range tcs {
					tc, ok := tci.(map[string]any)
					if !ok {
						continue
					}
					if id, ok := tc["id"].(string); ok && id != "" {
						callIDs[id] = true
						hasTraffic = true
					}
				}
			}
		}
	}
	if !hasTraffic {
		return messages, false
	}
	// keepCalls：调用 id 是否双侧齐全（调用存在且结果存在）。重复 id 与乱序均按集合处理。
	keepCalls := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keepCalls[id] = true
		}
	}
	changed := false
	// 1) assistant.tool_calls：按 keepCalls 对称裁剪——只留有结果的调用，过滤后为空则删键。
	// 两侧共用同一份 keepCalls 按 id 对称裁剪，任何输入都不会产生半截配对
	// （历史实现的「整批保留/整批删除」会留下无主结果，上游判 11148）。
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		tcs, ok := msg["tool_calls"].([]any)
		if !ok || len(tcs) == 0 {
			continue
		}
		keptCalls := make([]any, 0, len(tcs))
		seenInAssistant := make(map[string]bool, len(tcs))
		for _, tci := range tcs {
			tc, ok := tci.(map[string]any)
			if !ok {
				continue
			}
			id, _ := tc["id"].(string)
			if !keepCalls[id] {
				continue
			}
			// 同一条 assistant 内重复 id 只保留首次出现：上游对重复 id 必拒 11148。
			if seenInAssistant[id] {
				continue
			}
			seenInAssistant[id] = true
			keptCalls = append(keptCalls, tc)
		}
		if len(keptCalls) == len(tcs) {
			continue // 整批齐全：零改动
		}
		changed = true
		if len(keptCalls) == 0 {
			delete(msg, "tool_calls")
			continue
		}
		msg["tool_calls"] = keptCalls
	}
	// 2) role:tool 结果：只保留对应 tool_call 被保留的首条（同 id 多结果同样触发
	// 11148）；孤儿结果与重复结果整条删除。
	kept := make([]any, 0, len(messages))
	seenResult := make(map[string]bool, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		if role, _ := msg["role"].(string); role == "tool" {
			id, _ := msg["tool_call_id"].(string)
			if !keepCalls[id] {
				changed = true
				continue
			}
			if seenResult[id] {
				changed = true
				continue
			}
			seenResult[id] = true
		}
		kept = append(kept, m)
	}
	if !changed {
		return messages, false
	}
	return kept, true
}

// workbuddyDefaultDeepSeekEffort 官方客户端默认档兜底（configure thinking 无来源时
// warn fallback to 'high'）。补入后走降级管线，模型不支持时自动落到 ≤high 的最高支持档。
const workbuddyDefaultDeepSeekEffort = "high"

// isWorkbuddyDeepSeekModel 模型名以 deepseek 为前缀（不区分大小写）。
// 覆盖 deepseek-v4.1-flash / deepseek-v4-pro 等变体；前缀匹配对齐官方
// thinkingFormat:"deepseek" 的判定口径，避免漏注。
func isWorkbuddyDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// injectWorkbuddyThinking 按 DeepSeek 思维链开关规则改写请求体。非 deepseek 零改动。
//
// 根因：官方客户端对 deepseek 系模型「开思考」必须显式带 thinking:{type:"enabled"} +
// 某档 effort，否则上游默认按不思考应答（思维链不返回）。
// 行为对齐官方客户端：
//   - thinking.type 已显式非空 → 客户端显式控制：disabled 尊重并删 reasoning_effort
//     （snake/camel 双字段）；enabled 缺 effort 时补默认档。
//   - 无 thinking / type 空 / 已有 effort → 注入 enabled 并补默认档（已有 effort 不覆盖）。
//
// defaultEffort 为该模型声明的默认档（sub2api 暂无模型目录缓存，传空串即回退硬编码 "high"）。
func injectWorkbuddyThinking(obj map[string]any, defaultEffort string) {
	model, _ := obj["model"].(string)
	if !isWorkbuddyDeepSeekModel(model) {
		return
	}
	th, ok := obj["thinking"].(map[string]any)
	typ := ""
	if ok {
		typ, _ = th["type"].(string)
		typ = strings.TrimSpace(typ)
	}
	// 显式控制分支：type 非空（enabled/disabled 均为明确意图）→ 不改 type。
	if typ != "" {
		if strings.EqualFold(typ, "disabled") {
			delete(obj, "reasoning_effort")
			delete(obj, "reasoningEffort")
			return // disabled：关思考且不带任何 effort
		}
		ensureWorkbuddyDeepSeekEffort(obj, defaultEffort) // 显式 enabled 缺 effort → 补默认档
		return
	}
	// 无 thinking（或 thinking 非法非对象值）或 type 缺失/为空：注入 enabled。
	// 有 reasoning_effort 也走此分支（effort 保留给既有降级逻辑，开关照开）。
	if !ok {
		obj["thinking"] = map[string]any{"type": "enabled"}
	} else {
		th["type"] = "enabled"
	}
	ensureWorkbuddyDeepSeekEffort(obj, defaultEffort)
}

// ensureWorkbuddyDeepSeekEffort 缺 effort 档位时补默认档（snake 优先，camel 兜底）。
// 已有任一 effort → 不覆盖（显式档位不做任何改写，降级交给降级管线）。
// defaultEffort 空串 → 回退 workbuddyDefaultDeepSeekEffort（硬编码 "high"）。
func ensureWorkbuddyDeepSeekEffort(obj map[string]any, defaultEffort string) {
	if _, hasSnake := obj["reasoning_effort"]; hasSnake {
		return
	}
	if _, hasCamel := obj["reasoningEffort"]; hasCamel {
		return
	}
	if strings.TrimSpace(defaultEffort) == "" {
		defaultEffort = workbuddyDefaultDeepSeekEffort
	}
	obj["reasoning_effort"] = defaultEffort
}

// backfillWorkbuddyReasoningContent DeepSeek 多轮一致性：保证每条 assistant 消息带
// reasoning_content 字段且值为 string（requiresReasoningContentOnAssistantMessages）。
//
// 门控（对齐官方 ReasoningContentBackfillRule：thinkingEnabled || hasTrace）：
//   - 非 deepseek 模型 → 零改动。
//   - deepseek + enabled（含注入后）→ 每条 assistant 保证 reasoning_content 是 string：
//     已有 string 原样保留（不覆盖）；reasoning 是非空 string 且 rc 非 string → 复制
//     reasoning 值；两者皆无 → 补空串 ""。
//   - deepseek + disabled + 无痕迹 → 零改动。
//   - deepseek + disabled + 有痕迹 → 照补（hasTrace 半边）。
//
// 归一化对齐官方 "string"!=typeof 语义：reasoning_content 为 null/数字等非 string
// 值时不算「已有」，落补 ""/复制分支。
func backfillWorkbuddyReasoningContent(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !isWorkbuddyDeepSeekModel(model) {
		return
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	// thinkingEnabled 半边：读注入后的 thinking.type。
	thinkingEnabled := false
	if th, ok := obj["thinking"].(map[string]any); ok {
		if typ, _ := th["type"].(string); strings.EqualFold(strings.TrimSpace(typ), "enabled") {
			thinkingEnabled = true
		}
	}
	// hasTrace 半边：检测是否有任何 reasoning 痕迹（非空 reasoning 或已有 reasoning_content）。
	hasTrace := false
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		if r, ok := msg["reasoning"].(string); ok && r != "" {
			hasTrace = true
			break
		}
		if _, ok := msg["reasoning_content"]; ok {
			hasTrace = true
			break
		}
	}
	if !thinkingEnabled && !hasTrace {
		return
	}
	// 第二遍：所有 assistant 消息补/复制 reasoning_content 字段（跳过条件只认 string）。
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		if _, ok := msg["reasoning_content"].(string); ok {
			continue // 已有 string → 不覆盖
		}
		if r, ok := msg["reasoning"].(string); ok {
			msg["reasoning_content"] = r
		} else {
			msg["reasoning_content"] = ""
		}
	}
}

// injectWorkbuddyPromptCacheKey 注入上游 prompt_cache_key 字段（P0 费用优化）。
//
// 上游服务端支持 prompt_cache_key，同一段前缀命中缓存时费用降 ~17×。
// 优先级：
//  1. body 已带 prompt_cache_key → 原值保留（客户端自知复用哪个键）；
//  2. body 内 conversation_id / conversationId → 用它做会话哈希源；
//  3. 都没有 → conv 为空串（会话段仍由 uid 单独哈希，跨账号绝不碰撞）。
//
// 安全约束——按账号隔离：格式 `wb2a-<uid8>-<convHex>`，uid 是硬隔离因子
// （跨账号复用同一 cache key 会让上游命中错账号的前缀缓存、泄露对方对话）。
func injectWorkbuddyPromptCacheKey(obj map[string]any, creds WorkbuddyCredentials) {
	if existing, ok := obj["prompt_cache_key"].(string); ok && strings.TrimSpace(existing) != "" {
		return // 优先级 1：客户端已显式带 key → 绝不覆盖
	}
	conv := ""
	if v, ok := obj["conversation_id"].(string); ok {
		conv = strings.TrimSpace(v)
	}
	if conv == "" {
		if v, ok := obj["conversationId"].(string); ok {
			conv = strings.TrimSpace(v)
		}
	}
	obj["prompt_cache_key"] = buildWorkbuddyCacheKey(strings.TrimSpace(creds.UID), conv)
}

// buildWorkbuddyCacheKey 生成 `wb2a-<uid8>-<convHex>` 格式的稳定 cache key。
// uid8 = uid 前 8 字符（空则 "-"）提供账号隔离段；
// convHex = sha256(uid + "|" + conversation) 前 16 字节的 hex（同账号同会话稳定、
// 不同会话不同、跨账号绝不碰撞——uid 必须参与哈希）。
func buildWorkbuddyCacheKey(uid, conversation string) string {
	uid8 := uid
	if len(uid8) > 8 {
		uid8 = uid8[:8]
	}
	if uid8 == "" {
		uid8 = "-"
	}
	sum := sha256.Sum256([]byte(uid + "|" + conversation))
	return "wb2a-" + uid8 + "-" + hex.EncodeToString(sum[:16])
}

// ---------------------------------------------------------------------------
// 指纹脱敏（移植自 workbuddy2api/internal/upstream/sanitize.go）
//
// 背景：客户端（Claude Code 类 CLI）在 system prompt 注入若干固定模板句，
// 上游内容审核按逐字精确匹配拦截（非语义审核），一字改动即可绕过。
// 策略：键值/header 型指纹整段剥离；承载语义的模板句最小改写（换一词），语义不变。
// ---------------------------------------------------------------------------

// workbuddySanitizeFeatures 特征预检：任一命中才进入净化（strings.Contains 快速路径，
// 普通请求全不中 → 原样返回，零分配）。
var workbuddySanitizeFeatures = []string{
	"x-anthropic-billing-header",                      // header 键值段键名
	"cc_entrypoint=",                                  // 尾随裸键值（截断前缀即可命中）
	"You are Claude Code",                             // 身份句（截断前缀即可命中）
	"Main branch (",                                   // 注入指令句（截断前缀即可命中）
	"You are a coding agent running in the Codex CLI", // Codex instructions 首段（截断前缀即可命中）
	"github.com/anthropics/",                          // 反馈句里的 Anthropic 仓库链接
	"11128",                                           // 上游反探测：裸数字错误码
}

// workbuddySanitizeHdrRe 剥离层：header 键名即触发（与值无关），整段删除。
var workbuddySanitizeHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)

// workbuddySanitizeBareHdrRe 兜底层：裸键名（无冒号无值）同样是指纹——assistant
// 消息里反引号引用裸键名即触发 11128，而剥离层要求冒号、对裸串无效。
// 键值形态被整段删除后，残留的裸键名做最小缩写（header→hdr）：破坏逐字匹配、
// 语义不变、保留可读性。该正则不要求冒号，是 workbuddySanitizeHdrRe 的超集，
// 两者替换语义不同（整段删除 vs 最小缩写），不可合并。
var workbuddySanitizeBareHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header`)

// workbuddySanitizeKvRe 剥离层：尾随裸键值（cc_xxx=...;）循环清理。
var workbuddySanitizeKvRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// workbuddySanitizeRewrites 改写层：全模板句逐字替换（每句只改一个词，语义不变）。
//
// 身份句的匹配串**不带结尾标点**（只到 "…for Claude" 为止）：CLI 版以句号收尾，
// 桌面版以逗号接后继内容，去掉结尾标点后两种形态一并覆盖（替换串同样不带标点）。
// 11128 反探测：只要请求体里出现裸数字 11128 就整单拦截（与该数字上下文无关），
// 插入连字符保留可读性与指代。
var workbuddySanitizeRewrites = [][2]string{
	{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	},
	{
		// 反馈句：整句带 Anthropic 仓库链接，上游按整句拦截（give→provide 一词之差即可绕过）。
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	{
		"11128",
		"11-128",
	},
}

// workbuddyHasFingerprint 特征预检：先走 strings.Contains 快速路径（零分配）；
// header 键名有大小写变体且可能以裸键名形态出现（无冒号），Contains 大小写敏感、
// 剥离层正则要求冒号——两者都会漏掉「混合大小写 + 裸键名」，必须再用不要求冒号的
// (?i) 正则兜底（workbuddySanitizeBareHdrRe）。
func workbuddyHasFingerprint(text string) bool {
	for _, f := range workbuddySanitizeFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	return workbuddySanitizeBareHdrRe.MatchString(text)
}

// sanitizeWorkbuddyText 单段文本净化：预检不中 → 返回原串（零分配）。
// 末尾 TrimSpace 只适用于 prose 文本（content / reasoning_content）：剥离层删掉
// 尾随 kv 段后常留空白。嵌入 JSON 的字符串叶子不能用本函数，必须走
// sanitizeWorkbuddyTextRaw——那里的首尾空白是参数语义的一部分。
func sanitizeWorkbuddyText(text string) string {
	return strings.TrimSpace(sanitizeWorkbuddyTextRaw(text))
}

// sanitizeWorkbuddyTextRaw 与 sanitizeWorkbuddyText 同规则，但不做 TrimSpace，
// 供嵌入 JSON 的字符串叶子使用（保留参数原样首尾空白）。
func sanitizeWorkbuddyTextRaw(text string) string {
	if !workbuddyHasFingerprint(text) {
		return text
	}
	for _, rw := range workbuddySanitizeRewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	if workbuddySanitizeHdrRe.MatchString(text) {
		text = workbuddySanitizeHdrRe.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "cc_") {
		prev := ""
		for prev != text { // 清尾随裸 kv（cc_version=...; cc_entrypoint=...;）
			prev = text
			text = workbuddySanitizeKvRe.ReplaceAllString(text, "")
		}
	}
	// 兜底：键值形态已在上面整段删除，这里只剩裸键名（引用/示例文本形态）。
	return workbuddySanitizeBareHdrRe.ReplaceAllString(text, "x-anthropic-billing-hdr")
}

// sanitizeWorkbuddyContent 兼容字符串与多模态数组；只动 text part，image 等 part 不动。
// 返回净化后的值及是否发生变化。
func sanitizeWorkbuddyContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		s := sanitizeWorkbuddyText(c)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			if s := sanitizeWorkbuddyText(text); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// sanitizeWorkbuddyToolCalls 净化 assistant.tool_calls[].function.arguments。
// 这块是历史盲区：工具调用消息的 content 通常是 null，若在 content 缺失时直接
// continue，整条消息连 tool_calls 一起被跳过——历史里任何写进工具参数的被拦
// 字符串（文件名、命令、写入内容）都会原样漏出。
//
// arguments 是**字符串化的 JSON**，不能按纯文本整体替换：剥离/改写层会跨 JSON
// token 改写（实测把数字字面量插进连字符、截断引号内片段），产出非法 JSON。
// 上游实测容忍非法 arguments（不触发 11148），但模型侧读到的是坏参数。故改为
// 「解析 → 只净化字符串叶子 → 重新序列化」，结构化部分（数字/布尔/嵌套）零改动。
func sanitizeWorkbuddyToolCalls(v any) bool {
	callList, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range callList {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if s := sanitizeWorkbuddyToolCallArguments(args); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// sanitizeWorkbuddyToolCallArguments 净化单条字符串化 JSON 的 arguments。
// 不变式：返回值要么是合法 JSON，要么与入参完全相同——绝不把合法 JSON 变成非法。
// 解码用 UseNumber：float64 往返会把大整数写成 1e+21 之类的字面量，破坏参数语义。
func sanitizeWorkbuddyToolCallArguments(args string) string {
	if !workbuddyHasFingerprint(args) {
		return args
	}
	dec := json.NewDecoder(strings.NewReader(args))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		// 不是合法 JSON（截断/历史脏数据）：任何文本级改写都只会更坏，原样放行。
		return args
	}
	cleaned, changed := sanitizeWorkbuddyJSONValue(parsed)
	if !changed {
		return args
	}
	out, err := json.Marshal(cleaned)
	if err != nil || !json.Valid(out) {
		return args
	}
	return string(out)
}

// sanitizeWorkbuddyJSONValue 递归净化 JSON 树，只改字符串叶子；数组/对象结构、
// 数字（json.Number，原样字面量）、布尔、null 一律不动。对象键名同样不动：
// 工具参数里的键名常是 schema 字段名，改写会破坏下游解析。
func sanitizeWorkbuddyJSONValue(v any) (any, bool) {
	switch node := v.(type) {
	case string:
		s := sanitizeWorkbuddyTextRaw(node)
		return s, s != node
	case []any:
		changed := false
		for i, item := range node {
			nv, ch := sanitizeWorkbuddyJSONValue(item)
			if ch {
				node[i] = nv
				changed = true
			}
		}
		return node, changed
	case map[string]any:
		changed := false
		for k, item := range node {
			nv, ch := sanitizeWorkbuddyJSONValue(item)
			if ch {
				node[k] = nv
				changed = true
			}
		}
		return node, changed
	}
	return v, false
}

// sanitizeWorkbuddyMessages 净化 messages 中的 content、reasoning_content 与 tool_calls；
// 任一命中返回 true。
func sanitizeWorkbuddyMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		// content 与 tool_calls 各自独立判断：content 可以为 null（工具调用轮）。
		if c, ok := m["content"]; ok {
			if nc, ch := sanitizeWorkbuddyContent(c); ch {
				m["content"] = nc
				changed = true
			}
		}
		// reasoning_content（思维链回填字段）实测同样携带指纹，与 content 同等净化。
		if rc, ok := m["reasoning_content"].(string); ok {
			if s := sanitizeWorkbuddyText(rc); s != rc {
				m["reasoning_content"] = s
				changed = true
			}
		}
		if tc, ok := m["tool_calls"]; ok {
			if sanitizeWorkbuddyToolCalls(tc) {
				changed = true
			}
		}
	}
	return changed
}
