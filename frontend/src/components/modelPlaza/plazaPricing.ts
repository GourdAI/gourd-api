/**
 * 模型广场卡片的价格展示逻辑（纯函数，无组件依赖，便于单测覆盖）。
 *
 * 口径：
 * - 只展示「实付价」= 渠道单价 × 生效倍率（用户专属倍率优先，时段行再乘时段倍率）；
 *   官方参考价不再出现在卡片上（模型广场是给用户看「在我这儿花多少钱」的地方）；
 * - token 计费按 $/1M token 展示，按次/按图按单价 × 倍率展示。
 */
import { formatScaled, resolveIntervalPrices } from '@/utils/pricing'
import { BILLING_MODE_IMAGE, BILLING_MODE_TOKEN, type BillingMode } from '@/constants/channel'
import type { PlazaLongContextBasis, PlazaModel, PlazaTimePricingPeriod } from '@/api/modelPlaza'
import type { UserPricingInterval } from '@/api/channels'

const PER_MILLION = 1_000_000

/** 价格统一保底 2 位小数,更长的有效小数原样保留。 */
const MIN_DECIMALS = 2

/** 一个价格列（输入/输出/缓存）的档位行数。 */
export type PriceCellKey = 'input' | 'output' | 'cache'

/** 行内片段:如缓存行的「写 / 读」标签,按次要样式渲染。 */
export interface PricePart {
  text: string
  muted?: boolean
}

/** 一行价格;tier 为档位标签（多档时与各行对齐）。 */
export interface PriceLine {
  tier?: string
  parts: PricePart[]
}

/** 一个价格列的实付行。 */
export interface PriceCell {
  key: PriceCellKey
  paid: PriceLine[]
}

/** 按次 / 按图片计费的单档价格（生图分辨率阶梯、搜索按次等）。 */
export interface RequestPriceItem {
  tier?: string
  price: string
}

/** 卡片价格区数据。token 模式看 cells,其余看 requests。 */
export interface ModelPriceDisplay {
  billingMode: BillingMode
  isToken: boolean
  cells: PriceCell[]
  requests: RequestPriceItem[]
  /** 阶梯档数（>1 时卡片带档位标签）。 */
  tierCount: number
  longContextBasis?: PlazaLongContextBasis
}

/** 卡片所需的价格环境（由分组派生:生效倍率、生图独立倍率、高峰窗口）。 */
export interface PlazaPriceContext {
  /** 分组默认倍率。 */
  rateMultiplier: number
  /** 用户专属倍率;与默认不同则实付按此计算并划线展示原倍率。 */
  userRateMultiplier?: number | null
  /** 生图独立倍率:true 时图片计费模型按 imageRateMultiplier 计价。 */
  imageRateIndependent?: boolean
  imageRateMultiplier?: number | null
  /** 高峰窗口描述（含倍率与时区标注）;空串/缺省 = 未启用高峰。 */
  peakWindow?: string
  peakRateMultiplier?: number | null
}

export function billingModeOf(model: PlazaModel): BillingMode {
  return (model.pricing?.billing_mode || BILLING_MODE_TOKEN) as BillingMode
}

export function isTokenModel(model: PlazaModel): boolean {
  return billingModeOf(model) === BILLING_MODE_TOKEN
}

/** 后端零值/脏数据不能把整页价格算成 NaN,非有限值一律回落 1x。 */
function safeRate(value: number | null | undefined): number {
  return Number.isFinite(value) ? (value as number) : 1
}

/** 分组生效倍率 = 用户专属倍率 ?? 分组默认倍率。 */
export function effectiveGroupRate(ctx: PlazaPriceContext): number {
  return safeRate(ctx.userRateMultiplier ?? ctx.rateMultiplier)
}

/** 分时倍率时段（后端只给出倍率 ≠ 1 的时段,已升序）。 */
export function timePeriodsOf(model: PlazaModel): PlazaTimePricingPeriod[] {
  return model.time_pricing?.periods ?? []
}

/** “00:30–08:30”;整分钟的 HH:mm:ss 省略秒。 */
export function formatTimeWindow(period: PlazaTimePricingPeriod): string {
  const clock = (value: string) => value.replace(/^(\d{2}:\d{2}):00$/, '$1')
  return `${clock(period.start_time)}–${clock(period.end_time)}`
}

/**
 * 上下文档位按下限升序展示（后端已升序,此处兜底）;
 * 仅配置区间倍率的档位先折算成绝对单价。
 */
function tokenIntervals(model: PlazaModel): UserPricingInterval[] {
  return [...(model.pricing?.intervals ?? [])]
    .sort((a, b) => a.min_tokens - b.min_tokens)
    .map((iv) =>
      Object.assign(resolveIntervalPrices(iv, model.pricing!), {
        hasExplicitCache1h: iv.cache_write_1h_price != null
      })
    )
}

/**
 * 档位标签:优先后端/管理员给出的 tier_label,否则按区间生成统一形态——
 * 有上限为「≤上限」,末档为「>下限」。
 */
export function tierLabel(iv: UserPricingInterval): string {
  if (iv.tier_label) return iv.tier_label
  const min = iv.min_tokens
  const max = iv.max_tokens
  return max == null ? `>${formatTokenCount(min)}` : `≤${formatTokenCount(max)}`
}

function formatTokenCount(n: number): string {
  if (n >= 1_000_000) return `${trimZero(n / 1_000_000)}M`
  if (n >= 1_000) {
    const k = trimZero(n / 1_000)
    // 999999 这类四舍五入到 1000K 的档位升级成 M,避免出现「≤1000K」。
    return Number(k) >= 1_000 ? `${trimZero(Number(k) / 1_000)}M` : `${k}K`
  }
  return String(n)
}

function trimZero(n: number): string {
  return String(Math.round(n * 100) / 100)
}

/** 图片计费模型且分组开启生图独立倍率:实付倍率取独立倍率,与计费口径一致。 */
export function usesIndependentImageRate(model: PlazaModel, ctx: PlazaPriceContext): boolean {
  return billingModeOf(model) === BILLING_MODE_IMAGE && ctx.imageRateIndependent === true
}

/** 按次/按图片行的生效倍率。 */
export function requestRate(model: PlazaModel, ctx: PlazaPriceContext): number {
  return usesIndependentImageRate(model, ctx)
    ? safeRate(ctx.imageRateMultiplier)
    : effectiveGroupRate(ctx)
}

/** 时段行的生效倍率 = 生效倍率 × 时段倍率(去掉浮点噪声)。 */
function periodRate(period: PlazaTimePricingPeriod, ctx: PlazaPriceContext): number {
  return Math.round(effectiveGroupRate(ctx) * safeRate(period.multiplier) * 1000) / 1000
}

/** 卡片倍率标签（已格式化为 `0.8x` 形态,含时段倍率叠加后的生效值）。 */
export function modelRateValue(model: PlazaModel, ctx: PlazaPriceContext): number {
  return isTokenModel(model) ? effectiveGroupRate(ctx) : requestRate(model, ctx)
}

/** 需要划线展示的原倍率(仅专属倍率场景;生图独立倍率不划线)。 */
export function modelRateStruck(model: PlazaModel, ctx: PlazaPriceContext): number | null {
  const hasCustomRate = ctx.userRateMultiplier != null && ctx.userRateMultiplier !== ctx.rateMultiplier
  if (!hasCustomRate || usesIndependentImageRate(model, ctx)) return null
  return ctx.rateMultiplier
}

function paidPerMillion(
  value: number | null | undefined,
  ctx: PlazaPriceContext,
  period: PlazaTimePricingPeriod | null = null
): string {
  if (value == null) return '-'
  const rate = period ? periodRate(period, ctx) : effectiveGroupRate(ctx)
  return formatScaled(value * rate, PER_MILLION, MIN_DECIMALS)
}

function paidRequestPrice(
  value: number | null | undefined,
  model: PlazaModel,
  ctx: PlazaPriceContext
): string {
  if (value == null) return '-'
  return formatScaled(value * requestRate(model, ctx), 1, MIN_DECIMALS)
}

/** 缓存价格来源（渠道定价字段）。 */
type CachePrices = {
  cache_write_price: number | null
  cache_write_1h_price?: number | null
  cache_read_price: number | null
  /** 该档是否真的配了 1h 写入价(resolveIntervalPrices 会把 5m 价回落到 1h 字段上)。 */
  hasExplicitCache1h?: boolean
}

/**
 * 是否值得单独列出 1h 写入价:仅当管理员确实配了 1h 价、且与 5m 价不同。
 * 回落到 5m 的 1h 字段不是「1h 与 5m 同价」的事实,展示出来会误导计费预期。
 */
function showsCache1h(src: CachePrices | null | undefined): boolean {
  if (!src || src.hasExplicitCache1h !== true) return false
  return src.cache_write_1h_price != null && src.cache_write_1h_price !== src.cache_write_price
}

type PriceFormatter = (value: number | null | undefined) => string

/** 任一档带缓存价才按档渲染缓存列;否则沿用平价的写入/读取两行。 */
function hasTierCachePricing(intervals: UserPricingInterval[]): boolean {
  return intervals.some((iv) =>
    iv.cache_write_price != null || iv.cache_write_1h_price != null || iv.cache_read_price != null ||
    iv.cache_write_multiplier != null || iv.cache_read_multiplier != null
  )
}

function hasCachePricing(src: CachePrices | null | undefined): boolean {
  return (
    src?.cache_write_price != null || src?.cache_write_1h_price != null || src?.cache_read_price != null
  )
}

function label(text: string): PricePart {
  return { text, muted: true }
}

/** 缓存行的「写 / 读」短标签（由 PriceLabels 适配而来）。 */
type CacheLabels = { write: string; read: string }

function cacheLabels(labels: PriceLabels): CacheLabels {
  return { write: labels.cacheWrite, read: labels.cacheRead }
}

/** 单行内联「写 x (1h y) 读 z」（阶梯缓存价）。 */
function cacheInlineParts(
  src: CachePrices,
  fmt: PriceFormatter,
  labels: CacheLabels
): PricePart[] {
  if (!hasCachePricing(src)) return [{ text: '-' }]
  const parts: PricePart[] = [label(labels.write), { text: ' ' + fmt(src.cache_write_price) }]
  if (showsCache1h(src)) {
    parts.push(label(' (1h'), { text: ' ' + fmt(src.cache_write_1h_price) }, label(')'))
  }
  parts.push(label(' ' + labels.read), { text: ' ' + fmt(src.cache_read_price) })
  return parts
}

/** 平价缓存拆「写」「读」两行。 */
function cacheSplitLines(
  src: CachePrices,
  fmt: PriceFormatter,
  labels: CacheLabels
): PriceLine[] {
  const writeParts: PricePart[] = [label(labels.write), { text: ' ' + fmt(src.cache_write_price) }]
  if (showsCache1h(src)) {
    writeParts.push(label(' (1h'), { text: ' ' + fmt(src.cache_write_1h_price) }, label(')'))
  }
  return [
    { parts: writeParts },
    { parts: [label(labels.read), { text: ' ' + fmt(src.cache_read_price) }] }
  ]
}

export interface PriceLabels {
  cacheWrite: string
  cacheRead: string
  /** 非 token 计费的单位后缀（/ 次、/ 张）。 */
  perRequest: string
  perImage: string
}

/** token 模式:输入 / 输出 / 缓存三列的实付行。 */
function tokenCells(
  model: PlazaModel,
  ctx: PlazaPriceContext,
  period: PlazaTimePricingPeriod | null,
  labels: PriceLabels
): PriceCell[] {
  const intervals = tokenIntervals(model)
  const hasIntervals = intervals.length > 0
  const cache = cacheLabels(labels)
  const fmtPaid = (value: number | null | undefined) => paidPerMillion(value, ctx, period)

  const paidToken = (field: 'input_price' | 'output_price'): PriceLine[] =>
    hasIntervals
      ? intervals.map((iv) => ({ tier: tierLabel(iv), parts: [{ text: fmtPaid(iv[field]) }] }))
      : [{ parts: [{ text: fmtPaid(model.pricing?.[field]) }] }]

  const paidCache = (): PriceLine[] => {
    if (hasTierCachePricing(intervals)) {
      return intervals.map((iv) => ({
        tier: tierLabel(iv),
        parts: cacheInlineParts(iv, fmtPaid, cache)
      }))
    }
    if (!hasCachePricing(model.pricing)) return [{ parts: [{ text: '-' }] }]
    return cacheSplitLines(
      { ...model.pricing!, hasExplicitCache1h: model.pricing!.cache_write_1h_price != null },
      fmtPaid,
      cache
    )
  }

  return [
    { key: 'input', paid: paidToken('input_price') },
    { key: 'output', paid: paidToken('output_price') },
    { key: 'cache', paid: paidCache() }
  ]
}

/** 按次 / 按图片:阶梯（多档单价）优先,否则用平铺的按次单价。 */
function requestPrices(model: PlazaModel, ctx: PlazaPriceContext): RequestPriceItem[] {
  const tiers = (model.pricing?.intervals ?? []).filter((iv) => iv.per_request_price != null)
  if (tiers.length) {
    return tiers.map((iv) => ({
      tier: tierLabel(iv),
      price: paidRequestPrice(iv.per_request_price, model, ctx)
    }))
  }
  const flat = model.pricing?.per_request_price
  if (flat != null) return [{ price: paidRequestPrice(flat, model, ctx) }]
  return []
}

/**
 * 单个模型的卡片价格数据。
 * `period` 传入时按时段倍率折算（分时计费的独立时段价）。
 */
export function modelPrice(
  model: PlazaModel,
  ctx: PlazaPriceContext,
  labels: PriceLabels,
  period: PlazaTimePricingPeriod | null = null
): ModelPriceDisplay {
  const mode = billingModeOf(model)
  if (mode !== BILLING_MODE_TOKEN) {
    return {
      billingMode: mode,
      isToken: false,
      cells: [],
      requests: requestPrices(model, ctx),
      tierCount: 0
    }
  }
  const cells = tokenCells(model, ctx, period, labels)
  return {
    billingMode: mode,
    isToken: true,
    cells,
    requests: [],
    tierCount: Math.max(cells[0].paid.length, 1),
    longContextBasis: model.long_context_basis
  }
}

/**
 * 排序/跨组比价用的实付单价:token 模型统一折到 $/1M token,按次/按图为 $/次;无价为 null。
 * token 取「输入价」——卡片首行就是输入价,比价口径必须与用户看到的第一个数字一致,
 * 否则「全场最低」与排序结果会互相矛盾。平价缺失时回落最低档,再回落输出价。
 * 两种量纲不可直接互比,调用方先按计费模式分组。
 */
export function modelSortPrice(model: PlazaModel, ctx: PlazaPriceContext): number | null {
  if (!isTokenModel(model)) {
    const flat = model.pricing?.per_request_price
    const perRequest =
      flat ??
      [...(model.pricing?.intervals ?? [])]
        .filter((iv) => iv.per_request_price != null)
        .sort((a, b) => a.min_tokens - b.min_tokens)[0]?.per_request_price ??
      null
    if (perRequest == null) return null
    const perRequestPaid = perRequest * requestRate(model, ctx)
    return Number.isFinite(perRequestPaid) ? perRequestPaid : null
  }
  const lowest = [...(model.pricing?.intervals ?? [])]
    .sort((a, b) => a.min_tokens - b.min_tokens)
    .map((iv) => resolveIntervalPrices(iv, model.pricing!))[0]
  const base =
    model.pricing?.input_price ??
    lowest?.input_price ??
    model.pricing?.output_price ??
    lowest?.output_price ??
    null
  if (base == null) return null
  const paid = base * effectiveGroupRate(ctx) * PER_MILLION
  return Number.isFinite(paid) ? paid : null
}

/**
 * 时段 tooltip:仅工作日生效的配置换用带周末回落说明的文案;
 * 分组启用高峰倍率时追加披露——本时段价格不含高峰因子,重叠部分实付再乘高峰倍率。
 */
export function timePeriodHint(
  model: PlazaModel,
  ctx: PlazaPriceContext,
  translate: (key: string, named?: Record<string, unknown>) => string
): string {
  const key = model.time_pricing?.weekdays_only
    ? 'modelPlaza.card.timePeriodHintWeekdays'
    : 'modelPlaza.card.timePeriodHint'
  let hint = translate(key, { timezone: model.time_pricing?.timezone })
  if (ctx.peakWindow) {
    hint += translate('modelPlaza.card.timePeriodHintPeak', {
      window: ctx.peakWindow,
      multiplier: ctx.peakRateMultiplier ?? 1
    })
  }
  return hint
}

/**
 * 分时时段的一行摘要:「00:30–08:30 ×0.5、18:00–22:00 ×1.2」。
 * 分隔符由调用方按语种传入（英文界面不该用中文顿号）;缺倍率的老数据不渲染 ×undefined。
 */
export function timePeriodsSummary(model: PlazaModel, separator = '、'): string {
  return timePeriodsOf(model)
    .map((p) => (Number.isFinite(p.multiplier) ? `${formatTimeWindow(p)} ×${p.multiplier}` : formatTimeWindow(p)))
    .join(separator || '、')
}
