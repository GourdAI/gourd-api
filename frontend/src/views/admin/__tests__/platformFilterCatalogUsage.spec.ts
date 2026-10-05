import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

function readSource(path: string): string {
  return readFileSync(resolve(path), 'utf8')
}

describe('admin platform filters', () => {
  it('does not filter subscriptions by group platform (subscriptions are group-free wallets)', () => {
    // 订阅 = 个人额度钱包，不绑定分组，因此订阅页不再提供分组/平台筛选。
    // 旧用例断言本页使用 GROUP_PLATFORM_OPTIONS，该断言随「订阅不绑分组」退役。
    const source = readSource('src/views/admin/SubscriptionsView.vue')
    expect(source).not.toContain('GROUP_PLATFORM_OPTIONS')
    expect(source).not.toContain('platform')
  })

  it('uses the shared catalogs on the groups page', () => {
    const source = readSource('src/views/admin/GroupsView.vue')
    expect(source).toContain('...GROUP_PLATFORM_OPTIONS')
    expect(source).toContain('...CONCRETE_PLATFORM_OPTIONS')
  })

  it('uses the concrete platform catalog wherever concrete platforms are selected', () => {
    for (const path of [
      'src/components/admin/account/AccountTableFilters.vue',
      'src/components/admin/ErrorPassthroughRulesModal.vue',
      'src/views/admin/ops/components/OpsDashboardHeader.vue'
    ]) {
      const source = readSource(path)
      expect(source).toContain("import { CONCRETE_PLATFORM_OPTIONS } from '@/constants/platforms'")
      expect(source).toMatch(/platformOptions\s*=.*CONCRETE_PLATFORM_OPTIONS|pOpts.*\.\.\.CONCRETE_PLATFORM_OPTIONS/s)
    }
  })
})
