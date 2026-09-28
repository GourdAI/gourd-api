<template>
  <div>
    <!-- Multi-select Dropdown -->
    <div class="relative mb-3">
      <div
        @click="toggleDropdown"
        class="cursor-pointer rounded-lg border border-gray-300 bg-white px-3 py-2 dark:border-dark-500 dark:bg-dark-700"
      >
        <div class="grid grid-cols-2 gap-1.5">
          <span
            v-for="model in modelValue"
            :key="model"
            class="inline-flex items-center justify-between gap-1 rounded bg-gray-100 px-2 py-1 text-xs text-gray-700 dark:bg-dark-600 dark:text-gray-300"
          >
            <span class="flex items-center gap-1 truncate">
              <ModelIcon :model="model" size="14px" />
              <span class="truncate">{{ model }}</span>
            </span>
            <button
              type="button"
              @click.stop="removeModel(model)"
              class="shrink-0 rounded-full hover:bg-gray-200 dark:hover:bg-dark-500"
            >
              <Icon name="x" size="xs" class="h-3.5 w-3.5" :stroke-width="2" />
            </button>
          </span>
        </div>
        <div class="mt-2 flex items-center justify-between border-t border-gray-200 pt-2 dark:border-dark-600">
          <span class="text-xs text-gray-400">{{ t('admin.accounts.modelCount', { count: modelValue.length }) }}</span>
          <svg class="h-5 w-5 text-gray-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7" />
          </svg>
        </div>
      </div>
      <!-- Dropdown List -->
      <div
        v-if="showDropdown"
        class="absolute left-0 right-0 top-full z-50 mt-1 rounded-lg border border-gray-200 bg-white shadow-lg dark:border-dark-600 dark:bg-dark-700"
      >
        <div class="sticky top-0 border-b border-gray-200 bg-white p-2 dark:border-dark-600 dark:bg-dark-700">
          <input
            v-model="searchQuery"
            type="text"
            class="input w-full text-sm"
            :placeholder="t('admin.accounts.searchModels')"
            @click.stop
          />
        </div>
        <div class="max-h-52 overflow-auto">
          <div
            v-for="model in filteredModels"
            :key="model.value"
            data-testid="model-option"
            class="group flex items-center hover:bg-gray-100 dark:hover:bg-dark-600"
          >
            <button
              type="button"
              data-testid="select-model"
              class="flex min-w-0 flex-1 items-center gap-2 px-3 py-2 text-left text-sm"
              @click="toggleModel(model.value)"
            >
              <span
                :class="[
                  'flex h-4 w-4 shrink-0 items-center justify-center rounded border',
                  modelValue.includes(model.value)
                    ? 'border-primary-500 bg-primary-500 text-white'
                    : 'border-gray-300 dark:border-dark-500'
                ]"
              >
                <svg v-if="modelValue.includes(model.value)" class="h-3 w-3" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="3" d="M5 13l4 4L19 7" />
                </svg>
              </span>
              <ModelIcon :model="model.value" size="18px" />
              <span class="truncate text-gray-900 dark:text-white">{{ model.value }}</span>
            </button>
            <button
              type="button"
              data-testid="copy-model-id"
              class="mr-2 rounded p-1.5 text-gray-400 opacity-70 transition-colors hover:bg-gray-200 hover:text-primary-600 focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 group-hover:opacity-100 dark:text-gray-500 dark:hover:bg-dark-500 dark:hover:text-primary-400"
              :title="`${t('common.copy')} ${model.value}`"
              :aria-label="`${t('common.copy')} ${model.value}`"
              @click="copyModelId(model.value)"
            >
              <Icon name="copy" size="sm" />
            </button>
          </div>
          <div v-if="filteredModels.length === 0" class="px-3 py-4 text-center text-sm text-gray-500">
            {{ t('admin.accounts.noMatchingModels') }}
          </div>
        </div>
      </div>
    </div>

    <!-- Quick Actions -->
    <div class="mb-4 flex flex-wrap gap-2">
      <button
        type="button"
        @click="fillRelated"
        class="rounded-lg border border-blue-200 px-3 py-1.5 text-sm text-blue-600 hover:bg-blue-50 dark:border-blue-800 dark:text-blue-400 dark:hover:bg-blue-900/30"
      >
        {{ t('admin.accounts.fillRelatedModels') }}
      </button>
      <button
        v-if="canSyncUpstream"
        type="button"
        data-testid="sync-upstream-models"
        @click="syncUpstreamModels"
        :disabled="isSyncingUpstream"
        class="rounded-lg border border-emerald-200 px-3 py-1.5 text-sm text-emerald-600 hover:bg-emerald-50 disabled:cursor-not-allowed disabled:opacity-60 dark:border-emerald-800 dark:text-emerald-400 dark:hover:bg-emerald-900/30"
      >
        {{ isSyncingUpstream ? t('admin.accounts.syncUpstreamModelsLoading') : t('admin.accounts.syncUpstreamModels') }}
      </button>
      <button
        type="button"
        @click="clearAll"
        class="rounded-lg border border-red-200 px-3 py-1.5 text-sm text-red-600 hover:bg-red-50 dark:border-red-800 dark:text-red-400 dark:hover:bg-red-900/30"
      >
        {{ t('admin.accounts.clearAllModels') }}
      </button>
    </div>

    <!-- Upstream Candidates (require explicit confirmation) -->
    <div
      v-if="upstreamCandidates.length > 0"
      data-testid="upstream-candidates"
      class="mb-4 rounded-lg border border-emerald-200 bg-emerald-50/50 p-3 dark:border-emerald-800 dark:bg-emerald-900/10"
    >
      <div class="mb-2 flex items-center justify-between gap-2">
        <span class="text-xs font-medium text-emerald-700 dark:text-emerald-400">
          {{ t('admin.accounts.upstreamCandidatesTitle', { count: upstreamCandidates.length }) }}
        </span>
        <span class="flex shrink-0 items-center gap-1.5">
          <button
            type="button"
            data-testid="add-all-upstream-candidates"
            @click="addAllUpstreamCandidates"
            class="rounded border border-emerald-200 px-2 py-1 text-xs text-emerald-600 hover:bg-emerald-100 dark:border-emerald-800 dark:text-emerald-400 dark:hover:bg-emerald-900/30"
          >
            {{ t('admin.accounts.addAllUpstreamCandidates') }}
          </button>
          <button
            type="button"
            data-testid="dismiss-upstream-candidates"
            @click="dismissUpstreamCandidates"
            class="rounded p-1 text-gray-400 hover:bg-gray-200 hover:text-gray-600 dark:hover:bg-dark-500 dark:hover:text-gray-300"
            :title="t('admin.accounts.dismissUpstreamCandidates')"
            :aria-label="t('admin.accounts.dismissUpstreamCandidates')"
          >
            <Icon name="x" size="xs" class="h-3.5 w-3.5" :stroke-width="2" />
          </button>
        </span>
      </div>
      <div class="max-h-52 overflow-auto">
        <div
          v-for="candidate in upstreamCandidates"
          :key="candidate.id"
          data-testid="upstream-candidate"
          class="flex items-center justify-between gap-2 py-1"
        >
          <span class="flex min-w-0 flex-1 items-center gap-1.5 text-xs text-gray-700 dark:text-gray-300">
            <ModelIcon :model="candidate.id" size="14px" class="shrink-0" />
            <!-- 候选行是管理员唯一能看到「上游上新了什么」的地方，只示 config_name
                 很难判断是不是自己要的模型，因此补上上游展示名与上下文窗口。
                 两段文本都必须 min-w-0 + truncate：上游名字长度不可控，
                 不做约束会在窄屏撑破整行、把「添加」按钮顶出可视区。 -->
            <span
              class="min-w-0 truncate font-medium"
              :title="candidate.displayName || candidate.id"
            >{{ candidate.displayName || candidate.id }}</span>
            <span
              v-if="candidate.displayName"
              class="min-w-0 flex-1 truncate font-mono text-gray-400 dark:text-gray-500"
              :title="candidate.id"
            >{{ candidate.id }}</span>
            <span
              v-if="candidate.contextWindow"
              class="shrink-0 text-gray-400 dark:text-gray-500"
            >{{ formatContextWindow(candidate.contextWindow) }}</span>
          </span>
          <button
            type="button"
            :data-testid="`add-upstream-candidate:${candidate.id}`"
            @click="addUpstreamCandidate(candidate.id)"
            class="shrink-0 rounded border border-emerald-200 px-2 py-0.5 text-xs text-emerald-600 hover:bg-emerald-100 dark:border-emerald-800 dark:text-emerald-400 dark:hover:bg-emerald-900/30"
          >
            {{ t('admin.accounts.addCandidateToSelected') }}
          </button>
        </div>
      </div>
    </div>

    <!-- Custom Model Input -->
    <div class="mb-3">
      <label class="mb-1.5 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.accounts.customModelName') }}</label>
      <div class="flex gap-2">
        <input
          v-model="customModel"
          type="text"
          class="input flex-1"
          :placeholder="t('admin.accounts.enterCustomModelName')"
          @keydown.enter.prevent="handleEnter"
          @compositionstart="isComposing = true"
          @compositionend="isComposing = false"
        />
        <button
          type="button"
          @click="addCustom"
          class="rounded-lg bg-primary-50 px-4 py-2 text-sm font-medium text-primary-600 hover:bg-primary-100 dark:bg-primary-900/30 dark:text-primary-400 dark:hover:bg-primary-900/50"
        >
          {{ t('admin.accounts.addModel') }}
        </button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { accountsAPI } from '@/api/admin/accounts'
import type { SyncUpstreamPreviewParams } from '@/api/admin/accounts'
import { useClipboard } from '@/composables/useClipboard'
import ModelIcon from '@/components/common/ModelIcon.vue'
import Icon from '@/components/icons/Icon.vue'
import { allModels, getModelsByPlatform } from '@/composables/useModelWhitelist'
import { extractApiErrorMessage } from '@/utils/apiError'

const { t } = useI18n()

const props = defineProps<{
  modelValue: string[]
  platform?: string
  platforms?: string[]
  accountId?: number
  syncCredentials?: {
    platform: string
    type: string
    base_url?: string
    api_key: string
  }
}>()

const emit = defineEmits<{
  'update:modelValue': [value: string[]]
  'upstream-synced': []
}>()

const appStore = useAppStore()
const { copyToClipboard } = useClipboard()

const showDropdown = ref(false)
const searchQuery = ref('')
const customModel = ref('')
const isComposing = ref(false)
const isSyncingUpstream = ref(false)
const upstreamCandidates = ref<UpstreamCandidate[]>([])

// 候选同时携带上游展示名与上下文窗口：上游直出的 display_name 已随 metadata
// 返回，不在候选里展示等于白产出（管理员只能对着 config_name 猜）。
type UpstreamCandidate = {
  id: string
  displayName?: string
  contextWindow?: number
}

// 同步结果不能自动写进已选白名单（上游可能含内部实验 / 已下线通道），
// 因此只暂存为候选；账号上下文或平台集变化意味着候选已不属于当前表单，必须丢弃。
watch(
  () => [
    props.accountId,
    props.platform,
    props.platforms?.join(',') ?? '',
    props.syncCredentials?.platform ?? ''
  ] as const,
  () => {
    upstreamCandidates.value = []
  }
)
const normalizedPlatforms = computed(() => {
  const rawPlatforms =
    props.platforms && props.platforms.length > 0
      ? props.platforms
      : props.platform
        ? [props.platform]
        : []

  return Array.from(
    new Set(
      rawPlatforms
        .map(platform => platform?.trim())
        .filter((platform): platform is string => Boolean(platform))
    )
  )
})

const upstreamSyncPlatforms = new Set([
  'anthropic',
  'openai',
  'gemini',
  'antigravity',
  'grok',
  'kimi',
  'zhipu',
  'deepseek',
  'minimax',
  'opencode_go',
  'qoder',
  'trae'
])

// preview（新建未落库）同步只递 platform/type/base_url/api_key，而 Trae / Qoder 的
// 拉取链路读的是 credentials.access_token / personal_access_token（Qoder 连 api_key
// 都不认），走 preview 必定只能得到配置错误。不展示按钮比让人点了拿到 400 好；
// 已建账号（accountId 分支）从库内取真凭据，仍然可用。
const previewUnsupportedPlatforms = new Set(['trae', 'qoder'])

const canSyncUpstream = computed(() => {
  if (props.accountId) {
    if (normalizedPlatforms.value.length === 0) return true
    return normalizedPlatforms.value.some(platform => upstreamSyncPlatforms.has(platform.toLowerCase()))
  }
  if (props.syncCredentials) {
    const platform = props.syncCredentials.platform.toLowerCase()
    return upstreamSyncPlatforms.has(platform) && !previewUnsupportedPlatforms.has(platform)
  }
  return false
})

const availableOptions = computed(() => {
  if (normalizedPlatforms.value.length === 0) {
    return allModels
  }

  const allowedModels = new Set<string>()
  for (const platform of normalizedPlatforms.value) {
    for (const model of getModelsByPlatform(platform)) {
      allowedModels.add(model)
    }
  }

  return allModels.filter(model => allowedModels.has(model.value))
})

const filteredModels = computed(() => {
  const query = searchQuery.value.toLowerCase().trim()
  if (!query) return availableOptions.value
  return availableOptions.value.filter(
    m => m.value.toLowerCase().includes(query) || m.label.toLowerCase().includes(query)
  )
})

const toggleDropdown = () => {
  showDropdown.value = !showDropdown.value
  if (!showDropdown.value) searchQuery.value = ''
}

const removeModel = (model: string) => {
  emit('update:modelValue', props.modelValue.filter(m => m !== model))
}

const toggleModel = (model: string) => {
  if (props.modelValue.includes(model)) {
    removeModel(model)
  } else {
    emit('update:modelValue', [...props.modelValue, model])
  }
}

const copyModelId = async (model: string) => {
  await copyToClipboard(model)
}

const addCustom = () => {
  const model = customModel.value.trim()
  if (!model) return
  if (props.modelValue.includes(model)) {
    appStore.showInfo(t('admin.accounts.modelExists'))
    return
  }
  emit('update:modelValue', [...props.modelValue, model])
  customModel.value = ''
}

const handleEnter = () => {
  if (!isComposing.value) addCustom()
}

const fillRelated = () => {
  const newModels = [...props.modelValue]
  for (const platform of normalizedPlatforms.value) {
    for (const model of getModelsByPlatform(platform)) {
      if (!newModels.includes(model)) {
        newModels.push(model)
      }
    }
  }
  emit('update:modelValue', newModels)
}

const syncUpstreamModels = async () => {
  if (isSyncingUpstream.value) return
  if (!props.accountId && !props.syncCredentials) return

  isSyncingUpstream.value = true
  try {
    let result
    if (props.accountId) {
      result = await accountsAPI.syncUpstreamModels(props.accountId)
    } else if (props.syncCredentials) {
      result = await accountsAPI.syncUpstreamModelsPreview(props.syncCredentials as SyncUpstreamPreviewParams)
    } else {
      return
    }

    const upstreamModels = result.models.map(model => model.trim()).filter(Boolean)
    if (upstreamModels.length === 0) {
      appStore.showInfo(t('admin.accounts.syncUpstreamModelsEmpty'))
      return
    }

    if (!props.accountId) {
      emit('upstream-synced')
    }

    // 已在已选白名单里的不进入候选；重复点同步做并集合并，不覆盖已有候选。
    const metadata = result.metadata ?? {}
    const pending: UpstreamCandidate[] = upstreamModels
      .filter(model => !props.modelValue.includes(model))
      .map(model => {
        const meta = metadata[model]
        return {
          id: model,
          displayName: meta?.display_name?.trim() || undefined,
          contextWindow: meta?.context_window || meta?.max_context_window || undefined
        }
      })
    if (pending.length > 0) {
      const merged = new Map(upstreamCandidates.value.map(candidate => [candidate.id, candidate]))
      for (const candidate of pending) {
        // 新轮同步拿到的元数据更贴近上游现状，覆盖旧暂存项。
        merged.set(candidate.id, candidate)
      }
      upstreamCandidates.value = Array.from(merged.values())
    }

    const warnings = result.warnings ?? []
    const hasPartialMetadata = warnings.some(
      warning => warning.code === 'upstream_model_metadata_partial'
    )
    const hasIncompleteMetadata = warnings.some(
      warning => warning.code === 'upstream_model_metadata_incomplete'
    )
    if (hasIncompleteMetadata) {
      appStore.showWarning(t('admin.accounts.syncUpstreamModelsMetadataIncomplete'))
      return
    }
    if (pending.length > 0) {
      appStore.showSuccess(
        t('admin.accounts.syncUpstreamModelsPending', {
          count: pending.length,
          total: upstreamModels.length
        })
      )
    } else {
      appStore.showInfo(t('admin.accounts.syncUpstreamModelsNoChanges', { count: upstreamModels.length }))
    }
    if (hasPartialMetadata) {
      appStore.showWarning(t('admin.accounts.syncUpstreamModelsMetadataPartial'))
    }
  } catch (error) {
    // apiClient 拦截器 reject 的是普通对象（非 Error 实例），必须走统一提取否则后端
    // 真实原因（如 “Unsupported platform for upstream model sync: xxx”）会被吞掉。
    const message = extractApiErrorMessage(error, t('admin.accounts.syncUpstreamModelsFailed'))
    appStore.showError(t('admin.accounts.syncUpstreamModelsError', { message }))
  } finally {
    isSyncingUpstream.value = false
  }
}

const addUpstreamCandidate = (model: string) => {
  upstreamCandidates.value = upstreamCandidates.value.filter(candidate => candidate.id !== model)
  if (!props.modelValue.includes(model)) {
    emit('update:modelValue', [...props.modelValue, model])
  }
}

const addAllUpstreamCandidates = () => {
  const newModels = [...props.modelValue]
  for (const candidate of upstreamCandidates.value) {
    if (!newModels.includes(candidate.id)) {
      newModels.push(candidate.id)
    }
  }
  upstreamCandidates.value = []
  emit('update:modelValue', newModels)
}

const dismissUpstreamCandidates = () => {
  upstreamCandidates.value = []
}

// 上下文窗口折成 k 单位展示（256000 → 256k；不整除保留一位小数）。
const formatContextWindow = (tokens: number) => {
  if (!Number.isFinite(tokens) || tokens <= 0) return ''
  if (tokens < 1000) return `${tokens}`
  const value = tokens / 1000
  return `${Number.isInteger(value) ? value : value.toFixed(1)}k`
}

const clearAll = () => {
  emit('update:modelValue', [])
}

</script>
