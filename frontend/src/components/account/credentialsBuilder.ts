import { openAIPlanTypeLabel } from '@/utils/planType'

export function applyInterceptWarmup(
  credentials: Record<string, unknown>,
  enabled: boolean,
  mode: 'create' | 'edit'
): void {
  if (enabled) {
    credentials.intercept_warmup_requests = true
  } else if (mode === 'edit') {
    delete credentials.intercept_warmup_requests
  }
}

export const ANTIGRAVITY_PROJECT_ID_CREDENTIAL_KEY = 'antigravity_project_id'

export function applyAntigravityProjectID(
  credentials: Record<string, unknown>,
  projectId: string,
  mode: 'create' | 'edit'
): void {
  const trimmed = projectId.trim()
  if (trimmed) {
    credentials[ANTIGRAVITY_PROJECT_ID_CREDENTIAL_KEY] = trimmed
  } else if (mode === 'edit') {
    delete credentials[ANTIGRAVITY_PROJECT_ID_CREDENTIAL_KEY]
  }
}

// ========== 上游倍率探测资格 ==========

/**
 * 上游倍率探测资格（与后端 IsUpstreamBillingProbeIdentity 保持一致，白名单）。
 * 探测取数直读 credentials.api_key 并请求 `{base_url}/v1/sub2api/billing`，
 * 因此只有持静态 API Key 的平台可用；WorkBuddy 凭据为 access/refresh token，
 * 后端已把它移出资格名单——前端所有探测入口（创建/编辑/批量编辑/列表手动探测）
 * 必须共用本判定，否则提交或点击会被后端以 probe 资格不符拒绝（400）。
 */
export function isUpstreamBillingProbeCapable(platform: string, type: string): boolean {
  if (type !== 'apikey') return false
  return (
    platform === 'openai' ||
    platform === 'anthropic' ||
    platform === 'gemini' ||
    platform === 'antigravity' ||
    platform === 'grok' ||
    platform === 'kimi' ||
    platform === 'zhipu' ||
    platform === 'deepseek' ||
    platform === 'minimax' ||
    platform === 'opencode_go'
  )
}

// ========== 请求头覆写（API-key 平台 + grok 的 api_key/oauth 账号） ==========

export const HEADER_OVERRIDE_ENABLED_CREDENTIAL_KEY = 'header_override_enabled'
export const HEADER_OVERRIDES_CREDENTIAL_KEY = 'header_overrides'

export interface HeaderOverrideRow {
  name: string
  value: string
}

/** 请求头覆写资格（与后端 IsHeaderOverrideEligible 保持一致） */
export function isHeaderOverrideCapable(platform: string, type: string): boolean {
  if (
    platform === 'anthropic' ||
    platform === 'openai' ||
    platform === 'kimi' ||
    platform === 'zhipu' ||
    platform === 'deepseek' ||
    platform === 'minimax' ||
    platform === 'opencode_go'
  ) {
    return type === 'apikey'
  }
  if (platform === 'grok') {
    return type === 'apikey' || type === 'oauth'
  }
  return false
}

/** 禁止覆写的请求头（与后端 headerOverrideBlockedNames 保持一致） */
const HEADER_OVERRIDE_BLOCKED_NAMES = new Set([
  'host',
  'content-length',
  'content-type',
  'transfer-encoding',
  'connection',
  'keep-alive',
  'proxy-authenticate',
  'proxy-authorization',
  'proxy-connection',
  'te',
  'trailer',
  'upgrade',
  'authorization',
  'x-api-key',
  'x-goog-api-key',
  'cookie',
  'accept-encoding',
  'sec-websocket-key',
  'sec-websocket-version',
  'sec-websocket-extensions',
  'sec-websocket-protocol',
  'sec-websocket-accept',
  'session_id',
  'conversation_id',
  'x-codex-turn-state',
  'x-codex-turn-metadata',
  'chatgpt-account-id',
  'x-claude-code-session-id',
  'x-client-request-id',
  'x-grok-conv-id'
])

/** RFC 7230 token：合法的 HTTP header 名称字符集 */
const HEADER_NAME_PATTERN = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/

function isValidHeaderOverrideName(name: string): boolean {
  return HEADER_NAME_PATTERN.test(name)
}

/** 与后端 maxHeaderOverride* 常量保持一致 */
const HEADER_OVERRIDE_MAX_ENTRIES = 64
const HEADER_OVERRIDE_MAX_NAME_LENGTH = 200
const HEADER_OVERRIDE_MAX_VALUE_LENGTH = 8192

/** header value 不允许包含控制字符（与后端 httpguts.ValidHeaderFieldValue 对齐） */
// eslint-disable-next-line no-control-regex
const HEADER_VALUE_INVALID_PATTERN = /[\x00-\x08\x0a-\x1f\x7f]/

/** 长度限制按 UTF-8 字节计（与后端 Go len() 对齐，避免多字节值前端放行后端 400） */
const HEADER_TEXT_ENCODER = new TextEncoder()
function utf8ByteLength(value: string): number {
  return HEADER_TEXT_ENCODER.encode(value).length
}

/**
 * 校验请求头覆写行，返回首个错误的 i18n key（无错误返回 null）。
 * 名称为空但值非空 → invalidName；名称非法 → invalidName；
 * 禁止覆写 → blockedName；大小写不敏感重名 → duplicateName；
 * 值含控制字符或超长 → invalidValue；条目过多 → tooManyEntries。
 */
export function validateHeaderOverrideRows(
  rows: HeaderOverrideRow[]
): 'invalidName' | 'blockedName' | 'duplicateName' | 'invalidValue' | 'tooManyEntries' | null {
  const seen = new Set<string>()
  for (const row of rows) {
    const name = row.name.trim()
    const value = row.value.trim()
    if (!name) {
      if (value) return 'invalidName'
      continue
    }
    if (!isValidHeaderOverrideName(name) || name.length > HEADER_OVERRIDE_MAX_NAME_LENGTH) {
      return 'invalidName'
    }
    const lower = name.toLowerCase()
    if (HEADER_OVERRIDE_BLOCKED_NAMES.has(lower)) return 'blockedName'
    if (seen.has(lower)) return 'duplicateName'
    if (
      HEADER_VALUE_INVALID_PATTERN.test(value) ||
      utf8ByteLength(value) > HEADER_OVERRIDE_MAX_VALUE_LENGTH
    ) {
      return 'invalidValue'
    }
    seen.add(lower)
  }
  if (seen.size > HEADER_OVERRIDE_MAX_ENTRIES) return 'tooManyEntries'
  return null
}

/** 行数组 → credentials 存储对象（名称小写化，丢弃空行） */
export function buildHeaderOverridesObject(rows: HeaderOverrideRow[]): Record<string, string> {
  const result: Record<string, string> = {}
  for (const row of rows) {
    const name = row.name.trim().toLowerCase()
    if (!name) continue
    result[name] = row.value.trim()
  }
  return result
}

/** credentials 存储对象 → 行数组（按名称排序保证稳定展示） */
export function splitHeaderOverridesObject(record: unknown): HeaderOverrideRow[] {
  if (!record || typeof record !== 'object' || Array.isArray(record)) return []
  return Object.entries(record as Record<string, unknown>)
    .filter(([, value]) => typeof value === 'string')
    .map(([name, value]) => ({ name, value: value as string }))
    .sort((a, b) => a.name.localeCompare(b.name))
}

/**
 * 解析粘贴的 JSON 文本为请求头覆写行。
 * 仅接受扁平 JSON 对象；值允许 string/number/boolean（统一转字符串），
 * 其余类型或非对象输入返回 null 表示格式非法。键为空白的条目直接丢弃。
 */
export function parseHeaderOverridesJson(text: string): HeaderOverrideRow[] | null {
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch {
    return null
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return null
  const rows: HeaderOverrideRow[] = []
  for (const [rawName, rawValue] of Object.entries(parsed as Record<string, unknown>)) {
    const name = rawName.trim()
    if (!name) continue
    if (
      typeof rawValue !== 'string' &&
      typeof rawValue !== 'number' &&
      typeof rawValue !== 'boolean'
    ) {
      return null
    }
    rows.push({ name, value: String(rawValue).trim() })
  }
  return rows.sort((a, b) => a.name.localeCompare(b.name))
}

/** 请求头覆写行 → 便于迁移/备份的 JSON 文本（跳过名称为空的占位行） */
export function serializeHeaderOverrideRows(rows: HeaderOverrideRow[]): string {
  const record: Record<string, string> = {}
  for (const row of rows) {
    const name = row.name.trim()
    if (!name) continue
    record[name] = row.value.trim()
  }
  return JSON.stringify(record, null, 2)
}

// ========== Grok 自定义转发地址（base_url 仅改写转发端点，凭证生命周期不受影响） ==========

/** OAuth 账号建号/刷新默认写入的 CLI 网关 host——只有它视同"未定制"。 */
const GROK_DEFAULT_GATEWAY_HOST = 'cli-chat-proxy.grok.com'

/**
 * 判断 Grok 账号存储的 base_url 是否为主动指定的上游端点。
 * 运营方可在官方 API / 区域 API / 第三方转发地址之间手动切换（应对单端点
 * 不可用），这些值都必须回显（开关开启 + 显示地址）。仅默认 CLI 网关
 * （建号/刷新自动写入）、空值与无法解析的值视为"未定制"（与后端
 * GetGrokBaseURL 的回落语义对齐），用于 OAuth 账号编辑时决定开关初始状态。
 */
export function isCustomGrokBaseUrl(value: unknown): boolean {
  if (typeof value !== 'string') return false
  const trimmed = value.trim()
  if (!trimmed) return false
  let parsed: URL
  try {
    parsed = new URL(trimmed)
  } catch {
    return false
  }
  return parsed.hostname.toLowerCase() !== GROK_DEFAULT_GATEWAY_HOST
}

export interface GrokBaseUrlPreset {
  /** i18n 子键：admin.accounts.grokCustomBaseUrl.presets.<labelKey> */
  labelKey?: 'cli' | 'official'
  /** 字面标签（如区域标识 us-east-1），专有名词不参与 i18n */
  label?: string
  url: string
}

/**
 * Grok 快捷端点（仅供快速填充，输入框仍可自由填写任意转发地址）。
 * 官方端点偶发不可用时，运营方靠这组预设在端点间手动切换。
 */
export const GROK_BASE_URL_PRESETS: GrokBaseUrlPreset[] = [
  { labelKey: 'cli', url: 'https://cli-chat-proxy.grok.com/v1' },
  { labelKey: 'official', url: 'https://api.x.ai/v1' },
  { label: 'us-east-1', url: 'https://us-east-1.api.x.ai/v1' },
  { label: 'us-west-2', url: 'https://us-west-2.api.x.ai/v1' },
  { label: 'eu-west-1', url: 'https://eu-west-1.api.x.ai/v1' }
]

// ========== 国产供应商（Kimi / Zhipu / DeepSeek）base_url 预设 ==========
// 与后端 service/domain_constants.go 的默认 base url 保持一致。
// 账号类型（payg 按量付费 / coding 编程套餐）决定额度监控方式；
// API 协议（chat_completions / anthropic / responses）决定转发端点与格式，
// 两者正交。同协议请求零转换直通，跨协议组合才走转换链。

export type CnAccountMode = 'payg' | 'coding'
export type OpenCodeAccountMode = 'zen' | 'go'
export type CnProviderPlatform = 'kimi' | 'zhipu' | 'deepseek' | 'minimax'

/** deepseek / kimi / minimax 支持原生 responses；adaptive 会按入站协议选择原生端点。 */
export type CnApiProtocol = 'adaptive' | 'chat_completions' | 'anthropic' | 'responses'
export type CnNativeApiProtocol = Exclude<CnApiProtocol, 'adaptive'>

export function isCNProviderPlatform(platform: string): platform is CnProviderPlatform {
  return platform === 'kimi' || platform === 'zhipu' || platform === 'deepseek' || platform === 'minimax'
}

/** DeepSeek、Kimi 与 MiniMax 提供原生 Responses 端点。 */
export function cnSupportsNativeResponses(platform: string): boolean {
  return platform === 'deepseek' || platform === 'kimi' || platform === 'minimax' || platform === 'opencode_go'
}

export const OPENCODE_GO_BASE_URL = 'https://opencode.ai/zen/go/v1'
export const OPENCODE_GO_ANTHROPIC_BASE_URL = 'https://opencode.ai/zen/go'
export const OPENCODE_ZEN_BASE_URL = 'https://opencode.ai/zen/v1'
export const OPENCODE_ZEN_ANTHROPIC_BASE_URL = 'https://opencode.ai/zen'

export function isOpenCodeGoPlatform(platform: string): boolean {
  return platform === 'opencode_go'
}

export const OPENCODE_GO_PROTOCOL_RULES_KEY = 'protocol_rules'

export interface OpenCodeGoProtocolRule {
  pattern: string
  protocol: CnNativeApiProtocol
}

export const DEFAULT_OPENCODE_GO_PROTOCOL_RULES: OpenCodeGoProtocolRule[] = [
  { pattern: 'grok-*', protocol: 'responses' },
  { pattern: 'gpt-*', protocol: 'responses' },
  { pattern: 'muse-spark-*', protocol: 'responses' },
  { pattern: 'minimax-*', protocol: 'anthropic' },
  { pattern: 'qwen*', protocol: 'anthropic' }
]

export const DEFAULT_OPENCODE_ZEN_PROTOCOL_RULES: OpenCodeGoProtocolRule[] = [
  { pattern: 'grok-*', protocol: 'responses' },
  { pattern: 'gpt-*', protocol: 'responses' },
  { pattern: 'muse-spark-*', protocol: 'responses' },
  { pattern: 'claude-*', protocol: 'anthropic' },
  { pattern: 'qwen*', protocol: 'anthropic' }
]

export function resolveOpenCodeAccountMode(value: unknown): OpenCodeAccountMode {
  return value === 'zen' ? 'zen' : 'go'
}

export function defaultOpenCodeProtocolRules(mode: OpenCodeAccountMode = 'go'): OpenCodeGoProtocolRule[] {
  return mode === 'zen' ? DEFAULT_OPENCODE_ZEN_PROTOCOL_RULES : DEFAULT_OPENCODE_GO_PROTOCOL_RULES
}

export function cloneOpenCodeGoProtocolRules(
  rules: OpenCodeGoProtocolRule[] = DEFAULT_OPENCODE_GO_PROTOCOL_RULES
): OpenCodeGoProtocolRule[] {
  return rules.map(rule => ({ pattern: rule.pattern, protocol: rule.protocol }))
}

function isNativeOpenCodeGoProtocol(value: unknown): value is CnNativeApiProtocol {
  return value === 'chat_completions' || value === 'anthropic' || value === 'responses'
}

export function parseOpenCodeGoProtocolRules(raw: unknown): OpenCodeGoProtocolRule[] | null {
  if (raw == null) return null
  if (!Array.isArray(raw)) return cloneOpenCodeGoProtocolRules()
  const rules: OpenCodeGoProtocolRule[] = []
  for (const item of raw) {
    if (!item || typeof item !== 'object') continue
    const pattern = typeof (item as { pattern?: unknown }).pattern === 'string'
      ? (item as { pattern: string }).pattern.trim()
      : ''
    const protocol = (item as { protocol?: unknown }).protocol
    if (!pattern || !isNativeOpenCodeGoProtocol(protocol)) continue
    rules.push({ pattern, protocol })
  }
  return rules
}

export function applyOpenCodeGoProtocolRules(
  credentials: Record<string, unknown>,
  rules: OpenCodeGoProtocolRule[],
  mode: 'create' | 'edit'
): void {
  const serialized = rules
    .map(rule => ({
      pattern: rule.pattern.trim().toLowerCase(),
      protocol: rule.protocol
    }))
    .filter(rule => rule.pattern.length > 0 && isNativeOpenCodeGoProtocol(rule.protocol))
  if (serialized.length > 0 || mode === 'edit') {
    credentials[OPENCODE_GO_PROTOCOL_RULES_KEY] = serialized
  }
}

// ========== WorkBuddy（双域 APIKey 网关） ==========
// 上游为 OpenAI Chat Completions 协议，双域（国内 copilot.tencent.com /
// 国际 www.workbuddy.ai）。账号类型为 APIKey，凭据字段（snake_case，与后端约定）：
// access_token / refresh_token / realm / uid / enterprise_id / device_token / base_url。
// 不走国产多协议供应商路径（无 account_mode / api_protocol）。

export type WorkBuddyRealm = 'cn' | 'global'

export const WORKBUDDY_CN_BASE_URL = 'https://copilot.tencent.com'
// 国际域必须带 www：裸域 workbuddy.ai 在上游边缘会被 301 跳转（Location 指向 www），
// Go 客户端跟随 301 会把 POST 改写为 GET 并丢弃请求体 → 上游返回 "404 page not found"。
// 该裸域曾被本文件预填入库导致「国际版无法使用模型」（2026-09 事件）；后端另有
// 存量账号的读取侧兜底归一化（normalizeWorkbuddyStoredBaseURL）。
export const WORKBUDDY_GLOBAL_BASE_URL = 'https://www.workbuddy.ai'

/** realm 下拉选项（labelKey 对应 i18n admin.accounts.workbuddy.realm.<key>）。 */
export const WORKBUDDY_REALM_OPTIONS: ReadonlyArray<{ value: WorkBuddyRealm; labelKey: 'cn' | 'global' }> = [
  { value: 'cn', labelKey: 'cn' },
  { value: 'global', labelKey: 'global' }
]

export function isWorkBuddyPlatform(platform: string): boolean {
  return platform === 'workbuddy'
}

/** realm → 默认 base url（高级覆盖项留空时使用）。 */
export function defaultWorkBuddyBaseUrl(realm: WorkBuddyRealm = 'cn'): string {
  return realm === 'global' ? WORKBUDDY_GLOBAL_BASE_URL : WORKBUDDY_CN_BASE_URL
}

export function resolveWorkBuddyRealm(value: unknown): WorkBuddyRealm {
  return value === 'global' ? 'global' : 'cn'
}

export interface WorkBuddyCredentialFields {
  accessToken: string
  refreshToken: string
  realm: WorkBuddyRealm
  uid: string
  enterpriseId: string
  deviceToken: string
  baseUrl: string
}

/**
 * 组装 WorkBuddy 凭据（snake_case 键，与后端约定一致）。
 * 空字段不写入：密钥类字段（access_token / refresh_token / device_token）
 * 留空表示沿用现有值；uid / enterprise_id 的清空删除由调用方处理（全量替换语义）。
 */
export function buildWorkbuddyCredentials(fields: WorkBuddyCredentialFields): Record<string, unknown> {
  const credentials: Record<string, unknown> = {}
  const accessToken = fields.accessToken.trim()
  const refreshToken = fields.refreshToken.trim()
  const uid = fields.uid.trim()
  const enterpriseId = fields.enterpriseId.trim()
  const deviceToken = fields.deviceToken.trim()
  const baseUrl = fields.baseUrl.trim()

  if (accessToken) credentials.access_token = accessToken
  if (refreshToken) credentials.refresh_token = refreshToken
  if (deviceToken) credentials.device_token = deviceToken
  credentials.realm = fields.realm
  if (uid) credentials.uid = uid
  if (enterpriseId) credentials.enterprise_id = enterpriseId
  if (baseUrl) credentials.base_url = baseUrl
  return credentials
}

/** WorkBuddy 凭据校验：access_token / refresh_token 至少填一个。 */
export function validateWorkbuddyCredentials(accessToken: string, refreshToken: string): boolean {
  return Boolean(accessToken.trim() || refreshToken.trim())
}

// ========== Qoder（双域 chat 网关） ==========
// 上游为 OpenAI Chat Completions 协议，双域（国内 gateway.qoder.com.cn /
// 国际 api1.qoder.sh）。凭据字段（snake_case，与后端约定）：
// access_token / refresh_token / expires_at / device_token / uid / realm /
// base_url / nickname。凭据里存 base_url 供后端覆盖；不走国产多协议供应商路径。

export type QoderRealm = 'cn' | 'global'

export const QODER_CN_BASE_URL = 'https://gateway.qoder.com.cn'
export const QODER_GLOBAL_BASE_URL = 'https://api1.qoder.sh'

/** realm 下拉选项（labelKey 对应 i18n admin.accounts.qoder.realm.<key>）。 */
export const QODER_REALM_OPTIONS: ReadonlyArray<{ value: QoderRealm; labelKey: 'cn' | 'global' }> = [
  { value: 'cn', labelKey: 'cn' },
  { value: 'global', labelKey: 'global' }
]

/** PAT（在 Qoder IDE 设置生成的 pt- 令牌）或 dt- / drt- 设备令牌可直接填入 access_token。 */
export function isQoderPlatform(platform: string): boolean {
  return platform === 'qoder'
}

/** realm → 默认 base url（高级覆盖项留空时使用）。 */
export function defaultQoderBaseUrl(realm: QoderRealm = 'cn'): string {
  return realm === 'global' ? QODER_GLOBAL_BASE_URL : QODER_CN_BASE_URL
}

export function resolveQoderRealm(value: unknown): QoderRealm {
  return value === 'global' ? 'global' : 'cn'
}

export interface QoderCredentialFields {
  accessToken: string
  refreshToken: string
  expiresAt: string
  deviceToken: string
  uid: string
  realm: QoderRealm
  baseUrl: string
  nickname: string
}

/**
 * 组装 Qoder 凭据（snake_case 键，与后端约定一致）。
 * 空字段不写入：密钥类字段（access_token / refresh_token / device_token）
 * 留空表示沿用现有值；uid / nickname 的清空删除由调用方处理（全量替换语义）。
 */
export function buildQoderCredentials(fields: QoderCredentialFields): Record<string, unknown> {
  const credentials: Record<string, unknown> = {}
  const accessToken = fields.accessToken.trim()
  const refreshToken = fields.refreshToken.trim()
  const expiresAt = fields.expiresAt.trim()
  const deviceToken = fields.deviceToken.trim()
  const uid = fields.uid.trim()
  const baseUrl = fields.baseUrl.trim()
  const nickname = fields.nickname.trim()

  if (accessToken) credentials.access_token = accessToken
  if (refreshToken) credentials.refresh_token = refreshToken
  if (expiresAt) {
    const parsed = Number(expiresAt)
    if (Number.isFinite(parsed)) credentials.expires_at = parsed
  }
  if (deviceToken) credentials.device_token = deviceToken
  credentials.realm = fields.realm
  if (uid) credentials.uid = uid
  if (baseUrl) credentials.base_url = baseUrl
  if (nickname) credentials.nickname = nickname
  return credentials
}

/**
 * Qoder 凭据校验：access_token 必填（也接受以 dt- / drt- / pt- 开头的设备/采购令牌）。
 * 与 validateWorkbuddyCredentials 同风格：返回 boolean，不抛错。
 */
export function validateQoderCredentials(accessToken: string): boolean {
  return Boolean(accessToken.trim())
}

// ========== Trae（字节 Trae IDE 代理，双域固定协议平台） ==========
// 上游为 OpenAI 兼容 Chat Completions 协议，双域（国内 trae.cn / 国际 trae.ai）。
// 账号类型为 APIKey，凭据字段（snake_case，与后端 NormalizeTraeCredentials 约定）：
// access_token / refresh_token / realm / uid / device_id / machine_id / base_url /
// billing_base_url / ide_version_code。与 WorkBuddy / Qoder 同属固定协议平台：
// 不写 account_mode / api_protocol（不走国产多协议供应商路径）。

export type TraeRealm = 'cn' | 'global'

// 这里预填的是**聊天域**端点（表单的 base_url 会写入 credentials.base_url，后端
// GetTraeBaseURL 优先取用它转发对话），与后端 DefaultTraeBaseURL /
// DefaultTraeGlobalBaseURL 逐字一致。积分/签到走 UG 域、换票走 OAuth 域，都是
// 另外的主机，由后端按 realm 自己推导，不得拿本值去覆盖（见 billing_base_url）。
export const TRAE_CN_BASE_URL = 'https://trae-api-cn.mchost.guru'
export const TRAE_GLOBAL_BASE_URL = 'https://a0ai-api-sg.byteintlapi.com'

/** realm 下拉选项（labelKey 对应 i18n admin.accounts.trae.realm.<key>）。 */
export const TRAE_REALM_OPTIONS: ReadonlyArray<{ value: TraeRealm; labelKey: 'cn' | 'global' }> = [
  { value: 'cn', labelKey: 'cn' },
  { value: 'global', labelKey: 'global' }
]

export function isTraePlatform(platform: string): boolean {
  return platform === 'trae'
}

/** realm → 默认 base url（高级覆盖项留空时使用）。 */
export function defaultTraeBaseUrl(realm: TraeRealm = 'cn'): string {
  return realm === 'global' ? TRAE_GLOBAL_BASE_URL : TRAE_CN_BASE_URL
}

export function resolveTraeRealm(value: unknown): TraeRealm {
  return value === 'global' ? 'global' : 'cn'
}

export interface TraeCredentialFields {
  accessToken: string
  refreshToken: string
  realm: TraeRealm
  uid: string
  deviceId: string
  machineId: string
  baseUrl: string
  billingBaseUrl: string
  ideVersionCode: string
  /**
   * 凭据到期时刻（epoch 秒）。Trae 的 refreshToken 过期后**无法自动续期**（必须
   * 人工重登 Trae 客户端重取凭据），故换票响应里的 TokenExpireAt / RefreshExpireAt
   * 必须随凭据入库，否则管理页永远看不到「还能用多久」，账号会静默变成不可用。
   */
  expiresAt?: number | null
  refreshExpiresAt?: number | null
}

/**
 * 组装 Trae 凭据（snake_case 键，与后端约定一致）。
 * 空字段不写入：密钥类字段（access_token / refresh_token）留空表示沿用现有值；
 * uid / device_id / machine_id 的清空删除由调用方处理（全量替换语义）。
 */
export function buildTraeCredentials(fields: TraeCredentialFields): Record<string, unknown> {
  const credentials: Record<string, unknown> = {}
  const accessToken = fields.accessToken.trim()
  const refreshToken = fields.refreshToken.trim()
  const uid = fields.uid.trim()
  const deviceId = fields.deviceId.trim()
  const machineId = fields.machineId.trim()
  const baseUrl = fields.baseUrl.trim()
  const billingBaseUrl = fields.billingBaseUrl.trim()
  const ideVersionCode = fields.ideVersionCode.trim()

  if (accessToken) credentials.access_token = accessToken
  if (refreshToken) credentials.refresh_token = refreshToken
  credentials.realm = fields.realm
  if (uid) credentials.uid = uid
  if (deviceId) credentials.device_id = deviceId
  if (machineId) credentials.machine_id = machineId
  if (baseUrl) credentials.base_url = baseUrl
  if (billingBaseUrl) credentials.billing_base_url = billingBaseUrl
  if (ideVersionCode) credentials.ide_version_code = ideVersionCode
  // 到期时刻：仅正数写入（0/null/undefined 表示未知，不得写 0 冒充「已过期」）。
  if (typeof fields.expiresAt === 'number' && fields.expiresAt > 0) {
    credentials.expires_at = fields.expiresAt
  }
  if (typeof fields.refreshExpiresAt === 'number' && fields.refreshExpiresAt > 0) {
    credentials.refresh_expires_at = fields.refreshExpiresAt
  }
  return credentials
}

/** Trae 凭据校验：access_token / refresh_token 至少填一个（与后端一致）。 */
export function validateTraeCredentials(accessToken: string, refreshToken: string): boolean {
  return Boolean(accessToken.trim() || refreshToken.trim())
}

export function isMultiProtocolApiKeyPlatform(platform: string): boolean {
  return platform === 'kimi' || platform === 'zhipu' || platform === 'deepseek' || platform === 'minimax' || platform === 'opencode_go'
}

export interface CnBaseUrlPreset {
  mode: CnAccountMode
  protocol: CnApiProtocol
  /** 专有名词，不参与 i18n */
  label: string
  url: string
}

/** 各供应商按账号类型 × API 协议分档的快捷端点（点击快速填充，输入框仍可自由填写）。 */
export const CN_BASE_URL_PRESETS: Record<CnProviderPlatform, CnBaseUrlPreset[]> = {
  kimi: [
    { mode: 'payg', protocol: 'chat_completions', label: 'Moonshot', url: 'https://api.moonshot.cn/v1' },
    { mode: 'payg', protocol: 'anthropic', label: 'Moonshot Anthropic', url: 'https://api.moonshot.cn/anthropic' },
    { mode: 'payg', protocol: 'responses', label: 'Moonshot Responses', url: 'https://api.moonshot.cn/v1' },
    { mode: 'coding', protocol: 'chat_completions', label: 'Kimi For Coding', url: 'https://api.kimi.com/coding/v1' },
    { mode: 'coding', protocol: 'anthropic', label: 'Kimi Coding Anthropic', url: 'https://api.kimi.com/coding' },
    { mode: 'coding', protocol: 'responses', label: 'Kimi Coding Responses', url: 'https://api.kimi.com/coding/v1' }
  ],
  zhipu: [
    { mode: 'payg', protocol: 'chat_completions', label: 'GLM PaaS', url: 'https://open.bigmodel.cn/api/paas/v4' },
    { mode: 'payg', protocol: 'anthropic', label: 'GLM Anthropic', url: 'https://open.bigmodel.cn/api/anthropic' },
    { mode: 'coding', protocol: 'chat_completions', label: 'GLM Coding', url: 'https://open.bigmodel.cn/api/coding/paas/v4' },
    { mode: 'coding', protocol: 'anthropic', label: 'GLM Coding Anthropic', url: 'https://open.bigmodel.cn/api/anthropic' }
  ],
  deepseek: [
    { mode: 'payg', protocol: 'chat_completions', label: 'DeepSeek', url: 'https://api.deepseek.com' },
    { mode: 'payg', protocol: 'anthropic', label: 'DeepSeek Anthropic', url: 'https://api.deepseek.com/anthropic' },
    { mode: 'payg', protocol: 'responses', label: 'DeepSeek Responses', url: 'https://api.deepseek.com' }
  ],
  minimax: [
    { mode: 'payg', protocol: 'chat_completions', label: 'MiniMax CN', url: 'https://api.minimaxi.com/v1' },
    { mode: 'payg', protocol: 'anthropic', label: 'MiniMax CN Anthropic', url: 'https://api.minimaxi.com/anthropic' },
    { mode: 'payg', protocol: 'responses', label: 'MiniMax CN Responses', url: 'https://api.minimaxi.com/v1' },
    { mode: 'payg', protocol: 'chat_completions', label: 'MiniMax Intl', url: 'https://api.minimax.io/v1' },
    { mode: 'payg', protocol: 'anthropic', label: 'MiniMax Intl Anthropic', url: 'https://api.minimax.io/anthropic' },
    { mode: 'payg', protocol: 'responses', label: 'MiniMax Intl Responses', url: 'https://api.minimax.io/v1' },
    { mode: 'coding', protocol: 'chat_completions', label: 'MiniMax Coding CN', url: 'https://api.minimaxi.com/v1' },
    { mode: 'coding', protocol: 'anthropic', label: 'MiniMax Coding CN Anthropic', url: 'https://api.minimaxi.com/anthropic' },
    { mode: 'coding', protocol: 'responses', label: 'MiniMax Coding CN Responses', url: 'https://api.minimaxi.com/v1' },
    { mode: 'coding', protocol: 'chat_completions', label: 'MiniMax Coding Intl', url: 'https://api.minimax.io/v1' },
    { mode: 'coding', protocol: 'anthropic', label: 'MiniMax Coding Intl Anthropic', url: 'https://api.minimax.io/anthropic' },
    { mode: 'coding', protocol: 'responses', label: 'MiniMax Coding Intl Responses', url: 'https://api.minimax.io/v1' }
  ]
}

/** 返回指定供应商 + 账号类型 + API 协议的默认 base url。 */
export function defaultCNBaseUrl(
  platform: string,
  mode: CnAccountMode | OpenCodeAccountMode,
  protocol: CnApiProtocol = 'chat_completions'
): string {
  if (protocol === 'anthropic') {
    switch (platform) {
      case 'kimi':
        return mode === 'coding' ? 'https://api.kimi.com/coding' : 'https://api.moonshot.cn/anthropic'
      case 'zhipu':
        return 'https://open.bigmodel.cn/api/anthropic'
      case 'deepseek':
        return 'https://api.deepseek.com/anthropic'
      case 'minimax':
        return 'https://api.minimaxi.com/anthropic'
      case 'opencode_go':
        return mode === 'zen' ? OPENCODE_ZEN_ANTHROPIC_BASE_URL : OPENCODE_GO_ANTHROPIC_BASE_URL
      default:
        return ''
    }
  }
  // responses：Kimi / DeepSeek / MiniMax 的 base 与 chat_completions 相同（端点路径差异由后端处理）。
  switch (platform) {
    case 'kimi':
      return mode === 'coding' ? 'https://api.kimi.com/coding/v1' : 'https://api.moonshot.cn/v1'
    case 'zhipu':
      return mode === 'coding'
        ? 'https://open.bigmodel.cn/api/coding/paas/v4'
        : 'https://open.bigmodel.cn/api/paas/v4'
    case 'deepseek':
      return 'https://api.deepseek.com'
    case 'minimax':
      return 'https://api.minimaxi.com/v1'
    case 'opencode_go':
      return mode === 'zen' ? OPENCODE_ZEN_BASE_URL : OPENCODE_GO_BASE_URL
    default:
      return ''
  }
}

/** 返回自适应模式下需要配置的原生协议及其默认端点。 */
export function defaultCNAdaptiveBaseUrls(
  platform: CnProviderPlatform | 'opencode_go',
  mode: CnAccountMode | OpenCodeAccountMode
): Record<CnNativeApiProtocol, string> {
  return {
    chat_completions: defaultCNBaseUrl(platform, mode, 'chat_completions'),
    anthropic: defaultCNBaseUrl(platform, mode, 'anthropic'),
    responses: cnSupportsNativeResponses(platform) ? defaultCNBaseUrl(platform, mode, 'responses') : ''
  }
}

// ===== 国产供应商用量单元格可见性（单一事实源） =====
// CNProviderQuotaCell / CNProviderBalanceCell 与 AccountUsageCell 的占位符判定
// 共用，避免多处复制条件后一处改另一处漏改。

export function cnQuotaCellVisible(platform: string, accountMode: string): boolean {
  if (platform === 'opencode_go') return accountMode !== 'zen'
  return (platform === 'kimi' || platform === 'zhipu' || platform === 'minimax') && accountMode === 'coding'
}

export function cnBalanceCellVisible(platform: string, accountMode: string): boolean {
  return (platform === 'kimi' || platform === 'deepseek') && accountMode !== 'coding'
}

/**
 * 将请求头覆写写入 credentials。
 * create 模式：关闭时不写入任何字段；edit 模式：关闭时删除字段（全量替换语义）。
 */
export function applyHeaderOverride(
  credentials: Record<string, unknown>,
  enabled: boolean,
  rows: HeaderOverrideRow[],
  mode: 'create' | 'edit'
): void {
  if (enabled) {
    credentials[HEADER_OVERRIDE_ENABLED_CREDENTIAL_KEY] = true
    credentials[HEADER_OVERRIDES_CREDENTIAL_KEY] = buildHeaderOverridesObject(rows)
  } else if (mode === 'edit') {
    delete credentials[HEADER_OVERRIDE_ENABLED_CREDENTIAL_KEY]
    delete credentials[HEADER_OVERRIDES_CREDENTIAL_KEY]
  }
}

// ===== OpenAI plan_type (ChatGPT 订阅档位) 手动覆盖 =====

export interface PlanTypeOption {
  value: string
  label: string
  // 兼容 common/Select.vue 的 SelectOption(含索引签名)
  [key: string]: unknown
}

/**
 * plan_type 值的友好显示标签（ChatGPT 档位命名）。
 * 与 PlatformTypeBadge 共用 openAIPlanTypeLabel，避免两处映射漂移；
 * canonical 值 chatgptpro 显示为 Pro 20x，team 显示为 Business Standard。未知值原样返回。
 */
export function planTypeDisplayLabel(value: string): string {
  return openAIPlanTypeLabel(value) || value
}

/**
 * 从凭据里读取 plan_type，仅接受字符串（脏数据 42/true 等一律视为空，
 * 避免被当作合法自定义项保留）。
 */
export function readPlanType(credentials: Record<string, unknown> | undefined | null): string {
  const v = credentials?.plan_type
  return typeof v === 'string' ? v : ''
}

/**
 * 构建 plan_type 下拉选项：清空 + Plus/Pro 20x/Pro 5x/Business Premium/Free 预设。
 * 若当前值是某预设的别名（如 chatgptpro↔Pro 20x），用当前的 canonical 值占据该
 * 标签位（保留 canonical，显示友好标签，避免重复项）；若是完全预设外的值
 * （如 team 或异常值），追加为一项，避免编辑时下拉丢失原值。
 */
export function buildPlanTypeOptions(current: string, clearLabel: string): PlanTypeOption[] {
  const cur = (current || '').trim()
  const curLabel = cur ? planTypeDisplayLabel(cur) : ''
  const presets: PlanTypeOption[] = [
    { value: 'plus', label: 'Plus' },
    { value: 'pro', label: 'Pro 20x' },
    { value: 'prolite', label: 'Pro 5x' },
    { value: 'self_serve_business_prolite', label: 'Business Premium' },
    { value: 'free', label: 'Free' }
  ]
  const opts: PlanTypeOption[] = [{ value: '', label: clearLabel }]
  for (const p of presets) {
    if (cur && p.value !== cur.toLowerCase() && p.label === curLabel) {
      // 当前值是该预设的别名：用 canonical 当前值占位，标签仍显示友好名
      opts.push({ value: cur, label: p.label })
    } else {
      opts.push(p)
    }
  }
  if (cur && !opts.some(o => o.value.toLowerCase() === cur.toLowerCase())) {
    opts.push({ value: cur, label: planTypeDisplayLabel(cur) })
  }
  return opts
}

/**
 * 把手动选择的 plan_type 写入凭据：非空则设置，空则删除该键（清空/自动识别）。
 * 直接修改传入对象并返回。
 */
export function applyPlanType(
  credentials: Record<string, unknown>,
  planType: string
): Record<string, unknown> {
  const pt = (planType || '').trim()
  if (pt) {
    credentials.plan_type = pt
  } else {
    delete credentials.plan_type
  }
  return credentials
}
