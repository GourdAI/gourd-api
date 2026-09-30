import { describe, expect, it } from 'vitest'
import config from '../../../tailwind.config.js'

/**
 * 铂金黑主题色板闸门。
 *
 * 这些断言刻意不去读生成器的输出，而是直接审计 tailwind.config.js 这个
 * 真正生效的产物 —— 前几轮返工正是因为「脚本自校验通过但产物是 NaN」。
 * 规则与出处见 config 顶部注释块。
 */

type Ramp = Record<string, string>
const colors = (config as { theme: { extend: { colors: Record<string, Ramp> } } }).theme.extend.colors

const STEPS = ['50', '100', '200', '300', '400', '500', '600', '700', '800', '900', '950']
const DECORATIVE = ['sky', 'cyan', 'blue', 'fuchsia', 'violet', 'purple', 'indigo', 'teal']
// teal（内置 #14b8a6，色相 174°，全仓 136 处）也是绿色系，不能当品牌也不能当
// 信息态，归入冷银。它必须单独占一档而不是与 cyan 共用：实测 6 个文件
// 同时引用 teal 与 cyan（useModelWhitelist 100/200/400/700/900、
// AccountStatsModal 100/400/600/900、CreateAccountModal 等）。
const GATED = ['primary', ...DECORATIVE]
// 状态语义色：本轮明确保留彩度，不做收编
const STATUS = ['red', 'amber', 'green', 'emerald']

const bytes = (hex: string) => [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16))
const relLum = (hex: string) => {
  const [r, g, b] = bytes(hex).map((x) => x / 255).map((v) => (v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4)))
  return 0.2126 * r + 0.7152 * g + 0.0722 * b
}
const ratio = (a: string, b: string) => {
  const [x, y] = [relLum(a), relLum(b)].sort((m, n) => n - m)
  return (x + 0.05) / (y + 0.05)
}
/** 8bit 通道极差：>14 就开始被肉眼读成「有色」而不是「灰」 */
const spread = (hex: string) => {
  const v = bytes(hex)
  return Math.max(...v) - Math.min(...v)
}
const hue = (hex: string) => {
  const [r, g, b] = bytes(hex).map((x) => x / 255)
  const mx = Math.max(r, g, b)
  const mn = Math.min(r, g, b)
  const d = mx - mn
  if (!d) return 0
  const raw = mx === r ? ((g - b) / d) % 6 : mx === g ? (b - r) / d + 2 : (r - g) / d + 4
  return (raw * 60 + 360) % 360
}
const isHex = (v: unknown): v is string => /^#[0-9a-f]{6}$/.test(String(v))

describe('platinum palette: structural gates', () => {
  it('declares every ramp in full so no step silently falls back to Tailwind default', () => {
    for (const fam of GATED) {
      const ramp = colors[fam]
      expect(ramp, fam).toBeDefined()
      for (const step of STEPS) {
        expect(isHex(ramp[step]), `${fam}-${step} = ${ramp[step]}`).toBe(true)
      }
    }
  })

  it('C1 is strictly monotonic light->dark', () => {
    for (const fam of GATED) {
      for (let i = 1; i < STEPS.length; i++) {
        const a = relLum(colors[fam][STEPS[i - 1]])
        const b = relLum(colors[fam][STEPS[i]])
        expect(b, `${fam}: ${STEPS[i - 1]} -> ${STEPS[i]} must get darker`).toBeLessThan(a)
      }
    }
  })

  it('C2 keeps brand and decorative colours inside the grey budget', () => {
    // budget is tight on purpose: the site's own greys spread only 2-8
    // (#050607 -> 2, #18181b -> 3, #a3a3ab -> 8). Anything above 6 starts to
    // read as "blue" rather than "cool grey".
    for (const fam of GATED) {
      for (const step of STEPS) {
        expect(spread(colors[fam][step]), `${fam}-${step} spread`).toBeLessThanOrEqual(6)
      }
    }
  })

  it('rejects the NaN class of bug outright', () => {
    // '#NaNNaNNaN' passes >=/< comparisons silently; assert the shape first.
    for (const fam of GATED) {
      for (const step of STEPS) expect(isHex(colors[fam][step]), `${fam}-${step}`).toBe(true)
    }
  })
})

describe('platinum palette: the blue-purple red line', () => {
  it('carries no chromatic blue/indigo/violet in brand or decorative ramps', () => {
    const offenders: string[] = []
    for (const fam of GATED) {
      for (const step of STEPS) {
        const hex = colors[fam][step]
        if (hue(hex) >= 200 && hue(hex) <= 290 && spread(hex) > 8) offenders.push(`${fam}-${step} ${hex} h=${hue(hex).toFixed(0)}`)
      }
    }
    expect(offenders).toEqual([])
  })

  it('never falls back to the built-in chromatic blue/indigo/violet values', () => {
    // Tailwind defaults that used to be reachable through these family names
    const legacy = ['#3b82f6', '#6366f1', '#8b5cf6', '#0ea5e9', '#06b6d4', '#a855f7']
    for (const fam of GATED) {
      for (const step of STEPS) expect(legacy, `${fam}-${step}`).not.toContain(colors[fam][step])
    }
  })

  it('keeps the mint/aurora experiment of the previous round fully gone', () => {
    const source = JSON.stringify(config)
    for (const dead of ['00be93', '22d3a3', '008068', '5ee5ba', '036853', '012b24', '00ae7a', '12b76a']) {
      expect(source.toLowerCase(), `stale mint token #${dead}`).not.toContain(dead)
    }
  })
})

describe('platinum palette: accessibility floors', () => {
  const LIGHT_BG = '#ffffff'
  const DARK_BG = '#0a0a0b'

  it('C3 text steps and white-text fills clear AA on light surfaces', () => {
    for (const fam of GATED) {
      for (const step of STEPS.filter((s) => Number(s) >= 500)) {
        const asText = ratio(colors[fam][step], LIGHT_BG)
        expect(asText, `${fam}-${step} as text on white = ${asText.toFixed(2)}`).toBeGreaterThanOrEqual(4.5)
      }
      // 500 doubles as a fill under white text (102 call sites use bg-primary-500 + text-white)
      const asFill = ratio('#ffffff', colors[fam]['500'])
      expect(asFill, `${fam}-500 under white text = ${asFill.toFixed(2)}`).toBeGreaterThanOrEqual(4.5)
    }
  })

  it('C4 dark-mode text steps clear AA on the near-black page', () => {
    for (const fam of GATED) {
      for (const step of ['200', '300', '400']) {
        const v = ratio(colors[fam][step], DARK_BG)
        expect(v, `${fam}-${step} on ${DARK_BG} = ${v.toFixed(2)}`).toBeGreaterThanOrEqual(4.5)
      }
    }
  })

  it('C5 badge pairings stay readable in both modes', () => {
    for (const fam of GATED) {
      const light = ratio(colors[fam]['600'], colors[fam]['100'])
      expect(light, `${fam} badge 600-on-100 = ${light.toFixed(2)}`).toBeGreaterThanOrEqual(4.5)
      const dark = ratio(colors[fam]['300'], colors[fam]['900'])
      expect(dark, `${fam} badge 300-on-900 = ${dark.toFixed(2)}`).toBeGreaterThanOrEqual(4.5)
    }
  })
})

describe('platinum palette: decorative family separation', () => {
  it('C6 adjacent families differ at every banded step', () => {
    // 500 is intentionally shared across families (it is the fill+white-text slot
    // and sits at the AA floor), so separation lives in 600..950.
    for (let i = 1; i < DECORATIVE.length; i++) {
      for (const step of ['600', '700', '800', '900', '950']) {
        expect(colors[DECORATIVE[i]][step], `${DECORATIVE[i - 1]}/${DECORATIVE[i]} @${step}`).not.toBe(colors[DECORATIVE[i - 1]][step])
      }
    }
  })

  it('does not let the brand alias the palest decorative family', () => {
    for (const step of ['600', '700', '800', '900']) {
      expect(colors.primary[step], `primary/sky @${step}`).not.toBe(colors.sky[step])
    }
  })

  it('teal is distinct from cyan at every banded step (6 files use both)', () => {
    for (const step of ['600', '700', '800', '900', '950']) {
      expect(colors.teal[step], `teal/cyan @${step}`).not.toBe(colors.cyan[step])
    }
  })

  it('shares the light half so badges read as one silver material', () => {
    for (const step of ['50', '100', '200', '300', '400']) {
      const values = new Set(DECORATIVE.map((f) => colors[f][step]))
      expect(values.size, `step ${step} should collapse to one shared value`).toBe(1)
    }
  })

  it('orders families pale -> dark consistently across the banded half', () => {
    const lumAt = (fam: string, step: string) => relLum(colors[fam][step])
    for (const step of ['600', '700', '800']) {
      for (let i = 1; i < DECORATIVE.length; i++) {
        expect(lumAt(DECORATIVE[i], step), `${DECORATIVE[i]} @${step}`).toBeLessThan(lumAt(DECORATIVE[i - 1], step))
      }
    }
  })
})

describe('platinum palette: things this round must not touch', () => {
  it('leaves status semantics untouched so they stay chromatic', () => {
    // A grey success/error state would make the console unreadable, so these
    // families must NOT appear in the override block at all.
    for (const fam of STATUS) {
      expect(colors[fam], `${fam} must not be overridden by the theme`).toBeUndefined()
    }
  })

  it('keeps third-party platform identity colours at their real values', () => {
    expect(colors.brandblue[500]).toBe('#3b82f6') // Gemini
    expect(colors.brandpurple[500]).toBe('#a855f7') // Qoder
    expect(colors.brandcyan[500]).toBe('#06b6d4') // DeepSeek
  })

  it('provides a chromatic namespace for charts, which grey cannot serve', () => {
    for (const fam of ['viz1', 'viz2', 'viz3', 'viz4', 'viz5', 'viz6', 'viz7']) {
      expect(colors[fam], fam).toBeDefined()
      const mid = colors[fam]['500']
      expect(spread(mid), `${fam}-500 must carry visible hue for series separation`).toBeGreaterThan(18)
    }
    // and they must still be muted enough not to look like neon on black
    for (const fam of ['viz1', 'viz2', 'viz3', 'viz4', 'viz5', 'viz6', 'viz7']) {
      expect(spread(colors[fam]['500']), `${fam}-500 spread`).toBeLessThan(120)
    }
  })

  it('drives glow and ambience from light, not pigment', () => {
    const shadows = (config as { theme: { extend: { boxShadow: Record<string, string> } } }).theme.extend.boxShadow
    expect(shadows.glow).toContain('255, 255, 255')
    expect(shadows.glow).not.toContain('0, 190, 147')
    const bg = (config as { theme: { extend: { backgroundImage: Record<string, string> } } }).theme.extend.backgroundImage
    expect(bg['gradient-dark']).toContain('#3f3f46')
    expect(bg['mesh-gradient']).not.toMatch(/rgba\(\s*0\s*,\s*190/)
  })
})

