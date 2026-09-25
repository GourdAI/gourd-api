<template>
  <div class="space-y-3">
    <!-- 一级:分组(不展示平台维度) -->
    <div class="flex items-start gap-2">
      <span class="w-10 shrink-0 pt-2 text-xs font-semibold uppercase tracking-wider text-gray-400 dark:text-dark-500">
        {{ t('modelPlaza.filters.groupLabel') }}
      </span>
      <div class="flex flex-wrap items-center gap-2">
        <button
          type="button"
          class="rounded-lg px-3 py-1.5 text-sm font-medium transition"
          :class="chipClass(groupId === 'all')"
          @click="$emit('update:groupId', 'all')"
        >
          {{ t('modelPlaza.filters.all') }}
        </button>
        <button
          v-for="g in groups"
          :key="`group-${g.id}`"
          type="button"
          class="rounded-lg px-3 py-1.5 text-sm font-medium transition disabled:cursor-not-allowed disabled:opacity-40 disabled:grayscale"
          :class="chipClass(groupId === g.id)"
          :disabled="!groupEnabled(g)"
          @click="$emit('update:groupId', g.id)"
        >
          {{ g.name }}
        </button>
      </div>
    </div>

    <!-- 二级:倍率(当前组合下不存在的置灰) -->
    <div class="flex items-start gap-2">
      <span class="w-10 shrink-0 pt-2 text-xs font-semibold uppercase tracking-wider text-gray-400 dark:text-dark-500">
        {{ t('modelPlaza.filters.rateLabel') }}
      </span>
      <div class="flex flex-wrap items-center gap-2">
        <button
          type="button"
          class="rounded-lg px-3 py-1.5 text-sm font-medium transition"
          :class="chipClass(rate === 'all')"
          @click="$emit('update:rate', 'all')"
        >
          {{ t('modelPlaza.filters.all') }}
        </button>
        <button
          v-for="r in rates"
          :key="`rate-${r}`"
          type="button"
          class="rounded-lg px-3 py-1.5 font-mono text-sm font-medium transition disabled:cursor-not-allowed disabled:opacity-40 disabled:grayscale"
          :class="chipClass(rate === r)"
          :disabled="!rateEnabled(r)"
          @click="$emit('update:rate', r)"
        >
          {{ r }}x
        </button>
      </div>
    </div>

    <!-- 三级:模型名搜索(纯前端过滤) -->
    <div class="flex flex-wrap items-start gap-2">
      <span class="w-10 shrink-0 pt-2 text-xs font-semibold uppercase tracking-wider text-gray-400 dark:text-dark-500">
        {{ t('modelPlaza.filters.modelLabel') }}
      </span>
      <div class="relative w-full sm:w-72">
        <Icon
          name="search"
          size="sm"
          class="absolute left-3 top-1/2 -translate-y-1/2 text-gray-400 dark:text-dark-500"
        />
        <input
          :value="search"
          type="text"
          :placeholder="t('modelPlaza.filters.searchPlaceholder')"
          class="input rounded-lg py-1.5 pl-9 pr-9"
          @input="$emit('update:search', ($event.target as HTMLInputElement).value)"
        />
        <button
          v-if="search"
          type="button"
          class="absolute right-2.5 top-1/2 -translate-y-1/2 text-gray-400 transition-colors hover:text-gray-600 dark:text-dark-500 dark:hover:text-gray-300"
          @click="$emit('update:search', '')"
        >
          <Icon name="x" size="xs" class="h-3.5 w-3.5" />
        </button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'

const props = defineProps<{
  /** 全量分组(含生效倍率),两个维度的置灰联动由此推导。 */
  groups: Array<{ id: number; name: string; rate: number }>
  /** 全量生效倍率去重升序。 */
  rates: number[]
  groupId: number | 'all'
  rate: number | 'all'
  /** 模型名搜索词(纯前端过滤)。 */
  search: string
}>()

defineEmits<{
  'update:groupId': [value: number | 'all']
  'update:rate': [value: number | 'all']
  'update:search': [value: string]
}>()

const { t } = useI18n()

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
    ? 'bg-gradient-to-r from-primary-500 to-primary-600 text-white shadow-sm shadow-primary-500/30'
    : 'bg-white text-gray-600 ring-1 ring-inset ring-gray-200 enabled:hover:bg-gray-50 enabled:hover:text-gray-900 enabled:hover:ring-gray-300 dark:bg-dark-800/60 dark:text-dark-300 dark:ring-dark-700 dark:enabled:hover:bg-dark-800 dark:enabled:hover:text-white'
}
</script>
