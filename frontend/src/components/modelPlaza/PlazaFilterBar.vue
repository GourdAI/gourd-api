<template>
  <div class="plaza-filter-bar rounded-2xl border border-gray-200/70 bg-white p-3 shadow-card dark:border-dark-700/70 dark:bg-dark-800/80">
    <!-- 顶层:模型搜索 + 排序 + 命中数(最高频操作放第一行) -->
    <div class="flex flex-wrap items-center gap-2">
      <div class="relative min-w-0 flex-1 sm:max-w-xs">
        <Icon
          name="search"
          size="sm"
          class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-gray-400 dark:text-dark-500"
        />
        <input
          :value="search"
          type="text"
          :id="searchId"
          :placeholder="t('modelPlaza.filters.searchPlaceholder')"
          :aria-label="t('modelPlaza.filters.searchPlaceholder')"
          class="input rounded-xl py-1.5 pl-9 pr-9 text-sm"
          @input="$emit('update:search', ($event.target as HTMLInputElement).value)"
        />
        <button
          v-if="search"
          type="button"
          class="absolute right-2.5 top-1/2 -translate-y-1/2 text-gray-400 transition-colors hover:text-gray-600 dark:text-dark-500 dark:hover:text-gray-300"
          :aria-label="t('modelPlaza.filters.clearSearch')"
          @click="$emit('update:search', '')"
        >
          <Icon name="x" size="xs" class="h-3.5 w-3.5" />
        </button>
      </div>

      <label class="flex items-center gap-1.5 text-xs text-gray-500 dark:text-dark-400">
        <span class="shrink-0">{{ t('modelPlaza.filters.sortLabel') }}</span>
        <select
          :value="sort"
          class="input rounded-xl py-1.5 pr-8 text-sm"
          :aria-label="t('modelPlaza.filters.sortLabel')"
          @change="$emit('update:sort', ($event.target as HTMLSelectElement).value as PlazaSortKey)"
        >
          <option value="default">{{ t('modelPlaza.filters.sortDefault') }}</option>
          <option value="priceAsc">{{ t('modelPlaza.filters.sortPriceAsc') }}</option>
          <option value="priceDesc">{{ t('modelPlaza.filters.sortPriceDesc') }}</option>
          <option value="nameAsc">{{ t('modelPlaza.filters.sortNameAsc') }}</option>
        </select>
      </label>

      <span class="ml-auto shrink-0 text-xs tabular-nums text-gray-400 dark:text-dark-500">
        {{ t('modelPlaza.filters.resultCount', { count: total }) }}
      </span>
    </div>

    <!-- 分组维度 -->
    <div class="mt-2.5 flex items-start gap-2 border-t border-gray-100 pt-2.5 dark:border-dark-700/60">
      <span class="plaza-filter-label">
        {{ t('modelPlaza.filters.groupLabel') }}
      </span>
      <div role="group" :aria-label="t('modelPlaza.filters.groupLabel')" class="flex min-w-0 flex-wrap items-center gap-1.5">
        <button
          type="button"
          class="plaza-filter-chip"
          :class="chipClass(groupId === 'all')"
          :aria-pressed="groupId === 'all'"
          @click="$emit('update:groupId', 'all')"
        >
          {{ t('modelPlaza.filters.all') }}
        </button>
        <button
          v-for="g in groups"
          :key="`group-${g.id}`"
          type="button"
          class="plaza-filter-chip disabled:cursor-not-allowed disabled:opacity-40 disabled:grayscale"
          :class="chipClass(groupId === g.id)"
          :aria-pressed="groupId === g.id"
          :disabled="!groupEnabled(g)"
          @click="$emit('update:groupId', g.id)"
        >
          <span class="min-w-0 truncate">{{ g.name }}</span>
          <span v-if="g.modelCount" class="plaza-filter-count">{{ g.modelCount }}</span>
        </button>
      </div>
    </div>

    <!-- 倍率维度(当前组合下不存在的置灰) -->
    <div v-if="rates.length > 1" class="mt-2 flex items-start gap-2">
      <span class="plaza-filter-label">
        {{ t('modelPlaza.filters.rateLabel') }}
      </span>
      <div role="group" :aria-label="t('modelPlaza.filters.rateLabel')" class="flex min-w-0 flex-wrap items-center gap-1.5">
        <button
          type="button"
          class="plaza-filter-chip"
          :class="chipClass(rate === 'all')"
          :aria-pressed="rate === 'all'"
          @click="$emit('update:rate', 'all')"
        >
          {{ t('modelPlaza.filters.all') }}
        </button>
        <button
          v-for="r in rates"
          :key="`rate-${r}`"
          type="button"
          class="plaza-filter-chip font-mono disabled:cursor-not-allowed disabled:opacity-40 disabled:grayscale"
          :class="chipClass(rate === r)"
          :aria-pressed="rate === r"
          :disabled="!rateEnabled(r)"
          @click="$emit('update:rate', r)"
        >
          {{ r }}x
        </button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { useId } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import type { PlazaSortKey } from './plazaCatalog'

const props = defineProps<{
  /** 全量分组(含生效倍率与组内模型数),两个维度的置灰联动由此推导。 */
  groups: Array<{ id: number; name: string; rate: number; modelCount?: number }>
  /** 全量生效倍率去重升序。 */
  rates: number[]
  groupId: number | 'all'
  rate: number | 'all'
  /** 排序维度。 */
  sort: PlazaSortKey
  /** 模型名搜索词(纯前端过滤)。 */
  search: string
  /** 当前筛选命中的模型数。 */
  total: number
}>()

defineEmits<{
  'update:groupId': [value: number | 'all']
  'update:rate': [value: number | 'all']
  'update:sort': [value: PlazaSortKey]
  'update:search': [value: string]
}>()

const { t } = useI18n()

/** 搜索框 id（同一页只一个筛选栏,用 uid 避免多实例碰撞）。 */
const searchId = useId()

/**
 * 两个维度互为约束(faceted):某选项可点 ⟺ 在「另一维」当前选择下仍有分组命中。
 * 「全部」永远可点,作为解除本维约束的出口;可点项组合恒有结果,无需选择修正。
 */
function groupEnabled(g: { rate: number }): boolean {
  return props.rate === 'all' || g.rate === props.rate
}

function rateEnabled(r: number): boolean {
  return props.groups.some((g) => g.rate === r && (props.groupId === 'all' || g.id === props.groupId))
}

function chipClass(active: boolean): string {
  return active
    ? 'bg-primary-600 text-white shadow-sm shadow-primary-600/25 dark:bg-primary-500 dark:shadow-primary-500/20'
    : 'bg-gray-50 text-gray-600 ring-1 ring-inset ring-gray-200/80 enabled:hover:bg-white enabled:hover:text-gray-900 enabled:hover:ring-gray-300 dark:bg-dark-900/40 dark:text-dark-300 dark:ring-dark-700 dark:enabled:hover:bg-dark-700/60 dark:enabled:hover:text-white'
}
</script>

<style scoped>
.plaza-filter-label {
  @apply shrink-0 select-none pt-1.5 text-[11px] font-semibold uppercase tracking-wider text-gray-400 dark:text-dark-500;
}

.plaza-filter-chip {
  @apply inline-flex max-w-[12rem] items-center gap-1 rounded-full px-2.5 py-1.5 text-xs font-medium transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500/60;
}

.plaza-filter-count {
  @apply shrink-0 text-[10px] font-normal opacity-60 tabular-nums;
}
</style>
