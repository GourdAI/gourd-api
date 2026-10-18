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
          <div v-if="usesDynamicCatalog && dynamicLoading" class="px-3 py-4 text-center text-sm text-gray-500">
            {{ t('admin.accounts.modelCatalogLoading') }}
          </div>
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
              <span class="truncate text-gray-900 dark:text-white" :title="model.label !== model.value ? model.label : undefined">
                {{ model.value }}
              </span>
              <span
                v-if="model.label && model.label !== model.value"
                class="min-w-0 shrink truncate text-xs text-gray-400 dark:text-gray-500"
              >{{ model.label }}</span>
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
          <div v-if="filteredModels.length === 0 && !dynamicLoading" class="px-3 py-4 text-center text-sm text-gray-500">
            {{ dynamicEmptyHint }}
          </div>
        </div>
      </div>
    </div>

    <!-- Quick Actions -->
    <div class="mb-4 flex flex-wrap gap-2">
      <button
        type="button"
        data-testid="fill-related-models"
        :disabled="catalogLoading"
        @click="fillRelated"
        class="rounded-lg border border-blue-200 px-3 py-1.5 text-sm text-blue-600 hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-60 dark:border-blue-800 dark:text-blue-400 dark:hover:bg-blue-900/30"
      >
        {{ catalogLoading ? t('admin.accounts.modelCatalogLoading') : t('admin.accounts.fillRelatedModels') }}
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
import { fetchCatalogModels } from '@/components/account/modelCatalog'
import { useClipboard } from '@/composables/useClipboard'
import ModelIcon from '@/components/common/ModelIcon.vue'
import Icon from '@/components/icons/Icon.vue'
import { allModels, getModelsByPlatform, DYNAMIC_CATALOG_PLATFORMS } from '@/composables/useModelWhitelist'
import { extractApiErrorMessage } from '@/utils/apiError'

const { t } = useI18n()

interface ModelOption {
  value: string
  label: string
}

const props = defineProps<{
  modelValue: string[]
  platform?: string
  platforms?: string[]
  accountId?: number
  syncCredentials?: {
    platform: string
    type: string
    base_url?: string
    api_key?: string
    model_mapping?: Record<string, string>
    credentials?: Record<string, unknown>
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

// 动态目录（WorkBuddy / Trae / Qoder）：这三家的模型表由上游按身份下发，前端不再
// 写死（2026-10 改造）。候选来源优先级：
//   1. 已落库账号（accountId）→ GET /admin/accounts/:id/models，后端返回
//      「平台默认目录 + 上一轮同步快照 + model_mapping 键」的并集；
//   2. 新建流程（无 accountId）→ GET /admin/groups/0/model-allowlist-candidates
//      ?platform=xxx，返回后端权威默认表。
// 拉到的条目按 value 去重合并进 staticOptions，所以上游上新后无需改前端代码。
const dynamicOptions = ref<ModelOption[]>([])
const dynamicLoading = ref(false)
const dynamicLoadedKey = ref('')

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

// 显式选中的动态平台（不含 composite 兜底）：它决定「填充相关模型」能写入什么，
// 因为「填充」是**批量改动白名单**，不能把全平台并集灌进去（见 fillRelated）。
const explicitDynamicPlatforms = computed(() =>
  normalizedPlatforms.value.filter(platform => DYNAMIC_CATALOG_PLATFORMS.has(platform.toLowerCase()))
)

// 动态目录的目标平台（只影响**候选下拉**，不扩大填充范围）：
//   - 选了具体平台且属于三家固定协议网关 → 按平台拉；
//   - 完全没传 platform（如内容审核的模型过滤器）→ 拉 composite 并集，
//     保证写死表删除后这三家的 ID 仍出现在候选里；拉不到就退回静态表，不影响原有行为。
const dynamicTargetPlatforms = computed(() => {
  if (explicitDynamicPlatforms.value.length > 0) return explicitDynamicPlatforms.value
  return normalizedPlatforms.value.length === 0 ? ['composite'] : []
})

const usesDynamicCatalog = computed(() => dynamicTargetPlatforms.value.length > 0)

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
  'trae',
  'workbuddy'
])

// preview（新建未落库）同步的凭据门槛：通用平台走 api_key；WorkBuddy / Trae /
// Qoder 走 credentials.access_token（或 Qoder 的 PAT）。后端 SyncUpstreamModelsPreview
// 已同时接收两类入参，并对临时账号（ID=0）跳过写库。
// 集合复用 DYNAMIC_CATALOG_PLATFORMS：这三家既是「目录靠接口拉」也是「凭据靠票」，
// 分开写两份就会在新加固定协议平台时只改其中一处而静默失效。
const canSyncUpstream = computed(() => {
  if (props.accountId) {
    if (normalizedPlatforms.value.length === 0) return true
    return normalizedPlatforms.value.some(platform => upstreamSyncPlatforms.has(platform.toLowerCase()))
  }
  if (props.syncCredentials) {
    const platform = props.syncCredentials.platform.toLowerCase()
    if (!upstreamSyncPlatforms.has(platform)) return false
    if (DYNAMIC_CATALOG_PLATFORMS.has(platform)) {
      const credentials = props.syncCredentials.credentials ?? {}
      const hasToken = (key: string) => typeof credentials[key] === 'string' && (credentials[key] as string).trim() !== ''
      return (
        hasToken('access_token') ||
        hasToken('accessToken') ||
        hasToken('personal_token') ||
        hasToken('personalToken')
      )
    }
    return Boolean(props.syncCredentials.api_key && props.syncCredentials.api_key.trim())
  }
  return false
})

const staticOptions = computed<ModelOption[]>(() => {
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

const availableOptions = computed<ModelOption[]>(() => {
  const seen = new Set<string>()
  const merged: ModelOption[] = []
  for (const option of [...staticOptions.value, ...dynamicOptions.value]) {
    if (seen.has(option.value)) continue
    seen.add(option.value)
    merged.push(option)
  }
  return merged
})

const filteredModels = computed(() => {
  const query = searchQuery.value.toLowerCase().trim()
  if (!query) return availableOptions.value
  return availableOptions.value.filter(
    m => m.value.toLowerCase().includes(query) || m.label.toLowerCase().includes(query)
  )
})

const dynamicEmptyHint = computed(() =>
  usesDynamicCatalog.value && availableOptions.value.length === 0
    ? t('admin.accounts.modelCatalogEmpty')
    : t('admin.accounts.noMatchingModels')
)

const catalogLoading = computed(() => dynamicLoading.value)

function catalogCacheKey(): string {
  return props.accountId
    ? `account:${props.accountId}`
    : `platform:${dynamicTargetPlatforms.value.join(',')}`
}

/** 拉取动态目录。已加载过同一目标则直接复用（避免每次开下拉都打后端）。 */
async function loadDynamicCatalog(force = false): Promise<ModelOption[]> {
  if (!usesDynamicCatalog.value) return []
  const key = catalogCacheKey()
  if (!force && dynamicLoadedKey.value === key) return dynamicOptions.value

  dynamicLoading.value = true
  try {
    let options: ModelOption[]
    if (props.accountId) {
      // 已落库账号：后端 GET /admin/accounts/:id/models 返的是「平台默认目录 + 上一轮
      // 同步快照 + model_mapping 键」。但配了映射后它**只列映射键**（见
      // fixed_protocol_models.go），直接拿它当候选就会退化成「已选的就是全部候选」，
      // 管理员无法从下拉里新选一个上游型号。因此这里同时并一份平台默认目录。
      const [models, defaults] = await Promise.all([
        accountsAPI.getAvailableModels(props.accountId).catch(() => []),
        Promise.all(
          dynamicTargetPlatforms.value.map(target => fetchCatalogModels(target, force).catch(() => [] as string[]))
        ).then(lists => lists.flat())
      ])
      const byId = new Map<string, ModelOption>()
      for (const id of defaults) byId.set(id, { value: id, label: id })
      for (const model of models) {
        const id = (model.id || '').trim()
        if (!id) continue
        byId.set(id, { value: id, label: (model.display_name || '').trim() || id })
      }
      options = Array.from(byId.values())
    } else {
      const lists = await Promise.all(
        dynamicTargetPlatforms.value.map(target => fetchCatalogModels(target, force).catch(() => [] as string[]))
      )
      options = lists.flat().map(id => ({ value: id, label: id }))
    }
    // 同一个目标可能已经手输过自定义条目，保留它们以免刷新后候选倒退。
    for (const model of props.modelValue) {
      if (!options.some(option => option.value === model)) {
        options.push({ value: model, label: model })
      }
    }
    dynamicOptions.value = options
    dynamicLoadedKey.value = key
  } catch (error) {
    const message = extractApiErrorMessage(error, t('admin.accounts.syncUpstreamModelsFailed'))
    appStore.showError(t('admin.accounts.modelCatalogLoadError', { message }))
  } finally {
    dynamicLoading.value = false
  }
  return dynamicOptions.value
}

// 账号 / 平台变化后缓存失效：候选必须重新解析，否则编辑 B 账号时显示 A 的目录。
watch(
  () => [props.accountId ?? 0, normalizedPlatforms.value.join(',')] as const,
  () => {
    dynamicLoadedKey.value = ''
    if (usesDynamicCatalog.value) void loadDynamicCatalog()
  },
  { immediate: true }
)

const toggleDropdown = () => {
  showDropdown.value = !showDropdown.value
  if (!showDropdown.value) {
    searchQuery.value = ''
  } else if (usesDynamicCatalog.value && dynamicOptions.value.length === 0) {
    void loadDynamicCatalog()
  }
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

/**
 * 「同步最新支持模型」：候选 = 显式平台的静态表 ∪ 显式动态平台的接口目录。
 *
 * 两条纪律（都是旧版语义，改造时必须守住）：
 * 1. **只认显式选中的平台**。没传 platform 的调用方（内容审核的模型过滤器）旧版是
 *    静默 no-op；若把候选下拉用的 composite 并集灌进来，点一次按钮就会往过滤规则里
 *    塞进上百个模型 ID —— 那是一次意外的批量写入，不是「填充相关模型」。
 * 2. **静态平台不能被动态目录顶掉**。批量编辑可同时选中 openai + trae，旧版两者都填；
 *    若用 `fetched.length > 0` 做三元互斥，trae 目录一旦拉到就会让 openai 静态表整个消失。
 */
const fillRelated = async () => {
  const candidates: string[] = normalizedPlatforms.value.flatMap(platform => getModelsByPlatform(platform))
  if (explicitDynamicPlatforms.value.length > 0) {
    const fetched = await loadDynamicCatalog(true)
    candidates.push(...fetched.map(option => option.value))
  }

  const newModels = [...props.modelValue]
  for (const model of candidates) {
    if (!newModels.includes(model)) newModels.push(model)
  }
  if (newModels.length === props.modelValue.length) {
    appStore.showInfo(t('admin.accounts.syncUpstreamModelsNoChanges', { count: candidates.length }))
    return
  }
  emit('update:modelValue', newModels)
}

/**
 * 「同步上游支持的模型」：结果**直接写入白名单**。
 *
 * 旧实现在这里停一步、把上游目录放进「候选区」等管理员逐条确认。实际使用中
 * 后端已把不可调条目过滤掉（Trae 的 config_switch=false / is_invisible_to_user /
 * 空 display_name，WorkBuddy 的非对话条目），剩下的就是该账号真能用的模型，
 * 逐条点确认只是把一次点击变成 N 次。因此改为直接落进白名单，管理员要删可在
 * 已选标签上点 ×。
 */
const syncUpstreamModels = async () => {
  if (isSyncingUpstream.value) return
  if (!props.accountId && !props.syncCredentials) return

  isSyncingUpstream.value = true
  try {
    let result
    if (props.accountId) {
      result = await accountsAPI.syncUpstreamModels(props.accountId)
    } else {
      result = await accountsAPI.syncUpstreamModelsPreview(props.syncCredentials as SyncUpstreamPreviewParams)
    }

    const upstreamModels = result.models.map(model => model.trim()).filter(Boolean)
    if (upstreamModels.length === 0) {
      appStore.showInfo(t('admin.accounts.syncUpstreamModelsEmpty'))
      return
    }

    // 同步结果同时是下拉候选：把上游展示名/新模型并进动态目录，避免刚同步完
    // 又在下拉里看不到它们。
    const metadata = result.metadata ?? {}
    const syncedOptions: ModelOption[] = upstreamModels.map(model => ({
      value: model,
      label: metadata[model]?.display_name?.trim() || model
    }))
    const mergedOptions = new Map(dynamicOptions.value.map(option => [option.value, option]))
    for (const option of syncedOptions) mergedOptions.set(option.value, option)
    dynamicOptions.value = Array.from(mergedOptions.values())
    dynamicLoadedKey.value = catalogCacheKey()

    const added = upstreamModels.filter(model => !props.modelValue.includes(model))
    if (!props.accountId) {
      emit('upstream-synced')
    }
    if (added.length > 0) {
      emit('update:modelValue', [...props.modelValue, ...added])
    }

    const warnings = result.warnings ?? []
    const hasPartialMetadata = warnings.some(w => w.code === 'upstream_model_metadata_partial')
    const hasIncompleteMetadata = warnings.some(w => w.code === 'upstream_model_metadata_incomplete')
    if (hasIncompleteMetadata) {
      appStore.showWarning(t('admin.accounts.syncUpstreamModelsMetadataIncomplete'))
      return
    }
    if (added.length > 0) {
      appStore.showSuccess(
        t('admin.accounts.syncUpstreamModelsSuccess', { count: added.length, total: upstreamModels.length })
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

const clearAll = () => {
  emit('update:modelValue', [])
}

</script>
