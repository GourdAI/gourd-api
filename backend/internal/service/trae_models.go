package service

// trae_models.go Trae 上游模型目录拉取：POST {chat_base}/api/ide/v1/get_detail_param。
//
// 为什么用这个端点而不是 /v1/models：Trae 聊天域没有 OpenAI 形态的模型列表端点，
// 上游把「当前 IDE 版本可用哪些模型」放在对话参数详情接口里下发（请求体带
// function=solo_work_lite，与 llm_utils_chat 同一 function 取值），响应
// config_info_list[].config_name 就是聊天请求体里的 model / config_name 取值，
// 因此拉到即可直接用作对外模型 ID，无需再做名字换算。
//
// 三条过滤纪律（缺一会把不可调的条目开放给用户）：
//  1. config_switch=false → 上游已下线，调用必失败；
//  2. is_invisible_to_user=true → 内部 subagent / 实验通道（如 browser_use_subagent），
//     对用户暴露没有意义；
//  3. display_name 为空 → 企业租户占位模板，不是真实模型。
// 两个布尔字段都是「缺省即视为可见/启用」，上游并不总是显式下发。
//
// 该端点不提供 reasoning / modalities，所以这里产出的元数据一律不完整，
// SyncUpstreamModelCatalog 会回一条 metadata_partial 警告（而不是假装同步成功）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"
)

// traeModelListBody get_detail_param 的请求体（字段与聊天侧同族，取默认目录形态：
// 只要模型表，不要 prompt）。null 字段必须保留，上游按缺键/空值区分处理。
const traeModelListBody = `{"function":"solo_work_lite","config_names":null,"need_prompt":false,"current_config_info":null,"poly_prompt":true,"mode_type":null,"agent_type":null}`

// parseTraeModelCatalog 解析并过滤目录响应，返回保持上游顺序的模型 ID 与元数据表。
// 全被过滤时返回空切片（调用方据此报「上游没有返回可同步模型」，不静默成功）。
//
// 信封形态：本端点没有一手抓包证据，而 Trae 在本仓已证实的响应全部走火山信封
// （{code,msg,Result:{...}}，见 traeExtractLoginHost / traeParseAuthCodeExchange），
// 第三方实现另见裸对象与 data 包裹。因此这里不猜单一结构，而是按
// top → Result/result/data（再向下嵌套一层）的顺序找 config_info_list，
// 任意一种真值形态命中即可解析；全不命中时报文里带上真实 keys 与 body 摘要，
// 避免线上只留下一句无信息量的「是空的」。
func parseTraeModelCatalog(body []byte) ([]string, map[string]UpstreamModelMetadata, error) {
	entries, found := traeLocateModelConfigList(body)
	if !found {
		return nil, nil, fmt.Errorf("parse trae model catalog: config_info_list not found (top-level keys: %s; body: %s)",
			traeJSONObjectKeys(body), traeTruncateForError(string(body)))
	}
	if len(entries) == 0 {
		return nil, nil, fmt.Errorf("parse trae model catalog: config_info_list is empty (body: %s)",
			traeTruncateForError(string(body)))
	}

	modelIDs := make([]string, 0, len(entries))
	metadata := make(map[string]UpstreamModelMetadata, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		name := strings.TrimSpace(traeCatalogEntryString(entry, "config_name"))
		if name == "" {
			continue
		}
		// 两个开关都是「缺省即视为启用/可见」：上游并不总是显式下发。
		// config_switch 的值语义是「开关打开」，故 ok && !switchOn 才是已下线。
		if switchOn, present := traeCatalogEntryBool(entry, "config_switch"); present && !switchOn {
			continue
		}
		if hidden, present := traeCatalogEntryBool(entry, "is_invisible_to_user"); present && hidden {
			continue
		}
		displayName := strings.TrimSpace(traeCatalogEntryNestedString(entry, "display_config", "display_name"))
		if displayName == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		modelIDs = append(modelIDs, name)
		entryMeta := UpstreamModelMetadata{ID: name, DisplayName: displayName}
		if window, ok := traeCatalogEntryNestedInt64(entry, "context_window_tokens", "dev"); ok && window > 0 {
			entryMeta.ContextWindow = window
			entryMeta.MaxContextWindow = window
		}
		metadata[name] = entryMeta
	}
	return modelIDs, metadata, nil
}

// traeLocateModelConfigList 在多候选信封路径里定位 config_info_list。
// 返回 (条目, 是否找到列表键)：找到但为空数组也要如实回报，
// 让调用方区分「信封不认识」与「上游确实没模型」两种故障。
func traeLocateModelConfigList(body []byte) ([]map[string]any, bool) {
	var top map[string]any
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return nil, false
	}
	// 与 traeExtractLoginHost 同族的扫描顺序；额外支持 Result 再嵌一层 Result
	// （火山网关在业务信封外包一层的情况）。
	for _, candidate := range traeCatalogScopes(top) {
		for _, key := range []string{"config_info_list", "configInfoList"} {
			if raw, ok := candidate[key]; ok {
				return traeCoerceMapSlice(raw), true
			}
		}
	}
	return nil, false
}

// traeCatalogScopes 按广度优先展开待扫描的对象层级（top → Result/result/data/Data，
// 最多下探 traeCatalogScopeMaxDepth 层）。调用方据此依次尝试取 config_info_list。
func traeCatalogScopes(top map[string]any) []map[string]any {
	const traeCatalogScopeMaxDepth = 2
	type node struct {
		obj   map[string]any
		depth int
	}
	out := make([]map[string]any, 0, 8)
	visited := make(map[uintptr]bool, 8)
	queue := []node{{obj: top}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.obj == nil {
			continue
		}
		// 环形引用防护：同一对象实例只访问一次（JSON 解出的 map 不会自指，
		// 但 Result 与 data 指向同一 map 的异常形态下会死循环）。
		addr := reflect.ValueOf(current.obj).Pointer()
		if visited[addr] {
			continue
		}
		visited[addr] = true
		out = append(out, current.obj)
		if current.depth >= traeCatalogScopeMaxDepth {
			continue
		}
		for _, key := range []string{"Result", "result", "data", "Data"} {
			if sub := traeMapKey(current.obj, key); sub != nil {
				queue = append(queue, node{obj: sub, depth: current.depth + 1})
			}
		}
	}
	return out
}

// traeCoerceMapSlice 把任意 JSON 数组形态转成对象切片（非对象条目按丢弃处理）。
func traeCoerceMapSlice(raw any) []map[string]any {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if entry, ok := item.(map[string]any); ok {
			out = append(out, entry)
		}
	}
	return out
}

// traeCatalogEntryString / Bool / NestedString / NestedInt64 是目录条目的宽容取值助手：
// 上游同一字段存在 snake_case 与 camelCase 两种下发记录，两种都认。
func traeCatalogEntryString(entry map[string]any, key string) string {
	return traeFirstStringKey(entry, key, traeSnakeToCamel(key))
}

// traeSnakeToCamel 把 config_switch 之类转成 configSwitch（已在目标键上则返回空串，
// 避免向取值助手传重复键）。
func traeSnakeToCamel(key string) string {
	if !strings.Contains(key, "_") {
		return ""
	}
	parts := strings.Split(key, "_")
	out := parts[0]
	for _, part := range parts[1:] {
		if part == "" {
			continue
		}
		out += strings.ToUpper(part[:1]) + part[1:]
	}
	if out == key {
		return ""
	}
	return out
}

// traeCatalogEntryBool 取布尔开关，返回 (值, 是否存在)。上游偶发以 "false"/0 下发，
// 这里一并宽容；但字段缺失必须回报 false-exists，否则「缺省视为启用」的纪律会被破坏。
func traeCatalogEntryBool(entry map[string]any, key string) (bool, bool) {
	for _, candidate := range []string{key, traeSnakeToCamel(key)} {
		if candidate == "" {
			continue
		}
		v, ok := entry[candidate]
		if !ok || v == nil {
			continue
		}
		switch typed := v.(type) {
		case bool:
			return typed, true
		case string:
			switch strings.ToLower(strings.TrimSpace(typed)) {
			case "true", "1", "yes":
				return true, true
			case "false", "0", "no":
				return false, true
			}
		default:
			if n, ok := traeToInt64(v); ok {
				return n != 0, true
			}
		}
	}
	return false, false
}

func traeCatalogEntryNestedString(entry map[string]any, objectKey, fieldKey string) string {
	for _, candidate := range []string{objectKey, traeSnakeToCamel(objectKey)} {
		if candidate == "" {
			continue
		}
		if sub := traeMapKey(entry, candidate); sub != nil {
			if v := traeCatalogEntryString(sub, fieldKey); v != "" {
				return v
			}
		}
	}
	return ""
}

func traeCatalogEntryNestedInt64(entry map[string]any, objectKey, fieldKey string) (int64, bool) {
	for _, candidate := range []string{objectKey, traeSnakeToCamel(objectKey)} {
		if candidate == "" {
			continue
		}
		if sub := traeMapKey(entry, candidate); sub != nil {
			for _, key := range []string{fieldKey, traeSnakeToCamel(fieldKey)} {
				if key == "" {
					continue
				}
				if n, ok := traeToInt64(sub[key]); ok {
					return n, true
				}
			}
		}
	}
	return 0, false
}

// traeJSONObjectKeys 列出顶层键名（供错误报文定位信封形态）；非对象返回 "<not-an-object>"。
func traeJSONObjectKeys(body []byte) string {
	var top map[string]any
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return "<not-an-object>"
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 12 {
		keys = append(keys[:12], fmt.Sprintf("+%d more", len(keys)-12))
	}
	return strings.Join(keys, ",")
}

// fetchTraeUpstreamModels 拉取 Trae 账号当前可用模型目录。
//
// 凭据处理与连接测试同语义：access_token 缺失/临近过期且有 refresh_token 时先换票；
// 上游 401 时再换一次并重试一次（本端点与聊天共用同一令牌，过期是常态而非异常）。
func (s *AccountTestService) fetchTraeUpstreamModels(ctx context.Context, account *Account) ([]string, map[string]UpstreamModelMetadata, error) {
	if s == nil || s.httpUpstream == nil {
		return nil, nil, newUpstreamModelSyncConfigError("Upstream HTTP client is not configured", nil)
	}
	if err := s.refreshTraeAccountTokenIfNeeded(ctx, account); err != nil {
		return nil, nil, err
	}

	baseURL := account.GetTraeBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		return nil, nil, newUpstreamModelSyncConfigError("Trae base URL is not configured", nil)
	}
	validatedBase, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, nil, newUpstreamModelSyncConfigError("Invalid Trae base URL", err)
	}
	targetURL := strings.TrimRight(validatedBase, "/") + traeModelListPath
	proxyURL := upstreamModelsProxyURL(account)

	fetch := func() (*http.Response, []byte, error) {
		creds := account.GetTraeCredentials()
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader([]byte(traeModelListBody)))
		if reqErr != nil {
			return nil, nil, newUpstreamModelSyncInternalError("Failed to build Trae model list request", reqErr)
		}
		req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
		applyTraeChatHeaders(req, creds, account)
		// 目录接口返回 JSON，不是 SSE：沿用聊天域头族但必须改 Accept。
		req.Header.Set("Accept", "application/json")
		resp, doErr := s.doUpstreamModelsRequest(req, proxyURL, account)
		if doErr != nil {
			return nil, nil, newUpstreamModelSyncUpstreamError("Failed to request Trae model list", doErr)
		}
		defer func() { _ = resp.Body.Close() }()
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, resolveModelsListReadLimit(s.cfg)+1))
		if readErr != nil {
			return resp, nil, newUpstreamModelSyncUpstreamError("Failed to read Trae model list", readErr)
		}
		return resp, body, nil
	}

	resp, body, err := fetch()
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		if refreshErr := s.refreshTraeAccountToken(ctx, account); refreshErr != nil {
			return nil, nil, newUpstreamModelSyncUpstreamError(
				fmt.Sprintf("Trae model list rejected (401): %s", traeTruncateForError(string(body))), refreshErr)
		}
		resp, body, err = fetch()
		if err != nil {
			return nil, nil, err
		}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, nil, &UpstreamModelSyncError{
			Kind:       UpstreamModelSyncErrorUpstream,
			Message:    fmt.Sprintf("Trae model list returned HTTP %d: %s", resp.StatusCode, traeTruncateForError(string(body))),
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("trae model list returned HTTP %d", resp.StatusCode),
		}
	}

	modelIDs, metadata, parseErr := parseTraeModelCatalog(body)
	if parseErr != nil {
		return nil, nil, newUpstreamModelSyncUpstreamError("Trae model list response was not usable", parseErr)
	}
	if len(modelIDs) == 0 {
		return nil, nil, newUpstreamModelSyncUpstreamError("Trae returned no schedulable models", nil)
	}
	return modelIDs, metadata, nil
}

// refreshTraeAccountTokenIfNeeded 同步前的预刷新（无 refresh_token 时不报错，
// 交由实际请求的 401 分支处理）。
func (s *AccountTestService) refreshTraeAccountTokenIfNeeded(ctx context.Context, account *Account) error {
	creds := account.GetTraeCredentials()
	if !traeTokenNeedsRefresh(creds, time.Now()) || strings.TrimSpace(creds.RefreshToken) == "" {
		if strings.TrimSpace(creds.AccessToken) == "" {
			return newUpstreamModelSyncConfigError("No Trae access token is available", nil)
		}
		return nil
	}
	if err := s.refreshTraeAccountToken(ctx, account); err != nil {
		// 有旧 token 就宽容放行（与聊天链路一致），完全无 token 才是配置错误。
		if strings.TrimSpace(account.GetTraeCredentials().AccessToken) == "" {
			return newUpstreamModelSyncUpstreamError("Failed to refresh Trae access token", err)
		}
	}
	return nil
}
