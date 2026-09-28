import { describe, expect, it } from 'vitest'
import {
  buildPlazaModelDrafts,
  buildPlazaModelEntries,
  entrySortPrice,
  matchPlazaModelEntry,
  sortPlazaModelEntries
} from '../plazaCatalog'
import type { ModelPlazaGroup, PlazaModel } from '@/api/modelPlaza'

/** 输入价按输出价的 1/5 派生(与真实模型定价结构一致),保证比价口径可验。 */
function tokenModel(name: string, output: number | null, overrides: Partial<PlazaModel> = {}): PlazaModel {
  return {
    name,
    platform: 'openai',
    pricing: {
      billing_mode: 'token',
      input_price: output == null ? null : output / 5,
      output_price: output,
      cache_write_price: null,
      cache_read_price: null,
      image_input_price: null,
      image_output_price: null,
      per_request_price: null,
      intervals: []
    },
    official_pricing: null,
    ...overrides
  }
}

function requestModel(name: string, price: number | null): PlazaModel {
  return {
    name,
    platform: 'openai',
    pricing: {
      billing_mode: 'image',
      input_price: null,
      output_price: null,
      cache_write_price: null,
      cache_read_price: null,
      image_input_price: null,
      image_output_price: null,
      per_request_price: price,
      intervals: []
    },
    official_pricing: null
  }
}

function group(id: number, name: string, models: PlazaModel[], rate = 1): ModelPlazaGroup {
  return {
    id,
    name,
    description: '',
    platform: 'openai',
    subscription_type: 'standard',
    rate_multiplier: rate,
    peak_rate_enabled: false,
    peak_start: '',
    peak_end: '',
    peak_rate_multiplier: 1,
    is_exclusive: false,
    image_rate_independent: false,
    image_rate_multiplier: 1,
    long_context_pricing_enabled: true,
    models
  }
}

function allVisible(groups: ModelPlazaGroup[]) {
  return new Set(groups.map((g) => g.id))
}

describe('buildPlazaModelEntries 模型维度展平', () => {
  it('同名模型跨分组合并成一条卡片,分组标签齐全', () => {
    const groups = [
      group(1, '标准组', [tokenModel('gpt-5.6', 1.5e-5), tokenModel('o3', 1e-5)]),
      group(2, 'VIP 组', [tokenModel('gpt-5.6', 1.5e-5)], 0.5)
    ]
    const entries = buildPlazaModelEntries(groups, { visibleGroupIds: allVisible(groups) })
    expect(entries.map((e) => e.name)).toEqual(['gpt-5.6', 'o3'])
    expect(entries[0].groups.map((g) => g.id)).toEqual([1, 2])
    expect(entries[1].groups).toHaveLength(1)
  })

  it('「全部」视图取最低实付价所在分组', () => {
    const groups = [
      group(1, '标准组', [tokenModel('gpt-5.6', 1.5e-5)]),
      group(2, 'VIP 组', [tokenModel('gpt-5.6', 1.5e-5)], 0.5)
    ]
    const entries = buildPlazaModelEntries(groups, { visibleGroupIds: allVisible(groups) })
    expect(entries[0].displayGroupId).toBe(2)
    expect(entries[0].context.rateMultiplier).toBe(0.5)
    expect(entrySortPrice(entries[0])).toBeCloseTo(1.5, 10)
  })

  it('选中分组时按该分组计价(new-api #5341 的反例)', () => {
    const groups = [
      group(1, '标准组', [tokenModel('gpt-5.6', 1.5e-5)]),
      group(2, 'VIP 组', [tokenModel('gpt-5.6', 1.5e-5)], 0.5)
    ]
    const entries = buildPlazaModelEntries(groups, {
      visibleGroupIds: new Set([1]),
      preferredGroupId: 1
    })
    expect(entries).toHaveLength(1)
    expect(entries[0].displayGroupId).toBe(1)
    expect(entries[0].context.rateMultiplier).toBe(1)
    expect(entrySortPrice(entries[0])).toBeCloseTo(3, 10)
    // 底部仍列出它属于的全部两组,但计价只看可见分组
    expect(entries[0].groups.map((g) => [g.id, g.visible])).toEqual([
      [1, true],
      [2, false]
    ])
  })

  it('分组筛选先落地:只剩 VIP 组时 VIP 模型不会出现在标准组卡片里', () => {
    const groups = [
      group(1, '标准组', [tokenModel('gpt-5.6', 1.5e-5)]),
      group(2, 'VIP 组', [tokenModel('claude-only', 1e-5)], 0.5)
    ]
    const entries = buildPlazaModelEntries(groups, { visibleGroupIds: new Set([2]) })
    expect(entries.map((e) => e.name)).toEqual(['claude-only'])
  })

  it('各分组模型都无实付价时保留卡片(渲染占位),不静默丢失模型', () => {
    const groups = [group(1, '标准组', [tokenModel('unpriced', null)])]
    const entries = buildPlazaModelEntries(groups, { visibleGroupIds: allVisible(groups) })
    expect(entries).toHaveLength(1)
    expect(entrySortPrice(entries[0])).toBeNull()
  })

  it('「全部」按实付输入价比价(卡片首行就是输入价)', () => {
    // 组 1 输入贵、输出便宜;组 2 输入便宜、输出贵——只有按输入价才会选组 2
    const groups = [
      group(1, '标准组', [tokenModel('gpt-x', 1.5e-5)]),
      group(2, 'VIP 组', [tokenModel('gpt-x', 4e-5, { pricing: { billing_mode: 'token', input_price: 1e-6, output_price: 4e-5, cache_write_price: null, cache_read_price: null, image_input_price: null, image_output_price: null, per_request_price: null, intervals: [] }, official_pricing: null })])
    ]
    const entries = buildPlazaModelEntries(groups, { visibleGroupIds: allVisible(groups) })
    // 输出价口径会选组 1(15 < 40),输入价口径才选组 2(1 < 3)
    expect(entries[0].displayGroupId).toBe(2)
    expect(entries[0].model.pricing?.input_price).toBe(1e-6)
    expect(entrySortPrice(entries[0])).toBeCloseTo(1, 10)
  })

  it('同模型的定价按分组各取一份:展示价来自 displayGroupId', () => {
    const groups = [
      group(1, '标准组', [tokenModel('gpt-5.6', 1.5e-5)]),
      group(2, 'VIP 组', [tokenModel('gpt-5.6', 3e-5)], 0.5)
    ]
    const entries = buildPlazaModelEntries(groups, { visibleGroupIds: allVisible(groups) })
    // VIP: 输入 6e-6 × 0.5 = 3;标准: 输入 3e-6 × 1 = 3 → 同价取先出现的分组
    expect(entries[0].displayGroupId).toBe(1)
    expect(entries[0].model.pricing?.output_price).toBe(1.5e-5)
  })

  it('同名跨平台模型的平台去重合并', () => {
    const groups = [
      group(1, 'A 组', [{ ...tokenModel('deepseek-v4', 1e-5), platform: 'deepseek' }]),
      group(2, 'B 组', [{ ...tokenModel('deepseek-v4', 2e-5), platform: 'trae' }])
    ]
    const entries = buildPlazaModelEntries(groups, { visibleGroupIds: allVisible(groups) })
    expect(entries[0].platforms).toEqual(['deepseek', 'trae'])
  })

  it('同分组内不同平台的同名模型不会被后一份定价静默覆盖', () => {
    // composite 分组内允许两条 (platform,name) 相同的模型:卡片合并,但取价必须精确到平台
    const zhipu = { ...tokenModel('glm-4', 2e-5), platform: 'zhipu' }
    const silicon = { ...tokenModel('glm-4', 5e-6), platform: 'siliconflow' }
    const groups = [group(1, '综合组', [zhipu, silicon])]
    const drafts = buildPlazaModelDrafts(groups, { visibleGroupIds: new Set([1]) })
    expect(drafts[0].groups.map((g) => g.platform)).toEqual(['zhipu', 'siliconflow'])
    // 旧实现以 group.id 为键 → 两条同组条目后写覆盖前写,卡片永远只能拿到 siliconflow
    expect(drafts[0].models.size).toBe(2)

    // 「全部」:输入价 zhipu 4 < silicon 1 → 取 siliconflow,且 model 必须跟着走
    const cheapest = buildPlazaModelEntries(groups, { visibleGroupIds: new Set([1]) })
    expect(cheapest[0].model.platform).toBe('siliconflow')
    expect(cheapest[0].model.pricing?.output_price).toBe(5e-6)

    // 选中该分组:取分组内第一条(zhipu),不能拿到被覆盖后的 siliconflow 定价
    const preferred = buildPlazaModelEntries(groups, { visibleGroupIds: new Set([1]), preferredGroupId: 1 })
    expect(preferred[0].model.platform).toBe('zhipu')
    expect(preferred[0].model.pricing?.output_price).toBe(2e-5)
  })

  it('分组名为空时兜底成 #id,chip 不会退化', () => {
    const entries = buildPlazaModelEntries([group(7, '', [tokenModel('m', 1e-5)])], {
      visibleGroupIds: new Set([7])
    })
    expect(entries[0].groups[0].name).toBe('#7')
  })

  it('models 缺失(null/undefined)不炸整页', () => {
    const broken = { ...group(1, '坏组'), models: undefined } as unknown as ModelPlazaGroup
    expect(() => buildPlazaModelEntries([broken], { visibleGroupIds: new Set([1]) })).not.toThrow()
    expect(buildPlazaModelEntries([broken], { visibleGroupIds: new Set([1]) })).toEqual([])
  })

  it('高峰窗口描述按分组注入价格环境', () => {
    const groups = [group(1, '标准组', [tokenModel('gpt-5.6', 1.5e-5)])]
    const entries = buildPlazaModelEntries(groups, {
      visibleGroupIds: allVisible(groups),
      peakWindows: new Map([[1, '14:00-18:00 ×1.5']])
    })
    expect(entries[0].context.peakWindow).toBe('14:00-18:00 ×1.5')
  })
})

describe('sortPlazaModelEntries', () => {
  const groups = [
    group(1, '标准组', [
      tokenModel('cheap', 1e-6),
      tokenModel('expensive', 5e-5),
      tokenModel('unpriced', null),
      requestModel('gpt-image-2', 0.04)
    ])
  ]
  const entries = buildPlazaModelEntries(groups, { visibleGroupIds: allVisible(groups) })

  it('默认:token 在前按实付输入价降序,未定价与按图计费沉底', () => {
    expect(
      sortPlazaModelEntries(entries, 'default').map((e) => e.name)
    ).toEqual(['expensive', 'cheap', 'unpriced', 'gpt-image-2'])
  })

  it('价格升序:未定价仍在最后', () => {
    expect(
      sortPlazaModelEntries(entries, 'priceAsc').map((e) => e.name)
    ).toEqual(['cheap', 'expensive', 'unpriced', 'gpt-image-2'])
  })

  it('名称升序按模型名(token 计费仍在非 token 之前)', () => {
    expect(
      sortPlazaModelEntries(entries, 'nameAsc').map((e) => e.name)
    ).toEqual(['cheap', 'expensive', 'unpriced', 'gpt-image-2'])
  })

  it('同价按名称降序(新版本号在前,且按数值比较)', () => {
    const same = buildPlazaModelEntries(
      [group(1, 'g', [tokenModel('gpt-5.5', 1e-5), tokenModel('gpt-5.6-sol', 1e-5)])],
      { visibleGroupIds: new Set([1]) }
    )
    expect(sortPlazaModelEntries(same, 'default').map((e) => e.name)).toEqual(['gpt-5.6-sol', 'gpt-5.5'])
  })

  it('单位数与双位数版本号按数值而非字符比较(gpt-10 不算比 gpt-9 旧)', () => {
    const same = buildPlazaModelEntries(
      [group(1, 'g', [tokenModel('gpt-9', 1e-5), tokenModel('gpt-10', 1e-5)])],
      { visibleGroupIds: new Set([1]) }
    )
    expect(sortPlazaModelEntries(same, 'default').map((e) => e.name)).toEqual(['gpt-10', 'gpt-9'])
    expect(sortPlazaModelEntries(same, 'nameAsc').map((e) => e.name)).toEqual(['gpt-9', 'gpt-10'])
  })

  it('不修改入参数组', () => {
    const snapshot = entries.map((e) => e.name)
    sortPlazaModelEntries(entries, 'priceAsc')
    expect(entries.map((e) => e.name)).toEqual(snapshot)
  })
})

describe('matchPlazaModelEntry / 计数', () => {
  const groups = [
    group(1, 'A 组', [{ ...tokenModel('deepseek-v4-flash', 1e-5), platform: 'deepseek' }]),
    group(2, 'B 组', [tokenModel('gpt-5.6', 1e-5)])
  ]
  const entries = buildPlazaModelEntries(groups, { visibleGroupIds: allVisible(groups) })

  it('空查询命中全部', () => {
    expect(entries.every((e) => matchPlazaModelEntry(e, '   '))).toBe(true)
  })

  it('按模型名命中(大小写不敏感)', () => {
    expect(entries.filter((e) => matchPlazaModelEntry(e, 'DEEPSEEK'))).toHaveLength(1)
  })

  it('按平台名命中:new-api 支持按供应商搜索', () => {
    expect(entries.filter((e) => matchPlazaModelEntry(e, 'deepseek'))[0].name).toBe('deepseek-v4-flash')
    const trae = buildPlazaModelEntries([group(1, 'g', [{ ...tokenModel('kimi-k2', 1e-5), platform: 'trae' }])], {
      visibleGroupIds: new Set([1])
    })
    expect(matchPlazaModelEntry(trae[0], 'trae')).toBe(true)
  })

  it('目录主键唯一且可用于 v-for key', () => {
    const keys = entries.map((e) => e.key)
    expect(new Set(keys).size).toBe(keys.length)
  })
})

describe('buildPlazaModelDrafts', () => {
  it('保留被筛掉的分组(卡片底部信息完整),并标记 visible', () => {
    const groups = [
      group(1, '标准组', [tokenModel('gpt-5.6', 1.5e-5)]),
      group(2, 'VIP 组', [tokenModel('gpt-5.6', 1.5e-5)], 0.5)
    ]
    const drafts = buildPlazaModelDrafts(groups, { visibleGroupIds: new Set([1]) })
    expect(drafts).toHaveLength(1)
    expect(drafts[0].groups.map((g) => [g.id, g.visible])).toEqual([
      [1, true],
      [2, false]
    ])
  })
})
