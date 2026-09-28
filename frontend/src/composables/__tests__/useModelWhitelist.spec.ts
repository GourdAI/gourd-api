import { describe, expect, it, vi } from 'vitest'

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn()
}))

import { allModels, buildModelMappingObject, getModelsByPlatform, getPresetMappingsByPlatform, splitModelMappingObject } from '../useModelWhitelist'

// 下拉候选的唯一数据源是 allModels（由 allModelsList 派生），而 ModelWhitelistSelector
// 用 `allModels.filter(m => platformModels.has(m.value))` 做交集。任何一个
// getModelsByPlatform 分支漏并进 allModelsList，该平台账号的模型下拉就会恒显示
// 「无匹配模型」（trae / qoder / workbuddy / antigravity / opencode_go 均踩过这个坑）。
const allModelValues = new Set(allModels.map(model => model.value))
const CATALOG_PLATFORMS = [
  'openai',
  'anthropic',
  'claude',
  'gemini',
  'antigravity',
  'zhipu',
  'qwen',
  'deepseek',
  'mistral',
  'meta',
  'xai',
  'grok',
  'cohere',
  'yi',
  'moonshot',
  'kimi',
  'opencode_go',
  'workbuddy',
  'qoder',
  'trae',
  'doubao',
  'minimax',
  'baidu',
  'spark',
  'hunyuan',
  'perplexity'
]

describe('useModelWhitelist', () => {
  it('openai 模型列表包含 GPT-5.4 官方快照', () => {
    const models = getModelsByPlatform('openai')

    expect(models).toContain('gpt-5.4')
    expect(models).toContain('gpt-5.4-mini')
    expect(models).toContain('gpt-5.4-2026-03-05')
    expect(models).toContain('codex-auto-review')
    expect(models).toContain('gpt-5.6')
    expect(models).toContain('gpt-6')
    expect(models).toContain('gpt-6-astra')
  })

  it('openai 预设映射包含 GPT-6 别名和 Astra', () => {
    expect(getPresetMappingsByPlatform('openai')).toEqual(expect.arrayContaining([
      expect.objectContaining({ label: 'GPT-6', from: 'gpt-6', to: 'gpt-6' }),
      expect.objectContaining({ label: 'GPT-6 Astra', from: 'gpt-6-astra', to: 'gpt-6-astra' })
    ]))
  })

  it('openai 模型列表不再暴露已下线的 ChatGPT 登录 Codex 模型', () => {
    const models = getModelsByPlatform('openai')

    expect(models).not.toContain('gpt-5')
    expect(models).not.toContain('gpt-5.1')
    expect(models).not.toContain('gpt-5.1-codex')
    expect(models).not.toContain('gpt-5.1-codex-max')
    expect(models).not.toContain('gpt-5.1-codex-mini')
    expect(models).not.toContain('gpt-5.2-codex')
  })

  it('antigravity 模型列表包含图片模型兼容项', () => {
    const models = getModelsByPlatform('antigravity')

    expect(models).toContain('gemini-2.5-flash-image')
    expect(models).toContain('gemini-3.1-flash-image')
    expect(models).toContain('gemini-3-pro-image')
  })

  it('Claude 模型列表包含新发布的 Claude 模型', () => {
    expect(getModelsByPlatform('claude')).toContain('claude-fable-5-1')
    expect(getModelsByPlatform('antigravity')).toContain('claude-fable-5-1')
    expect(getModelsByPlatform('claude')).toContain('claude-fable-5')
    expect(getModelsByPlatform('antigravity')).toContain('claude-fable-5')
    expect(getModelsByPlatform('claude')).toContain('claude-opus-4-8')
    expect(getModelsByPlatform('antigravity')).toContain('claude-opus-4-8')
  })

  it('xAI 模型列表包含 Grok 4.5 官方模型和别名', () => {
    const models = getModelsByPlatform('grok')

    expect(models).toContain('grok-4.6')
    expect(models).toContain('grok-4.6-latest')
    expect(models).toContain('grok-4.5')
    expect(models).toContain('grok-4.5-latest')
    expect(models).toContain('grok-build-latest')
    expect(models).toContain('grok-imagine-image-2.0')
    expect(models).toContain('grok-imagine-video-1.5')
  })

  it('combined 模式支持 Grok 4.5 官方别名映射', () => {
    const mapping = buildModelMappingObject(
      'combined',
      ['grok-4.5'],
      [
        { from: 'grok-latest', to: 'grok-4.5' },
        { from: 'grok-4.5-latest', to: 'grok-4.5' },
        { from: 'grok-build-latest', to: 'grok-4.5' }
      ]
    )

    expect(mapping).toEqual({
      'grok-4.5': 'grok-4.5',
      'grok-latest': 'grok-4.5',
      'grok-4.5-latest': 'grok-4.5',
      'grok-build-latest': 'grok-4.5'
    })
  })

  it('grok 模型列表包含 Composer 默认项和兼容别名', () => {
    const models = getModelsByPlatform('grok')

    expect(models).toContain('grok-composer-2.5-fast')
    expect(models).not.toContain('grok-composer')
    expect(models).toContain('composer-2.5')
  })

  it('gemini 模型列表包含原生生图模型', () => {
    const models = getModelsByPlatform('gemini')

    expect(models).toContain('gemini-2.5-flash-image')
    expect(models).toContain('gemini-3.1-flash-image')
    expect(models.indexOf('gemini-3.1-flash-image')).toBeLessThan(models.indexOf('gemini-2.0-flash'))
    expect(models.indexOf('gemini-2.5-flash-image')).toBeLessThan(models.indexOf('gemini-2.5-flash'))
  })

  it('antigravity 模型列表会把新的 Gemini 图片模型排在前面', () => {
    const models = getModelsByPlatform('antigravity')

    expect(models.indexOf('gemini-3.1-flash-image')).toBeLessThan(models.indexOf('gemini-2.5-flash'))
    expect(models.indexOf('gemini-2.5-flash-image')).toBeLessThan(models.indexOf('gemini-2.5-flash-lite'))
  })

  it('antigravity 模型列表包含 Gemini 3.1 Pro 通用别名', () => {
    const models = getModelsByPlatform('antigravity')

    expect(models).toContain('gemini-3.1-pro')
  })

  it('whitelist 模式会忽略通配符条目', () => {
    const mapping = buildModelMappingObject('whitelist', ['claude-*', 'gemini-3.1-flash-image'], [])
    expect(mapping).toEqual({
      'gemini-3.1-flash-image': 'gemini-3.1-flash-image'
    })
  })

  it('whitelist 模式会保留 GPT-5.4 官方快照的精确映射', () => {
    const mapping = buildModelMappingObject('whitelist', ['gpt-5.4-2026-03-05'], [])

    expect(mapping).toEqual({
      'gpt-5.4-2026-03-05': 'gpt-5.4-2026-03-05'
    })
  })

  it('whitelist keeps GPT-5.4 mini exact mappings', () => {
    const mapping = buildModelMappingObject('whitelist', ['gpt-5.4-mini'], [])

    expect(mapping).toEqual({
      'gpt-5.4-mini': 'gpt-5.4-mini'
    })
  })

  it('combined 模式会同时保留白名单身份映射和模型映射', () => {
    const mapping = buildModelMappingObject(
      'combined',
      ['gpt-5.4', 'claude-*'],
      [
        { from: 'gpt-latest', to: 'gpt-5.4' },
        { from: 'gpt-5.4', to: 'gpt-5.4-mini' }
      ]
    )

    expect(mapping).toEqual({
      'gpt-5.4': 'gpt-5.4-mini',
      'gpt-latest': 'gpt-5.4'
    })
  })

  it('splitModelMappingObject 会把身份映射还原成白名单，其余保留为映射', () => {
    const parsed = splitModelMappingObject({
      'gpt-5.4': 'gpt-5.4',
      'gpt-latest': 'gpt-5.4',
      ' ': 'gpt-empty',
      broken: 123
    })

    expect(parsed).toEqual({
      allowedModels: ['gpt-5.4'],
      modelMappings: [{ from: 'gpt-latest', to: 'gpt-5.4' }]
    })
  })

  // Qoder 目录真值回归（2026-09-22 实测对齐上游 /api/v2/model/list；
  // 来源：Qoder CN 客户端 model classes app 场景 14 项）。
  it('qoder 模型列表与上游真实目录一致（14 项）', () => {
    const models = getModelsByPlatform('qoder')

    expect(models).toHaveLength(14)
    expect(models).toEqual(expect.arrayContaining([
      'auto',
      'qmodel_38max', 'qfmodel',
      'qmodel_latest', 'qmodel', 'q37fmodel',
      'dmodel', 'dfmodel',
      'gmodel', 'gfmodel', 'gm51model',
      'kmodel_latest', 'kmodel',
      'mmodel'
    ]))
  })

  it('qoder 模型列表不含上游已不可用的旧 key', () => {
    const models = getModelsByPlatform('qoder')

    expect(models).not.toContain('q36fmodel')
  })

  it('every getModelsByPlatform platform branch is covered by allModels', () => {
    for (const platform of CATALOG_PLATFORMS) {
      const missing = getModelsByPlatform(platform).filter(model => !allModelValues.has(model))
      expect(missing, `platform "${platform}" has models missing from allModels`).toEqual([])
    }
  })

  it('allModels exposes no duplicate option values', () => {
    const duplicates = allModels
      .map(model => model.value)
      .filter((value, index, values) => values.indexOf(value) !== index)

    expect(duplicates).toEqual([])
  })

  it('网关平台独有模型可从 allModels 里选到', () => {
    for (const model of ['Doubao-Seed-2.1-Pro', 'seed-code-pro-0430']) {
      expect(allModelValues.has(model), `expected allModels to contain ${model}`).toBe(true)
    }
    expect(allModelValues.has('qmodel_38max')).toBe(true)
    expect(allModelValues.has('hy3-preview')).toBe(true)
    expect(allModelValues.has('longcat-2.0')).toBe(true)
    expect(allModelValues.has('gemini-3-pro-high')).toBe(true)
    expect(allModelValues.has('tab_flash_lite_preview')).toBe(true)
  })
})
