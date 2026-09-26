<template>
  <article
    class="plaza-model-card group/card relative flex min-w-0 flex-col rounded-xl border border-gray-200/80 bg-white p-4 shadow-card transition duration-200 hover:-translate-y-0.5 hover:border-primary-300 hover:shadow-card-hover dark:border-dark-700/60 dark:bg-dark-800/70 dark:hover:border-primary-500/50"
  >
    <!-- 头部：模型图标 + 名称 + 计费模式徽章；右上角倍率徽章与复制入口 -->
    <header class="flex items-start gap-2.5">
      <span
        class="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-gray-50 ring-1 ring-inset ring-gray-100 dark:bg-dark-900/40 dark:ring-dark-700/60"
      >
        <ModelIcon :model="model.name" size="20px" />
      </span>

      <div class="min-w-0 flex-1">
        <h4 class="truncate text-sm font-semibold text-gray-900 dark:text-white" :title="model.name">
          {{ model.name }}
        </h4>
        <div class="mt-1 flex flex-wrap items-center gap-1">
          <span
            v-if="!isToken"
            class="inline-flex items-center rounded bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-500 dark:bg-dark-700/70 dark:text-dark-300"
          >
            {{ billingModeLabel }}
          </span>
          <span
            v-if="model.long_context_basis === 'marginal'"
            class="inline-flex items-center rounded bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-500 dark:bg-dark-700/70 dark:text-dark-300"
            :title="t('modelPlaza.card.tierHintMarginal')"
          >
            {{ t('modelPlaza.card.marginalBadge') }}
          </span>
          <span
            v-if="model.pricing?.max_reasoning_effort_multiplier"
            class="inline-flex items-center rounded bg-amber-50 px-1.5 py-0.5 text-[10px] font-medium text-amber-600 dark:bg-amber-900/20 dark:text-amber-400"
            :title="t('modelPlaza.card.maxReasoningMultiplierHint', { multiplier: model.pricing.max_reasoning_effort_multiplier })"
          >
            {{ t('modelPlaza.card.maxReasoningMultiplierBadge', { multiplier: model.pricing.max_reasoning_effort_multiplier }) }}
          </span>
        </div>
      </div>

      <div class="flex shrink-0 flex-col items-end gap-1">
        <span
          class="whitespace-nowrap rounded-md bg-primary-50 px-2 py-0.5 font-mono text-xs font-bold text-primary-700 dark:bg-primary-400/10 dark:text-primary-300"
          :title="rateTooltip"
        >
          <span
            v-if="struckRate != null"
            class="mr-1 font-normal text-primary-600/50 line-through dark:text-primary-400/50"
            >{{ struckRate }}x</span
          >{{ rateValue }}x
        </span>
        <button
          type="button"
          class="text-gray-300 opacity-0 transition hover:text-gray-500 focus-visible:opacity-100 group-hover/card:opacity-100 dark:text-dark-600 dark:hover:text-dark-300 [@media(hover:none)]:opacity-60"
          :title="t('modelPlaza.card.copyModelName')"
          :aria-label="t('modelPlaza.card.copyModelName')"
          @click="copyModelName"
        >
          <Icon :name="copied ? 'check' : 'copy'" size="xs" class="h-3.5 w-3.5" />
        </button>
      </div>
    </header>

    <!-- 价格块：标准价 + 每个分时时段各一块（同量纲便于横向对比） -->
    <div
      v-for="block in blocks"
      :key="block.key"
      :class="block.period ? 'mt-2.5 border-t border-dashed border-gray-200 pt-2.5 dark:border-dark-700/70' : 'mt-3'"
    >
      <div v-if="block.period" class="mb-1 flex items-center justify-between gap-2">
        <span
          class="inline-flex items-center gap-1 font-mono text-[10px] font-medium text-gray-500 dark:text-dark-300"
          :title="periodHint"
        >
          <Icon name="clock" size="xs" class="h-3 w-3" />
          <span v-if="model.time_pricing?.weekdays_only" class="font-sans">{{
            t('modelPlaza.card.timePricingWeekdays')
          }}</span>
          {{ formatTimeWindow(block.period) }}
        </span>
        <span
          class="whitespace-nowrap font-mono text-[10px] font-bold text-primary-600 dark:text-primary-400"
          :title="t('modelPlaza.card.timeRateHint', { rate: effectiveRate, multiplier: block.period.multiplier })"
        >
          {{ periodRateValue(block.period) }}x
        </span>
      </div>

      <!-- token 计费：输入 / 输出 / 缓存三行，实付为主行，官方参考为弱化副行 -->
      <dl v-if="block.display.isToken" class="space-y-1">
        <div
          v-for="cell in block.display.cells"
          :key="cell.key"
          class="flex items-start justify-between gap-3"
        >
          <dt class="shrink-0 pt-0.5 text-[11px] text-gray-400 dark:text-dark-500">
            {{ columnLabel(cell.key) }}
          </dt>
          <dd class="min-w-0 flex-1 text-right">
            <div
              v-for="(line, idx) in cell.paid"
              :key="`paid-${idx}`"
              class="font-mono text-xs font-semibold leading-5 text-gray-900 dark:text-gray-50"
              :class="cell.key === 'cache' ? 'break-words' : 'whitespace-nowrap'"
            >
              <span
                v-if="line.tier"
                class="mr-1 font-sans font-normal text-gray-400 dark:text-dark-500"
                :title="tierHint"
                >{{ line.tier }}</span
              >
              <span
                v-for="(part, pi) in line.parts"
                :key="pi"
                :class="part.muted ? 'font-sans font-normal text-gray-400 dark:text-dark-500' : undefined"
                >{{ part.text }}</span
              >
            </div>
            <!-- 官方参考价与实付同量纲且不随时段变化,只在标准价块披露一次 -->
            <div v-if="cell.official && block.period === null" class="mt-0.5 space-y-0.5">
              <div
                v-for="(line, idx) in cell.official"
                :key="`official-${idx}`"
                class="font-mono text-[10px] leading-4 text-gray-400 dark:text-dark-500"
                :class="cell.key === 'cache' ? 'break-words' : 'whitespace-nowrap'"
                :title="t('modelPlaza.card.officialPrice')"
              >
                <span class="mr-1 font-sans">{{ t('modelPlaza.card.officialTag') }}</span>
                <span v-if="line.tier" class="mr-1 font-sans">{{ line.tier }}</span>
                <span v-for="(part, pi) in line.parts" :key="pi">{{ part.text }}</span>
              </div>
            </div>
          </dd>
        </div>
        <p v-if="block.period === null" class="pt-0.5 text-right text-[10px] text-gray-300 dark:text-dark-600">
          {{ t('modelPlaza.card.unitPerMillion') }}
        </p>
      </dl>

      <!-- 按次 / 按图片计费：单价芯片（多档时每档一枚） -->
      <div v-else class="plaza-request-prices flex flex-wrap items-center gap-1.5">
        <span
          v-for="(item, idx) in block.display.requests"
          :key="idx"
          class="inline-flex items-center gap-1 rounded-md bg-gray-50 px-2 py-1 font-mono text-xs font-semibold text-gray-900 ring-1 ring-inset ring-gray-100 dark:bg-dark-900/40 dark:text-gray-50 dark:ring-dark-700/60"
        >
          <span v-if="item.tier" class="font-sans text-[10px] font-normal text-gray-400 dark:text-dark-500">{{
            item.tier
          }}</span>
          {{ item.price }}
          <span class="font-sans text-[10px] font-normal text-gray-400 dark:text-dark-500">{{
            unitSuffix
          }}</span>
        </span>
        <span v-if="!block.display.requests.length" class="text-xs text-gray-400 dark:text-dark-500">-</span>
      </div>
    </div>
  </article>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import ModelIcon from '@/components/common/ModelIcon.vue'
import { useClipboard } from '@/composables/useClipboard'
import { BILLING_MODE_IMAGE } from '@/constants/channel'
import type { PlazaModel, PlazaTimePricingPeriod } from '@/api/modelPlaza'
import {
  effectiveGroupRate,
  formatTimeWindow,
  isTokenModel,
  modelPrice,
  modelRateStruck,
  modelRateValue,
  timePeriodHint,
  timePeriodsOf,
  type ModelPriceDisplay,
  type PlazaPriceContext,
  type PriceLabels
} from './plazaPricing'

const props = defineProps<{
  model: PlazaModel
  /** 分组派生的价格环境（倍率 / 生图独立倍率 / 高峰窗口）。 */
  context: PlazaPriceContext
}>()

const { t } = useI18n()
const { copied, copyToClipboard } = useClipboard()

const isToken = computed(() => isTokenModel(props.model))
const effectiveRate = computed(() => effectiveGroupRate(props.context))
const rateValue = computed(() => modelRateValue(props.model, props.context))
const struckRate = computed(() => modelRateStruck(props.model, props.context))

const billingModeLabel = computed(() =>
  props.model.pricing?.billing_mode === BILLING_MODE_IMAGE
    ? t('modelPlaza.card.perImage')
    : t('modelPlaza.card.perRequest')
)

const unitSuffix = computed(() =>
  props.model.pricing?.billing_mode === BILLING_MODE_IMAGE
    ? t('modelPlaza.card.perUnitImage')
    : t('modelPlaza.card.perUnitRequest')
)

const tierHint = computed(() =>
  props.model.long_context_basis === 'marginal'
    ? t('modelPlaza.card.tierHintMarginal')
    : t('modelPlaza.card.tierHint')
)

const rateTooltip = computed(() => {
  if (struckRate.value == null) return t('modelPlaza.card.rateTooltip', { rate: rateValue.value })
  return t('modelPlaza.card.customRateTooltip', {
    groupRate: props.context.rateMultiplier,
    rate: rateValue.value
  })
})

const periodHint = computed(() => timePeriodHint(props.model, props.context, t))

const labels = computed<PriceLabels>(() => ({
  cacheWrite: t('modelPlaza.card.cacheWriteShort'),
  cacheRead: t('modelPlaza.card.cacheReadShort'),
  perRequest: t('modelPlaza.card.perUnitRequest'),
  perImage: t('modelPlaza.card.perUnitImage')
}))

/** 一个价格块：period 为 null 时是标准价，否则为该分时时段生效价。 */
interface PriceBlock {
  key: string
  period: PlazaTimePricingPeriod | null
  display: ModelPriceDisplay
}

const blocks = computed<PriceBlock[]>(() => {
  const standard: PriceBlock = {
    key: 'standard',
    period: null,
    display: modelPrice(props.model, props.context, labels.value)
  }
  // 分时倍率只对 token 计费生效（后端写侧/读侧都拒绝非 token 配置），按次/按图的单价
  // 不乘时段倍率——若真收到脏数据，这里也不展开时段块,避免「价格与标准价相同却标注时段倍率」。
  const periodBlocks = isToken.value
    ? timePeriodsOf(props.model).map<PriceBlock>((period, idx) => ({
        key: `period-${idx}`,
        period,
        display: modelPrice(props.model, props.context, labels.value, period)
      }))
    : []
  return [standard, ...periodBlocks]
})

/** 时段生效倍率 = 分组生效倍率 × 时段倍率（去掉浮点噪声）。 */
function periodRateValue(period: PlazaTimePricingPeriod): number {
  return Math.round(effectiveRate.value * period.multiplier * 1000) / 1000
}

function columnLabel(key: 'input' | 'output' | 'cache'): string {
  if (key === 'input') return t('modelPlaza.card.input')
  if (key === 'output') return t('modelPlaza.card.output')
  return t('modelPlaza.card.cache')
}

function copyModelName() {
  void copyToClipboard(props.model.name)
}
</script>
