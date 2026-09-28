<template>
  <article
    class="plaza-model-card group/card relative flex min-w-0 flex-col overflow-hidden rounded-2xl border border-gray-200/70 bg-white shadow-card transition duration-200 hover:-translate-y-0.5 hover:border-primary-300/70 hover:shadow-card-hover dark:border-dark-700/70 dark:bg-dark-800 dark:hover:border-primary-500/40"
  >
    <!-- 头部：模型图标 + 名称 + 计费说明徽章；右上角倍率徽章与复制入口 -->
    <header class="flex items-start gap-2.5 px-4 pb-3 pt-4">
      <span
        class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-gradient-to-br from-gray-50 to-gray-100 ring-1 ring-inset ring-gray-200/70 dark:from-dark-700/60 dark:to-dark-800 dark:ring-dark-600/70"
      >
        <ModelIcon :model="entry.name" size="20px" />
      </span>

      <div class="min-w-0 flex-1">
        <h4 class="truncate text-[13px] font-semibold leading-5 tracking-tight text-gray-900 dark:text-white" :title="entry.name">
          {{ entry.name }}
        </h4>
        <div class="mt-1.5 flex flex-wrap items-center gap-1">
          <span
            v-if="model.free"
            class="inline-flex items-center rounded bg-emerald-50 px-1.5 py-0.5 text-[10px] font-medium text-emerald-700 dark:bg-emerald-900/25 dark:text-emerald-400"
            :title="t('modelPlaza.card.freeBadgeHint')"
          >
            {{ t('modelPlaza.card.freeBadge') }}
          </span>
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

      <div class="flex shrink-0 flex-col items-end gap-1.5 pt-0.5">
        <span
          class="whitespace-nowrap rounded-full bg-primary-50 px-2 py-0.5 font-mono text-[11px] font-bold leading-4 text-primary-700 ring-1 ring-inset ring-primary-200/70 dark:bg-primary-400/10 dark:text-primary-300 dark:ring-primary-400/25"
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

    <!-- 价格块：只展示实付价（官方参考价已下线）。档位行用固定列网格,标签左、金额右基线对齐 -->
    <div class="plaza-price-block px-4 pb-4">
      <!-- token 计费：输入 / 输出 / 缓存三行,阶梯时每档一行 -->
      <dl v-if="display.isToken" class="plaza-price-rows">
        <div
          v-for="cell in display.cells"
          :key="cell.key"
          class="plaza-price-row"
          :class="{ 'plaza-price-row-lead': cell.key === 'input' }"
        >
          <dt class="plaza-price-label">
            {{ columnLabel(cell.key) }}
          </dt>
          <dd class="plaza-price-values">
            <div
              v-for="(line, idx) in cell.paid"
              :key="`paid-${idx}`"
              class="plaza-price-line"
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
          </dd>
        </div>
        <p class="plaza-price-unit">
          {{ t('modelPlaza.card.unitPerMillion') }}
        </p>
      </dl>

      <!-- 按次 / 按图片计费：计费口径已在头部徽章说过,价格行与 token 行同一权重排版 -->
      <div v-else class="plaza-request-prices">
        <span
          v-for="(item, idx) in display.requests"
          :key="idx"
          class="plaza-request-chip"
          :title="item.tier ? tierHint : undefined"
        >
          <span v-if="item.tier" class="plaza-request-tier">{{ item.tier }}</span>
          <span class="plaza-request-amount">{{ item.price }}</span>
          <span class="plaza-request-tier">{{ unitSuffix }}</span>
        </span>
        <span v-if="!display.requests.length" class="plaza-request-amount">-</span>
      </div>
    </div>

    <!-- 分时倍率：折叠成一行提示,完整说明在 tooltip -->
    <p
      v-if="timeSummary"
      class="flex items-start gap-1.5 border-t border-dashed border-gray-100 bg-gray-50/70 px-4 py-2 text-[11px] leading-4 text-gray-500 dark:border-dark-700/60 dark:bg-dark-900/30 dark:text-dark-400"
      :title="periodHint"
    >
      <Icon name="clock" size="xs" class="mt-[1px] h-3 w-3 shrink-0 text-amber-500 dark:text-amber-400" />
      <span class="min-w-0 font-mono break-words">{{ t('modelPlaza.card.timePricingSummary') }} {{ timeSummary }}</span>
    </p>

    <!-- 底部：该模型可用的分组（点击切到该组价格） -->
    <footer
      class="mt-auto flex flex-wrap items-center gap-1.5 border-t border-gray-100 bg-gray-50/60 px-4 py-2.5 dark:border-dark-700/70 dark:bg-dark-900/20"
    >
      <span class="shrink-0 text-[10px] font-medium uppercase tracking-wide text-gray-400 dark:text-dark-500">{{
        t('modelPlaza.card.availableIn')
      }}</span>
      <button
        v-for="ref in entry.groups"
        :key="ref.key"
        type="button"
        class="inline-flex min-w-0 max-w-full items-center gap-1 rounded-lg px-2 py-1 text-[11px] font-medium transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500/60"
        :class="
          ref.id === entry.displayGroupId
            ? 'bg-primary-50 text-primary-700 ring-1 ring-inset ring-primary-200 dark:bg-primary-400/10 dark:text-primary-300 dark:ring-primary-400/30'
            : ref.visible
              ? 'bg-gray-50 text-gray-500 ring-1 ring-inset ring-gray-100 hover:bg-gray-100 hover:text-gray-700 dark:bg-dark-900/40 dark:text-dark-400 dark:ring-dark-700/60 dark:hover:bg-dark-800 dark:hover:text-white'
              : 'bg-gray-50/60 text-gray-400 ring-1 ring-dashed ring-inset ring-gray-200 hover:bg-gray-100 hover:text-gray-600 dark:bg-dark-900/30 dark:text-dark-500 dark:ring-dark-600/60 dark:hover:bg-dark-800 dark:hover:text-dark-200'
        "
        :title="groupChipTitle(ref)"
        :aria-pressed="ref.id === entry.displayGroupId"
        @click="$emit('selectGroup', ref.id)"
      >
        <span class="min-w-0 truncate">{{ ref.name }}</span>
        <span class="shrink-0 font-mono">{{ ref.rateValue }}x</span>
      </button>
    </footer>
  </article>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import ModelIcon from '@/components/common/ModelIcon.vue'
import { useClipboard } from '@/composables/useClipboard'
import { BILLING_MODE_IMAGE, BILLING_MODE_VIDEO } from '@/constants/channel'
import type { PlazaGroupRef, PlazaModelEntry } from './plazaCatalog'
import {
  isTokenModel,
  modelPrice,
  modelRateStruck,
  modelRateValue,
  timePeriodHint,
  timePeriodsOf,
  timePeriodsSummary,
  type ModelPriceDisplay,
  type PriceLabels
} from './plazaPricing'

const props = defineProps<{
  /** 一个模型一张卡片（含它可用的全部分组）。 */
  entry: PlazaModelEntry
}>()

defineEmits<{
  /** 点击分组标签:把该模型的展示价切到该分组。 */
  selectGroup: [groupId: number]
}>()

const { t, locale } = useI18n()
const { copied, copyToClipboard } = useClipboard()

const model = computed(() => props.entry.model)
const isToken = computed(() => isTokenModel(model.value))
const rateValue = computed(() => modelRateValue(model.value, props.entry.context))
const struckRate = computed(() => modelRateStruck(model.value, props.entry.context))

const billingModeLabel = computed(() => {
  const mode = model.value.pricing?.billing_mode
  if (mode === BILLING_MODE_IMAGE) return t('modelPlaza.card.perImage')
  if (mode === BILLING_MODE_VIDEO) return t('modelPlaza.card.perVideo')
  return t('modelPlaza.card.perRequest')
})

const unitSuffix = computed(() => {
  const mode = model.value.pricing?.billing_mode
  if (mode === BILLING_MODE_IMAGE) return t('modelPlaza.card.perUnitImage')
  if (mode === BILLING_MODE_VIDEO) return t('modelPlaza.card.perUnitVideo')
  return t('modelPlaza.card.perUnitRequest')
})

const tierHint = computed(() =>
  model.value.long_context_basis === 'marginal'
    ? t('modelPlaza.card.tierHintMarginal')
    : t('modelPlaza.card.tierHint')
)

const groupName = computed(
  () => props.entry.groups.find((g) => g.id === props.entry.displayGroupId)?.name ?? ''
)

const rateTooltip = computed(() => {
  if (struckRate.value == null) {
    return t('modelPlaza.card.rateTooltip', { group: groupName.value, rate: rateValue.value })
  }
  return t('modelPlaza.card.customRateTooltip', {
    group: groupName.value,
    groupRate: props.entry.context.rateMultiplier,
    rate: rateValue.value
  })
})

const labels = computed<PriceLabels>(() => ({
  cacheWrite: t('modelPlaza.card.cacheWriteShort'),
  cacheRead: t('modelPlaza.card.cacheReadShort'),
  perRequest: t('modelPlaza.card.perUnitRequest'),
  perImage: t('modelPlaza.card.perUnitImage')
}))

const display = computed<ModelPriceDisplay>(() =>
  modelPrice(model.value, props.entry.context, labels.value)
)

/**
 * 分时摘要:一行带倍率,详情进 tooltip;非 token 模型后端不会配分时,这里同样不展示。
 * 多段分隔符跟随语种,英文界面不用中文顿号。
 */
const timeSummary = computed(() => {
  if (!isToken.value || !timePeriodsOf(model.value).length) return ''
  const prefix = model.value.time_pricing?.weekdays_only
    ? `${t('modelPlaza.card.timePricingWeekdays')} `
    : ''
  return (
    prefix +
    timePeriodsSummary(model.value, String(locale.value ?? '').toLowerCase().startsWith('zh') ? '、' : ', ')
  )
})

const periodHint = computed(() => timePeriodHint(model.value, props.entry.context, t))

/**
 * chip 提示语。当前展示组→告知可点回「全部」;被筛掉的组→明确告知会同时解除倍率筛选,
 * 避免点了之后用户的筛选约束静默丢失。
 */
function groupChipTitle(ref: PlazaGroupRef): string {
  if (ref.id === props.entry.displayGroupId) {
    return t('modelPlaza.card.groupChipCurrent', { group: ref.name })
  }
  if (!ref.visible) {
    return t('modelPlaza.card.groupChipHidden', { group: ref.name })
  }
  return t('modelPlaza.card.groupChipSwitch', { group: ref.name })
}

function columnLabel(key: 'input' | 'output' | 'cache'): string {
  if (key === 'input') return t('modelPlaza.card.input')
  if (key === 'output') return t('modelPlaza.card.output')
  return t('modelPlaza.card.cache')
}

function copyModelName() {
  void copyToClipboard(props.entry.name)
}
</script>

<style scoped>
/*
 * 价格块网格:标签列固定宽度,金额列撑满并右对齐。
 * 阶梯多档时每档一行,同一列内数字按列对齐,卡片之间高度差只来自档数差。
 */
.plaza-price-rows {
  @apply space-y-1;
}

.plaza-price-row {
  @apply grid grid-cols-[2.75rem_minmax(0,1fr)] items-baseline gap-2;
}

/* 输入价是比价基准(排序口径与它一致),给最强视觉权重 */
.plaza-price-row-lead .plaza-price-line {
  @apply text-[15px] leading-6;
}

.plaza-price-row-lead .plaza-price-label {
  @apply text-gray-500 dark:text-dark-300;
}

.plaza-price-label {
  @apply shrink-0 text-[11px] text-gray-400 dark:text-dark-500;
}

.plaza-price-values {
  @apply min-w-0 text-right;
}

.plaza-price-line {
  @apply font-mono text-[13px] font-semibold leading-5 tabular-nums text-gray-900 dark:text-gray-50;
}

.plaza-price-unit {
  @apply pt-0.5 text-right text-[10px] text-gray-400 dark:text-dark-500;
}

/* 按次/按图:金额与 token 首行同字号,单栏靠右;不用卡片包裹避与 token 行争视觉权重 */
.plaza-request-prices {
  @apply flex flex-wrap items-baseline justify-end gap-x-2 gap-y-1;
}

.plaza-request-chip {
  @apply inline-flex items-baseline gap-1 whitespace-nowrap;
}

.plaza-request-tier {
  @apply font-sans text-[10px] font-normal text-gray-400 dark:text-dark-500;
}

.plaza-request-amount {
  @apply font-mono text-[15px] font-semibold leading-6 tabular-nums text-gray-900 dark:text-gray-50;
}
</style>
