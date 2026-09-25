<template>
  <div class="plaza-pricing-table">
    <!-- 桌面视口:紧凑价格表(模型 / 输入 / 输出 / 缓存 / 倍率)。
         实付价为主行;官方参考价与实付不一致时以弱化副行展示,一致时不重复。 -->
    <div v-if="isDesktopViewport" class="overflow-x-auto">
      <table class="w-full min-w-[620px] table-auto border-collapse text-sm tabular-nums">
        <thead>
          <tr
            class="border-b border-gray-200 text-left text-[11px] font-semibold uppercase tracking-wider text-gray-400 dark:border-dark-700 dark:text-dark-500"
          >
            <th class="sticky left-0 z-[1] bg-white py-2.5 pl-5 pr-4 align-bottom dark:bg-dark-800">
              {{ t('modelPlaza.table.model') }}
            </th>
            <th class="px-3 py-2.5 align-bottom">
              {{ t('modelPlaza.table.input') }}
              <span class="block font-normal normal-case tracking-normal text-gray-300 dark:text-dark-600">
                {{ t('modelPlaza.table.unitPerMillion') }}
              </span>
            </th>
            <th class="px-3 py-2.5 align-bottom">
              {{ t('modelPlaza.table.output') }}
              <span class="block font-normal normal-case tracking-normal text-gray-300 dark:text-dark-600">
                {{ t('modelPlaza.table.unitPerMillion') }}
              </span>
            </th>
            <th class="px-3 py-2.5 align-bottom">
              {{ t('modelPlaza.table.cache') }}
              <span class="block font-normal normal-case tracking-normal text-gray-300 dark:text-dark-600">
                {{ t('modelPlaza.table.unitPerMillion') }}
              </span>
            </th>
            <th class="py-2.5 pl-3 pr-5 text-right align-bottom">{{ t('modelPlaza.table.rate') }}</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="row in rows"
            :key="row.key"
            class="group border-b border-gray-100 transition-colors last:border-b-0 hover:bg-gray-50 dark:border-dark-800/70 dark:hover:bg-dark-700/40"
          >
            <!-- 模型名 + 计费模式/阶梯/推理强度徽章;分时时段行额外标注时段 -->
            <td
              class="sticky left-0 z-[1] border-r border-gray-100 bg-white py-2.5 pl-5 pr-4 align-middle group-hover:bg-gray-50 dark:border-dark-700/60 dark:bg-dark-800 dark:group-hover:bg-dark-700"
            >
              <div class="flex flex-wrap items-center gap-1.5">
                <span class="font-medium text-gray-900 dark:text-white">{{ row.model.name }}</span>
                <!-- 时段徽章紧跟模型名,其余徽章排在后面,空间不足时先换行的是它们 -->
                <span
                  v-if="row.period"
                  class="inline-flex items-center whitespace-nowrap rounded-md bg-gray-100 px-1 py-0.5 font-mono text-[10px] font-medium text-gray-500 dark:bg-dark-700/70 dark:text-dark-300"
                  :title="timePricingRowHint(row.model)"
                >
                  <span v-if="row.model.time_pricing?.weekdays_only" class="mr-1 font-sans">{{
                    t('modelPlaza.table.timePricingWeekdays')
                  }}</span>
                  {{ formatTimeWindow(row.period) }}
                </span>
                <span
                  v-if="billingMode(row.model) !== BILLING_MODE_TOKEN"
                  class="rounded-md bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-500 dark:bg-dark-700/70 dark:text-dark-300"
                >
                  {{ billingModeLabel(row.model) }}
                </span>
                <span
                  v-if="row.model.long_context_basis === 'marginal'"
                  class="rounded-md bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-500 dark:bg-dark-700/70 dark:text-dark-300"
                  :title="t('modelPlaza.table.tierHintMarginal')"
                >
                  {{ t('modelPlaza.table.marginalBadge') }}
                </span>
                <span
                  v-if="row.model.pricing?.max_reasoning_effort_multiplier"
                  class="rounded-md bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-500 dark:bg-dark-700/70 dark:text-dark-300"
                  :title="t('modelPlaza.table.maxReasoningMultiplierHint', { multiplier: row.model.pricing.max_reasoning_effort_multiplier })"
                >
                  {{ t('modelPlaza.table.maxReasoningMultiplierBadge', { multiplier: row.model.pricing.max_reasoning_effort_multiplier }) }}
                </span>
              </div>
            </td>

            <!-- token 计费:输入 / 输出 / 缓存三列;有阶梯时每档一行,档位标签只放输入列 -->
            <template v-if="billingMode(row.model) === BILLING_MODE_TOKEN">
              <td v-for="cell in priceCells(row)" :key="cell.key" class="px-3 py-2.5 align-middle">
                <div
                  v-for="(line, idx) in cell.paid"
                  :key="idx"
                  class="whitespace-nowrap font-mono text-xs font-semibold leading-5 text-gray-900 dark:text-gray-50"
                >
                  <span
                    v-if="line.tier && cell.key === 'input'"
                    class="mr-1 font-sans font-normal text-gray-400 dark:text-dark-500"
                    :title="tierHint(row.model)"
                    >{{ line.tier }}</span
                  >
                  <span
                    v-for="(part, pi) in line.parts"
                    :key="pi"
                    :class="part.muted ? 'font-sans font-normal text-gray-400 dark:text-dark-500' : undefined"
                    >{{ part.text }}</span
                  >
                </div>
                <!-- 官方参考价副行:弱化小字;与实付一致或缺失时整体不渲染 -->
                <div v-if="cell.official" class="mt-0.5">
                  <div
                    v-for="(line, idx) in cell.official"
                    :key="idx"
                    class="whitespace-nowrap font-mono text-[10px] leading-4 text-gray-400 dark:text-dark-500"
                    :title="t('modelPlaza.table.officialPrice')"
                  >
                    <span class="mr-1 font-sans">{{ t('modelPlaza.table.officialTag') }}</span>
                    <span
                      v-if="line.tier && cell.key === 'input'"
                      class="mr-1 font-sans"
                      :title="tierHint(row.model)"
                      >{{ line.tier }}</span
                    >
                    <span v-for="(part, pi) in line.parts" :key="pi">{{ part.text }}</span>
                  </div>
                </div>
              </td>
            </template>

            <!-- 按次 / 按图片计费:实付区整体合并 -->
            <template v-else>
              <td colspan="3" class="px-3 py-2.5 align-middle">
                <div v-if="requestIntervals(row.model).length" class="flex flex-wrap items-center gap-1.5">
                  <span
                    v-for="(iv, idx) in requestIntervals(row.model)"
                    :key="idx"
                    class="inline-flex items-center gap-1 rounded-md bg-gray-100 px-2 py-0.5 font-mono text-xs text-gray-800 dark:bg-dark-700/60 dark:text-gray-200"
                  >
                    <span class="font-sans text-gray-400 dark:text-dark-500">{{ tierLabel(iv) }}</span>
                    {{ paidRequestPrice(row.model, iv.per_request_price)
                    }}<span class="font-sans text-gray-400 dark:text-dark-500">{{ perUnitSuffix(row.model) }}</span>
                  </span>
                </div>
                <template v-else-if="row.model.pricing?.per_request_price != null">
                  <span class="font-mono text-xs font-semibold text-gray-900 dark:text-gray-50">
                    {{ paidRequestPrice(row.model, row.model.pricing.per_request_price) }}
                  </span>
                  <span class="ml-1 text-xs text-gray-400 dark:text-dark-500">{{ perUnitSuffix(row.model) }}</span>
                </template>
                <span v-else class="text-gray-400 dark:text-dark-500">-</span>
              </td>
            </template>

            <!-- 折扣倍率(分时时段行展示 生效倍率×时段倍率;生图独立倍率行展示独立倍率;专属倍率划线展示原倍率) -->
            <td class="py-2.5 pl-3 pr-5 text-right align-middle font-mono text-xs">
              <span
                v-if="rowRateStruck(row) != null"
                class="mr-1 text-gray-400 line-through dark:text-dark-500"
                >{{ rowRateStruck(row) }}x</span
              >
              <span
                :class="rowRateClass(row)"
                :title="row.period ? t('modelPlaza.table.timePricingRateHint', { rate: effectiveRate, multiplier: row.period.multiplier }) : undefined"
                >{{ rowRateValue(row) }}x</span
              >
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- 窄视口:卡片列表降级(横向滚动不可用时按模型逐张展示) -->
    <div v-else class="divide-y divide-gray-100 dark:divide-dark-800/70">
      <article v-for="row in rows" :key="row.key" class="space-y-2.5 px-5 py-4">
        <div class="flex items-start justify-between gap-3">
          <div class="flex min-w-0 flex-wrap items-center gap-1.5">
            <span class="font-medium text-gray-900 dark:text-white">{{ row.model.name }}</span>
            <span
              v-if="row.period"
              class="inline-flex items-center whitespace-nowrap rounded-md bg-gray-100 px-1 py-0.5 font-mono text-[10px] font-medium text-gray-500 dark:bg-dark-700/70 dark:text-dark-300"
              :title="timePricingRowHint(row.model)"
            >
              <span v-if="row.model.time_pricing?.weekdays_only" class="mr-1 font-sans">{{
                t('modelPlaza.table.timePricingWeekdays')
              }}</span>
              {{ formatTimeWindow(row.period) }}
            </span>
            <span
              v-if="billingMode(row.model) !== BILLING_MODE_TOKEN"
              class="rounded-md bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-500 dark:bg-dark-700/70 dark:text-dark-300"
            >
              {{ billingModeLabel(row.model) }}
            </span>
            <span
              v-if="row.model.long_context_basis === 'marginal'"
              class="rounded-md bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-500 dark:bg-dark-700/70 dark:text-dark-300"
              :title="t('modelPlaza.table.tierHintMarginal')"
            >
              {{ t('modelPlaza.table.marginalBadge') }}
            </span>
            <span
              v-if="row.model.pricing?.max_reasoning_effort_multiplier"
              class="rounded-md bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-500 dark:bg-dark-700/70 dark:text-dark-300"
              :title="t('modelPlaza.table.maxReasoningMultiplierHint', { multiplier: row.model.pricing.max_reasoning_effort_multiplier })"
            >
              {{ t('modelPlaza.table.maxReasoningMultiplierBadge', { multiplier: row.model.pricing.max_reasoning_effort_multiplier }) }}
            </span>
          </div>
          <span class="shrink-0 font-mono text-xs">
            <span v-if="rowRateStruck(row) != null" class="mr-1 text-gray-400 line-through dark:text-dark-500"
              >{{ rowRateStruck(row) }}x</span
            >
            <span :class="rowRateClass(row)">{{ rowRateValue(row) }}x</span>
          </span>
        </div>

        <template v-if="billingMode(row.model) === BILLING_MODE_TOKEN">
          <div v-for="cell in priceCells(row)" :key="cell.key" class="flex items-start gap-3">
            <span class="w-11 shrink-0 pt-0.5 text-[11px] text-gray-400 dark:text-dark-500">{{
              columnLabel(cell.key)
            }}</span>
            <div class="min-w-0 flex-1 space-y-0.5">
              <div
                v-for="(line, idx) in cell.paid"
                :key="idx"
                class="font-mono text-xs font-semibold leading-5 text-gray-900 dark:text-gray-50"
              >
                <span v-if="line.tier" class="mr-1 font-sans font-normal text-gray-400 dark:text-dark-500" :title="tierHint(row.model)">{{
                  line.tier
                }}</span>
                <span
                  v-for="(part, pi) in line.parts"
                  :key="pi"
                  :class="part.muted ? 'font-sans font-normal text-gray-400 dark:text-dark-500' : undefined"
                  >{{ part.text }}</span
                >
              </div>
              <div v-if="cell.official" class="space-y-0.5">
                <div
                  v-for="(line, idx) in cell.official"
                  :key="idx"
                  class="font-mono text-[10px] leading-4 text-gray-400 dark:text-dark-500"
                >
                  <span class="mr-1 font-sans">{{ t('modelPlaza.table.officialTag') }}</span>
                  <span v-if="line.tier" class="mr-1 font-sans">{{ line.tier }}</span>
                  <span v-for="(part, pi) in line.parts" :key="pi">{{ part.text }}</span>
                </div>
              </div>
            </div>
          </div>
          <p class="text-right text-[10px] text-gray-300 dark:text-dark-600">
            {{ t('modelPlaza.table.unitPerMillion') }}
          </p>
        </template>

        <div v-else class="flex items-start gap-3">
          <span class="w-11 shrink-0 pt-0.5 text-[11px] text-gray-400 dark:text-dark-500">{{
            billingModeLabel(row.model)
          }}</span>
          <div class="min-w-0 flex-1">
            <div v-if="requestIntervals(row.model).length" class="flex flex-wrap items-center gap-1.5">
              <span
                v-for="(iv, idx) in requestIntervals(row.model)"
                :key="idx"
                class="inline-flex items-center gap-1 rounded-md bg-gray-100 px-2 py-0.5 font-mono text-xs text-gray-800 dark:bg-dark-700/60 dark:text-gray-200"
              >
                <span class="font-sans text-gray-400 dark:text-dark-500">{{ tierLabel(iv) }}</span>
                {{ paidRequestPrice(row.model, iv.per_request_price)
                }}<span class="font-sans text-gray-400 dark:text-dark-500">{{ perUnitSuffix(row.model) }}</span>
              </span>
            </div>
            <template v-else-if="row.model.pricing?.per_request_price != null">
              <span class="font-mono text-xs font-semibold text-gray-900 dark:text-gray-50">
                {{ paidRequestPrice(row.model, row.model.pricing.per_request_price) }}
              </span>
              <span class="ml-1 text-xs text-gray-400 dark:text-dark-500">{{ perUnitSuffix(row.model) }}</span>
            </template>
            <span v-else class="text-gray-400 dark:text-dark-500">-</span>
          </div>
        </div>
      </article>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { formatScaled, resolveIntervalPrices } from '@/utils/pricing'
import {
  BILLING_MODE_TOKEN,
  BILLING_MODE_IMAGE,
  type BillingMode
} from '@/constants/channel'
import type { PlazaModel, PlazaTimePricingPeriod } from '@/api/modelPlaza'
import type { UserPricingInterval } from '@/api/channels'

const props = defineProps<{
  models: PlazaModel[]
  /** 分组默认倍率。 */
  rateMultiplier: number
  /** 用户专属倍率;与默认不同,实付价按此计算并划线展示原倍率。 */
  userRateMultiplier?: number | null
  /** 生图独立倍率:true 时图片计费模型的实付倍率取 imageRateMultiplier,不取分组/专属倍率。 */
  imageRateIndependent?: boolean
  imageRateMultiplier?: number | null
  /**
   * 高峰窗口描述(含倍率与服务器时区标注),空串/缺省 = 分组未启用高峰。
   * 表格所有价格均为不含高峰因子的口径,该窗口仅用于分时时段行的 tooltip 披露:
   * 与高峰重叠的部分实付还会再乘高峰倍率。
   */
  peakWindow?: string
  peakRateMultiplier?: number | null
}>()

const { t } = useI18n()

const PER_MILLION = 1_000_000

/** 价格统一保底 2 位小数,更长的有效小数原样保留。 */
const MIN_DECIMALS = 2

/** 桌面视口(≥768px)渲染表格;窄视口降级为卡片列表。 */
const desktopViewportQuery = '(min-width: 768px)'
const isDesktopViewport = ref(
  typeof window === 'undefined' || typeof window.matchMedia !== 'function'
    ? true
    : window.matchMedia(desktopViewportQuery).matches
)
let desktopViewportMediaQuery: MediaQueryList | null = null
const onViewportChange = (event: MediaQueryListEvent) => {
  isDesktopViewport.value = event.matches
}

onMounted(() => {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return
  desktopViewportMediaQuery = window.matchMedia(desktopViewportQuery)
  isDesktopViewport.value = desktopViewportMediaQuery.matches
  if (typeof desktopViewportMediaQuery.addEventListener === 'function') {
    desktopViewportMediaQuery.addEventListener('change', onViewportChange)
  }
})

onUnmounted(() => {
  if (desktopViewportMediaQuery && typeof desktopViewportMediaQuery.removeEventListener === 'function') {
    desktopViewportMediaQuery.removeEventListener('change', onViewportChange)
  }
})

/**
 * 展示顺序:
 * 1. token 计费的排在前,按图/按次计费的沉到末尾——它们的官方 token 价与实付的按张/按次价不同量纲,混排无意义;
 * 2. 组内按官方输出价从高到低,无官方价的排最后;
 * 3. 同价按名称降序(新版本号在前,如 gpt-5.6 先于 gpt-5.5)。
 */
const sortedModels = computed(() => {
  return [...props.models].sort((a, b) => {
    const ta = billingMode(a) === BILLING_MODE_TOKEN
    const tb = billingMode(b) === BILLING_MODE_TOKEN
    if (ta !== tb) return ta ? -1 : 1
    const pa = a.official_pricing?.output_price ?? null
    const pb = b.official_pricing?.output_price ?? null
    if (pa != null && pb != null && pa !== pb) return pb - pa
    if (pa != null && pb == null) return -1
    if (pa == null && pb != null) return 1
    return b.name.localeCompare(a.name)
  })
})

const effectiveRate = computed(() => props.userRateMultiplier ?? props.rateMultiplier)
const hasCustomRate = computed(
  () => props.userRateMultiplier != null && props.userRateMultiplier !== props.rateMultiplier
)

function billingMode(m: PlazaModel): BillingMode {
  return (m.pricing?.billing_mode || BILLING_MODE_TOKEN) as BillingMode
}

function billingModeLabel(m: PlazaModel): string {
  return billingMode(m) === BILLING_MODE_IMAGE
    ? t('modelPlaza.table.perImage')
    : t('modelPlaza.table.perRequest')
}

/** 表格行:每个模型一行标准价;配置了分时倍率的模型再按时段各加一行。 */
interface PlazaRow {
  model: PlazaModel
  period: PlazaTimePricingPeriod | null
  key: string
}

const rows = computed<PlazaRow[]>(() =>
  sortedModels.value.flatMap((m) => {
    const base: PlazaRow = { model: m, period: null, key: `${m.platform}:${m.name}` }
    const periodRows = timePeriods(m).map<PlazaRow>((p, idx) => ({
      model: m,
      period: p,
      key: `${m.platform}:${m.name}:${idx}`
    }))
    return [base, ...periodRows]
  })
)

/** 时段行的生效倍率 = 生效倍率 × 时段倍率(去掉浮点噪声)。 */
function periodRate(period: PlazaTimePricingPeriod): number {
  return Math.round(effectiveRate.value * period.multiplier * 1000) / 1000
}

/** 实付价 = 渠道单价 × 生效倍率(时段行再乘时段倍率),按 $/1M token 展示。 */
function paidPerMillion(value: number | null | undefined, period: PlazaTimePricingPeriod | null = null): string {
  if (value == null) return '-'
  const rate = period ? periodRate(period) : effectiveRate.value
  return formatScaled(value * rate, PER_MILLION, MIN_DECIMALS)
}

/** 图片计费模型且分组开启生图独立倍率:实付倍率取独立倍率,与计费口径一致。 */
function usesIndependentImageRate(m: PlazaModel): boolean {
  return billingMode(m) === BILLING_MODE_IMAGE && props.imageRateIndependent === true
}

/** 按次/按图片行的生效倍率。 */
function requestRate(m: PlazaModel): number {
  return usesIndependentImageRate(m) ? (props.imageRateMultiplier ?? 1) : effectiveRate.value
}

/** 按次 / 按图片单价(乘该行生效倍率,不换算 1M)。 */
function paidRequestPrice(m: PlazaModel, value: number | null | undefined): string {
  if (value == null) return '-'
  return formatScaled(value * requestRate(m), 1, MIN_DECIMALS)
}

/** 官方参考价不乘倍率。 */
function official(value: number | null | undefined): string {
  if (value == null) return '-'
  return formatScaled(value, PER_MILLION, MIN_DECIMALS)
}

/** 非 token 计费的单位后缀:按图片 → “/ 张”,按次 → “/ 次”。 */
function perUnitSuffix(m: PlazaModel): string {
  return billingMode(m) === BILLING_MODE_IMAGE
    ? t('modelPlaza.table.perUnitImage')
    : t('modelPlaza.table.perUnitRequest')
}

function hasCachePricing(m: PlazaModel): boolean {
  return m.pricing?.cache_write_price != null || m.pricing?.cache_write_1h_price != null || m.pricing?.cache_read_price != null
}

function hasOfficialCache(o: NonNullable<PlazaModel['official_pricing']>): boolean {
  return o.cache_write_price != null || o.cache_read_price != null || o.cache_write_1h_price != null
}

/** 分时倍率时段(后端只给出倍率 ≠ 1 的时段,已升序)。 */
function timePeriods(m: PlazaModel): PlazaTimePricingPeriod[] {
  return m.time_pricing?.periods ?? []
}

/**
 * 时段行 tooltip:仅工作日生效的配置换用带周末回落说明的文案;
 * 分组启用高峰倍率时追加披露——本行价格不含高峰因子,与高峰窗口重叠的部分实付再乘高峰倍率。
 */
function timePricingRowHint(m: PlazaModel): string {
  const key = m.time_pricing?.weekdays_only
    ? 'modelPlaza.table.timePricingRowHintWeekdays'
    : 'modelPlaza.table.timePricingRowHint'
  let hint = t(key, { timezone: m.time_pricing?.timezone })
  if (props.peakWindow) {
    hint += t('modelPlaza.table.timePricingRowHintPeak', {
      window: props.peakWindow,
      multiplier: props.peakRateMultiplier ?? 1
    })
  }
  return hint
}

/** “00:30–08:30”;整分钟的 HH:mm:ss 省略秒。 */
function formatTimeWindow(p: PlazaTimePricingPeriod): string {
  const clock = (v: string) => v.replace(/^(\d{2}:\d{2}):00$/, '$1')
  return `${clock(p.start_time)}–${clock(p.end_time)}`
}

/** 上下文档位按下限升序展示(后端已升序,此处兜底)。 */
function sortByContext(intervals: UserPricingInterval[]): UserPricingInterval[] {
  return [...intervals].sort((a, b) => a.min_tokens - b.min_tokens)
}

/** token 模式的阶梯定价(内联进输入/输出/缓存列)。 */
function tokenIntervals(m: PlazaModel): UserPricingInterval[] {
  return sortByContext(m.pricing?.intervals ?? []).map(iv => resolveIntervalPrices(iv, m.pricing!))
}

/** 官方阶梯(后端按目录规则合成,不受分组开关影响)。 */
function officialIntervals(m: PlazaModel): UserPricingInterval[] {
  return sortByContext(m.official_pricing?.intervals ?? [])
}

/** 任一档带缓存价才按档渲染缓存列;否则沿用平价的写入/读取两行。 */
function hasTierCachePricing(intervals: UserPricingInterval[]): boolean {
  return intervals.some((iv) =>
    iv.cache_write_price != null || iv.cache_write_1h_price != null || iv.cache_read_price != null ||
    iv.cache_write_multiplier != null || iv.cache_read_multiplier != null
  )
}

/** 档位说明:整单按档计价,或(平台旧规则)仅超出部分按档计价。 */
function tierHint(m: PlazaModel): string {
  return m.long_context_basis === 'marginal'
    ? t('modelPlaza.table.tierHintMarginal')
    : t('modelPlaza.table.tierHint')
}

/** 按次/按图模式的阶梯定价(仅保留配了按次价的档位)。 */
function requestIntervals(m: PlazaModel): UserPricingInterval[] {
  return (m.pricing?.intervals ?? []).filter((iv) => iv.per_request_price != null)
}

/**
 * 档位标签:优先后端/管理员给出的 tier_label,否则按区间生成统一形态——
 * 有上限为「≤上限」,末档为「>下限」;档位升序排列,相邻的 ≤100K / ≤200K 即表示 (100K,200K]。
 */
function tierLabel(iv: UserPricingInterval): string {
  if (iv.tier_label) return iv.tier_label
  const { min_tokens: min, max_tokens: max } = iv
  return max == null ? `>${formatTokenCount(min)}` : `≤${formatTokenCount(max)}`
}

function formatTokenCount(n: number): string {
  if (n >= 1_000_000) return `${trimZero(n / 1_000_000)}M`
  if (n >= 1_000) return `${trimZero(n / 1_000)}K`
  return String(n)
}

function trimZero(n: number): string {
  return String(Math.round(n * 100) / 100)
}

// ── 价格单元格:表格与窄屏卡片共用同一份数据,保证两种形态信息一致 ──

/** 行内片段:如缓存行的「写 / 读」标签,按次要样式渲染。 */
interface PricePart {
  text: string
  muted?: boolean
}

/** 一行价格;tier 为档位标签(桌面表格仅输入列展示,卡片全列展示)。 */
interface PriceLine {
  tier?: string
  parts: PricePart[]
}

/** 一个价格列(输入/输出/缓存)的实付与官方参考行。 */
interface PriceCellData {
  key: 'input' | 'output' | 'cache'
  paid: PriceLine[]
  /** 官方参考行;与实付一致或缺失时为 null(不渲染副行)。 */
  official: PriceLine[] | null
}

/** 缓存价格来源(渠道定价与官方定价的公共字段)。 */
type CachePrices = {
  cache_write_price: number | null
  cache_write_1h_price?: number | null
  cache_read_price: number | null
}

type PriceFormatter = (value: number | null | undefined) => string

/** 行的展示文本签名,用于判断官方副行是否与实付重复。 */
function lineSignature(lines: PriceLine[]): string {
  return lines.map((line) => line.parts.map((p) => p.text).join('')).join('|')
}

/**
 * 官方参考与实付完全一致时(倍率 1、无折扣)不渲染副行,避免同一数字重复两遍;
 * 有折扣/分时/阶梯差异时副行照常展示。
 */
function distinctOfficial(paid: PriceLine[], officialLines: PriceLine[] | null): PriceLine[] | null {
  if (!officialLines) return null
  return lineSignature(officialLines) === lineSignature(paid) ? null : officialLines
}

function paidTokenLines(
  m: PlazaModel,
  period: PlazaTimePricingPeriod | null,
  field: 'input_price' | 'output_price'
): PriceLine[] {
  const intervals = tokenIntervals(m)
  if (intervals.length) {
    return intervals.map((iv) => ({ tier: tierLabel(iv), parts: [{ text: paidPerMillion(iv[field], period) }] }))
  }
  return [{ parts: [{ text: paidPerMillion(m.pricing?.[field], period) }] }]
}

function officialTokenLines(
  m: PlazaModel,
  field: 'input_price' | 'output_price'
): PriceLine[] | null {
  const intervals = officialIntervals(m)
  if (intervals.length) {
    return intervals.map((iv) => ({ tier: tierLabel(iv), parts: [{ text: official(iv[field]) }] }))
  }
  const value = m.official_pricing?.[field]
  if (value == null) return null
  return [{ parts: [{ text: official(value) }] }]
}

/** 单行内联「写 x (1h y) 读 z」(阶梯缓存价与官方阶梯缓存共用)。 */
function cacheInlineParts(src: CachePrices, fmt: PriceFormatter): PricePart[] {
  const hasAny = src.cache_write_price != null || src.cache_write_1h_price != null || src.cache_read_price != null
  if (!hasAny) return [{ text: '-' }]
  const parts: PricePart[] = [
    { text: t('modelPlaza.table.cacheWriteShort'), muted: true },
    { text: ' ' + fmt(src.cache_write_price) }
  ]
  if (src.cache_write_1h_price != null) {
    parts.push({ text: ' (1h', muted: true }, { text: ' ' + fmt(src.cache_write_1h_price) }, { text: ')', muted: true })
  }
  parts.push({ text: ' ' + t('modelPlaza.table.cacheReadShort'), muted: true }, { text: ' ' + fmt(src.cache_read_price) })
  return parts
}

/** 平价缓存拆「写」「读」两行。 */
function cacheSplitLines(src: CachePrices, fmt: PriceFormatter): PriceLine[] {
  const writeParts: PricePart[] = [
    { text: t('modelPlaza.table.cacheWriteShort'), muted: true },
    { text: ' ' + fmt(src.cache_write_price) }
  ]
  if (src.cache_write_1h_price != null) {
    writeParts.push({ text: ' (1h', muted: true }, { text: ' ' + fmt(src.cache_write_1h_price) }, { text: ')', muted: true })
  }
  return [
    { parts: writeParts },
    { parts: [{ text: t('modelPlaza.table.cacheReadShort'), muted: true }, { text: ' ' + fmt(src.cache_read_price) }] }
  ]
}

function paidCacheLines(m: PlazaModel, period: PlazaTimePricingPeriod | null): PriceLine[] {
  const intervals = tokenIntervals(m)
  if (hasTierCachePricing(intervals)) {
    return intervals.map((iv) => ({ tier: tierLabel(iv), parts: cacheInlineParts(iv, (v) => paidPerMillion(v, period)) }))
  }
  const p = m.pricing
  if (!p || !hasCachePricing(m)) return [{ parts: [{ text: '-' }] }]
  return cacheSplitLines(p, (v) => paidPerMillion(v, period))
}

function officialCacheLines(m: PlazaModel): PriceLine[] | null {
  const intervals = officialIntervals(m)
  if (hasTierCachePricing(intervals)) {
    return intervals.map((iv) => ({ tier: tierLabel(iv), parts: cacheInlineParts(iv, official) }))
  }
  const o = m.official_pricing
  if (!o || !hasOfficialCache(o)) return null
  return cacheSplitLines(o, official)
}

/** 一行模型在输入/输出/缓存三列的完整展示数据(实付 + 官方参考)。 */
function priceCells(row: PlazaRow): PriceCellData[] {
  const m = row.model
  const cells: PriceCellData[] = [
    { key: 'input', paid: paidTokenLines(m, row.period, 'input_price'), official: officialTokenLines(m, 'input_price') },
    { key: 'output', paid: paidTokenLines(m, row.period, 'output_price'), official: officialTokenLines(m, 'output_price') },
    { key: 'cache', paid: paidCacheLines(m, row.period), official: officialCacheLines(m) }
  ]
  return cells.map((cell) => ({ ...cell, official: distinctOfficial(cell.paid, cell.official) }))
}

function columnLabel(key: PriceCellData['key']): string {
  if (key === 'input') return t('modelPlaza.table.input')
  if (key === 'output') return t('modelPlaza.table.output')
  return t('modelPlaza.table.cache')
}

/** 行的生效倍率:时段行 = 生效倍率 × 时段倍率;生图独立倍率行取独立倍率。 */
function rowRateValue(row: PlazaRow): number {
  return row.period ? periodRate(row.period) : requestRate(row.model)
}

/** 需要划线展示的原倍率(仅专属倍率场景;生图独立倍率与时段行不划线)。 */
function rowRateStruck(row: PlazaRow): number | null {
  if (row.period || usesIndependentImageRate(row.model)) return null
  return hasCustomRate.value ? props.rateMultiplier : null
}

/** 时段行与专属倍率用品牌青强调,其余为常规深灰。 */
function rowRateClass(row: PlazaRow): string {
  const highlighted = row.period != null || (!usesIndependentImageRate(row.model) && hasCustomRate.value)
  return highlighted ? 'font-bold text-primary-600 dark:text-primary-400' : 'font-bold text-gray-700 dark:text-gray-300'
}
</script>
