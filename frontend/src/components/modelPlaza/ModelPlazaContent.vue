<template>
  <div class="plaza-content space-y-4">
    <!-- 页头(独立形态下展示标题;后台形态 AppHeader 已有页面标题) -->
    <div v-if="!embedded" class="flex flex-wrap items-end justify-between gap-3">
      <div class="min-w-0">
        <h1 class="text-2xl font-bold tracking-tight text-gray-900 dark:text-white sm:text-[28px]">
          {{ t('modelPlaza.title') }}
        </h1>
        <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ t('modelPlaza.description') }}</p>
      </div>
      <p v-if="!isAuthenticated" class="plaza-inline-hint">
        <Icon name="infoCircle" size="xs" class="h-3.5 w-3.5 shrink-0" />
        {{ t('modelPlaza.anonymousHint') }}
      </p>
    </div>
    <!-- 后台形态下未登录提示留在右上角,不占独立一行 -->
    <p v-else-if="!isAuthenticated" class="plaza-inline-hint -mb-1">
      <Icon name="infoCircle" size="xs" class="h-3.5 w-3.5 shrink-0" />
      {{ t('modelPlaza.anonymousHint') }}
    </p>

    <!-- 全局价格说明(管理员配置,Markdown):浅底次级面板,不与卡片争层级 -->
    <details
      v-if="descriptionHtml"
      class="plaza-description-panel group/panel overflow-hidden rounded-2xl border border-gray-200/60 bg-gray-50/80 dark:border-dark-700/60 dark:bg-dark-800/40"
      open
    >
      <summary
        class="flex cursor-pointer select-none items-center gap-2 px-4 py-3 text-sm font-semibold text-gray-700 dark:text-dark-200 [&::-webkit-details-marker]:hidden"
      >
        <Icon name="book" size="xs" class="h-4 w-4 shrink-0 text-primary-600 dark:text-primary-400" />
        <span class="min-w-0 flex-1">{{ t('modelPlaza.pricingNotes') }}</span>
        <Icon
          name="chevronUp"
          size="xs"
          class="h-4 w-4 shrink-0 text-gray-400 transition-transform group-open/panel:rotate-180 dark:text-dark-500"
        />
      </summary>
      <div class="plaza-description border-t border-gray-200/60 px-4 py-3 text-sm dark:border-dark-700/60" v-html="descriptionHtml"></div>
    </details>

    <!-- 加载/错误/空 -->
    <div v-if="loading" class="grid gap-4 sm:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4" aria-hidden="true">
      <div
        v-for="n in 8"
        :key="n"
        class="h-[168px] animate-pulse rounded-2xl border border-gray-200/70 bg-white dark:border-dark-700/70 dark:bg-dark-800"
      ></div>
    </div>
    <div
      v-else-if="error"
      class="flex flex-col items-center gap-2 rounded-2xl border border-red-200 bg-red-50/70 px-5 py-12 text-center dark:border-red-500/30 dark:bg-red-500/10"
    >
      <Icon name="exclamationCircle" size="md" class="h-6 w-6 text-red-500 dark:text-red-400" />
      <p class="text-sm font-medium text-red-700 dark:text-red-300">{{ t('modelPlaza.loadFailed') }}</p>
    </div>
    <template v-else>
      <!-- 筛选区:搜索/排序 → 分组 → 倍率 -->
      <PlazaFilterBar
        :groups="groupOptions"
        :rates="rates"
        :group-id="selectedGroupId"
        :rate="selectedRate"
        :sort="sortKey"
        :search="searchQuery"
        :total="filteredEntries.length"
        @update:group-id="selectedGroupId = $event"
        @update:rate="selectedRate = $event"
        @update:sort="sortKey = $event"
        @update:search="searchQuery = $event"
      />

      <!-- 分组口径提示:选中分组时先展示该分组自身信息,再补充高峰/阶梯等影响实付价的说明 -->
      <div
        v-if="selectedGroup || groupNotes.length > 0"
        class="plaza-group-panel space-y-1.5 rounded-xl border border-primary-100/80 bg-primary-50/50 px-4 py-3 dark:border-primary-400/20 dark:bg-primary-400/[0.06]"
      >
        <div v-if="selectedGroup" class="flex flex-wrap items-center gap-2">
          <GroupBadge
            :name="selectedGroup.name"
            :subscription-type="(selectedGroup.subscription_type || 'standard') as SubscriptionType"
            :rate-multiplier="selectedGroup.rate_multiplier"
            :user-rate-multiplier="selectedGroup.user_rate_multiplier ?? null"
            :peak-rate-enabled="selectedGroup.peak_rate_enabled"
            :peak-start="selectedGroup.peak_start"
            :peak-end="selectedGroup.peak_end"
            :peak-rate-multiplier="selectedGroup.peak_rate_multiplier"
            brand
            always-show-rate
          />
          <span
            v-if="selectedGroup.is_exclusive"
            class="inline-flex items-center gap-1 rounded-md bg-purple-50 px-2 py-0.5 text-xs font-medium text-purple-600 dark:bg-purple-900/20 dark:text-purple-400"
          >
            <Icon name="shield" size="xs" class="h-3 w-3" />
            {{ t('modelPlaza.badges.exclusive') }}
          </span>
          <span
            v-if="selectedGroup.subscription_type === 'subscription'"
            class="inline-flex items-center rounded-md bg-violet-50 px-2 py-0.5 text-xs font-medium text-violet-600 dark:bg-violet-900/20 dark:text-violet-400"
          >
            {{ t('modelPlaza.badges.subscription') }}
          </span>
        </div>
        <p v-if="selectedGroup?.description" class="text-xs text-gray-500 dark:text-dark-400">
          {{ selectedGroup.description }}
        </p>
        <ul v-if="groupNotes.length" class="plaza-group-notes space-y-1">
          <li
            v-for="note in groupNotes"
            :key="note.key"
            class="flex items-start gap-1.5 text-xs"
            :class="note.tone === 'peak' ? 'text-amber-600 dark:text-amber-400' : 'text-gray-500 dark:text-dark-400'"
          >
            <Icon :name="note.tone === 'peak' ? 'clock' : 'infoCircle'" size="xs" class="mt-[2px] h-3 w-3 shrink-0" />
            <span class="min-w-0">{{ note.text }}</span>
          </li>
        </ul>
      </div>

      <!-- 模型卡片网格:一张卡片一个模型,价格随筛选语境切换 -->
      <div v-if="filteredEntries.length > 0" class="grid gap-4 sm:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4">
        <PlazaModelCard
          v-for="entry in filteredEntries"
          :key="entry.key"
          :entry="entry"
          @select-group="focusGroup"
        />
      </div>
      <div
        v-else
        class="flex flex-col items-center gap-2 rounded-2xl border border-dashed border-gray-200 bg-white/60 px-5 py-14 text-center dark:border-dark-600/80 dark:bg-dark-800/40"
      >
        <Icon name="search" size="md" class="h-6 w-6 text-gray-300 dark:text-dark-600" />
        <p class="text-sm text-gray-500 dark:text-dark-400">
          {{ searchActive ? t('modelPlaza.noSearchResult') : t('modelPlaza.empty') }}
        </p>
        <button
          v-if="searchActive"
          type="button"
          class="text-xs font-medium text-primary-600 transition hover:text-primary-700 dark:text-primary-300"
          @click="searchQuery = ''"
        >
          {{ t('modelPlaza.clearSearch') }}
        </button>
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import Icon from '@/components/icons/Icon.vue'
import PlazaFilterBar from './PlazaFilterBar.vue'
import PlazaModelCard from './PlazaModelCard.vue'
import GroupBadge from '@/components/common/GroupBadge.vue'
import { buildPlazaModelEntries, matchPlazaModelEntry, sortPlazaModelEntries } from './plazaCatalog'
import type { PlazaSortKey } from './plazaCatalog'
import { hasPeakRate, formatPeakRateWindow, serverTimezoneLabel } from '@/utils/peak-rate'
import type { SubscriptionType } from '@/types'
import type { ModelPlazaGroup, ModelPlazaResponse, PlazaModel } from '@/api/modelPlaza'
import { useAuthStore } from '@/stores/auth'
import { useAppStore } from '@/stores/app'

const props = defineProps<{
  response: ModelPlazaResponse | null
  loading: boolean
  error?: boolean
  /** 后台内嵌形态(AppLayout 内):隐藏页头。 */
  embedded?: boolean
}>()

const { t } = useI18n()
const authStore = useAuthStore()
const appStore = useAppStore()
const isAuthenticated = computed(() => authStore.isAuthenticated)

const selectedGroupId = ref<number | 'all'>('all')
const selectedRate = ref<number | 'all'>('all')
const sortKey = ref<PlazaSortKey>('default')
const searchQuery = ref('')

const searchActive = computed(() => searchQuery.value.trim() !== '')

const descriptionHtml = computed(() => {
  const md = props.response?.description?.trim()
  if (!md) return ''
  return DOMPurify.sanitize(marked.parse(md) as string)
})

/** 生效倍率 = 用户专属倍率 ?? 分组默认倍率。 */
function effectiveRate(g: ModelPlazaGroup): number {
  return g.user_rate_multiplier ?? g.rate_multiplier
}

const allGroups = computed(() => props.response?.groups ?? [])

/** 接口给到 null/缺失的 models 不能把整页炸成白屏。 */
function modelsOf(g: ModelPlazaGroup): PlazaModel[] {
  return g.models ?? []
}

const groupOptions = computed(() =>
  allGroups.value.map((g) => ({
    id: g.id,
    name: g.name,
    rate: effectiveRate(g),
    modelCount: new Set(modelsOf(g).map((m) => m.name)).size
  }))
)

/** 全量生效倍率;当前组合下不可用的项由 FilterBar 置灰而非隐藏。 */
const rates = computed(() =>
  [...new Set(allGroups.value.map(effectiveRate))].sort((a, b) => a - b)
)

/** 数据刷新后选中的倍率可能不复存在,重置为全部。 */
watch(rates, (list) => {
  if (selectedRate.value !== 'all' && !list.includes(selectedRate.value)) {
    selectedRate.value = 'all'
  }
})

/**
 * 数据刷新后选中的分组可能已被删除/不再可见。不重置的话
 * visibleGroupIds 会空掉 → 整页空态且筛选条上无项可点,用户只能刷新页面。
 */
watch(allGroups, (list) => {
  if (selectedGroupId.value !== 'all' && !list.some((g) => g.id === selectedGroupId.value)) {
    selectedGroupId.value = 'all'
  }
})

/** 分组 + 倍率两个维度共同决定的可见分组集合(卡片价格与底部标签都以此为准)。 */
const visibleGroupIds = computed(() => {
  const ids = new Set<number>()
  for (const g of allGroups.value) {
    if (selectedGroupId.value !== 'all' && g.id !== selectedGroupId.value) continue
    if (selectedRate.value !== 'all' && effectiveRate(g) !== selectedRate.value) continue
    ids.add(g.id)
  }
  return ids
})

/** 分组 id → 高峰窗口描述(倍率 + 服务器时区);未启用高峰的分组不入表。 */
const peakWindows = computed(() => {
  const label = serverTimezoneLabel(appStore.cachedPublicSettings?.server_utc_offset)
  const map = new Map<number, string>()
  for (const g of allGroups.value) {
    if (hasPeakRate(g)) map.set(g.id, formatPeakRateWindow(g, label))
  }
  return map
})

/**
 * 模型目录。专属倍率会改变生效值,不能只依赖后端按默认倍率的排序,
 * 因此每次筛选变化都重建(分组数量有限,展平成本可忽略)。
 */
const entries = computed(() =>
  buildPlazaModelEntries(allGroups.value, {
    visibleGroupIds: visibleGroupIds.value,
    preferredGroupId: selectedGroupId.value,
    peakWindows: peakWindows.value
  })
)

const filteredEntries = computed(() =>
  sortPlazaModelEntries(
    entries.value.filter((entry) => matchPlazaModelEntry(entry, searchQuery.value)),
    sortKey.value
  )
)

/** 点击卡片底部某个分组标签:整页切到该分组价格(再点一次回到「全部」)。 */
function focusGroup(groupId: number) {
  if (selectedGroupId.value === groupId) {
    selectedGroupId.value = 'all'
    return
  }
  const group = allGroups.value.find((g) => g.id === groupId)
  // 分组已不存在于本次数据时回到「全部」,避免落到空白页。
  if (!group) {
    selectedGroupId.value = 'all'
    return
  }
  // 该分组不在当前倍率筛选内时一并解除倍率约束,避免切过去得到空结果。
  if (selectedRate.value !== 'all' && effectiveRate(group) !== selectedRate.value) {
    selectedRate.value = 'all'
  }
  selectedGroupId.value = groupId
}

/** 选中分组的原始数据;'all' 时为 null。 */
const selectedGroup = computed(() =>
  selectedGroupId.value === 'all'
    ? null
    : allGroups.value.find((g) => g.id === selectedGroupId.value) ?? null
)

interface GroupNote {
  key: string
  tone: 'peak' | 'info'
  text: string
}

/** 分组说明区最多条数(超出折成「还有 N 条」)。 */
const MAX_GROUP_NOTES = 4

/**
 * 影响实付价的分组级说明:高峰倍率、关闭长上下文阶梯。
 * 选中单分组时只列该组;「全部」时逐组列出(同名分组用 id 区分 key)。
 */
const groupNotes = computed<GroupNote[]>(() => {
  const notes: GroupNote[] = []
  for (const g of allGroups.value) {
    if (!visibleGroupIds.value.has(g.id)) continue
    if (peakWindows.value.has(g.id)) {
      notes.push({
        key: `peak-${g.id}`,
        tone: 'peak',
        text: t('modelPlaza.detail.peakNote', {
          group: g.name,
          window: peakWindows.value.get(g.id),
          multiplier: g.peak_rate_multiplier
        })
      })
    }
    // 分组关掉阶梯时后端会把实付压成单档,实付列看不出「本来有阶梯」;取不到官方价时用实付多档兜底。
    const hasLadderEvidence =
      modelsOf(g).some((m) => (m.official_pricing?.intervals?.length ?? 0) > 1) ||
      modelsOf(g).some((m) => (m.pricing?.intervals?.length ?? 0) > 1)
    if (g.long_context_pricing_enabled === false && hasLadderEvidence) {
      notes.push({
        key: `ladder-${g.id}`,
        tone: 'info',
        text: selectedGroupId.value === 'all'
          ? t('modelPlaza.detail.longContextDisabledNoteGroup', { group: g.name })
          : t('modelPlaza.detail.longContextDisabledNote')
      })
    }
  }
  // 分组很多时只留前几条,避免说明区长过卡片网格。
  if (notes.length > MAX_GROUP_NOTES) {
    return [
      ...notes.slice(0, MAX_GROUP_NOTES),
      { key: 'more', tone: 'info', text: t('modelPlaza.detail.moreNotes', { count: notes.length - MAX_GROUP_NOTES }) }
    ]
  }
  return notes
})
</script>

<style scoped>
.plaza-inline-hint {
  @apply inline-flex items-center gap-1.5 rounded-full bg-white/70 px-2.5 py-1 text-xs text-gray-500 ring-1 ring-inset ring-gray-200/70 dark:bg-dark-800/60 dark:text-dark-400 dark:ring-dark-700;
}

.plaza-description {
  line-height: 1.7;
  overflow-wrap: anywhere;
}

.plaza-description :deep(h1),
.plaza-description :deep(h2),
.plaza-description :deep(h3) {
  @apply mb-2 mt-3 font-semibold text-gray-900 first:mt-0 dark:text-white;
}

.plaza-description :deep(p) {
  @apply mb-2 text-gray-700 last:mb-0 dark:text-dark-200;
}

.plaza-description :deep(a) {
  @apply text-primary-600 underline underline-offset-4 hover:text-primary-700 dark:text-primary-300;
}

.plaza-description :deep(ul) {
  @apply mb-2 list-disc pl-5;
}

.plaza-description :deep(ol) {
  @apply mb-2 list-decimal pl-5;
}

.plaza-description :deep(li) {
  @apply mb-0.5 text-gray-700 dark:text-dark-200;
}

.plaza-description :deep(code) {
  @apply rounded bg-gray-100 px-1.5 py-0.5 font-mono text-xs dark:bg-dark-800;
}

.plaza-description :deep(blockquote) {
  @apply my-2 border-l-4 border-gray-300 pl-3 text-gray-600 dark:border-dark-600 dark:text-dark-300;
}
</style>
