import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import ModelPlazaContent from '../ModelPlazaContent.vue'
import type { ModelPlazaGroup, ModelPlazaResponse, PlazaModel } from '@/api/modelPlaza'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ isAuthenticated: true })
}))

const cachedPublicSettings = vi.fn(() => undefined)
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ get cachedPublicSettings() { return cachedPublicSettings() } })
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copied: { value: false }, copyToClipboard: vi.fn() })
}))

function tokenModel(name: string, input: number | null, platform = 'openai'): PlazaModel {
  return {
    name,
    platform,
    pricing: {
      billing_mode: 'token',
      input_price: input,
      output_price: input == null ? null : input * 5,
      cache_write_price: null,
      cache_read_price: null,
      image_input_price: null,
      image_output_price: null,
      per_request_price: null,
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

function response(groups: ModelPlazaGroup[]): ModelPlazaResponse {
  return { groups, description: '' } as ModelPlazaResponse
}

function mountContent(res: ModelPlazaResponse | null) {
  return mount(ModelPlazaContent, {
    props: { response: res, loading: false, embedded: true },
    global: { stubs: { Icon: true, ModelIcon: true, GroupBadge: true } }
  })
}

/** 网格里的卡片标题（= 模型名）。 */
function cardNames(wrapper: ReturnType<typeof mountContent>): string[] {
  return wrapper.findAll('article h4').map((h) => h.text())
}

/** 按模型名定位卡片。 */
function cardOf(wrapper: ReturnType<typeof mountContent>, name: string) {
  const card = wrapper.findAll('article').find((a) => a.find('h4').text() === name)
  if (!card) throw new Error(`卡片未渲染: ${name}（实际: ${cardNames(wrapper).join(', ')}）`)
  return card
}

/** 某张卡片底部的分组 chip 文本。 */
function chipsOf(card: ReturnType<typeof cardOf>): string[] {
  return card.findAll('footer button').map((b) => b.text())
}

/** 点击某张卡片第 n 个分组 chip。 */
async function chipsTrigger(wrapper: ReturnType<typeof mountContent>, name: string, idx: number) {
  await cardOf(wrapper, name).findAll('footer button')[idx].trigger('click')
}

beforeEach(() => {
  cachedPublicSettings.mockClear()
})

const twoGroups = () => [
  group(1, '标准组', [tokenModel('gpt-5.6', 3e-6)]),
  group(2, 'VIP 组', [tokenModel('gpt-5.6', 3e-6), tokenModel('only-vip', 1e-5)], 0.5)
]

describe('ModelPlazaContent 模型维度渲染', () => {
  it('同名模型跨分组只渲染一张卡片,底部列出两个分组与倍率', () => {
    const wrapper = mountContent(response(twoGroups()))
    // 默认按实付输入价降序:only-vip 输入 10 > gpt-5.6 输入 3
    expect(cardNames(wrapper)).toEqual(['only-vip', 'gpt-5.6'])
    expect(chipsOf(cardOf(wrapper, 'gpt-5.6'))).toEqual(['标准组1x', 'VIP 组0.5x'])
  })

  it('点击卡片分组标签 → 整页切到该分组价格(再点一次回到「全部」)', async () => {
    const wrapper = mountContent(response(twoGroups()))
    await chipsTrigger(wrapper, 'gpt-5.6', 0)
    // 锁定标准组:gpt-5.6 改按 1x 计价,VIP 独有模型被筛掉
    expect(cardNames(wrapper)).toEqual(['gpt-5.6'])
    expect(cardOf(wrapper, 'gpt-5.6').findAll('footer button')[0].attributes('aria-pressed')).toBe('true')
    expect(wrapper.find('.plaza-group-panel').exists()).toBe(true)
    // 再点一次回到「全部」:VIP 独有模型回来,分组面板收起(无高峰/阶梯说明)
    await chipsTrigger(wrapper, 'gpt-5.6', 0)
    expect(cardNames(wrapper)).toEqual(['only-vip', 'gpt-5.6'])
    expect(wrapper.find('.plaza-group-panel').exists()).toBe(false)
  })

  it('选中分组时说明区展示该分组自身信息(不依赖是否有高峰/阶梯说明)', async () => {
    const wrapper = mountContent(response(twoGroups()))
    // fixture 未配高峰也未关阶梯 → groupNotes 为空,但分组信息仍该在
    expect(wrapper.text()).not.toContain('modelPlaza.detail.peakNote')
    await chipsTrigger(wrapper, 'gpt-5.6', 0)
    expect(wrapper.find('.plaza-group-panel').exists()).toBe(true)
    expect(wrapper.find('.plaza-group-panel').html()).toContain('group-badge')
  })

  it('刷新后原选中分组消失时回落「全部」,不会整页空白', async () => {
    const wrapper = mountContent(response(twoGroups()))
    await chipsTrigger(wrapper, 'gpt-5.6', 1)
    expect(wrapper.findAll('article').length).toBeGreaterThan(0)
    // 管理员删掉了 VIP 组
    await wrapper.setProps({ response: response([group(1, '标准组', [tokenModel('gpt-5.6', 3e-6)])]) })
    expect(cardNames(wrapper)).toEqual(['gpt-5.6'])
    expect(wrapper.text()).not.toContain('modelPlaza.empty')
  })

  it('搜索无结果时给出搜索专用空态', async () => {
    const wrapper = mountContent(response(twoGroups()))
    await wrapper.find('input[type="text"]').setValue('不存在的模型')
    expect(wrapper.findAll('article')).toHaveLength(0)
    expect(wrapper.text()).toContain('modelPlaza.noSearchResult')
  })

  it('分组 models 缺失时不抛错', () => {
    const broken = { ...group(1, '坏组', []), models: undefined } as unknown as ModelPlazaGroup
    expect(() => mountContent(response([broken]))).not.toThrow()
  })
})
