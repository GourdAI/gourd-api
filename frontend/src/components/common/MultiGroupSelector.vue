<template>
  <div>
    <div
      v-if="isSearchable"
      class="flex items-center gap-2 rounded-t-lg border border-b-0 border-gray-200 bg-gray-50 px-3 py-2 dark:border-dark-600 dark:bg-dark-800"
    >
      <Icon name="search" size="sm" class="shrink-0 text-gray-400" />
      <input
        v-model="searchText"
        type="text"
        :disabled="disabled"
        :placeholder="searchPlaceholder"
        class="flex-1 bg-transparent text-sm text-gray-900 placeholder:text-gray-400 focus:outline-none disabled:cursor-not-allowed dark:text-gray-100 dark:placeholder:text-dark-400"
      />
    </div>
    <div
      :class="[
        'grid max-h-40 grid-cols-1 gap-1 overflow-y-auto p-2 sm:grid-cols-2',
        isSearchable
          ? 'rounded-b-lg border border-t-0 border-gray-200 bg-gray-50 dark:border-dark-600 dark:bg-dark-800'
          : 'rounded-lg border border-gray-200 bg-gray-50 dark:border-dark-600 dark:bg-dark-800',
        disabled ? 'opacity-60' : ''
      ]"
      role="group"
      :aria-label="t('keys.selectGroups')"
    >
      <label
        v-for="group in filteredGroups"
        :key="group.id"
        class="flex cursor-pointer items-center gap-2 rounded px-2 py-1.5 transition-colors hover:bg-white dark:hover:bg-dark-700"
        :class="isTypeLocked(group) && !disabled ? 'cursor-not-allowed opacity-40' : ''"
        :title="isTypeLocked(group) ? t('keys.mixedGroupTypeHint') : groupTitle(group)"
        :data-test="`${testId}-option-${group.id}`"
        :data-type-locked="isTypeLocked(group) ? 'true' : undefined"
      >
        <input
          type="checkbox"
          class="h-3.5 w-3.5 shrink-0 rounded border-gray-300 text-primary-500 focus:ring-primary-500 dark:border-dark-500"
          :value="group.id"
          :disabled="disabled || isTypeLocked(group)"
          :checked="modelValue.includes(group.id)"
          :data-test="`${testId}-checkbox-${group.id}`"
          @change="handleChange(group.id, ($event.target as HTMLInputElement).checked)"
        />
        <GroupBadge
          :name="group.name"
          :platform="group.platform"
          :subscription-type="group.subscription_type || undefined"
          :rate-multiplier="group.rate_multiplier == null ? undefined : group.rate_multiplier"
          :user-rate-multiplier="userGroupRates[group.id] ?? null"
          :peak-rate-enabled="group.peak_rate_enabled"
          :peak-start="group.peak_start"
          :peak-end="group.peak_end"
          :peak-rate-multiplier="group.peak_rate_multiplier"
          class="min-w-0 flex-1"
        />
        <span
          v-if="modelValue[0] === group.id"
          class="shrink-0 rounded bg-primary-100 px-1.5 py-0.5 text-[10px] font-semibold text-primary-700 dark:bg-primary-900/40 dark:text-primary-300"
        >
          {{ t('keys.primaryGroupBadge') }}
        </span>
      </label>
      <div
        v-if="filteredGroups.length === 0"
        class="col-span-full py-2 text-center text-sm text-gray-500 dark:text-gray-400"
        :data-test="`${testId}-empty`"
      >
        {{ emptyText }}
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import GroupBadge from './GroupBadge.vue'
import Icon from '@/components/icons/Icon.vue'
import type { Group } from '@/types'

/** Groups carrying the optional account counter returned by list endpoints. */
export type MultiGroupOption = Group & { account_count?: number }

/**
 * Multi-select group picker used by API key create/edit dialogs and bulk edit.
 *
 * Selection order is meaningful: `modelValue[0]` is the primary group sent to
 * the backend as `group_ids[0]`. The component never mutates `modelValue`
 * in place, it only emits a new array through `update:modelValue`.
 */
interface Props {
  modelValue: number[]
  groups: MultiGroupOption[]
  searchable?: boolean | 'auto'
  disabled?: boolean
  /** Per-group user rate overrides, forwarded to `GroupBadge`. */
  userGroupRates?: Record<number, number>
  /** Stable prefix for `data-test` hooks so parallel usages do not clash. */
  testId?: string
}

const props = withDefaults(defineProps<Props>(), {
  searchable: 'auto',
  disabled: false,
  userGroupRates: () => ({}),
  testId: 'multi-group'
})

const emit = defineEmits<{
  'update:modelValue': [value: number[]]
}>()

const { t } = useI18n()

const searchText = ref('')

const isSearchable = computed(() => {
  if (props.searchable === 'auto') return props.groups.length > 5
  return props.searchable
})

const searchPlaceholder = computed(() => t('keys.searchGroups'))

const filteredGroups = computed(() => {
  if (!isSearchable.value || !searchText.value) return props.groups
  const q = searchText.value.trim().toLowerCase()
  if (!q) return props.groups
  return props.groups.filter(
    (group) =>
      group.name.toLowerCase().includes(q) ||
      (group.description?.toLowerCase().includes(q) ?? false)
  )
})

const emptyText = computed(() =>
  searchText.value.trim() ? t('keys.noGroupsFound') : t('common.noGroupsAvailable')
)

const subscriptionTypeOf = (group: MultiGroupOption) => group.subscription_type ?? 'standard'

/**
 * 后端禁止一个 key 混绑订阅型与标准型分组（MIXED_GROUP_SUBSCRIPTION_TYPE）：
 * 生效分组在请求期决议产生，若候选类型不一致，订阅限额校验会被整体跳过。
 * 这里把约束前移到 UI：一旦已有选中，类型不同的分组直接禁用，
 * 让用户选不出非法组合，而不是提交后才撞后端错误。
 */
const selectedSubscriptionType = computed<string | null>(() => {
  for (const id of props.modelValue) {
    const group = props.groups.find((candidate) => candidate.id === id)
    if (group) return subscriptionTypeOf(group)
  }
  return null
})

const isTypeLocked = (group: MultiGroupOption) => {
  const selected = selectedSubscriptionType.value
  if (selected === null) return false
  if (props.modelValue.includes(group.id)) return false
  return subscriptionTypeOf(group) !== selected
}

const groupTitle = (group: MultiGroupOption) =>
  group.account_count == null
    ? group.name
    : t('admin.groups.rateAndAccounts', { rate: group.rate_multiplier, count: group.account_count })

const handleChange = (groupId: number, checked: boolean) => {
  if (checked) {
    if (props.modelValue.includes(groupId)) return
    emit('update:modelValue', [...props.modelValue, groupId])
    return
  }
  emit('update:modelValue', props.modelValue.filter((id) => id !== groupId))
}
</script>
