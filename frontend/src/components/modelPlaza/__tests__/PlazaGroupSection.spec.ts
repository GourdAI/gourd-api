import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import PlazaGroupSection from '../PlazaGroupSection.vue'
import PlazaModelCard from '../PlazaModelCard.vue'
import GroupBadge from '@/components/common/GroupBadge.vue'
import type { ModelPlazaGroup, PlazaModel } from '@/api/modelPlaza'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ cachedPublicSettings: null })
}))

function ladderModel(tiers: number, name = 'gpt-5.6-sol'): PlazaModel {
  const intervals = Array.from({ length: tiers }, (_, i) => ({
    min_tokens: i * 272000,
    max_tokens: i === tiers - 1 ? null : (i + 1) * 272000,
    tier_label: '',
    input_price: 5e-6,
    output_price: 3e-5,
    cache_write_price: null,
    cache_read_price: null,
    per_request_price: null
  }))
  return {
    name,
    platform: 'openai',
    pricing: {
      billing_mode: 'token',
      input_price: 5e-6,
      output_price: 3e-5,
      cache_write_price: null,
      cache_read_price: null,
      image_input_price: null,
      image_output_price: null,
      per_request_price: null,
      intervals: []
    },
    official_pricing: {
      input_price: 5e-6,
      output_price: 3e-5,
      cache_write_price: null,
      cache_read_price: null,
      intervals
    }
  }
}

function group(overrides: Partial<ModelPlazaGroup> = {}): ModelPlazaGroup {
  return {
    id: 1,
    name: 'g',
    description: '',
    platform: 'openai',
    subscription_type: 'standard',
    rate_multiplier: 1,
    peak_rate_enabled: false,
    peak_start: '',
    peak_end: '',
    peak_rate_multiplier: 1,
    is_exclusive: false,
    image_rate_independent: false,
    image_rate_multiplier: 1,
    long_context_pricing_enabled: true,
    models: [ladderModel(2)],
    ...overrides
  }
}

function mountSection(g: ModelPlazaGroup) {
  return mount(PlazaGroupSection, {
    props: { group: g },
    global: {
      stubs: {
        GroupBadge: true,
        Icon: true,
        PlazaModelCard: true
      }
    }
  })
}

const NOTE = 'modelPlaza.detail.longContextDisabledNote'

describe('PlazaGroupSection 卡片网格', () => {
  it('组内每个模型渲染一张卡片,网格为响应式多列', () => {
    const wrapper = mountSection(
      group({ models: [ladderModel(1, 'a'), ladderModel(1, 'b'), ladderModel(1, 'c')] })
    )
    const grid = wrapper.find('.grid')
    expect(grid.exists()).toBe(true)
    // 表格视图已下线
    expect(wrapper.find('table').exists()).toBe(false)
    expect(wrapper.findAllComponents(PlazaModelCard)).toHaveLength(3)
    expect(grid.classes()).toEqual(
      expect.arrayContaining(['sm:grid-cols-2', 'xl:grid-cols-3'])
    )
  })

  it('按官方输出价降序排卡片,token 计费排在按次/按图之前', () => {
    const cheap = ladderModel(1, 'model-cheap')
    const expensive = ladderModel(1, 'model-expensive')
    expensive.official_pricing = { ...expensive.official_pricing!, output_price: 7.5e-5 }
    const image: PlazaModel = {
      ...ladderModel(1, 'gpt-image-2'),
      pricing: {
        billing_mode: 'image',
        input_price: null,
        output_price: null,
        cache_write_price: null,
        cache_read_price: null,
        image_input_price: null,
        image_output_price: null,
        per_request_price: 0.02,
        intervals: []
      }
    }
    const wrapper = mountSection(group({ models: [image, cheap, expensive] }))
    expect(
      wrapper.findAllComponents(PlazaModelCard).map((card) => card.props('model').name)
    ).toEqual(['model-expensive', 'model-cheap', 'gpt-image-2'])
  })

  it('分组无模型时展示空态而不是空网格', () => {
    const wrapper = mountSection(group({ models: [] }))
    expect(wrapper.find('.grid').exists()).toBe(false)
    expect(wrapper.text()).toContain('modelPlaza.detail.noModels')
  })

  it('组头展示模型数量', () => {
    const wrapper = mountSection(group({ models: [ladderModel(1, 'a'), ladderModel(1, 'b')] }))
    expect(wrapper.text()).toContain('modelPlaza.card.modelCount')
  })
})

describe('PlazaGroupSection 价格环境传递', () => {
  it('把分组倍率、专属倍率与生图独立倍率传给每张卡片', () => {
    const wrapper = mountSection(
      group({
        rate_multiplier: 1.2,
        user_rate_multiplier: 0.6,
        image_rate_independent: true,
        image_rate_multiplier: 2
      })
    )
    const context = wrapper.findComponent(PlazaModelCard).props('context')
    expect(context).toMatchObject({
      rateMultiplier: 1.2,
      userRateMultiplier: 0.6,
      imageRateIndependent: true,
      imageRateMultiplier: 2
    })
  })

  it('分组启用高峰时把窗口描述与倍率并入价格环境(appStore mock 无时区,故不带时区标注)', () => {
    const wrapper = mountSection(
      group({
        subscription_type: 'subscription',
        peak_rate_enabled: true,
        peak_start: '14:00',
        peak_end: '18:00',
        peak_rate_multiplier: 1.5
      })
    )
    const context = wrapper.findComponent(PlazaModelCard).props('context')
    expect(context.peakWindow).toBe('14:00-18:00 ×1.5')
    expect(context.peakRateMultiplier).toBe(1.5)
    // 高峰说明在组头披露
    expect(wrapper.text()).toContain('modelPlaza.detail.peakNote')
  })

  it('分组未启用高峰时窗口描述为空串', () => {
    const wrapper = mountSection(group())
    expect(wrapper.findComponent(PlazaModelCard).props('context').peakWindow).toBe('')
    expect(wrapper.text()).not.toContain('modelPlaza.detail.peakNote')
  })
})

describe('PlazaGroupSection 长上下文说明', () => {
  it('分组关闭阶梯且组内有官方阶梯模型时显示说明', () => {
    const wrapper = mountSection(group({ long_context_pricing_enabled: false }))
    expect(wrapper.text()).toContain(NOTE)
  })

  it('分组开启阶梯时不显示', () => {
    const wrapper = mountSection(group({ long_context_pricing_enabled: true }))
    expect(wrapper.text()).not.toContain(NOTE)
  })

  it('分组关闭但没有官方阶梯模型时不显示', () => {
    const wrapper = mountSection(
      group({ long_context_pricing_enabled: false, models: [ladderModel(1)] })
    )
    expect(wrapper.text()).not.toContain(NOTE)
  })

  it('旧后端缺少开关字段时不显示', () => {
    const g = group()
    delete (g as Partial<ModelPlazaGroup>).long_context_pricing_enabled
    const wrapper = mountSection(g)
    expect(wrapper.text()).not.toContain(NOTE)
  })
})

describe('PlazaGroupSection 平台展示移除', () => {
  it('不再把平台传给分组徽章,徽章改用品牌青', () => {
    const wrapper = mount(PlazaGroupSection, {
      props: { group: group({ platform: 'anthropic' }) },
      global: {
        stubs: { Icon: true, PlazaModelCard: true }
      }
    })
    const badge = wrapper.findComponent(GroupBadge)
    expect(badge.exists()).toBe(true)
    // 不传平台 → GroupBadge 不渲染平台 logo / 平台主题色
    expect(badge.props('platform')).toBeUndefined()
    expect(badge.props('brand')).toBe(true)
  })

  it('分组卡片不再带平台描边色类,仅中性边框', () => {
    const wrapper = mountSection(group({ platform: 'openai' }))
    const cls = wrapper.find('section').attributes('class') ?? ''
    // 旧实现按平台派生 border-green-500/35 等强描边;现为中性 gray/dark 边框
    expect(cls).not.toMatch(/border-(orange|green|purple|blue|zinc|pink|indigo|teal|rose|amber|violet|cyan)-500/)
    expect(cls).toContain('border-gray-200/70')
  })
})
