import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

describe('Composite channel platform options', () => {
  it('includes the CN concrete providers for pricing and model mapping', () => {
    const source = readFileSync(resolve('src/views/admin/ChannelsView.vue'), 'utf8')
    const declaration = source.match(/const compositePlatforms:[^=]+=[^\n]+/)?.[0]

    expect(declaration).toContain("'kimi'")
    expect(declaration).toContain("'zhipu'")
    expect(declaration).toContain("'deepseek'")
    expect(declaration).toContain("'minimax'")
    expect(declaration).toContain("'opencode_go'")
    expect(declaration).toContain("'workbuddy'")
    expect(declaration).toContain("'trae'")
  })

  // Qoder 定价/映射缺口回归（2026-09-22）：渠道定价页的 platformOrder 与
  // compositePlatforms 曾漏配 qoder，导致：a) UI 无法为 Qoder 配置渠道级
  // model_pricing / model_mapping；b) 存量 platform=qoder 条目在保存渠道时
  // 被 formToAPI 静默抹掉。两个数组都必须包含 qoder，任何收敛都会在此报警。
  it('includes qoder in both platformOrder and compositePlatforms', () => {
    const source = readFileSync(resolve('src/views/admin/ChannelsView.vue'), 'utf8')
    const orderDeclaration = source.match(/const platformOrder:[^=]+=[^\n]+/)?.[0]
    const compositeDeclaration = source.match(/const compositePlatforms:[^=]+=[^\n]+/)?.[0]

    expect(orderDeclaration).toContain("'qoder'")
    expect(compositeDeclaration).toContain("'qoder'")
  })

  // Trae 与 qoder 同险（2026-10）：platformOrder / compositePlatforms 漏配会导致
  // 无法为 Trae 配置渠道级定价/映射，且存量 platform=trae 条目在保存时被静默抹掉。
  it('includes trae in both platformOrder and compositePlatforms', () => {
    const source = readFileSync(resolve('src/views/admin/ChannelsView.vue'), 'utf8')
    const orderDeclaration = source.match(/const platformOrder:[^=]+=[^\n]+/)?.[0]
    const compositeDeclaration = source.match(/const compositePlatforms:[^=]+=[^\n]+/)?.[0]

    expect(orderDeclaration).toContain("'trae'")
    expect(compositeDeclaration).toContain("'trae'")
  })
})
