import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

const {
  copyToClipboard,
  showError,
  showSuccess,
  showInfo,
  showWarning,
  syncUpstreamModels,
  syncUpstreamModelsPreview
} = vi.hoisted(() => ({
  copyToClipboard: vi.fn().mockResolvedValue(true),
  showError: vi.fn(),
  showSuccess: vi.fn(),
  showInfo: vi.fn(),
  showWarning: vi.fn(),
  syncUpstreamModels: vi.fn(),
  syncUpstreamModelsPreview: vi.fn()
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      // Keys stay verbatim so assertions can match them; params are appended so
      // interpolated payloads (e.g. the extracted upstream error message) stay visible.
      t: (key: string, params?: Record<string, unknown>) => {
        if (key === 'common.copy') return '复制'
        return params ? `${key}:${JSON.stringify(params)}` : key
      }
    })
  }
})

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showSuccess,
    showInfo,
    showWarning
  })
}))

vi.mock('@/api/admin/accounts', () => ({
  accountsAPI: {
    syncUpstreamModels,
    syncUpstreamModelsPreview
  }
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({
    copyToClipboard
  })
}))

import ModelWhitelistSelector from '../ModelWhitelistSelector.vue'

function mountSelector(props: Record<string, unknown> = {}) {
  return mount(ModelWhitelistSelector, {
    props: {
      modelValue: [],
      platform: 'openai',
      ...props,
    },
    global: {
      stubs: {
        ModelIcon: true
      }
    }
  })
}

function findModelRow(wrapper: ReturnType<typeof mountSelector>, modelId: string) {
  const row = wrapper
    .findAll('[data-testid="model-option"]')
    .find(candidate => candidate.text().includes(modelId))

  if (!row) {
    throw new Error(`Model row not found: ${modelId}`)
  }

  return row
}

function findSyncButton(wrapper: ReturnType<typeof mountSelector>) {
  return wrapper
    .findAll('button')
    .find(button => button.text() === 'admin.accounts.syncUpstreamModels')
}

function candidateIds(wrapper: ReturnType<typeof mountSelector>) {
  return wrapper.findAll('[data-testid="upstream-candidate"]').map(row => row.text())
}

describe('ModelWhitelistSelector', () => {
  beforeEach(() => {
    copyToClipboard.mockClear()
    showError.mockReset()
    showSuccess.mockReset()
    showInfo.mockReset()
    showWarning.mockReset()
    syncUpstreamModels.mockReset()
    syncUpstreamModelsPreview.mockReset()
  })

  it('copies a model ID without selecting the model', async () => {
    const wrapper = mountSelector()
    await wrapper.get('div.cursor-pointer').trigger('click')

    const row = findModelRow(wrapper, 'gpt-5.6-sol')

    const copyButton = row.get('[data-testid="copy-model-id"]')
    expect(copyButton.attributes('aria-label')).toBe('复制 gpt-5.6-sol')

    await copyButton.trigger('click')
    await flushPromises()

    expect(copyToClipboard).toHaveBeenCalledWith('gpt-5.6-sol')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
  })

  it('keeps the existing model selection behavior', async () => {
    const wrapper = mountSelector()
    await wrapper.get('div.cursor-pointer').trigger('click')

    const row = findModelRow(wrapper, 'gpt-5.6-sol')
    await row.get('[data-testid="select-model"]').trigger('click')

    expect(wrapper.emitted('update:modelValue')).toEqual([[['gpt-5.6-sol']]])
    expect(copyToClipboard).not.toHaveBeenCalled()
  })

  // 目录回归：平台分支独有模型必须进入 allModels，否则下拉恒「无匹配模型」。
  it('lists platform-specific catalog models in the dropdown for trae accounts', async () => {
    const wrapper = mountSelector({ platform: 'trae', accountId: 12 })
    await wrapper.get('div.cursor-pointer').trigger('click')

    const texts = wrapper.findAll('[data-testid="model-option"]').map(row => row.text())
    expect(texts.length).toBeGreaterThan(0)
    expect(texts.some(text => text.includes('Doubao-Seed-2.1-Pro'))).toBe(true)
    expect(texts.some(text => text.includes('kimi-k2.7-code'))).toBe(true)
    expect(wrapper.text()).not.toContain('admin.accounts.noMatchingModels')
  })

  it('lists platform-specific catalog models in the dropdown for qoder accounts', async () => {
    const wrapper = mountSelector({ platform: 'qoder', accountId: 13 })
    await wrapper.get('div.cursor-pointer').trigger('click')

    const texts = wrapper.findAll('[data-testid="model-option"]').map(row => row.text())
    expect(texts.some(text => text.includes('qmodel_38max'))).toBe(true)
    expect(texts.some(text => text.includes('gm51model'))).toBe(true)
  })

  it('warns when model IDs sync but capability metadata is incomplete', async () => {
    syncUpstreamModels.mockResolvedValue({
      models: ['x-preview-f-free'],
      warnings: [
        {
          code: 'upstream_model_metadata_incomplete',
          message: 'Model IDs were synced, but capability metadata could not be updated.'
        }
      ]
    })
    const wrapper = mountSelector({ accountId: 46 })

    const syncButton = findSyncButton(wrapper)
    expect(syncButton).toBeDefined()
    await syncButton!.trigger('click')
    await flushPromises()

    // 候选模式：同步结果不再自动写入白名单
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(candidateIds(wrapper)).toHaveLength(1)
    expect(showWarning).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsMetadataIncomplete')
    expect(showSuccess).not.toHaveBeenCalled()
  })

  it('shows a pending-candidate message and a partial warning when some capabilities were saved', async () => {
    syncUpstreamModels.mockResolvedValue({
      models: ['gpt-6-astra', 'gpt-image-2'],
      warnings: [
        {
          code: 'upstream_model_metadata_partial',
          message: 'Some model capabilities were saved; remaining models are still incomplete.'
        }
      ]
    })
    const wrapper = mountSelector({ accountId: 46 })

    const syncButton = findSyncButton(wrapper)
    expect(syncButton).toBeDefined()
    await syncButton!.trigger('click')
    await flushPromises()

    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(showSuccess).toHaveBeenCalledWith(
      'admin.accounts.syncUpstreamModelsPending:{"count":2,"total":2}'
    )
    expect(showWarning).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsMetadataPartial')
    expect(showSuccess).not.toHaveBeenCalledWith(
      expect.stringContaining('admin.accounts.syncUpstreamModelsSuccess')
    )
  })

  it('adds a single upstream candidate to the whitelist only on confirmation', async () => {
    syncUpstreamModels.mockResolvedValue({ models: ['gpt-6-astra', 'gpt-image-2'] })
    const wrapper = mountSelector({ accountId: 46, modelValue: ['gpt-5.2'] })

    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()

    // 已选模型不重复出现在候选里
    expect(candidateIds(wrapper)).toHaveLength(2)

    await wrapper.get('[data-testid="add-upstream-candidate:gpt-image-2"]').trigger('click')

    expect(wrapper.emitted('update:modelValue')).toEqual([[['gpt-5.2', 'gpt-image-2']]])
    expect(wrapper.get('[data-testid="add-upstream-candidate:gpt-6-astra"]').exists()).toBe(true)
  })

  it('adds all upstream candidates in one click', async () => {
    syncUpstreamModels.mockResolvedValue({ models: ['gpt-6-astra', 'gpt-image-2'] })
    const wrapper = mountSelector({ accountId: 46, modelValue: ['gpt-5.2'] })

    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()

    await wrapper.get('[data-testid="add-all-upstream-candidates"]').trigger('click')

    expect(wrapper.emitted('update:modelValue')).toEqual([[['gpt-5.2', 'gpt-6-astra', 'gpt-image-2']]])
    expect(wrapper.find('[data-testid="upstream-candidates"]').exists()).toBe(false)
  })

  it('merges repeated syncs instead of overwriting candidates', async () => {
    syncUpstreamModels
      .mockResolvedValueOnce({ models: ['gpt-6-astra'] })
      .mockResolvedValueOnce({ models: ['gpt-image-2', 'gpt-6-astra'] })
    const wrapper = mountSelector({ accountId: 46 })

    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()
    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()

    expect(candidateIds(wrapper)).toHaveLength(2)
  })

  it('drops upstream models already selected from the candidate list', async () => {
    syncUpstreamModels.mockResolvedValue({ models: ['gpt-6-astra', 'gpt-6-astra '] })
    const wrapper = mountSelector({ accountId: 46, modelValue: ['gpt-6-astra'] })

    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="upstream-candidates"]').exists()).toBe(false)
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(showInfo).toHaveBeenCalledWith(
      'admin.accounts.syncUpstreamModelsNoChanges:{"count":2}'
    )
  })

  it('dismisses the candidate list without touching the whitelist', async () => {
    syncUpstreamModels.mockResolvedValue({ models: ['gpt-6-astra'] })
    const wrapper = mountSelector({ accountId: 46 })

    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()
    expect(candidateIds(wrapper)).toHaveLength(1)

    await wrapper.get('[data-testid="dismiss-upstream-candidates"]').trigger('click')

    expect(wrapper.find('[data-testid="upstream-candidates"]').exists()).toBe(false)
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
  })

  it('clears stale candidates when the account context changes', async () => {
    syncUpstreamModels.mockResolvedValue({ models: ['gpt-6-astra'] })
    const wrapper = mountSelector({ accountId: 46 })

    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()
    expect(candidateIds(wrapper)).toHaveLength(1)

    await wrapper.setProps({ accountId: 47 })
    await flushPromises()

    expect(wrapper.find('[data-testid="upstream-candidates"]').exists()).toBe(false)
  })

  it('surfaces the backend reason when upstream sync rejects with a plain object', async () => {
    // apiClient 拦截器 reject 的是普通对象（非 Error 实例）
    syncUpstreamModels.mockRejectedValue({
      status: 400,
      reason: 'unsupported_platform',
      message: 'WorkBuddy upstream exposes no model list API for personal accounts'
    })
    const wrapper = mountSelector({ accountId: 46 })

    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith(
      'admin.accounts.syncUpstreamModelsError:{"message":"WorkBuddy upstream exposes no model list API for personal accounts"}'
    )
    expect(showError).not.toHaveBeenCalledWith(
      expect.stringContaining('同步上游模型失败：同步上游模型失败')
    )
  })

  it('keeps the generic fallback when a real Error is thrown', async () => {
    syncUpstreamModels.mockRejectedValue(new Error('network down'))
    const wrapper = mountSelector({ accountId: 46 })

    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsError:{"message":"network down"}')
  })

  it('reports a successful preview so account creation can persist metadata', async () => {
    syncUpstreamModelsPreview.mockResolvedValue({
      models: ['x-preview-f-free'],
      metadata: {
        'x-preview-f-free': {
          id: 'x-preview-f-free',
          reasoning: true,
          supported_reasoning_levels: ['low', 'high', 'max'],
        },
      },
    })
    const wrapper = mountSelector({
      syncCredentials: {
        platform: 'openai',
        type: 'apikey',
        base_url: 'https://opencode.ai/zen/v1',
        api_key: 'test-key',
      },
    })
    const syncButton = findSyncButton(wrapper)

    expect(syncButton).toBeDefined()
    await syncButton?.trigger('click')
    await flushPromises()

    expect(syncUpstreamModelsPreview).toHaveBeenCalledOnce()
    expect(wrapper.emitted('upstream-synced')).toEqual([[]])
    expect(candidateIds(wrapper)).toHaveLength(1)

    await wrapper.get('[data-testid="add-upstream-candidate:x-preview-f-free"]').trigger('click')
    expect(wrapper.emitted('update:modelValue')).toEqual([[['x-preview-f-free']]])
  })

  it('shows the upstream sync button for OpenCode Go create-account credentials', () => {
    const wrapper = mountSelector({
      platform: 'opencode_go',
      syncCredentials: {
        platform: 'opencode_go',
        type: 'apikey',
        base_url: 'https://opencode.ai/zen/go/v1',
        api_key: 'sk-test',
      },
    })
    const syncButton = findSyncButton(wrapper)

    expect(syncButton).toBeDefined()
    expect(syncButton?.exists()).toBe(true)
  })

  it('hides the upstream sync button for workbuddy (no upstream model list API)', () => {
    const wrapper = mountSelector({ platform: 'workbuddy', accountId: 51 })

    expect(findSyncButton(wrapper)).toBeUndefined()
    expect(wrapper.text()).not.toContain('admin.accounts.syncUpstreamModels')
  })

  it('shows the upstream sync button for trae and qoder accounts', () => {
    for (const platform of ['trae', 'qoder']) {
      const wrapper = mountSelector({ platform, accountId: 52 })
      expect(findSyncButton(wrapper)).toBeDefined()
    }
  })

  // 新建表单（无 accountId、走 preview）下，Trae / Qoder 的 token 不在 api_key 字段里，
  // preview 端点固定只会回配置错误，因此不能给它们同步入口。
  it('hides the upstream sync button for trae/qoder create-account preview', () => {
    for (const platform of ['trae', 'qoder']) {
      const wrapper = mountSelector({
        platform,
        syncCredentials: { platform, type: 'oauth', base_url: '', api_key: '' },
      })
      expect(findSyncButton(wrapper)).toBeUndefined()
      expect(wrapper.text()).not.toContain('admin.accounts.syncUpstreamModels')
    }
  })

  // display_name 是上游直出的展示名：不在候选行里展示，管理员就只能对着 config_name 猜。
  it('renders upstream display name and context window in candidate rows', async () => {
    syncUpstreamModels.mockResolvedValue({
      models: ['deepseek-v4.1-flash-official'],
      metadata: {
        'deepseek-v4.1-flash-official': {
          id: 'deepseek-v4.1-flash-official',
          display_name: 'DeepSeek-V4.1-Flash',
          context_window: 256000,
        },
      },
    })
    const wrapper = mountSelector({ accountId: 46 })

    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()

    const row = wrapper.get('[data-testid="upstream-candidate"]')
    expect(row.text()).toContain('DeepSeek-V4.1-Flash')
    expect(row.text()).toContain('deepseek-v4.1-flash-official')
    expect(row.text()).toContain('256k')
    // 添加的必须是真实可调 ID（config_name），不是展示名
    await wrapper.get('[data-testid="add-upstream-candidate:deepseek-v4.1-flash-official"]').trigger('click')
    expect(wrapper.emitted('update:modelValue')).toEqual([[["deepseek-v4.1-flash-official"]]])
  })

  it('falls back to the model ID when upstream gives no display name', async () => {
    syncUpstreamModels.mockResolvedValue({
      models: ['bare-model'],
      metadata: { 'bare-model': { id: 'bare-model', display_name: '   ' } },
    })
    const wrapper = mountSelector({ accountId: 46 })

    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()

    const row = wrapper.get('[data-testid="upstream-candidate"]')
    expect(row.text()).toContain('bare-model')
    expect(row.text()).not.toMatch(/\d+k/) // 无 context_window 时不该出现窗口徽标
  })

  it('merges latest metadata when the same candidate is synced twice', async () => {
    syncUpstreamModels
      .mockResolvedValueOnce({
        models: ['dup-model'],
        metadata: { 'dup-model': { id: 'dup-model', display_name: 'Old Name', context_window: 128000 } },
      })
      .mockResolvedValueOnce({
        models: ['dup-model'],
        metadata: { 'dup-model': { id: 'dup-model', display_name: 'New Name', context_window: 512000 } },
      })
    const wrapper = mountSelector({ accountId: 46 })

    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()
    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()

    expect(candidateIds(wrapper)).toHaveLength(1)
    const row = wrapper.get('[data-testid="upstream-candidate"]')
    expect(row.text()).toContain('New Name')
    expect(row.text()).toContain('512k')
    expect(row.text()).not.toContain('Old Name')
  })
})
