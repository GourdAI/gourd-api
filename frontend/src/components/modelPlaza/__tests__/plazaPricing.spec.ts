import { describe, expect, it } from 'vitest'
import {
  effectiveGroupRate,
  formatTimeWindow,
  modelPrice,
  modelRateStruck,
  modelRateValue,
  sortModelsForDisplay,
  tierLabel,
  timePeriodHint,
  type PlazaPriceContext,
  type PriceLabels
} from '../plazaPricing'
import { BILLING_MODE_IMAGE, BILLING_MODE_PER_REQUEST, BILLING_MODE_TOKEN } from '@/constants/channel'
import type { PlazaModel } from '@/api/modelPlaza'

const labels: PriceLabels = {
  cacheWrite: 'W',
  cacheRead: 'R',
  perRequest: '/ 次',
  perImage: '/ 张'
}

function ctx(overrides: Partial<PlazaPriceContext> = {}): PlazaPriceContext {
  return { rateMultiplier: 1, ...overrides }
}

function tokenModel(overrides: Partial<PlazaModel> = {}): PlazaModel {
  return {
    name: 'claude-sonnet',
    platform: 'anthropic',
    pricing: {
      billing_mode: BILLING_MODE_TOKEN,
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

/** 取某价格列的实付文本(拼接行内片段)。 */
function paidText(display: ReturnType<typeof modelPrice>, key: 'input' | 'output' | 'cache'): string[] {
  const cell = display.cells.find((c) => c.key === key)!
  return cell.paid.map((line) => line.parts.map((p) => p.text).join(''))
}

describe('plazaPricing 生效倍率', () => {
  it('专属倍率优先于分组默认倍率', () => {
    expect(effectiveGroupRate(ctx({ rateMultiplier: 1, userRateMultiplier: 0.8 }))).toBe(0.8)
    expect(effectiveGroupRate(ctx({ rateMultiplier: 1.2 }))).toBe(1.2)
  })

  it('专属倍率场景划线展示原倍率;无专属或非 token 独立倍率时为 null', () => {
    const model = tokenModel()
    expect(modelRateStruck(model, ctx({ rateMultiplier: 1, userRateMultiplier: 0.8 }))).toBe(1)
    expect(modelRateStruck(model, ctx({ rateMultiplier: 1 }))).toBeNull()
  })

  it('token 模型倍率取生效倍率,按图模型在独立倍率开启时取独立倍率', () => {
    expect(modelRateValue(tokenModel(), ctx({ rateMultiplier: 0.5 }))).toBe(0.5)
    const image = tokenModel({
      pricing: {
        billing_mode: BILLING_MODE_IMAGE,
        input_price: null,
        output_price: null,
        cache_write_price: null,
        cache_read_price: null,
        image_input_price: null,
        image_output_price: null,
        per_request_price: 0.2,
        intervals: []
      },
      official_pricing: null
    })
    expect(modelRateValue(image, ctx({ rateMultiplier: 0.1, imageRateIndependent: true, imageRateMultiplier: 1 }))).toBe(1)
    // 独立倍率关闭时回到分组生效倍率,且不划线
    const plain = ctx({ rateMultiplier: 0.1, imageRateIndependent: false })
    expect(modelRateValue(image, plain)).toBe(0.1)
    expect(modelRateStruck(image, ctx({ rateMultiplier: 0.1, userRateMultiplier: 0.05, imageRateIndependent: true, imageRateMultiplier: 2 }))).toBeNull()
  })
})

describe('plazaPricing token 价格', () => {
  it('倍率 1 时展示渠道单价原值($/1M),保底 2 位小数', () => {
    const display = modelPrice(tokenModel(), ctx(), labels)
    expect(paidText(display, 'input')).toEqual(['$3.00'])
    expect(paidText(display, 'output')).toEqual(['$15.00'])
    // 缓存拆「写 / 读」两行,超过 2 位小数的原样保留
    expect(paidText(display, 'cache')).toEqual(['W $3.75', 'R $0.30'])
  })

  it('倍率 ≠ 1 时实付按折后价,官方参考价作为副行同时展示', () => {
    const display = modelPrice(tokenModel(), ctx({ rateMultiplier: 0.5 }), labels)
    expect(paidText(display, 'input')).toEqual(['$1.50'])
    expect(paidText(display, 'output')).toEqual(['$7.50'])
    const inputCell = display.cells.find((c) => c.key === 'input')!
    expect(inputCell.official).not.toBeNull()
    expect(inputCell.official!.map((l) => l.parts.map((p) => p.text).join(''))).toEqual(['$3.00'])
  })

  it('实付与官方逐字一致时不重复展示官方参考行', () => {
    const display = modelPrice(tokenModel(), ctx(), labels)
    expect(display.cells.find((c) => c.key === 'input')!.official).toBeNull()
    expect(display.cells.find((c) => c.key === 'output')!.official).toBeNull()
    // 缓存列官方含 1h 价,与实付不同 → 仍有副行
    expect(display.cells.find((c) => c.key === 'cache')!.official).not.toBeNull()
  })

  it('official_pricing 缺失时不渲染官方参考行', () => {
    const display = modelPrice(tokenModel({ official_pricing: null }), ctx({ rateMultiplier: 0.5 }), labels)
    expect(display.cells.every((c) => c.official === null)).toBe(true)
  })

  it('实付价支持自定义 1h 缓存写入价', () => {
    const model = tokenModel()
    model.pricing!.cache_write_1h_price = 7e-6
    const display = modelPrice(model, ctx(), labels)
    expect(paidText(display, 'cache')).toEqual(['W $3.75 (1h $7.00)', 'R $0.30'])
  })

  it('阶梯定价按档分行,档位标签与行对齐;档数记录在 tierCount', () => {
    const model = tokenModel({
      pricing: {
        billing_mode: BILLING_MODE_TOKEN,
        input_price: 3e-6,
        output_price: 1.5e-5,
        cache_write_price: null,
        cache_read_price: null,
        image_input_price: null,
        image_output_price: null,
        per_request_price: null,
        intervals: [
          {
            min_tokens: 200000,
            max_tokens: null,
            tier_label: '',
            input_price: 6e-6,
            output_price: 3e-5,
            cache_write_price: null,
            cache_read_price: null,
            per_request_price: null
          },
          {
            min_tokens: 0,
            max_tokens: 200000,
            tier_label: '',
            input_price: 3e-6,
            output_price: 1.5e-5,
            cache_write_price: null,
            cache_read_price: null,
            per_request_price: null
          }
        ]
      }
    })
    const display = modelPrice(model, ctx({ rateMultiplier: 0.5 }), labels)
    const input = display.cells.find((c) => c.key === 'input')!
    // 档位按下限升序,折后 1.5 / 3
    expect(input.paid.map((l) => `${l.tier} ${l.parts.map((p) => p.text).join('')}`)).toEqual([
      '≤200K $1.50',
      '>200K $3.00'
    ])
    expect(display.tierCount).toBe(2)
  })

  it('仅配置区间倍率时按基础价折算各档单价', () => {
    const model = tokenModel({
      pricing: {
        billing_mode: BILLING_MODE_TOKEN,
        input_price: 10e-6,
        output_price: 50e-6,
        cache_write_price: 12.5e-6,
        cache_write_1h_price: 12.5e-6,
        cache_read_price: 2e-6,
        image_input_price: null,
        image_output_price: null,
        per_request_price: null,
        intervals: [
          {
            min_tokens: 272000,
            max_tokens: null,
            tier_label: '>272K',
            input_price: null,
            output_price: null,
            cache_write_price: null,
            cache_write_1h_price: null,
            cache_read_price: null,
            input_multiplier: 2,
            output_multiplier: 1.5,
            cache_write_multiplier: 2,
            cache_read_multiplier: 2,
            per_request_price: null
          }
        ]
      },
      official_pricing: null
    })
    const display = modelPrice(model, ctx(), labels)
    expect(paidText(display, 'input')).toEqual(['$20.00'])
    expect(paidText(display, 'output')).toEqual(['$75.00'])
    expect(paidText(display, 'cache')).toEqual(['W $25.00 (1h $25.00) R $4.00'])
  })

  it('阶梯缓存价每档一行「写 x 读 z」,与输入/输出列行对齐', () => {
    const intervals = [
      {
        min_tokens: 0,
        max_tokens: 272000,
        tier_label: '≤272K',
        input_price: 5e-6,
        output_price: 3e-5,
        cache_write_price: 6.25e-6,
        cache_read_price: 5e-7,
        per_request_price: null
      },
      {
        min_tokens: 272000,
        max_tokens: null,
        tier_label: '>272K',
        input_price: 1e-5,
        output_price: 4.5e-5,
        cache_write_price: 1.25e-5,
        cache_read_price: 1e-6,
        per_request_price: null
      }
    ]
    const model = tokenModel({
      pricing: {
        billing_mode: BILLING_MODE_TOKEN,
        input_price: 5e-6,
        output_price: 3e-5,
        cache_write_price: 6.25e-6,
        cache_read_price: 5e-7,
        image_input_price: null,
        image_output_price: null,
        per_request_price: null,
        intervals
      },
      official_pricing: {
        input_price: 5e-6,
        output_price: 3e-5,
        cache_write_price: 6.25e-6,
        cache_read_price: 5e-7,
        intervals
      },
      long_context_basis: 'whole_request'
    })
    const display = modelPrice(model, ctx({ rateMultiplier: 0.5 }), labels)
    const cache = display.cells.find((c) => c.key === 'cache')!
    // 阶梯行经 resolveIntervalPrices 后 1h 写入价回落为 5m 写入价
    expect(cache.paid.map((l) => l.parts.map((p) => p.text).join(''))).toEqual([
      'W $3.125 (1h $3.125) R $0.25',
      'W $6.25 (1h $6.25) R $0.50'
    ])
    // 官方阶梯不走倍率折算,也不补 1h 价
    expect(cache.official!.map((l) => l.parts.map((p) => p.text).join(''))).toEqual([
      'W $6.25 R $0.50',
      'W $12.50 R $1.00'
    ])
    // 官方阶梯不乘倍率,档数与实付一致
    expect(display.cells.find((c) => c.key === 'input')!.official!.map((l) => l.tier)).toEqual(['≤272K', '>272K'])
  })
})

describe('plazaPricing 按次 / 按图计费', () => {
  function requestModel(price: number | null, intervals: PlazaModel['pricing'] extends null ? never : any[] = []) {
    return tokenModel({
      name: 'gpt-image-2',
      pricing: {
        billing_mode: BILLING_MODE_IMAGE,
        input_price: null,
        output_price: null,
        cache_write_price: null,
        cache_read_price: null,
        image_input_price: null,
        image_output_price: 3e-5,
        per_request_price: price,
        intervals
      },
      official_pricing: null
    })
  }

  it('平铺按次价 × 倍率展示,不进 token 三列', () => {
    const display = modelPrice(requestModel(0.2), ctx({ rateMultiplier: 0.1 }), labels)
    expect(display.isToken).toBe(false)
    expect(display.cells).toEqual([])
    expect(display.requests).toEqual([{ price: '$0.02' }])
  })

  it('多档按图价逐档展示,不把每 token 图片输出价当按次单价', () => {
    const display = modelPrice(
      requestModel(null, [
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
      ]),
      ctx({ rateMultiplier: 0.1 }),
      labels
    )
    expect(display.requests).toEqual([
      { tier: '1K', price: '$0.001' },
      { tier: '2K', price: '$0.002' }
    ])
  })

  it('生图独立倍率开启时按独立倍率计价,不受分组倍率影响', () => {
    const display = modelPrice(
      requestModel(0.02),
      ctx({ rateMultiplier: 0.1, imageRateIndependent: true, imageRateMultiplier: 1 }),
      labels
    )
    expect(display.requests).toEqual([{ price: '$0.02' }])
  })

  it('未配价格时返回空档位列表(渲染为占位)', () => {
    const display = modelPrice(
      requestModel(null),
      ctx({ rateMultiplier: 0.1 }),
      labels
    )
    expect(display.requests).toEqual([])
  })

  it('按次计费模型走同一分支', () => {
    const display = modelPrice(
      tokenModel({
        name: 'search-tool',
        pricing: {
          billing_mode: BILLING_MODE_PER_REQUEST,
          input_price: null,
          output_price: null,
          cache_write_price: null,
          cache_read_price: null,
          image_input_price: null,
          image_output_price: null,
          per_request_price: 0.04,
          intervals: []
        },
        official_pricing: null
      }),
      ctx({ rateMultiplier: 0.5 }),
      labels
    )
    expect(display.billingMode).toBe(BILLING_MODE_PER_REQUEST)
    expect(display.requests).toEqual([{ price: '$0.02' }])
  })
})

describe('plazaPricing 分时倍率', () => {
  function timePriced() {
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

  it('时段价格 = 单价 × 生效倍率 × 时段倍率;整分钟省略秒', () => {
    const model = timePriced()
    expect(formatTimeWindow(model.time_pricing!.periods[0])).toBe('00:30–08:30')
    expect(formatTimeWindow(model.time_pricing!.periods[1])).toBe('18:00–22:00')

    const ctxIn = ctx({ rateMultiplier: 0.8 })
    const night = modelPrice(model, ctxIn, labels, model.time_pricing!.periods[0])
    expect(paidText(night, 'input')).toEqual(['$1.20'])
    expect(paidText(night, 'output')).toEqual(['$6.00'])
    // 官方参考价不受时段影响
    expect(night.cells.find((c) => c.key === 'input')!.official).not.toBeNull()
  })

  it('时段生效倍率去掉浮点噪声', () => {
    const model = timePriced()
    const period = model.time_pricing!.periods[1]
    const display = modelPrice(model, ctx({ rateMultiplier: 0.8 }), labels, period)
    // 0.8 × 1.2 = 0.96 而不是 0.9600000000000001
    expect(paidText(display, 'input')).toEqual(['$2.88'])
  })

  it('时段 tooltip 按 weekdays_only 换文案,并追加分时披露', () => {
    const translate = (key: string) => `[${key}]`
    const model = timePriced()
    expect(timePeriodHint(model, ctx(), translate)).toBe('[modelPlaza.card.timePeriodHint]')
    model.time_pricing!.weekdays_only = true
    expect(timePeriodHint(model, ctx(), translate)).toBe('[modelPlaza.card.timePeriodHintWeekdays]')
    expect(
      timePeriodHint(tokenModel(), ctx({ peakWindow: '14:00-18:00 ×1.5' }), translate)
    ).toContain('modelPlaza.card.timePeriodHint')
  })
})

describe('plazaPricing 排序与档位标签', () => {
  it('token 在前并按官方输出价降序,无官方价排最后,同价按名称降序', () => {
    const expensive = tokenModel({
      name: 'model-expensive',
      official_pricing: {
        input_price: 1e-5,
        output_price: 7.5e-5,
        cache_write_price: null,
        cache_read_price: null
      }
    })
    const cheap = tokenModel({
      name: 'model-cheap',
      official_pricing: {
        input_price: 1e-6,
        output_price: 5e-6,
        cache_write_price: null,
        cache_read_price: null
      }
    })
    const noOfficial = tokenModel({ name: 'model-no-official', official_pricing: null })
    const image = tokenModel({
      name: 'gpt-image-2',
      pricing: {
        billing_mode: BILLING_MODE_IMAGE,
        input_price: null,
        output_price: null,
        cache_write_price: null,
        cache_read_price: null,
        image_input_price: null,
        image_output_price: null,
        per_request_price: 0.04,
        intervals: []
      },
      official_pricing: {
        input_price: 5e-6,
        output_price: 1e-4,
        cache_write_price: null,
        cache_read_price: null
      }
    })

    expect(sortModelsForDisplay([cheap, image, noOfficial, expensive]).map((m) => m.name)).toEqual([
      'model-expensive',
      'model-cheap',
      'model-no-official',
      'gpt-image-2'
    ])
  })

  it('同官方价按模型名降序(新版本号在前)', () => {
    expect(
      sortModelsForDisplay([tokenModel({ name: 'gpt-5.5' }), tokenModel({ name: 'gpt-5.6-sol' })]).map((m) => m.name)
    ).toEqual(['gpt-5.6-sol', 'gpt-5.5'])
  })

  it('无 tier_label 时按区间生成统一形态', () => {
    const base = {
      tier_label: '',
      input_price: 1e-6,
      output_price: 1e-6,
      cache_write_price: null,
      cache_read_price: null,
      per_request_price: null
    }
    expect(tierLabel({ ...base, min_tokens: 0, max_tokens: 100000 })).toBe('≤100K')
    expect(tierLabel({ ...base, min_tokens: 200000, max_tokens: 1000000 })).toBe('≤1M')
    expect(tierLabel({ ...base, min_tokens: 1000000, max_tokens: null })).toBe('>1M')
    expect(tierLabel({ ...base, tier_label: '1K', min_tokens: 0, max_tokens: null })).toBe('1K')
  })
})
