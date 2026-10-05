import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'

import KeyUsageView from '../KeyUsageView.vue'

const { showInfo, showSuccess, showError, fetchPublicSettings } = vi.hoisted(() => ({
  showInfo: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn(),
  fetchPublicSettings: vi.fn(),
}))

const messages: Record<string, string> = {
  'keyUsage.title': 'API Key Usage',
  'keyUsage.subtitle': 'Usage status',
  'keyUsage.placeholder': 'sk-test',
  'keyUsage.query': 'Query',
  'keyUsage.querying': 'Querying...',
  'keyUsage.privacyNote': 'Privacy note',
  'keyUsage.dateRange': 'Date Range:',
  'keyUsage.dateRangeToday': 'Today',
  'keyUsage.dateRange7d': '7 Days',
  'keyUsage.dateRange30d': '30 Days',
  'keyUsage.dateRange90d': '90 Days',
  'keyUsage.dateRangeCustom': 'Custom',
  'keyUsage.apply': 'Apply',
  'keyUsage.used': 'Used',
  'keyUsage.detailInfo': 'Detail Information',
  'keyUsage.tokenStats': 'Token Statistics',
  'keyUsage.dailyDetail': 'Daily Detail',
  'keyUsage.date': 'Date',
  'keyUsage.requests': 'Requests',
  'keyUsage.inputTokens': 'Input Tokens',
  'keyUsage.outputTokens': 'Output Tokens',
  'keyUsage.cacheReadTokens': 'Cache Read',
  'keyUsage.cacheWriteTokens': 'Cache Write',
  'keyUsage.cost': 'Cost',
  'keyUsage.quotaMode': 'Key Quota Mode',
  'keyUsage.walletBalance': 'Wallet Balance',
  'keyUsage.totalQuota': 'Total Quota',
  'keyUsage.limit5h': '5-Hour Limit',
  'keyUsage.limitDaily': 'Daily Limit',
  'keyUsage.limit7d': '7-Day Limit',
  'keyUsage.limitWeekly': 'Weekly Limit',
  'keyUsage.limitMonthly': 'Monthly Limit',
  'keyUsage.remainingQuota': 'Remaining Quota',
  'keyUsage.usedQuota': 'Used Quota',
  'keyUsage.subscriptionType': 'Subscription Type',
  'keyUsage.billingType': 'Billing Type',
  'keyUsage.subscriptionExpires': 'Subscription Expires',
  'keyUsage.unlimited': 'Unlimited',
  'keyUsage.subscriptionCount': 'Active Subscriptions',
  'keyUsage.todayRequests': 'Today Requests',
  'keyUsage.todayInputTokens': 'Today Input',
  'keyUsage.todayOutputTokens': 'Today Output',
  'keyUsage.todayTokens': 'Today Tokens',
  'keyUsage.todayCacheCreation': 'Today Cache Creation',
  'keyUsage.todayCacheRead': 'Today Cache Read',
  'keyUsage.todayCost': 'Today Cost',
  'keyUsage.rpmTpm': 'RPM / TPM',
  'keyUsage.totalRequests': 'Total Requests',
  'keyUsage.totalInputTokens': 'Total Input',
  'keyUsage.totalOutputTokens': 'Total Output',
  'keyUsage.totalTokensLabel': 'Total Tokens',
  'keyUsage.totalCacheCreation': 'Total Cache Creation',
  'keyUsage.totalCacheRead': 'Total Cache Read',
  'keyUsage.totalCost': 'Total Cost',
  'keyUsage.avgDuration': 'Avg Duration',
  'keyUsage.querySuccess': 'Query successful',
  'keyUsage.queryFailed': 'Query failed',
  'keyUsage.queryFailedRetry': 'Query failed, please try again later',
  'home.viewDocs': 'Docs',
  'home.switchToLight': 'Light',
  'home.switchToDark': 'Dark',
  'home.footer.allRightsReserved': 'All rights reserved.',
}

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => messages[key] ?? key,
      locale: { value: 'en' },
    }),
  }
})

const appStoreState = vi.hoisted(() => ({
  cachedPublicSettings: null as Record<string, unknown> | null,
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({
    get cachedPublicSettings() {
      return appStoreState.cachedPublicSettings
    },
    siteName: 'Sub2API',
    siteLogo: '',
    docUrl: '',
    publicSettingsLoaded: true,
    fetchPublicSettings,
    showInfo,
    showSuccess,
    showError,
  }),
}))

describe('KeyUsageView daily detail', () => {
  beforeEach(() => {
    showInfo.mockReset()
    showSuccess.mockReset()
    showError.mockReset()
    fetchPublicSettings.mockReset()
    localStorage.clear()

    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      value: vi.fn().mockReturnValue({ matches: false }),
    })
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => window.setTimeout(() => cb(0), 0))
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({
        mode: 'quota_limited',
        isValid: true,
        status: 'active',
        quota: {
          limit: 10,
          used: 1,
          remaining: 9,
          unit: 'USD',
        },
        usage: {
          today: {
            requests: 1,
            input_tokens: 10,
            output_tokens: 20,
            cache_creation_tokens: 0,
            cache_read_tokens: 0,
            total_tokens: 30,
            actual_cost: 0.01,
          },
          total: {
            requests: 12,
            input_tokens: 100,
            output_tokens: 200,
            cache_creation_tokens: 10,
            cache_read_tokens: 30,
            total_tokens: 340,
            actual_cost: 0.12,
          },
          rpm: 0,
          tpm: 0,
        },
        daily_usage: [
          {
            date: '2026-05-19',
            requests: 12,
            input_tokens: 100,
            output_tokens: 200,
            cache_read_tokens: 30,
            cache_write_tokens: 10,
            total_tokens: 340,
            cost: 0.15,
            actual_cost: 0.12,
          },
        ],
      }),
    }))
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it('renders daily usage detail rows after a successful query', async () => {
    const wrapper = mount(KeyUsageView, {
      global: {
        stubs: {
          RouterLink: { template: '<a><slot /></a>' },
          LocaleSwitcher: true,
          Icon: true,
        },
      },
    })

    await wrapper.find('input').setValue('tk-test-key')
    await wrapper.find('input').trigger('keydown.enter')
    await flushPromises()
    await nextTick()

    const fetchMock = vi.mocked(fetch)
    expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining('/v1/usage?'),
      expect.objectContaining({
        headers: { Authorization: 'Bearer tk-test-key' },
      })
    )
    expect(String(fetchMock.mock.calls[0][0])).toContain('days=30')

    const text = wrapper.text()
    expect(text).toContain('Daily Detail')
    expect(text).toContain('Date')
    expect(text).toContain('Cache Read')
    expect(text).toContain('Cache Write')
    expect(text).toContain('2026-05-19')
    expect(text).toContain('12')
    expect(text).toContain('100')
    expect(text).toContain('200')
    expect(text).toContain('30')
    expect(text).toContain('10')
    expect(text).toContain('$0.12')

    wrapper.unmount()
  })

  it('queries the current local calendar date near midnight', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date(2026, 6, 13, 0, 30))

    const wrapper = mount(KeyUsageView, {
      global: {
        stubs: {
          RouterLink: { template: '<a><slot /></a>' },
          LocaleSwitcher: true,
          Icon: true,
        },
      },
    })

    await wrapper.find('input').setValue('tk-test-key')
    await wrapper.find('input').trigger('keydown.enter')
    await flushPromises()

    const requestUrl = String(vi.mocked(fetch).mock.calls[0][0])
    expect(requestUrl).toContain('start_date=2026-07-13')
    expect(requestUrl).toContain('end_date=2026-07-13')

    wrapper.unmount()
  })
})

describe('KeyUsageView subscription feature flag', () => {
  beforeEach(() => {
    localStorage.clear()
    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      value: vi.fn().mockReturnValue({ matches: false }),
    })
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => window.setTimeout(() => cb(0), 0))
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({
        mode: 'wallet',
        isValid: true,
        status: 'active',
        balance: 5.5,
        usage: {
          today: { requests: 0, input_tokens: 0, output_tokens: 0, cache_creation_tokens: 0, cache_read_tokens: 0, total_tokens: 0, actual_cost: 0 },
          total: { requests: 0, input_tokens: 0, output_tokens: 0, cache_creation_tokens: 0, cache_read_tokens: 0, total_tokens: 0, actual_cost: 0 },
          rpm: 0,
          tpm: 0,
        },
        daily_usage: [],
      }),
    }))
  })

  afterEach(() => {
    appStoreState.cachedPublicSettings = null
    vi.unstubAllGlobals()
  })

  async function mountAndQuery() {
    const wrapper = mount(KeyUsageView, {
      global: {
        stubs: {
          RouterLink: { template: '<a><slot /></a>' },
          LocaleSwitcher: true,
          Icon: true,
        },
      },
    })
    await wrapper.find('input').setValue('tk-test-key')
    await wrapper.find('input').trigger('keydown.enter')
    await flushPromises()
    await nextTick()
    return wrapper
  }

  it('labels the wallet row "Subscription Type" while subscriptions are enabled', async () => {
    const wrapper = await mountAndQuery()

    expect(wrapper.text()).toContain('Subscription Type')
    expect(wrapper.text()).toContain('Wallet Balance')
    wrapper.unmount()
  })

  it('drops the "Subscription" wording from the wallet row when subscriptions are disabled', async () => {
    appStoreState.cachedPublicSettings = { subscription_enabled: false }
    const wrapper = await mountAndQuery()

    expect(wrapper.text()).toContain('Billing Type')
    expect(wrapper.text()).not.toContain('Subscription Type')
    expect(wrapper.text()).toContain('Wallet Balance')
    wrapper.unmount()
  })
})

// 订阅钱包分支在 2026-10-04 之前零覆盖：后端已改为单一总额池（六键），
// 而前端仍在读 daily/weekly/monthly_* 退役字段，导致额度区整块空白且测试全绿。
describe('KeyUsageView subscription wallet rendering', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  async function mountWith(payload: Record<string, unknown>) {
    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      value: vi.fn().mockReturnValue({ matches: false }),
    })
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => window.setTimeout(() => cb(0), 0))
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, json: async () => payload }))

    const wrapper = mount(KeyUsageView, {
      global: { stubs: { RouterLink: { template: '<a><slot /></a>' }, LocaleSwitcher: true, Icon: true } },
    })
    await wrapper.find('input').setValue('tk-test-key')
    await wrapper.find('input').trigger('keydown.enter')
    await flushPromises()
    await nextTick()
    return wrapper
  }

  const base = {
    mode: 'unrestricted',
    isValid: true,
    status: 'active',
    // 后端 subscriptionWalletPlanName 在钱包模式下恒非空（兜底「个人订阅」），
    // 这里必须给上，否则前端会回落到「钱包余额」字样，测出来的是假现象。
    planName: 'Pro Wallet',
    usage: {
      today: { requests: 0, input_tokens: 0, output_tokens: 0, cache_creation_tokens: 0, cache_read_tokens: 0, total_tokens: 0, actual_cost: 0 },
      total: { requests: 0, input_tokens: 0, output_tokens: 0, cache_creation_tokens: 0, cache_read_tokens: 0, total_tokens: 0, actual_cost: 0 },
      rpm: 0,
      tpm: 0,
    },
    daily_usage: [],
  }

  it('renders the aggregated total pool from wallet keys', async () => {
    const wrapper = await mountWith({
      ...base,
      subscription: {
        subscription_count: 2,
        total_limit_usd: 10,
        total_usage_usd: 2.5,
        remaining_usd: 7.5,
        has_unlimited: false,
        expires_at: '2026-12-31T00:00:00Z',
      },
    })

    const text = wrapper.text()
    expect(text).toContain('Total Quota')
    // 可证伪的具体数字：用量/总额直接来自 total_usage_usd / total_limit_usd
    expect(text).toContain('$2.50 / $10.00')
    // 多份订阅时给出份数，证明读的是 subscription_count
    expect(text).toContain('Active Subscriptions')
    // 退役字段不得再出现在任何判据里
    expect(text).not.toContain('Daily Limit')
    expect(text).not.toContain('Weekly Limit')
    expect(text).not.toContain('Monthly Limit')
    wrapper.unmount()
  })

  it('shows Unlimited when the wallet holds an unlimited subscription', async () => {
    const wrapper = await mountWith({
      ...base,
      // 后端 subscriptionWalletRemaining 对不限额钱包返回 -1（无上限，不是欠费）
      remaining: -1,
      subscription: {
        subscription_count: 1,
        total_limit_usd: 0,
        total_usage_usd: 3,
        remaining_usd: -1,
        has_unlimited: true,
        expires_at: '2026-12-31T00:00:00Z',
      },
    })

    const text = wrapper.text()
    expect(text).toContain('Unlimited')
    expect(text).not.toContain('Daily Limit')
    // remaining = -1 不得被当成「已用尽」标红
    expect(wrapper.html()).not.toContain('text-rose-500')
    wrapper.unmount()
  })

  it('does not fall back to the wallet-balance row when a subscription wallet exists', async () => {
    const wrapper = await mountWith({
      ...base,
      balance: 5.5,
      subscription: {
        subscription_count: 1,
        total_limit_usd: 20,
        total_usage_usd: 4,
        remaining_usd: 16,
        has_unlimited: false,
        expires_at: '2026-12-31T00:00:00Z',
      },
    })

    const text = wrapper.text()
    expect(text).toContain('$4.00 / $20.00')
    // 有订阅钱包时不再另列余额行（两者互斥，避免误读成“还能花”）
    expect(text).not.toContain('Wallet Balance')
    wrapper.unmount()
  })
})
