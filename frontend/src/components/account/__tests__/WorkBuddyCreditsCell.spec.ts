import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import WorkBuddyCreditsCell from '../WorkBuddyCreditsCell.vue'
import type { Account } from '@/types'

const { queryWorkBuddyCredits, checkinWorkBuddyAccount, runWorkBuddyActivity, runWorkBuddyTravel } =
  vi.hoisted(() => ({
    queryWorkBuddyCredits: vi.fn(),
    checkinWorkBuddyAccount: vi.fn(),
    runWorkBuddyActivity: vi.fn(),
    runWorkBuddyTravel: vi.fn()
  }))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    workbuddy: {
      queryWorkBuddyCredits,
      checkinWorkBuddyAccount,
      runWorkBuddyActivity,
      runWorkBuddyTravel
    }
  }
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      // 简化插值：保留 key 原文并拼接参数值，兼容无参数 key 断言与带参文案断言。
      t: (key: string, params?: Record<string, unknown>) => {
        if (!params) return key
        return [key, ...Object.values(params)].join(' ')
      }
    })
  }
})

const nowSeconds = () => Math.floor(Date.now() / 1000)

function makeAccount(id: number, overrides: Partial<Account> = {}): Account {
  return {
    id,
    platform: 'workbuddy',
    type: 'oauth',
    ...overrides
  } as Account
}

describe('WorkBuddyCreditsCell', () => {
  beforeEach(() => {
    queryWorkBuddyCredits.mockReset()
    checkinWorkBuddyAccount.mockReset()
    runWorkBuddyActivity.mockReset()
    runWorkBuddyTravel.mockReset()
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('renders the persisted snapshot without probing when it is fresh', async () => {
    const account = makeAccount(9001, {
      extra: {
        workbuddy_credits: { remain: 900, realm: 'cn', fetched_at: nowSeconds() }
      }
    } as Partial<Account>)
    const wrapper = mount(WorkBuddyCreditsCell, { props: { account } })
    await flushPromises()

    const root = wrapper.get('[data-test="workbuddy-credits"]')
    expect(root.text()).toContain('admin.accounts.workbuddy.creditsRemain')
    expect(root.text()).toContain('900')
    expect(root.text()).toContain('cn')
    // 新鲜快照：不自动探测，避免打开列表页就打上游。
    expect(queryWorkBuddyCredits).not.toHaveBeenCalled()
  })

  it('auto-probes once when the snapshot is stale', async () => {
    queryWorkBuddyCredits.mockResolvedValue({
      success: true,
      remain: 777,
      realm: 'cn',
      fetched_at: nowSeconds()
    })
    const account = makeAccount(9002, {
      extra: {
        workbuddy_credits: { remain: 900, realm: 'cn', fetched_at: 0 }
      }
    } as Partial<Account>)
    const wrapper = mount(WorkBuddyCreditsCell, { props: { account } })
    await flushPromises()

    expect(queryWorkBuddyCredits).toHaveBeenCalledTimes(1)
    expect(queryWorkBuddyCredits).toHaveBeenCalledWith(9002)
    expect(wrapper.get('[data-test="workbuddy-credits"]').text()).toContain('777')
  })

  it('refreshes credits via the refresh button and renders the query result', async () => {
    queryWorkBuddyCredits.mockResolvedValue({
      success: true,
      remain: 555,
      realm: 'cn',
      fetched_at: nowSeconds()
    })
    const account = makeAccount(9003, {
      extra: {
        workbuddy_credits: { remain: 900, realm: 'cn', fetched_at: nowSeconds() }
      }
    } as Partial<Account>)
    const wrapper = mount(WorkBuddyCreditsCell, { props: { account } })
    await flushPromises()

    await wrapper.get('[data-test="workbuddy-credits-refresh"]').trigger('click')
    await flushPromises()

    expect(queryWorkBuddyCredits).toHaveBeenCalledWith(9003)
    expect(wrapper.get('[data-test="workbuddy-credits"]').text()).toContain('555')
  })

  it('keeps the snapshot and shows a truncated error when refresh fails', async () => {
    queryWorkBuddyCredits.mockResolvedValue({
      success: false,
      error: 'x'.repeat(90)
    })
    const account = makeAccount(9004, {
      extra: {
        workbuddy_credits: { remain: 900, realm: 'cn', fetched_at: nowSeconds() }
      }
    } as Partial<Account>)
    const wrapper = mount(WorkBuddyCreditsCell, { props: { account } })
    await flushPromises()

    await wrapper.get('[data-test="workbuddy-credits-refresh"]').trigger('click')
    await flushPromises()

    const text = wrapper.get('[data-test="workbuddy-credits"]').text()
    // 失败保留快照数字，仅叠加错误行（80 字符截断）。
    expect(text).toContain('900')
    expect(text).toContain('x'.repeat(80) + '...')
  })

  it('shows the checked-in label and updated credits after a successful check-in', async () => {
    checkinWorkBuddyAccount.mockResolvedValue({ success: true, status: 'ok', credits: 880 })
    const account = makeAccount(9005, {
      extra: {
        workbuddy_credits: { remain: 900, realm: 'cn', fetched_at: nowSeconds() }
      }
    } as Partial<Account>)
    const wrapper = mount(WorkBuddyCreditsCell, { props: { account } })
    await flushPromises()

    await wrapper.get('[data-test="workbuddy-checkin"]').trigger('click')
    await flushPromises()

    expect(checkinWorkBuddyAccount).toHaveBeenCalledWith(9005)
    const text = wrapper.get('[data-test="workbuddy-credits"]').text()
    expect(text).toContain('admin.accounts.workbuddy.checkinDoneToday')
    // 签到返回的 credits 直接更新展示（无需再查询）。
    expect(text).toContain('880')
    expect(queryWorkBuddyCredits).not.toHaveBeenCalled()
  })

  it('treats already-checked-in as success and keeps the label', async () => {
    checkinWorkBuddyAccount.mockResolvedValue({ success: true, status: 'already' })
    const account = makeAccount(9006, {
      extra: {
        workbuddy_credits: { remain: 900, realm: 'cn', fetched_at: nowSeconds() }
      }
    } as Partial<Account>)
    const wrapper = mount(WorkBuddyCreditsCell, { props: { account } })
    await flushPromises()

    await wrapper.get('[data-test="workbuddy-checkin"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-test="workbuddy-credits"]').text()).toContain(
      'admin.accounts.workbuddy.checkinDoneToday'
    )
  })

  it('renders the failure label and exposes the detail as a title on failure', async () => {
    checkinWorkBuddyAccount.mockResolvedValue({ success: false, status: 'fail', detail: 'boom-error' })
    const account = makeAccount(9007, {
      extra: {
        workbuddy_credits: { remain: 900, realm: 'cn', fetched_at: nowSeconds() }
      }
    } as Partial<Account>)
    const wrapper = mount(WorkBuddyCreditsCell, { props: { account } })
    await flushPromises()

    await wrapper.get('[data-test="workbuddy-checkin"]').trigger('click')
    await flushPromises()

    const root = wrapper.get('[data-test="workbuddy-credits"]')
    expect(root.text()).toContain('admin.accounts.workbuddy.checkinFailed')
    const statusSpan = root.findAll('span').find((node) => node.text() === 'admin.accounts.workbuddy.checkinFailed')
    expect(statusSpan?.attributes('title')).toBe('boom-error')
  })

  it('shows the persisted check-in status snapshot for today', async () => {
    const today = new Date().toISOString().slice(0, 10)
    const account = makeAccount(9008, {
      extra: {
        workbuddy_credits: { remain: 900, realm: 'cn', fetched_at: nowSeconds() },
        workbuddy_checkin: { date: today, status: 'ok' }
      }
    } as Partial<Account>)
    const wrapper = mount(WorkBuddyCreditsCell, { props: { account } })
    await flushPromises()

    expect(wrapper.get('[data-test="workbuddy-credits"]').text()).toContain(
      'admin.accounts.workbuddy.checkinDoneToday'
    )
  })

  it('stays hidden for non-workbuddy accounts', async () => {
    const account = makeAccount(9009, { platform: 'openai' } as Partial<Account>)
    const wrapper = mount(WorkBuddyCreditsCell, { props: { account } })
    await flushPromises()

    expect(wrapper.find('[data-test="workbuddy-credits"]').exists()).toBe(false)
    expect(queryWorkBuddyCredits).not.toHaveBeenCalled()
  })

  it('runs the activity task and renders streak and report counts', async () => {
    runWorkBuddyActivity.mockResolvedValue({
      success: true,
      realm: 'cn',
      reports: 3,
      reports_requested: 3,
      streak_days: 7,
      streak_checked: true,
      lottery_drawn: false,
      ran_at: nowSeconds()
    })
    const account = makeAccount(9010, {
      extra: {
        workbuddy_credits: { remain: 900, realm: 'cn', fetched_at: nowSeconds() }
      }
    } as Partial<Account>)
    const wrapper = mount(WorkBuddyCreditsCell, { props: { account } })
    await flushPromises()

    await wrapper.get('[data-test="workbuddy-activity-run"]').trigger('click')
    await flushPromises()

    expect(runWorkBuddyActivity).toHaveBeenCalledWith(9010)
    expect(runWorkBuddyTravel).not.toHaveBeenCalled()
    const snapshot = wrapper.get('[data-test="workbuddy-activity-snapshot"]')
    // 结果直接更新本地展示：连登天数与上报条数均可见。
    expect(snapshot.text()).toContain('7')
    expect(snapshot.text()).toContain('3')
  })

  it('runs the travel task and renders the action, buddy name and reward credit', async () => {
    runWorkBuddyTravel.mockResolvedValue({
      success: true,
      realm: 'cn',
      action: 'claim',
      buddy_name: '奶糕',
      state: 'home',
      reward_credit: 20,
      ran_at: nowSeconds()
    })
    const account = makeAccount(9011, {
      extra: {
        workbuddy_credits: { remain: 900, realm: 'cn', fetched_at: nowSeconds() }
      }
    } as Partial<Account>)
    const wrapper = mount(WorkBuddyCreditsCell, { props: { account } })
    await flushPromises()

    await wrapper.get('[data-test="workbuddy-travel-run"]').trigger('click')
    await flushPromises()

    expect(runWorkBuddyTravel).toHaveBeenCalledWith(9011)
    expect(runWorkBuddyActivity).not.toHaveBeenCalled()
    const snapshot = wrapper.get('[data-test="workbuddy-travel-snapshot"]')
    expect(snapshot.text()).toContain('admin.accounts.workbuddy.travelActionClaim')
    expect(snapshot.text()).toContain('奶糕')
    expect(snapshot.text()).toContain('+20')
  })

  it('renders today\'s activity and travel snapshots without probing', async () => {
    const today = new Date()
    const todayKey = `${today.getFullYear()}-${String(today.getMonth() + 1).padStart(2, '0')}-${String(
      today.getDate()
    ).padStart(2, '0')}`
    const account = makeAccount(9012, {
      extra: {
        workbuddy_credits: { remain: 900, realm: 'cn', fetched_at: nowSeconds() },
        workbuddy_activity: {
          date: todayKey,
          streak_days: 12,
          reports: 5,
          lottery_drawn: false,
          ran_at: nowSeconds(),
          ok: true
        },
        workbuddy_travel: {
          date: todayKey,
          action: 'depart',
          buddy_name: '团子',
          ran_at: nowSeconds(),
          ok: true
        }
      }
    } as Partial<Account>)
    const wrapper = mount(WorkBuddyCreditsCell, { props: { account } })
    await flushPromises()

    const activity = wrapper.get('[data-test="workbuddy-activity-snapshot"]')
    expect(activity.text()).toContain('12')
    expect(activity.text()).toContain('5')
    const travel = wrapper.get('[data-test="workbuddy-travel-snapshot"]')
    expect(travel.text()).toContain('admin.accounts.workbuddy.travelActionDepart')
    expect(travel.text()).toContain('团子')
    // 新鲜快照不触发探测。
    expect(queryWorkBuddyCredits).not.toHaveBeenCalled()
  })

  it('shows the enterprise not-applicable badge instead of a zero balance', async () => {
    const t = new Date()
    const todayKey = `${t.getFullYear()}-${String(t.getMonth() + 1).padStart(2, '0')}-${String(
      t.getDate()
    ).padStart(2, '0')}`
    const account = makeAccount(9021, {
      extra: {
        workbuddy_credits: {
          remain: 0,
          used: 0,
          size: 0,
          packs: 0,
          realm: 'cn',
          fetched_at: nowSeconds(),
          not_applicable: true,
          enterprise: true
        },
        // 修复前遗留的今日签到快照（当时确实尝试过）：展示层必须压住它。
        workbuddy_checkin: { date: todayKey, status: 'already', checked_at: nowSeconds() }
      }
    } as Partial<Account>)
    const wrapper = mount(WorkBuddyCreditsCell, { props: { account } })
    await flushPromises()

    expect(wrapper.get('[data-test="workbuddy-credits-enterprise"]').text()).toContain(
      'admin.accounts.workbuddy.creditsEnterpriseNA'
    )
    // 不能同时渲染「剩余 0」（这是本次修复的核心：企业号不得把空池读成余额 0）。
    expect(wrapper.find('[data-test="workbuddy-credits-remain"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('admin.accounts.workbuddy.creditsRemain')
    // 旧快照（修复前写的 today ok/already）不得再展示「今日已签到」。
    expect(wrapper.text()).not.toContain('admin.accounts.workbuddy.checkinDoneToday')
  })

  it('keeps normal credit display for personal accounts without the flag', async () => {
    const account = makeAccount(9022, {
      extra: { workbuddy_credits: { remain: 0, realm: 'cn', fetched_at: nowSeconds() } }
    } as Partial<Account>)
    const wrapper = mount(WorkBuddyCreditsCell, { props: { account } })
    await flushPromises()

    expect(wrapper.find('[data-test="workbuddy-credits-enterprise"]').exists()).toBe(false)
    expect(wrapper.get('[data-test="workbuddy-credits"]').text()).toContain(
      'admin.accounts.workbuddy.creditsRemain'
    )
  })

  it('renders the badge from a probe response marked not_applicable', async () => {
    queryWorkBuddyCredits.mockResolvedValue({
      success: true,
      realm: 'cn',
      remain: 0,
      packs: 0,
      fetched_at: nowSeconds(),
      enterprise: true,
      not_applicable: true
    })
    // 无快照 → 挂载时自动探测；探测后应切到 badge。
    const account = makeAccount(9023, { extra: {} } as Partial<Account>)
    const wrapper = mount(WorkBuddyCreditsCell, { props: { account } })
    await flushPromises()

    expect(queryWorkBuddyCredits).toHaveBeenCalledWith(9023)
    expect(wrapper.get('[data-test="workbuddy-credits-enterprise"]').text()).toContain(
      'admin.accounts.workbuddy.creditsEnterpriseNA'
    )
  })

  it('disables the check-in button for enterprise accounts', async () => {
    const account = makeAccount(9024, {
      extra: {
        workbuddy_credits: {
          remain: 0,
          realm: 'cn',
          packs: 0,
          fetched_at: nowSeconds(),
          not_applicable: true,
          enterprise: true
        }
      }
    } as Partial<Account>)
    const wrapper = mount(WorkBuddyCreditsCell, { props: { account } })
    await flushPromises()

    const btn = wrapper.get('[data-test="workbuddy-checkin"]')
    expect((btn.element as HTMLButtonElement).disabled).toBe(true)
    expect(btn.attributes('title')).toBe('admin.accounts.workbuddy.checkinEnterpriseTooltip')
    // 签到仍保持可点（个人号 enterprise 未标）的反例在其他用例已覆盖。
  })

  it('hides stale snapshots that are not from today', async () => {
    const tomorrow = new Date(Date.now() + 24 * 3600 * 1000)
    const staleKey = `${tomorrow.getFullYear()}-${String(tomorrow.getMonth() + 1).padStart(
      2,
      '0'
    )}-${String(tomorrow.getDate()).padStart(2, '0')}`
    const account = makeAccount(9013, {
      extra: {
        workbuddy_credits: { remain: 900, realm: 'cn', fetched_at: nowSeconds() },
        workbuddy_activity: {
          date: staleKey,
          streak_days: 12,
          reports: 5,
          lottery_drawn: false,
          ran_at: nowSeconds(),
          ok: true
        },
        workbuddy_travel: {
          date: staleKey,
          action: 'claim',
          buddy_name: '团子',
          reward_credit: 20,
          ran_at: nowSeconds(),
          ok: true
        }
      }
    } as Partial<Account>)
    const wrapper = mount(WorkBuddyCreditsCell, { props: { account } })
    await flushPromises()

    expect(wrapper.find('[data-test="workbuddy-activity-snapshot"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="workbuddy-travel-snapshot"]').exists()).toBe(false)
  })
})
