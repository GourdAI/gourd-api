import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import PlazaModelCard from '../PlazaModelCard.vue'
import { buildPlazaModelEntries } from '../plazaCatalog'
import type { ModelPlazaGroup, PlazaModel } from '@/api/modelPlaza'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
      locale: { value: 'zh-CN' }
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

function group(overrides: Partial<ModelPlazaGroup> = {}): ModelPlazaGroup {
  return {
    id: 1,
    name: '标准组',
    description: '',
    platform: 'anthropic',
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
    models: [tokenModel()],
    ...overrides
  }
}

/** 用真实目录构建逻辑取第一个模型条目作为卡片入参。 */
function entryOf(...groups: ModelPlazaGroup[]) {
  const entries = buildPlazaModelEntries(groups, { visibleGroupIds: new Set(groups.map((g) => g.id)) })
  return entries[0]
}

function mountCard(entry = entryOf(group())) {
  return mount(PlazaModelCard, {
    props: { entry },
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
  it('一张卡片一个模型:展示模型名与输入/输出/缓存三行实付价', () => {
    const wrapper = mountCard()
    expect(wrapper.find('article').exists()).toBe(true)
    expect(wrapper.find('h4').text()).toBe('claude-sonnet')
    expect(wrapper.find('table').exists()).toBe(false)
    expect(wrapper.findAll('dt').map((dt) => dt.text())).toEqual([
      'modelPlaza.card.input',
      'modelPlaza.card.output',
      'modelPlaza.card.cache'
    ])
    expect(column(wrapper, 0).text()).toBe('$3.00')
    expect(column(wrapper, 1).text()).toBe('$15.00')
    // 缓存拆写/读两行
    expect(column(wrapper, 2).findAll('.plaza-price-line')).toHaveLength(2)
    expect(wrapper.text()).toContain('modelPlaza.card.unitPerMillion')
  })

  it('官方参考价已下线:卡片不再出现官方行与相关标识', () => {
    const wrapper = mountCard(entryOf(group({ rate_multiplier: 0.5 })))
    // 实付折后价 1.5,官方 3.00 不再作为副行出现
    expect(column(wrapper, 0).text()).toBe('$1.50')
    expect(wrapper.find('[title="modelPlaza.card.officialPrice"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('modelPlaza.card.officialTag')
    expect(column(wrapper, 0).findAll('.plaza-price-line')).toHaveLength(1)
  })

  it('倍率徽章常驻卡片右上,专属倍率时划线展示分组默认倍率', () => {
    const plain = mountCard(entryOf(group({ rate_multiplier: 0.5 })))
    const badge = plain.find('.plaza-model-card > header > div span[title]')
    expect(badge.text()).toBe('0.5x')
    expect(badge.attributes('title')).toBe('modelPlaza.card.rateTooltip')

    const custom = mountCard(entryOf(group({ rate_multiplier: 1, user_rate_multiplier: 0.8 })))
    const customBadge = custom.find('.plaza-model-card > header > div span[title]')
    expect(customBadge.find('.line-through').text()).toBe('1x')
    expect(customBadge.text()).toBe('1x0.8x')
    expect(customBadge.attributes('title')).toBe('modelPlaza.card.customRateTooltip')
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
    const wrapper = mountCard(entryOf(group({ rate_multiplier: 0.5, models: [model] })))
    const labels = column(wrapper, 0).findAll('.plaza-price-line').map((row) => row.find('span').text())
    expect(labels).toEqual(['≤200K', '>200K'])
    expect(wrapper.text()).toContain('modelPlaza.card.marginalBadge')
    expect(wrapper.find('[title="modelPlaza.card.tierHintMarginal"]').exists()).toBe(true)
    expect(column(wrapper, 1).findAll('.plaza-price-line')).toHaveLength(2)
  })

  it('Max 推理强度倍率徽章带说明', () => {
    const model = tokenModel()
    model.pricing!.max_reasoning_effort_multiplier = 3
    const wrapper = mountCard(entryOf(group({ models: [model] })))
    expect(wrapper.text()).toContain('modelPlaza.card.maxReasoningMultiplierBadge')
    expect(wrapper.find('[title="modelPlaza.card.maxReasoningMultiplierHint"]').exists()).toBe(true)
  })

  it('分组免费模型命中时展示「免费」徽章与说明', () => {
    const wrapper = mountCard(entryOf(group({ models: [tokenModel({ free: true })] })))
    expect(wrapper.text()).toContain('modelPlaza.card.freeBadge')
    expect(wrapper.find('[title="modelPlaza.card.freeBadgeHint"]').exists()).toBe(true)
  })

  it('未命中免费名单（缺省 free）时不渲染「免费」徽章', () => {
    const wrapper = mountCard()
    expect(wrapper.text()).not.toContain('modelPlaza.card.freeBadge')
    expect(wrapper.find('[title="modelPlaza.card.freeBadgeHint"]').exists()).toBe(false)
  })

  it('按图片/按次计费展示单价芯片与单位后缀,不渲染 token 三列', () => {
    const model = tokenModel({
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
    })
    const wrapper = mountCard(entryOf(group({ rate_multiplier: 0.1, models: [model] })))
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
      entryOf(
        group({
          models: [
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
          ]
        })
      )
    )
    expect(wrapper.text()).toContain('modelPlaza.card.perRequest')
    expect(wrapper.find('.plaza-request-prices').text()).toBe('-')
  })

  it('复制按钮把模型名写入剪贴板', async () => {
    const wrapper = mountCard(entryOf(group({ models: [tokenModel({ name: 'gpt-5.6-sol' })] })))
    await wrapper.find('button[aria-label="modelPlaza.card.copyModelName"]').trigger('click')
    expect(copyToClipboard).toHaveBeenCalledWith('gpt-5.6-sol')
  })

  it('触屏设备（无 hover）下复制按钮常驻可见', () => {
    const wrapper = mountCard()
    const button = wrapper.find('button[aria-label="modelPlaza.card.copyModelName"]')
    expect(button.attributes('class')).toContain('[@media(hover:none)]:opacity-60')
  })
})

describe('PlazaModelCard 底部分组标签', () => {
  function twoGroups() {
    return [
      group({ id: 1, name: '标准组', rate_multiplier: 1 }),
      group({ id: 2, name: 'VIP 组', rate_multiplier: 0.5 })
    ]
  }

  it('列出该模型可用的全部分组与倍率', () => {
    const wrapper = mountCard(entryOf(...twoGroups()))
    const chips = wrapper.findAll('footer button')
    expect(chips).toHaveLength(2)
    expect(chips.map((chip) => chip.text())).toEqual(['标准组1x', 'VIP 组0.5x'])
    expect(wrapper.find('footer').text()).toContain('modelPlaza.card.availableIn')
  })

  it('默认高亮最低价分组,点击另一个分组抛出其 id 以切换价格', async () => {
    const wrapper = mountCard(entryOf(...twoGroups()))
    const chips = wrapper.findAll('footer button')
    expect(chips[1].attributes('aria-pressed')).toBe('true')
    expect(chips[0].attributes('aria-pressed')).toBe('false')
    await chips[0].trigger('click')
    expect(wrapper.emitted('selectGroup')).toEqual([[1]])
  })

  it('单分组模型也展示分组标签,提示当前价所属分组', () => {
    const wrapper = mountCard(entryOf(group()))
    const chip = wrapper.find('footer button')
    expect(chip.text()).toBe('标准组1x')
    expect(chip.attributes('aria-pressed')).toBe('true')
    expect(chip.attributes('title')).toBe('modelPlaza.card.groupChipCurrent')
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

  it('折叠成一行时段摘要,标准价仍是正文唯一价格块', () => {
    const wrapper = mountCard(entryOf(group({ rate_multiplier: 0.8, models: [timePriced()] })))
    // 只有标准价一块,时段不再各自展开
    expect(wrapper.findAll('dl')).toHaveLength(1)
    const line = wrapper.find('[title="modelPlaza.card.timePeriodHint"]')
    expect(line.exists()).toBe(true)
    expect(line.text()).toContain('modelPlaza.card.timePricingSummary')
    expect(line.text()).toContain('00:30–08:30 ×0.5')
    expect(line.text()).toContain('18:00–22:00 ×1.2')
    // 正文只展示标准价（输入 $2.40）
    expect(column(wrapper, 0).text()).toBe('$2.40')
  })

  it('时区与高峰说明收进 tooltip,不占卡片正文', () => {
    const wrapper = mountCard(entryOf(group({ models: [timePriced()] })))
    expect(wrapper.text()).not.toContain('Asia/Shanghai')
    expect(wrapper.find('[title="modelPlaza.card.timePeriodHint"]').exists()).toBe(true)
  })

  it('仅工作日生效时换用带周末回落说明的文案并标注工作日', () => {
    const model = timePriced()
    model.time_pricing!.weekdays_only = true
    const wrapper = mountCard(entryOf(group({ models: [model] })))
    const line = wrapper.find('[title="modelPlaza.card.timePeriodHintWeekdays"]')
    expect(line.exists()).toBe(true)
    expect(line.text()).toContain('modelPlaza.card.timePricingWeekdays')
    expect(wrapper.find('[title="modelPlaza.card.timePeriodHint"]').exists()).toBe(false)
  })

  it('无分时配置时不出现时段提示行', () => {
    const wrapper = mountCard()
    expect(wrapper.findAll('dl')).toHaveLength(1)
    expect(wrapper.find('[title*="modelPlaza.card.timePeriodHint"]').exists()).toBe(false)
  })

  it('按次/按图模型不展示时段提示(单价不受时段倍率影响)', () => {
    // 后端写侧/读侧均不允许非 token 模型配置分时;此处防止脏数据下出现自相矛盾的提示行。
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
    const wrapper = mountCard(entryOf(group({ rate_multiplier: 0.5, models: [model] })))
    expect(wrapper.findAll('dl')).toHaveLength(0)
    expect(wrapper.find('[title="modelPlaza.card.timePeriodHint"]').exists()).toBe(false)
    // 金额与单位之间的间距由 CSS gap 提供,断言不依赖文本空白
    const requestPrices = wrapper.find('.plaza-request-prices')
    expect(requestPrices.find('.plaza-request-amount').text()).toBe('$0.02')
    expect(requestPrices.text()).toContain('modelPlaza.card.perUnitImage')
  })
})
