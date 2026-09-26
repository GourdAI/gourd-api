import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import PlazaModelCard from '../PlazaModelCard.vue'
import type { PlazaModel } from '@/api/modelPlaza'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

const copyToClipboard = vi.fn(() => Promise.resolve(true))
vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copied: { value: false }, copyToClipboard })
}))

function tokenModel(overrides: Partial<PlazaModel> = {}): PlazaModel {
  return {
    name: 'claude-sonnet',
    platform: 'anthropic',
    pricing: {
      billing_mode: 'token',
      input_price: 3e-6,
      output_price: 1.5e-5,
      cache_write_price: 3.75e-6,
      cache_read_price: 3e-7,
      image_input_price: null,
      image_output_price: null,
      per_request_price: null,
      intervals: []
    },
    official_pricing: {
      input_price: 3e-6,
      output_price: 1.5e-5,
      cache_write_price: 3.75e-6,
      cache_write_1h_price: 6e-6,
      cache_read_price: 3e-7
    },
    ...overrides
  }
}

function mountCard(model: PlazaModel, context = { rateMultiplier: 1 }) {
  return mount(PlazaModelCard, {
    props: { model, context },
    global: { stubs: { Icon: true, ModelIcon: true } }
  })
}

/** 卡片内某个价格列（输入/输出/缓存）的 dd 节点。 */
function column(wrapper: ReturnType<typeof mountCard>, index: number) {
  return wrapper.findAll('dd')[index]
}

beforeEach(() => {
  copyToClipboard.mockClear()
})

describe('PlazaModelCard', () => {
  it('一张卡片一个模型:展示模型名与输入/输出/缓存三行', () => {
    const wrapper = mountCard(tokenModel())
    expect(wrapper.find('article').exists()).toBe(true)
    expect(wrapper.find('h4').text()).toBe('claude-sonnet')
    // 表格已下线:卡片内部不再使用 table/tbody
    expect(wrapper.find('table').exists()).toBe(false)
    expect(wrapper.findAll('dt').map((dt) => dt.text())).toEqual([
      'modelPlaza.card.input',
      'modelPlaza.card.output',
      'modelPlaza.card.cache'
    ])
    expect(column(wrapper, 0).text()).toBe('$3.00')
    expect(column(wrapper, 1).text()).toBe('$15.00')
    // 缓存拆写/读两行实付,官方参考另起一块
    expect(column(wrapper, 2).findAll('.leading-5')).toHaveLength(2)
    expect(column(wrapper, 2).findAll('[title="modelPlaza.card.officialPrice"]')).toHaveLength(2)
    expect(wrapper.text()).toContain('modelPlaza.card.unitPerMillion')
  })

  it('倍率徽章常驻卡片右上,专属倍率时划线展示分组默认倍率', () => {
    const plain = mountCard(tokenModel(), { rateMultiplier: 0.5 })
    const badge = plain.find('.plaza-model-card > header > div span[title]')
    expect(badge.text()).toBe('0.5x')
    expect(badge.attributes('title')).toBe('modelPlaza.card.rateTooltip')

    const custom = mountCard(tokenModel(), { rateMultiplier: 1, userRateMultiplier: 0.8 })
    const customBadge = custom.find('.plaza-model-card > header > div span[title]')
    expect(customBadge.find('.line-through').text()).toBe('1x')
    expect(customBadge.text()).toBe('1x0.8x')
    expect(customBadge.attributes('title')).toBe('modelPlaza.card.customRateTooltip')
  })

  it('倍率 ≠ 1 时官方参考价作为副行展示;与实付一致时不重复', () => {
    const discounted = mountCard(tokenModel(), { rateMultiplier: 0.5 })
    expect(discounted.text()).toContain('$1.50')
    expect(column(discounted, 0).text()).toContain('modelPlaza.card.officialTag')
    expect(column(discounted, 0).text()).toContain('$3.00')

    const flat = mountCard(tokenModel(), { rateMultiplier: 1 })
    // 输入/输出列无官方副行,缓存列因 1h 价差异仍保留
    expect(column(flat, 0).text()).toBe('$3.00')
    expect(column(flat, 1).text()).toBe('$15.00')
    expect(column(flat, 2).text()).toContain('modelPlaza.card.officialTag')
  })

  it('阶梯定价逐档成行并展示档位标签,超出部分计价带徽章与说明', () => {
    const model = tokenModel({
      long_context_basis: 'marginal',
      pricing: {
        billing_mode: 'token',
        input_price: 3e-6,
        output_price: 1.5e-5,
        cache_write_price: null,
        cache_read_price: null,
        image_input_price: null,
        image_output_price: null,
        per_request_price: null,
        intervals: [
          {
            min_tokens: 0,
            max_tokens: 200000,
            tier_label: '',
            input_price: 3e-6,
            output_price: 1.5e-5,
            cache_write_price: null,
            cache_read_price: null,
            per_request_price: null
          },
          {
            min_tokens: 200000,
            max_tokens: null,
            tier_label: '',
            input_price: 6e-6,
            output_price: 3e-5,
            cache_write_price: null,
            cache_read_price: null,
            per_request_price: null
          }
        ]
      }
    })
    const wrapper = mountCard(model, { rateMultiplier: 0.5 })
    const labels = column(wrapper, 0).findAll('.leading-5').map((row) => row.find('span').text())
    expect(labels).toEqual(['≤200K', '>200K'])
    expect(wrapper.text()).toContain('modelPlaza.card.marginalBadge')
    expect(wrapper.find('[title="modelPlaza.card.tierHintMarginal"]').exists()).toBe(true)
    expect(column(wrapper, 1).findAll('.leading-5')).toHaveLength(2)
  })

  it('Max 推理强度倍率徽章带说明', () => {
    const model = tokenModel()
    model.pricing!.max_reasoning_effort_multiplier = 3
    const wrapper = mountCard(model)
    expect(wrapper.text()).toContain('modelPlaza.card.maxReasoningMultiplierBadge')
    expect(wrapper.find('[title="modelPlaza.card.maxReasoningMultiplierHint"]').exists()).toBe(true)
  })

  it('按图片/按次计费展示单价芯片与单位后缀,不渲染 token 三列', () => {
    const wrapper = mountCard(
      tokenModel({
        name: 'gpt-image-2',
        pricing: {
          billing_mode: 'image',
          input_price: null,
          output_price: null,
          cache_write_price: null,
          cache_read_price: null,
          image_input_price: null,
          // 每 token 图片输出价:不应被当作按张单价展示
          image_output_price: 3e-5,
          per_request_price: null,
          intervals: [
            {
              min_tokens: 0,
              max_tokens: null,
              tier_label: '1K',
              input_price: null,
              output_price: null,
              cache_write_price: null,
              cache_read_price: null,
              per_request_price: 0.01
            },
            {
              min_tokens: 0,
              max_tokens: null,
              tier_label: '2K',
              input_price: null,
              output_price: null,
              cache_write_price: null,
              cache_read_price: null,
              per_request_price: 0.02
            }
          ]
        },
        official_pricing: null
      }),
      { rateMultiplier: 0.1 }
    )
    expect(wrapper.findAll('dt')).toHaveLength(0)
    expect(wrapper.text()).toContain('modelPlaza.card.perImage')
    expect(wrapper.text()).toContain('modelPlaza.card.perUnitImage')
    expect(wrapper.text()).toContain('1K')
    expect(wrapper.text()).toContain('$0.001')
    expect(wrapper.text()).toContain('2K')
    expect(wrapper.text()).toContain('$0.002')
    expect(wrapper.text()).not.toContain('$0.000003')
  })

  it('未配置单价时以占位符呈现', () => {
    const wrapper = mountCard(
      tokenModel({
        pricing: {
          billing_mode: 'per_request',
          input_price: null,
          output_price: null,
          cache_write_price: null,
          cache_read_price: null,
          image_input_price: null,
          image_output_price: null,
          per_request_price: null,
          intervals: []
        },
        official_pricing: null
      })
    )
    expect(wrapper.text()).toContain('modelPlaza.card.perRequest')
    expect(wrapper.find('.plaza-request-prices').text()).toBe('-')
  })

  it('复制按钮把模型名写入剪贴板', async () => {
    const wrapper = mountCard(tokenModel({ name: 'gpt-5.6-sol' }))
    await wrapper.find('button[aria-label="modelPlaza.card.copyModelName"]').trigger('click')
    expect(copyToClipboard).toHaveBeenCalledWith('gpt-5.6-sol')
  })

  it('触屏设备（无 hover）下复制按钮常驻可见', () => {
    const wrapper = mountCard(tokenModel())
    const button = wrapper.find('button[aria-label="modelPlaza.card.copyModelName"]')
    expect(button.attributes('class')).toContain('[@media(hover:none)]:opacity-60')
  })
})

describe('PlazaModelCard 分时计价', () => {
  function timePriced(): PlazaModel {
    return tokenModel({
      name: 'deepseek-chat',
      platform: 'deepseek',
      time_pricing: {
        timezone: 'Asia/Shanghai',
        periods: [
          { start_time: '00:30', end_time: '08:30:00', multiplier: 0.5 },
          { start_time: '18:00', end_time: '22:00', multiplier: 1.2 }
        ]
      }
    })
  }

  it('标准价之外为每个时段各渲染一块,价格与时段倍率折算', () => {
    const wrapper = mountCard(timePriced(), { rateMultiplier: 0.8 })
    // 1 个标准块 + 2 个时段块
    expect(wrapper.findAll('dl')).toHaveLength(3)
    const periods = wrapper.findAll('[title="modelPlaza.card.timePeriodHint"]')
    expect(periods).toHaveLength(2)
    expect(periods[0].text()).toContain('00:30–08:30')
    expect(periods[1].text()).toContain('18:00–22:00')
    // 时段生效倍率 0.8×0.5 / 0.8×1.2
    const rateChips = wrapper.findAll('.whitespace-nowrap')
    expect(rateChips.map((c) => c.text())).toContain('0.4x')
    expect(rateChips.map((c) => c.text())).toContain('0.96x')
    // 时段块输入价 3 × 0.4
    expect(wrapper.findAll('dd')[3].text()).toContain('$1.20')
    expect(wrapper.findAll('dd')[6].text()).toContain('$2.88')
  })

  it('时区与高峰说明收进 tooltip,不占卡片正文', () => {
    const wrapper = mountCard(timePriced())
    expect(wrapper.text()).not.toContain('Asia/Shanghai')
    expect(wrapper.find('[title="modelPlaza.card.timePeriodHint"]').exists()).toBe(true)
  })

  it('仅工作日生效时换用带周末回落说明的文案并标注工作日', () => {
    const model = timePriced()
    model.time_pricing!.weekdays_only = true
    const wrapper = mountCard(model)
    const badge = wrapper.find('[title="modelPlaza.card.timePeriodHintWeekdays"]')
    expect(badge.exists()).toBe(true)
    expect(badge.text()).toContain('modelPlaza.card.timePricingWeekdays')
    expect(wrapper.find('[title="modelPlaza.card.timePeriodHint"]').exists()).toBe(false)
  })

  it('无分时配置时只有标准价一块', () => {
    const wrapper = mountCard(tokenModel())
    expect(wrapper.findAll('dl')).toHaveLength(1)
    expect(wrapper.find('[title*="modelPlaza.card.timePeriodHint"]').exists()).toBe(false)
  })

  it('按次/按图模型不展开时段块(单价不受时段倍率影响)', () => {
    // 后端写侧/读侧均不允许非 token 模型配置分时;此处仅防脏数据下
    // 出现「价格与标准价相同却标注时段倍率」的自相矛盾卡片。
    const model = tokenModel({
      name: 'gpt-image-2',
      pricing: {
        billing_mode: 'image',
        input_price: null,
        output_price: null,
        cache_write_price: null,
        cache_read_price: null,
        image_input_price: null,
        image_output_price: null,
        per_request_price: 0.04,
        intervals: []
      },
      official_pricing: null,
      time_pricing: {
        timezone: 'Asia/Shanghai',
        periods: [{ start_time: '00:30', end_time: '08:30', multiplier: 0.5 }]
      }
    })
    const wrapper = mountCard(model, { rateMultiplier: 0.5 })
    expect(wrapper.findAll('dl')).toHaveLength(0)
    expect(wrapper.find('[title="modelPlaza.card.timePeriodHint"]').exists()).toBe(false)
    expect(wrapper.find('.plaza-request-prices').text()).toBe('$0.02 modelPlaza.card.perUnitImage')
  })
})
