import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

const {
  copyToClipboard,
  showError,
  showSuccess,
  showInfo,
  showWarning,
  syncUpstreamModels,
  syncUpstreamModelsPreview,
  getAvailableModels,
  getModelAllowlistCandidates
} = vi.hoisted(() => ({
  copyToClipboard: vi.fn().mockResolvedValue(true),
  showError: vi.fn(),
  showSuccess: vi.fn(),
  showInfo: vi.fn(),
  showWarning: vi.fn(),
  syncUpstreamModels: vi.fn(),
  syncUpstreamModelsPreview: vi.fn(),
  getAvailableModels: vi.fn(),
  getModelAllowlistCandidates: vi.fn()
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
    syncUpstreamModelsPreview,
    getAvailableModels
  }
}))

// 动态目录走 groups 的 model-allowlist-candidates（id=0 = 平台默认目录）。
vi.mock('@/api/admin/groups', () => ({
  getModelAllowlistCandidates
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({
    copyToClipboard
  })
}))

import ModelWhitelistSelector from '../ModelWhitelistSelector.vue'
import { resetModelCatalogCache } from '../modelCatalog'

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

async function openDropdown(wrapper: ReturnType<typeof mountSelector>) {
  await wrapper.get('div.cursor-pointer').trigger('click')
  await flushPromises()
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
    getAvailableModels.mockReset()
    getModelAllowlistCandidates.mockReset()
    // 模块级目录缓存跨用例复用会让「断言接口被调用」的用例假失败。
    resetModelCatalogCache()
  })

  it('copies a model ID without selecting the model', async () => {
    const wrapper = mountSelector()
    await openDropdown(wrapper)

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
    await openDropdown(wrapper)

    const row = findModelRow(wrapper, 'gpt-5.6-sol')
    await row.get('[data-testid="select-model"]').trigger('click')

    expect(wrapper.emitted('update:modelValue')).toEqual([[['gpt-5.6-sol']]])
    expect(copyToClipboard).not.toHaveBeenCalled()
  })

  // ── 动态目录（WorkBuddy / Trae / Qoder 的前端写死表已删除） ──

  it('loads the trae dropdown from the backend catalog instead of a hardcoded list', async () => {
    getModelAllowlistCandidates.mockResolvedValue(['glm-5.3', 'Doubao-Seed-2.1-Pro'])
    const wrapper = mountSelector({ platform: 'trae' })
    await openDropdown(wrapper)

    expect(getModelAllowlistCandidates).toHaveBeenCalledWith(0, 'trae')
    const texts = wrapper.findAll('[data-testid="model-option"]').map(row => row.text())
    expect(texts.some(text => text.includes('Doubao-Seed-2.1-Pro'))).toBe(true)
    expect(wrapper.text()).not.toContain('admin.accounts.noMatchingModels')
  })

  it('merges the saved account catalog with the platform default catalog', async () => {
    getAvailableModels.mockResolvedValue([
      { id: 'qmodel_38max', type: 'model', display_name: 'Qwen3.8-Max', created_at: '' },
      { id: 'gm51model', type: 'model', display_name: '', created_at: '' }
    ])
    // 账号已配映射时 GET /accounts/:id/models 只列映射键；不并一份平台默认目录的话，
    // 下拉就退化成「已选即全部候选」，管理员无法新选上游型号。
    getModelAllowlistCandidates.mockResolvedValue(['qmodel_38max', 'dmodel'])
    const wrapper = mountSelector({ platform: 'qoder', accountId: 13 })
    await flushPromises()
    await openDropdown(wrapper)

    expect(getAvailableModels).toHaveBeenCalledWith(13)
    expect(getModelAllowlistCandidates).toHaveBeenCalledWith(0, 'qoder')

    const texts = wrapper.findAll('[data-testid="model-option"]').map(row => row.text())
    // 默认目录独有的型号必须可选，且不重复出现。
    expect(texts.filter(text => text.includes('qmodel_38max'))).toHaveLength(1)
    expect(texts.some(text => text.includes('dmodel'))).toBe(true)
    // 账号接口的展示名优先于默认目录的裸 ID。
    expect(findModelRow(wrapper, 'qmodel_38max').text()).toContain('Qwen3.8-Max')
  })

  it('keeps manually entered models in the dropdown after a catalog refresh', async () => {
    getModelAllowlistCandidates.mockResolvedValue(['glm-5.3'])
    const wrapper = mountSelector({ platform: 'trae', modelValue: ['my-own-model'] })
    await flushPromises()
    await openDropdown(wrapper)

    expect(findModelRow(wrapper, 'my-own-model').exists()).toBe(true)
    expect(findModelRow(wrapper, 'glm-5.3').exists()).toBe(true)
  })

  it('refetches the catalog when the platform changes', async () => {
    getModelAllowlistCandidates.mockResolvedValue(['a-model'])
    const wrapper = mountSelector({ platform: 'trae' })
    await flushPromises()

    await wrapper.setProps({ platform: 'qoder' })
    await flushPromises()

    expect(getModelAllowlistCandidates).toHaveBeenCalledWith(0, 'trae')
    expect(getModelAllowlistCandidates).toHaveBeenCalledWith(0, 'qoder')
  })

  // ── 同步上游模型：结果直接写入白名单（不再经过候选确认区） ──

  it('adds synced upstream models straight into the whitelist', async () => {
    syncUpstreamModels.mockResolvedValue({ models: ['gpt-6-astra', 'gpt-image-2'] })
    const wrapper = mountSelector({ accountId: 46, modelValue: ['gpt-5.2'] })

    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()

    expect(wrapper.emitted('update:modelValue')).toEqual([[['gpt-5.2', 'gpt-6-astra', 'gpt-image-2']]])
    expect(showSuccess).toHaveBeenCalledWith(
      'admin.accounts.syncUpstreamModelsSuccess:{"count":2,"total":2}'
    )
    expect(wrapper.find('[data-testid="upstream-candidates"]').exists()).toBe(false)
  })

  it('does not emit an update when every upstream model is already selected', async () => {
    syncUpstreamModels.mockResolvedValue({ models: ['gpt-6-astra', 'gpt-6-astra '] })
    const wrapper = mountSelector({ accountId: 46, modelValue: ['gpt-6-astra'] })

    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()

    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(showInfo).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsNoChanges:{"count":2}')
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

    expect(wrapper.emitted('update:modelValue')).toEqual([[['x-preview-f-free']]])
    expect(showWarning).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsMetadataIncomplete')
    expect(showSuccess).not.toHaveBeenCalled()
  })

  it('shows a partial warning alongside the success message when some capabilities were saved', async () => {
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

    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()

    expect(showSuccess).toHaveBeenCalledWith(
      'admin.accounts.syncUpstreamModelsSuccess:{"count":2,"total":2}'
    )
    expect(showWarning).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsMetadataPartial')
  })

  it('merges repeated syncs into the whitelist without duplicating entries', async () => {
    syncUpstreamModels
      .mockResolvedValueOnce({ models: ['gpt-6-astra'] })
      .mockResolvedValueOnce({ models: ['gpt-image-2', 'gpt-6-astra'] })
    const wrapper = mountSelector({ accountId: 46 })

    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()
    expect(wrapper.emitted('update:modelValue')?.[0]).toEqual([['gpt-6-astra']])

    await wrapper.setProps({ modelValue: ['gpt-6-astra'] })
    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()

    expect(wrapper.emitted('update:modelValue')?.[1]).toEqual([['gpt-6-astra', 'gpt-image-2']])
  })

  it('feeds synced model IDs into the dropdown options as well', async () => {
    syncUpstreamModels.mockResolvedValue({
      models: ['brand-new-model'],
      metadata: { 'brand-new-model': { id: 'brand-new-model', display_name: 'Brand New' } }
    })
    const wrapper = mountSelector({ accountId: 46 })

    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()
    await openDropdown(wrapper)

    const row = findModelRow(wrapper, 'brand-new-model')
    expect(row.text()).toContain('Brand New')
  })

  it('surfaces the backend reason when upstream sync rejects with a plain object', async () => {
    // apiClient 拦截器 reject 的是普通对象（非 Error 实例）
    syncUpstreamModels.mockRejectedValue({
      status: 400,
      reason: 'unsupported_platform',
      message: 'WorkBuddy model sync expects the native /v3/config catalog path'
    })
    const wrapper = mountSelector({ accountId: 46 })

    await findSyncButton(wrapper)!.trigger('click')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith(
      'admin.accounts.syncUpstreamModelsError:{"message":"WorkBuddy model sync expects the native /v3/config catalog path"}'
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

  // ── 同步按钮可见性 ──

  it('shows the upstream sync button for saved workbuddy / trae / qoder accounts', () => {
    for (const platform of ['workbuddy', 'trae', 'qoder']) {
      const wrapper = mountSelector({ platform, accountId: 51 })
      expect(findSyncButton(wrapper)).toBeDefined()
    }
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
    expect(findSyncButton(wrapper)).toBeDefined()
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
    expect(wrapper.emitted('update:modelValue')).toEqual([[['x-preview-f-free']]])
  })

  // 本次改造的核心：新建三平台账号时也能拉目录，不必「先建号再进编辑页」。
  it('shows the upstream sync button for workbuddy create preview once an access token exists', () => {
    const withoutToken = mountSelector({
      platform: 'workbuddy',
      syncCredentials: { platform: 'workbuddy', type: 'apikey', base_url: '', credentials: {} },
    })
    expect(findSyncButton(withoutToken)).toBeUndefined()

    const withToken = mountSelector({
      platform: 'workbuddy',
      syncCredentials: {
        platform: 'workbuddy',
        type: 'apikey',
        base_url: 'https://copilot.tencent.com',
        credentials: { access_token: 'at', refresh_token: 'rt', realm: 'cn' },
      },
    })
    expect(findSyncButton(withToken)).toBeDefined()
  })

  it('shows the upstream sync button for trae/qoder create preview based on token credentials', async () => {
    syncUpstreamModelsPreview.mockResolvedValue({ models: ['glm-5.3'] })

    for (const [platform, credentials] of [
      ['trae', { access_token: 'jwt', realm: 'cn' }],
      ['qoder', { personal_token: 'pt-abc', realm: 'cn' }]
    ] as const) {
      const wrapper = mountSelector({
        platform,
        syncCredentials: { platform, type: 'apikey', base_url: '', credentials },
      })
      const syncButton = findSyncButton(wrapper)
      expect(syncButton).toBeDefined()

      await syncButton!.trigger('click')
      await flushPromises()
    }
    expect(syncUpstreamModelsPreview).toHaveBeenCalledTimes(2)
  })

  it('hides the upstream sync button for platforms outside the sync whitelist', () => {
    const wrapper = mountSelector({
      platform: 'baidu',
      syncCredentials: { platform: 'baidu', type: 'apikey', base_url: '', api_key: 'k' },
    })
    expect(findSyncButton(wrapper)).toBeUndefined()
  })

  // ── 填充相关模型 ──

  it('fetches the dynamic catalog before filling trae models', async () => {
    getModelAllowlistCandidates.mockResolvedValue(['glm-5.3', 'kimi-k2.7-code'])
    const wrapper = mountSelector({ platform: 'trae', modelValue: ['glm-5.3'] })
    // 目录首拉期间按钮是 disabled，要先等它落地再点。
    await flushPromises()
    getModelAllowlistCandidates.mockClear()

    await wrapper.get('[data-testid="fill-related-models"]').trigger('click')
    await flushPromises()

    expect(getModelAllowlistCandidates).toHaveBeenCalledWith(0, 'trae')
    expect(wrapper.emitted('update:modelValue')).toEqual([[['glm-5.3', 'kimi-k2.7-code']]])
  })

  it('fills static platforms from the local table without hitting the network', async () => {
    const wrapper = mountSelector({ platform: 'anthropic' })

    await wrapper.get('[data-testid="fill-related-models"]').trigger('click')
    await flushPromises()

    expect(getModelAllowlistCandidates).not.toHaveBeenCalled()
    const emitted = wrapper.emitted('update:modelValue')?.[0]?.[0] as string[]
    expect(emitted.length).toBeGreaterThan(0)
  })

  it('does not fill the composite catalog when no platform is selected', async () => {
    // 调用方：内容审核（RiskControlView）的模型过滤器，不传 platform。
    // 它需要 composite 目录出现在**下拉候选**里，但「填充相关模型」不能把
    // 13 个平台的并集一针灌进过滤规则。旧版此路径是静默 no-op，必须保持。
    getModelAllowlistCandidates.mockResolvedValue(['glm-5.3', 'gpt-5.6-sol', 'claude-fable-5-1'])
    const wrapper = mountSelector({ platform: undefined, modelValue: ['already-picked'] })
    await flushPromises()

    // 挂载时下拉候选拿到完整目录（只读，不改白名单）
    expect(getModelAllowlistCandidates).toHaveBeenCalledWith(0, 'composite')
    getModelAllowlistCandidates.mockClear()

    await wrapper.get('[data-testid="fill-related-models"]').trigger('click')
    await flushPromises()

    // 填充路径不得再去拉 composite 并集，也不得动白名单
    expect(getModelAllowlistCandidates).not.toHaveBeenCalled()
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(showInfo).toHaveBeenCalled()
  })

  it('fills both static and dynamic platforms when several are selected', async () => {
    // 批量编辑可同时选中 openai + trae。动态目录拉到后不得顶掉静态平台的候选。
    getModelAllowlistCandidates.mockResolvedValue(['glm-5.3'])
    const wrapper = mountSelector({
      platform: undefined,
      platforms: ['openai', 'trae'],
      modelValue: [],
    })
    await flushPromises()

    await wrapper.get('[data-testid="fill-related-models"]').trigger('click')
    await flushPromises()

    const emitted = wrapper.emitted('update:modelValue')?.[0]?.[0] as string[]
    // 静态那一路：openai 自带表（抽两个已知项）
    expect(emitted).toContain('gpt-5.6-sol')
    // 动态那一路：trae 接口目录
    expect(emitted).toContain('glm-5.3')
    expect(getModelAllowlistCandidates).toHaveBeenCalledWith(0, 'trae')
  })
})
