package service

// qoder_models.go Qoder 上游模型目录拉取：GET {algo2}/api/v2/model/list?Encode=1。
//
// 端点常量与 endpoints.ModelListURL 早已按取证写入 qoder.go，但一直没有生产调用点
// （只有 qoder_test.go 断言 URL 拼接），导致管理页「同步上游模型」按钮对 qoder 账号
// 必然失败。本文件完成接线。
//
// 响应契约（一手取证：与 CLI ModelCatalog.fetchFromRemote 同源）：顶层是「场景 → 模型
// 数组」的映射（chat / developer / assistant / experts / inline / quest / qwork / ...），
// 不是 data/list/models。单条模型对象的关键字段：
//   - key          —— 上游真正认的模型标识（形如 dmodel/gmodel/kmodel_latest），
//                     必须落库；聊天出站写 body.model_config.key + x-model-key 头都用它。
//   - display_name —— 展示名（DeepSeek-V4-Pro 等），**不能**直接当 ID 发上游：
//                     上游对非封闭 key 会静默降档（实测 credits=0、响应 model 恒为 auto）。
//   - enable       —— false 表示该场景已关闭，不可调。
//   - is_vl        —— 多模态（视觉输入）标记。
//
// 因此这里同时产出两套口径：models 用 key（对外可调 ID），metadata 用 display_name
// 作为展示名，并把 key→display_name 沉淀进账号快照，供后续展示与别名归一使用。
// "auto" 是路由入口而非真实模型，跳过（与 CLI 实现同口径）。
//
// 鉴权：与聊天完全同一套 COSY 签名头。GET 无请求体，故签名 body 取空串；
// Encode=1 是**请求体**编码版本开关（GET 无 body → 对该端点惰性），响应为明文 JSON。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// qoderModelListRequestKey 目录请求的 x-model-key 占位值：本端点不按模型定向，
// 但签名头族要求该头非空，取与聊天一致的路由入口 auto。
const qoderModelListRequestKey = "auto"

// qoderModelListScenes 是拉取目录时优先合并的场景键（决定输出顺序）。上游按场景下发
// 不同子集，少取一个场景就会漏模型；实测键集合会随版本增减，未知键在下面另行兜底。
var qoderModelListScenes = []string{
	"chat", "developer", "assistant", "experts", "inline", "quest", "qwork",
}

// qoderModelListEntry 是单条模型目录项（只取一手证据支持的字段）。
type qoderModelListEntry struct {
	Key          string `json:"key"`
	DisplayName  string `json:"display_name"`
	Enable       *bool  `json:"enable"`
	IsVL         bool   `json:"is_vl"`
	IsDefault    bool   `json:"is_default"`
	MaxInput     int64  `json:"max_input_tokens"`
	MaxOutput    int64  `json:"max_output_tokens"`
	Source       string `json:"source"`
	ThinkingMode any    `json:"thinking_config"`
	ContextMode  any    `json:"context_config"`
}

// parseQoderModelCatalog 解析场景映射形态的目录响应，按 key 去重合并。
// 输出顺序稳定：先已知场景（按 qoderModelListScenes 序）、再字典序排列的未知场景，
// 同一场景内保持上游数组序 —— 避免同一份上游数据两次解析产出不同顺序，
// 让管理页把「顺序变化」误报为「模型变更」。
func parseQoderModelCatalog(body []byte) ([]string, map[string]UpstreamModelMetadata, error) {
	var scenes map[string][]qoderModelListEntry
	if err := json.Unmarshal(body, &scenes); err != nil {
		return nil, nil, fmt.Errorf("parse qoder model catalog: %w", err)
	}
	if len(scenes) == 0 {
		return nil, nil, fmt.Errorf("parse qoder model catalog: response has no scene keys")
	}

	ordered := make([]string, 0, len(scenes))
	visited := make(map[string]struct{}, len(scenes))
	appendScene := func(scene string) {
		if _, done := visited[scene]; done {
			return
		}
		visited[scene] = struct{}{}
		ordered = append(ordered, scene)
	}
	for _, scene := range qoderModelListScenes {
		appendScene(scene)
	}
	unknown := make([]string, 0, len(scenes))
	for scene := range scenes {
		if _, listed := visited[scene]; !listed {
			unknown = append(unknown, scene)
		}
	}
	sort.Strings(unknown)
	for _, scene := range unknown {
		appendScene(scene)
	}

	modelIDs := make([]string, 0, len(scenes))
	metadata := make(map[string]UpstreamModelMetadata, len(scenes))
	for _, scene := range ordered {
		for _, entry := range scenes[scene] {
			key := strings.TrimSpace(entry.Key)
			// auto 是路由入口，不是可调模型；无 key 的条目上游自身也不认。
			if key == "" || key == "auto" {
				continue
			}
			if entry.Enable != nil && !*entry.Enable {
				continue
			}
			if _, ok := metadata[key]; ok {
				continue
			}
			displayName := strings.TrimSpace(entry.DisplayName)
			if displayName == "" {
				displayName = key
			}
			entryMeta := UpstreamModelMetadata{ID: key, DisplayName: displayName}
			if entry.IsVL {
				entryMeta.InputModalities = []string{"text", "image"}
			} else {
				entryMeta.InputModalities = []string{"text"}
			}
			if entry.MaxInput > 0 {
				entryMeta.ContextWindow = entry.MaxInput
				entryMeta.MaxContextWindow = entry.MaxInput
			}
			entryMeta.MaxOutputTokens = entry.MaxOutput
			// thinking_config 只在我们能拿到具体 effort 时才提升为 reasoning=true：
			// upstreamModelMetadataIsComplete 对「reasoning=true 但无 levels」判为不完整，
			// 所以上游没给档位时宁可不标（保留 nil），否则每轮同步都会回一条
			// metadata_partial 告警且快照永远写不进去（也不能谎报 reasoning=false）。
			if levels := qoderThinkingEfforts(entry.ThinkingMode); len(levels) > 0 {
				supportsReasoning := true
				entryMeta.Reasoning = &supportsReasoning
				entryMeta.SupportedReasoningLevels = levels
				entryMeta.DefaultReasoningLevel = levels[0]
			}
			metadata[key] = entryMeta
			modelIDs = append(modelIDs, key)
		}
	}
	if len(modelIDs) == 0 {
		return nil, nil, fmt.Errorf("parse qoder model catalog: no enabled models")
	}
	return modelIDs, metadata, nil
}

// qoderThinkingEfforts 从上游 thinking_config 里提取推理档位（字段形态随版本变化：
// {"disabled":...,"enabled":{"efforts":[...]}} 与扁平数组都出现过），故不假设结构，
// 递归找出能归一为合法档位的字符串。返回值经 normalizeReasoningLevels 去重保序。
func qoderThinkingEfforts(value any) []string {
	if value == nil {
		return nil
	}
	levels := make([]string, 0, 4)
	var walk func(node any)
	walk = func(node any) {
		switch typed := node.(type) {
		case string:
			levels = append(levels, typed)
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(value)
	return normalizeReasoningLevels(levels)
}

// fetchQoderUpstreamModels 拉取 Qoder 账号当前可用模型目录。
//
// 凭据处理对齐 testQoderAccountConnection：PAT/刷新令牌先换 jobToken、uid 缺失先做
// 身份自愈（COSY uid 为登录态校验要素，伪 uid 会被上游判 105 Login expired）。
func (s *AccountTestService) fetchQoderUpstreamModels(ctx context.Context, account *Account) ([]string, map[string]UpstreamModelMetadata, error) {
	if s == nil || s.httpUpstream == nil {
		return nil, nil, newUpstreamModelSyncConfigError("Upstream HTTP client is not configured", nil)
	}

	creds := account.GetQoderCredentials()
	if qoderNeedsJobTokenExchange(creds) {
		if err := s.exchangeQoderJobTokenForTest(ctx, account); err != nil {
			creds = account.GetQoderCredentials()
			if strings.TrimSpace(creds.AccessToken) == "" {
				return nil, nil, newUpstreamModelSyncUpstreamError("Failed to exchange Qoder jobToken", err)
			}
		} else {
			creds = account.GetQoderCredentials()
		}
	}
	creds = qoderHealIdentity(ctx, s.accountRepo, account, creds, s.qoderUserinfoFetch)
	securityToken := strings.TrimSpace(creds.AccessToken)
	if securityToken == "" {
		return nil, nil, newUpstreamModelSyncConfigError("No Qoder access token is available", nil)
	}

	endpoints := resolveQoderEndpoints(account.GetQoderRealm(), account.GetQoderBaseURL())
	validatedBase, err := s.validateUpstreamBaseURL(endpoints.Algo2Base)
	if err != nil {
		return nil, nil, newUpstreamModelSyncConfigError("Invalid Qoder base URL", err)
	}
	targetURL := strings.TrimRight(validatedBase, "/") + qoderModelListPath

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, nil, newUpstreamModelSyncInternalError("Failed to build Qoder model list request", err)
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	// 复用聊天的 COSY 头族：GET 无请求体，签名 body 为空串；accept 必须是 JSON
	// （聊天端点要求的 text/event-stream 不适用于本端点）。
	meta := &QoderBodyMeta{RequestID: uuid.NewString(), ModelKey: qoderModelListRequestKey}
	if headerErr := applyQoderChatHeaders(req, creds, securityToken, meta, nil, targetURL, "application/json"); headerErr != nil {
		return nil, nil, newUpstreamModelSyncInternalError("Failed to sign Qoder model list request", headerErr)
	}
	req.Header.Set("accept-encoding", "identity")

	resp, err := s.doUpstreamModelsRequest(req, upstreamModelsProxyURL(account), account)
	if err != nil {
		return nil, nil, newUpstreamModelSyncUpstreamError("Failed to request Qoder model list", err)
	}
	defer func() { _ = resp.Body.Close() }()

	bodyLimit := resolveModelsListReadLimit(s.cfg)
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
	if readErr != nil {
		return nil, nil, newUpstreamModelSyncUpstreamError("Failed to read Qoder model list", readErr)
	}
	if int64(len(body)) > bodyLimit {
		return nil, nil, newUpstreamModelSyncUpstreamError("Qoder model list response is too large", fmt.Errorf("response exceeds %d bytes", bodyLimit))
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, nil, &UpstreamModelSyncError{
			Kind:       UpstreamModelSyncErrorUpstream,
			Message:    fmt.Sprintf("Qoder model list returned HTTP %d: %s", resp.StatusCode, qoderTruncateReason(string(body), 200)),
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("qoder model list returned HTTP %d", resp.StatusCode),
		}
	}

	ids, metadata, parseErr := parseQoderModelCatalog(body)
	if parseErr != nil {
		return nil, nil, newUpstreamModelSyncUpstreamError("Qoder model list response was not usable", parseErr)
	}
	return ids, metadata, nil
}
