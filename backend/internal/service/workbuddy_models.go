package service

// workbuddy_models.go WorkBuddy 上游模型目录拉取：GET {chat_base}/v3/config。
//
// 【为什么是这个端点】WorkBuddy 聊天域没有 OpenAI 形态的 /v1/models（实测 404），
// 上游把「当前客户端可用哪些模型」放在产品配置接口 /v3/config 里下发：
//   - 目录 ID 清单在 agents[].models（字符串数组），其中 name=="cli" 的那份就是
//     桌面端模型选择器实际展示的那一张表；其余 agent 条目挂的是内部辅助模型
//     （lite / custom:luxen|* / builtin-lite），不是面向用户的对话模型。
//   - 逐模型能力元数据在另一份 models 数组（对象数组，带 id/name/credits/
//     contextWindow/reasoning/supportsImages 等）。
// 一手证据：本机 WorkBuddy 桌面端缓存 ~/.workbuddy/cache/acc-product-config-v3.json
// 正是该响应的落盘形态（第三方生产实现 workbuddy2api 原文写明「the cache file is
// this very response written to disk」），其 cli 清单 21 项、models 表 52 项。
//
// 【匿名实测（2026-10-13，本机三域）】/v3/config 在 copilot.tencent.com /
// www.workbuddy.ai / www.codebuddy.cn 全部 **HTTP 200**，信封为
// {"code":0,"msg":"OK","requestId":...,"data":{...}}，但 data.models 与
// data.agent.agents 均为 null —— 即目录按身份下发，必须带账号票。
// 对照 /v3/<随机路径> 返回 404，可确证该路由真实存在（不同于 /console/*
// 那种任意子路径都回 401 的 catch-all，状态码在那里无法区分有无路由）。
// 另一条候选 /v2/enterprises/personal/models 同样真实存在（401 vs 同前缀
// 随机路径 404），但第三方实现明确「returns a narrower list, so prefer」它，
// 故本文件只接 /v3/config。
//
// 【UA 决定目录内容】上游按 User-Agent 分片下发，未知 UA 直接 400（code 12403），
// 只有桌面端 UA 才返回完整清单。因此这里最多两次尝试：先用完整桌面端 UA
// （workbuddyUAFor，含 CLI 段），再用 billing 单段 UA（WorkBuddy/<ver>）——
// 与参考实现「one per desktop UA」同纪律；两次都拿不到目录才算失败。
//
// 【过滤口径：只做技术性排除，不做商务裁剪】参考实现在展示层还有一道「丢弃
// -x/-sg/-f 付费变体、有免费兄弟就只投免费」的策展规则，那是**转售免费额度**
// 的取舍；我们是网关、定价自配，管理员要能自己决定开哪些模型，照抄会把上游
// 真实可调的模型（hy3-x、glm-5.3-flashx）藏起来。因此这里只保留三类必然调不通
// 的技术性排除：内部辅助/虚拟别名、文本补全与 IDE 内联专用模型、生图专用模型。
// auto 保留：它是上游选择器展示的自动路由入口，且 DefaultWorkbuddyModelIDs
// 早已对外宣告，同步结果与静态表口径倒挂比多一个候选更糟。
//
// 元数据完整性：reasoning 有两种下发形态（supportedEfforts 数组，或单值 effort）。
// 后者也要产出一档，否则按 upstreamModelMetadataIsComplete 的
// 「reasoning=true 必须有 levels」判据，每轮同步都会回 metadata_partial 告警
// 且能力快照永远写不进去（Qoder 的教训，见 qoder_models.go 同段注释）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// workbuddyModelListPath 产品配置端点路径（挂在聊天域，不是 billing 域）。
const workbuddyModelListPath = "/v3/config"

// workbuddyCatalogAccept 目录接口的 Accept：参考实现取浏览器形态的多类型串，
// 与聊天流式的 text/event-stream 不同，本端点只回 JSON。
const workbuddyCatalogAccept = "application/json, text/plain, */*"

// workbuddyNonChatModelIDs 上游下发但不可能作为对话模型调用的条目：内部辅助
// 模型（lite 供标题生成/压缩，实测送上游回 11102）与虚拟别名（不是可固定的模型）。
var workbuddyNonChatModelIDs = map[string]struct{}{
	"lite":           {},
	"default-model":  {},
	"fast-model":     {},
	"balanced-model": {},
	"primary-model":  {},
	"deep-model":     {},
}

// workbuddyNonChatModelPrefixes 文本补全 / IDE 内联专用模型前缀（codewise-*、
// completion-*），以及租户自定义模型前缀（企业自定义 ID 形如
// custom:<ns>|<name>，含竖线且按租户而异，对外宣告没有意义）。
var workbuddyNonChatModelPrefixes = []string{
	"codewise-", "completion-", "custom:",
}

// workbuddyNonChatModelSuffixes 生图与行内补全专用后缀。
var workbuddyNonChatModelSuffixes = []string{
	"-image-alpha", "-image-alpha-edit", "-taco-completion",
}

// workbuddyCatalogIsChatModel 报告目录条目是否可作为对外对话模型。
func workbuddyCatalogIsChatModel(modelID string) bool {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if modelID == "" {
		return false
	}
	if _, blocked := workbuddyNonChatModelIDs[modelID]; blocked {
		return false
	}
	for _, prefix := range workbuddyNonChatModelPrefixes {
		if strings.HasPrefix(modelID, prefix) {
			return false
		}
	}
	for _, suffix := range workbuddyNonChatModelSuffixes {
		if strings.HasSuffix(modelID, suffix) {
			return false
		}
	}
	return true
}

// fetchWorkbuddyUpstreamModels 拉取 WorkBuddy 账号当前可用模型目录。
//
// 凭据语义与连接测试一致：access_token 临近过期且有 refresh_token 时先换票，
// 实际请求撞 401 再换一次并重试（本端点与聊天共用同一令牌，过期是常态）。
func (s *AccountTestService) fetchWorkbuddyUpstreamModels(ctx context.Context, account *Account) ([]string, map[string]UpstreamModelMetadata, error) {
	if s == nil || s.httpUpstream == nil {
		return nil, nil, newUpstreamModelSyncConfigError("Upstream HTTP client is not configured", nil)
	}
	if err := s.refreshWorkbuddyAccountTokenIfNeeded(ctx, account); err != nil {
		return nil, nil, err
	}

	baseURL := account.GetWorkbuddyBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		return nil, nil, newUpstreamModelSyncConfigError("WorkBuddy base URL is not configured", nil)
	}
	validatedBase, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, nil, newUpstreamModelSyncConfigError("Invalid WorkBuddy base URL", err)
	}
	targetURL := strings.TrimRight(validatedBase, "/") + workbuddyModelListPath
	proxyURL := upstreamModelsProxyURL(account)

	// 两次 UA 尝试：桌面端完整 UA（唯一被证实能拿到完整目录的形态）优先，
	// billing 单段 UA 兜底（某些构建只认它）。
	userAgents := []string{workbuddyUAFor(account.GetWorkbuddyRealm())}
	if billingUA := "WorkBuddy/" + workbuddyClientVersion; billingUA != userAgents[0] {
		userAgents = append(userAgents, billingUA)
	}

	var lastErr error
	for _, userAgent := range userAgents {
		modelIDs, metadata, err := s.fetchWorkbuddyCatalogOnce(ctx, account, targetURL, proxyURL, userAgent)
		if err != nil {
			// 401 是鉴权失败而不是 UA 分片问题，换 UA 没有意义：能换票就重试同一 UA，
			// 换不了票则**原样返回首次错误**（它带着真实 StatusCode=401 与上游 msg；
			// 另包一层新错误会把状态码抹成 0，handler 只能回一旬无状态的 502）。
			if isUpstreamModelSyncStatus(err, http.StatusUnauthorized) {
				if refreshErr := s.refreshWorkbuddyAccountToken(ctx, account); refreshErr == nil {
					return s.fetchWorkbuddyCatalogOnce(ctx, account, targetURL, proxyURL, userAgent)
				}
				return nil, nil, err
			}
			// 其余失败（含「200 但信封里没有目录」）继续试下一个 UA。
			lastErr = err
			continue
		}
		return modelIDs, metadata, nil
	}
	if lastErr == nil {
		lastErr = newUpstreamModelSyncUpstreamError("WorkBuddy returned no schedulable models", nil)
	}
	return nil, nil, lastErr
}

// fetchWorkbuddyCatalogOnce 单次目录请求 + 解析。
func (s *AccountTestService) fetchWorkbuddyCatalogOnce(
	ctx context.Context,
	account *Account,
	targetURL string,
	proxyURL string,
	userAgent string,
) ([]string, map[string]UpstreamModelMetadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, nil, newUpstreamModelSyncInternalError("Failed to build WorkBuddy model list request", err)
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	// 复用 billing 头组的鉴权骨架（Authorization / X-User-Id / 企业头 / 设备令牌），
	// 再覆写本端点要求的 Accept 与 UA；不注入会话头族（X-Conversation-*）——
	// 目录拉取不属于任何一次 user send，伪造聚合主键只会污染上游后台统计。
	applyWorkbuddyBillingHeaders(req, account.GetWorkbuddyCredentials(), account, account.GetWorkbuddyRealm())
	req.Header.Set("Accept", workbuddyCatalogAccept)
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}

	resp, err := s.doUpstreamModelsRequest(req, proxyURL, account)
	if err != nil {
		return nil, nil, newUpstreamModelSyncUpstreamError("Failed to request WorkBuddy model list", err)
	}
	defer func() { _ = resp.Body.Close() }()

	bodyLimit := resolveModelsListReadLimit(s.cfg)
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
	if readErr != nil {
		return nil, nil, newUpstreamModelSyncUpstreamError("Failed to read WorkBuddy model list", readErr)
	}
	if int64(len(body)) > bodyLimit {
		return nil, nil, newUpstreamModelSyncUpstreamError(
			"WorkBuddy model list response is too large", fmt.Errorf("response exceeds %d bytes", bodyLimit))
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, nil, &UpstreamModelSyncError{
			Kind:       UpstreamModelSyncErrorUpstream,
			Message:    fmt.Sprintf("WorkBuddy model list returned HTTP %d: %s", resp.StatusCode, workbuddyTruncateForError(string(body))),
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("workbuddy model list returned HTTP %d", resp.StatusCode),
		}
	}

	modelIDs, metadata, parseErr := parseWorkbuddyModelCatalog(body)
	if parseErr != nil {
		return nil, nil, newUpstreamModelSyncUpstreamError("WorkBuddy model list response was not usable", parseErr)
	}
	return modelIDs, metadata, nil
}

// refreshWorkbuddyAccountTokenIfNeeded 同步前的预刷新（无 refresh_token 时不报错，
// 交由实际请求的 401 分支处理）。
func (s *AccountTestService) refreshWorkbuddyAccountTokenIfNeeded(ctx context.Context, account *Account) error {
	creds := account.GetWorkbuddyCredentials()
	if !workbuddyTokenNeedsRefresh(creds, time.Now()) || strings.TrimSpace(creds.RefreshToken) == "" {
		if strings.TrimSpace(creds.AccessToken) == "" {
			return newUpstreamModelSyncConfigError("No WorkBuddy access token is available", nil)
		}
		return nil
	}
	if err := s.refreshWorkbuddyAccountToken(ctx, account); err != nil {
		// 有旧 token 就宽容放行（与聊天链路一致），完全无 token 才是配置错误。
		if strings.TrimSpace(account.GetWorkbuddyCredentials().AccessToken) == "" {
			return newUpstreamModelSyncUpstreamError("Failed to refresh WorkBuddy access token", err)
		}
	}
	return nil
}

// isUpstreamModelSyncStatus 判定同步错误是否携带指定 HTTP 状态码。
func isUpstreamModelSyncStatus(err error, statusCode int) bool {
	return err != nil && upstreamModelSyncStatusCode(err) == statusCode
}

// parseWorkbuddyModelCatalog 解析 /v3/config 响应：取目录 ID 清单 + 逐模型元数据。
//
// 信封容错：本端点的**带票**形态没有一手抓包（匿名实测只能确证路由存在与
// 「code/msg/data」外层信封），而同一份文档的落盘形态（桌面端缓存）是**无信封**
// 的顶层对象。因此不猜单一结构：在 top → data/Data/result/Result（含再向下嵌套）
// 的范围内找 agents（数组或「场景→对象」映射两种形态）与 models 对象数组，
// 任一真值形态命中即可解析；全不命中时把真实顶层键与 body 摘要写进错误，
// 避免线上只留下一句无信息量的失败。
func parseWorkbuddyModelCatalog(body []byte) ([]string, map[string]UpstreamModelMetadata, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, nil, fmt.Errorf("parse workbuddy model catalog: %w", err)
	}
	if root == nil {
		return nil, nil, fmt.Errorf("parse workbuddy model catalog: response is null")
	}
	// 业务错误码走 HTTP 200（与 workbuddy 全仓既有判据一致），code!=0 不是目录。
	if code, ok := workbuddyCatalogBusinessCode(root); ok && code != 0 {
		return nil, nil, fmt.Errorf("parse workbuddy model catalog: upstream code=%d msg=%s",
			code, workbuddyTruncateForError(workbuddyCatalogString(root, "msg")))
	}

	scopes := workbuddyCatalogScopes(root)
	ids := workbuddyCatalogIDs(scopes)
	if len(ids) == 0 {
		return nil, nil, fmt.Errorf("parse workbuddy model catalog: no agent model list found (top-level keys: %s; body: %s)",
			workbuddyCatalogKeys(body), workbuddyTruncateForError(string(body)))
	}
	meta := workbuddyCatalogMetadata(scopes)

	modelIDs := make([]string, 0, len(ids))
	metadata := make(map[string]UpstreamModelMetadata, len(ids))
	for _, id := range ids {
		if !workbuddyCatalogIsChatModel(id) {
			continue
		}
		if _, dup := metadata[id]; dup {
			continue
		}
		modelIDs = append(modelIDs, id)
		if entry, ok := meta[id]; ok {
			metadata[id] = entry
			continue
		}
		metadata[id] = UpstreamModelMetadata{ID: id, DisplayName: id}
	}
	if len(modelIDs) == 0 {
		return nil, nil, fmt.Errorf("parse workbuddy model catalog: every model was filtered as non-chat (ids: %s)",
			workbuddyTruncateForError(strings.Join(ids, ",")))
	}
	return modelIDs, metadata, nil
}

// workbuddyCatalogBusinessCode 读取信封业务码（缺键/非数字返回 ok=false：
// 落盘缓存形态没有 code，不能拿「没下发」当「业务失败」）。
func workbuddyCatalogBusinessCode(obj map[string]any) (int64, bool) {
	raw, ok := obj["code"]
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case float64:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}

// workbuddyCatalogScopes 收集可能承载目录的对象层级（深度上限 2，防环靠深度本身）。
func workbuddyCatalogScopes(root map[string]any) []map[string]any {
	scopes := []map[string]any{root}
	queue := []map[string]any{root}
	for depth := 0; depth < 2 && len(queue) > 0; depth++ {
		next := make([]map[string]any, 0, len(queue))
		for _, obj := range queue {
			for _, key := range []string{"data", "Data", "result", "Result"} {
				inner, ok := obj[key].(map[string]any)
				if !ok {
					continue
				}
				scopes = append(scopes, inner)
				next = append(next, inner)
			}
		}
		queue = next
	}
	return scopes
}

// workbuddyCatalogIDs 递归取目录 ID 清单：优先 name=="cli" 的 agent（桌面端选择器
// 实际展示的那张表），其余 agent 取最长的一份兜底（上游可能改名或按场景分片）。
//
// 为什么递归而不是固定层级：实测两个真值形态的 agents 位置不同——带票响应的
// 实测（2026-10-13 匿名）是 data.agent.agents，落盘缓存是顶层 agents。写死任一层
// 都会在另一个形态下报「没找到目录」。
func workbuddyCatalogIDs(scopes []map[string]any) []string {
	var cli, other []string
	var walk func(node any, depth int)
	walk = func(node any, depth int) {
		if depth > 4 {
			return
		}
		switch value := node.(type) {
		case map[string]any:
			for _, list := range workbuddyCatalogAgentLists(value["agents"]) {
				if list.cli {
					if len(list.ids) > len(cli) {
						cli = list.ids
					}
				} else if len(list.ids) > len(other) {
					other = list.ids
				}
			}
			// 确定遍历序（见 sortedMapKeys 注释）：同长度候选列表靠后 visited 不会
			// 覆盖前者，所以访问序一旦随机，同一份上游数据两次解析可能给出不同目录。
			for _, key := range sortedMapKeys(value) {
				walk(value[key], depth+1)
			}
		case []any:
			for _, child := range value {
				walk(child, depth+1)
			}
		}
	}
	for _, obj := range scopes {
		walk(obj, 0)
	}
	if len(cli) > 0 {
		return dedupeWorkbuddyCatalogIDs(cli)
	}
	return dedupeWorkbuddyCatalogIDs(other)
}

type workbuddyCatalogAgentList struct {
	cli bool
	ids []string
}

// workbuddyCatalogAgentLists 兼容 agents 的两种下发形态：对象数组（桌面端缓存）
// 与「名称→对象」映射（参考实现见 endpoint 侧为 dict）。
func workbuddyCatalogAgentLists(raw any) []workbuddyCatalogAgentList {
	lists := make([]workbuddyCatalogAgentList, 0, 4)
	collect := func(name string, entry map[string]any) {
		models, ok := entry["models"].([]any)
		if !ok {
			return
		}
		ids := make([]string, 0, len(models))
		for _, item := range models {
			if id, ok := item.(string); ok {
				if id = strings.TrimSpace(id); id != "" {
					ids = append(ids, id)
				}
			}
		}
		if len(ids) == 0 {
			return
		}
		lists = append(lists, workbuddyCatalogAgentList{
			cli: strings.EqualFold(strings.TrimSpace(name), "cli"),
			ids: ids,
		})
	}
	switch agents := raw.(type) {
	case []any:
		for _, item := range agents {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			collect(workbuddyCatalogString(entry, "name"), entry)
		}
	case map[string]any:
		for name, item := range agents {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			collect(name, entry)
		}
	}
	return lists
}

// workbuddyCatalogMetadata 递归收集「带 id 的对象数组」形态的 models 表。
// 与 ID 清单一样按 data/信封层级出现位置不定，故不限深度直扫（数组只读一层值）。
func workbuddyCatalogMetadata(scopes []map[string]any) map[string]UpstreamModelMetadata {
	metadata := make(map[string]UpstreamModelMetadata)
	var walk func(node any, depth int)
	walk = func(node any, depth int) {
		if depth > 4 {
			return
		}
		switch value := node.(type) {
		case map[string]any:
			if models, ok := value["models"].([]any); ok {
				for _, item := range models {
					entry, ok := item.(map[string]any)
					if !ok {
						continue
					}
					id := strings.TrimSpace(workbuddyCatalogString(entry, "id"))
					if id == "" {
						continue
					}
					if _, exists := metadata[id]; exists {
						continue
					}
					metadata[id] = workbuddyEntryMetadata(id, entry)
				}
			}
			// 子树按 key 字典序访问：同一 id 在多份 models 表里出现时“先到者赢”，
			// 访问序不定就等于元数据内容不定。
			for _, key := range sortedMapKeys(value) {
				walk(value[key], depth+1)
			}
		case []any:
			for _, child := range value {
				walk(child, depth+1)
			}
		}
	}
	for _, obj := range scopes {
		walk(obj, 0)
	}
	return metadata
}

// sortedMapKeys 给递归扫描提供确定访问序：Go 的 map 遍历序运行时随机，同一份
// 上游数据两次解析产出不同目录/元数据，会让管理页把「顺序变化」误报为「模型变更」。
func sortedMapKeys(obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// workbuddyEntryMetadata 把目录条目映射为对外元数据。
func workbuddyEntryMetadata(modelID string, entry map[string]any) UpstreamModelMetadata {
	meta := UpstreamModelMetadata{ID: modelID}
	if name := strings.TrimSpace(workbuddyCatalogString(entry, "name")); name != "" {
		meta.DisplayName = name
	} else {
		meta.DisplayName = modelID
	}
	if desc := strings.TrimSpace(workbuddyCatalogString(entry, "descriptionEn")); desc != "" {
		meta.Description = desc
	}

	// 上下文窗口：defaultLength 是选择器默认档位，作为对外 context_window；
	// MaxContextWindow 取「支持档位 / maxAllowedSize / maxInputTokens」三者最大，
	// 三者都是上游直出的容量上限，缺任一不影响另一个。
	if window := workbuddyCatalogNestedInt64(entry, "contextWindow", "defaultLength"); window > 0 {
		meta.ContextWindow = window
	}
	if maxInput := workbuddyCatalogInt64(entry, "maxInputTokens"); maxInput > meta.ContextWindow {
		meta.ContextWindow = maxInput
	}
	meta.MaxContextWindow = meta.ContextWindow
	for _, candidate := range append(workbuddyCatalogNestedInt64s(entry, "contextWindow", "supportedLengths"),
		workbuddyCatalogInt64(entry, "maxAllowedSize")) {
		if candidate > meta.MaxContextWindow {
			meta.MaxContextWindow = candidate
		}
	}
	meta.MaxOutputTokens = workbuddyCatalogInt64(entry, "maxOutputTokens")

	// 输入模态：supportsImages 是定论字段；disabledMultimodal 显式为真时压回纯文本。
	modalities := []string{"text"}
	if supportsImages, ok := entry["supportsImages"].(bool); ok && supportsImages {
		modalities = append(modalities, "image")
	}
	if disabled, ok := entry["disabledMultimodal"].(bool); ok && disabled {
		modalities = []string{"text"}
	}
	meta.InputModalities = modalities

	// 推理能力：supportsReasoning 为真才写 Reasoning，且必须同时拿到档位
	// （supportedEfforts 数组，或单值 effort 归一为一档）——否则
	// upstreamModelMetadataIsComplete 判不完整，快照写不进、每轮回 partial 告警。
	// 为假时写 Reasoning=false（上游给了定论）。
	supportsReasoning, haveReasoningFlag := entry["supportsReasoning"].(bool)
	if haveReasoningFlag && !supportsReasoning {
		flag := false
		meta.Reasoning = &flag
	} else if haveReasoningFlag && supportsReasoning {
		levels := normalizeReasoningLevels(workbuddyCatalogNestedStrings(entry, "reasoning", "supportedEfforts"))
		if len(levels) == 0 {
			if effort := normalizeReasoningLevel(workbuddyCatalogNestedString(entry, "reasoning", "effort")); effort != "" {
				levels = []string{effort}
			}
		}
		if len(levels) > 0 {
			flag := true
			meta.Reasoning = &flag
			meta.SupportedReasoningLevels = levels
			def := normalizeReasoningLevel(workbuddyCatalogNestedString(entry, "reasoning", "defaultEffort"))
			for _, level := range levels {
				if level == def {
					meta.DefaultReasoningLevel = def
					break
				}
			}
			if meta.DefaultReasoningLevel == "" {
				meta.DefaultReasoningLevel = levels[0]
			}
		}
	}
	return meta
}

// workbuddyCatalogKeys 返回 JSON 顶层键（逗号分隔，用于错误文案定位真实形态）。
func workbuddyCatalogKeys(body []byte) string {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return "(not a json object)"
	}
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func workbuddyCatalogString(obj map[string]any, key string) string {
	value, ok := obj[key].(string)
	if !ok {
		return ""
	}
	return value
}

func workbuddyCatalogInt64(obj map[string]any, key string) int64 {
	switch v := obj[key].(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

func workbuddyCatalogNestedInt64(obj map[string]any, objectKey, fieldKey string) int64 {
	inner, ok := obj[objectKey].(map[string]any)
	if !ok {
		return 0
	}
	return workbuddyCatalogInt64(inner, fieldKey)
}

func workbuddyCatalogNestedInt64s(obj map[string]any, objectKey, fieldKey string) []int64 {
	inner, ok := obj[objectKey].(map[string]any)
	if !ok {
		return nil
	}
	items, ok := inner[fieldKey].([]any)
	if !ok {
		return nil
	}
	values := make([]int64, 0, len(items))
	for _, item := range items {
		switch v := item.(type) {
		case float64:
			values = append(values, int64(v))
		case json.Number:
			if n, err := v.Int64(); err == nil {
				values = append(values, n)
			}
		}
	}
	return values
}

func workbuddyCatalogNestedString(obj map[string]any, objectKey, fieldKey string) string {
	inner, ok := obj[objectKey].(map[string]any)
	if !ok {
		return ""
	}
	return workbuddyCatalogString(inner, fieldKey)
}

func workbuddyCatalogNestedStrings(obj map[string]any, objectKey, fieldKey string) []string {
	inner, ok := obj[objectKey].(map[string]any)
	if !ok {
		return nil
	}
	items, ok := inner[fieldKey].([]any)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			values = append(values, s)
		}
	}
	return values
}

func dedupeWorkbuddyCatalogIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
