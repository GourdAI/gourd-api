import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import QoderCreditsCell from '../QoderCreditsCell.vue'
import type { Account } from '@/types'

// QoderCreditsCell：剩余 Credits 快照渲染 + 手动查询 + 每日活动领取。
// 关键口径（与后端 qoder_campaign_service.go 对齐）：
//   - 快照键 qoder_credits / qoder_checkin；
//   - 领取状态按「活动轮次」归属（每天 10:00 开新一轮，0-10 点算前一天），不是自然日；
//   - 上游探测失败保留快照，仅追加错误行。

const { queryQoderCredits, checkinQoderAccount } = vi.hoisted(() => ({
  queryQoderCredits: vi.fn(),
  checkinQoderAccount: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    qoder: {
      queryQoderCredits,
      checkinQoderAccount
    }
  }
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      // 简化插值：保留 key 原文并拼接参数值，便于断言带参文案。
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
    platform: 'qoder',
    type: 'apikey',
    ...overrides
  } as Account
}

function creditsPayload(overrides: Record<string, unknown> = {}) {
  return {
    success: true,
    realm: 'cn',
    user_type: 'personal_professional',
    remaining: 983,
    used: 1317,
    total: 2300,
    plan_remain: 883,
    plan_total: 2000,
    addon_remain: 100,
    addon_total: 300,
    packs: 1,
    quota_exceeded: false,
    fetched_at: nowSeconds(),
    claimable: true,
    claim_amount: 100,
    claim_campaign_key: 'act-20260923-159',
    ...overrides
  }
}

describe('QoderCreditsCell', () => {
  beforeEach(() => {
    queryQoderCredits.mockReset()
    checkinQoderAccount.mockReset()
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('renders the persisted snapshot without probing when it is fresh', async () => {
    const account = makeAccount(8001, {
      extra: {
        qoder_credits: {
          remain: 1234,
          realm: 'cn',
          fetched_at: nowSeconds(),
          addon_remain: 200
        }
      }
    } as Partial<Account>)
    const wrapper = mount(QoderCreditsCell, { props: { account } })
    await flushPromises()

    expect(queryQoderCredits).not.toHaveBeenCalled()
    const root = wrapper.get('[data-test="qoder-credits"]')
    expect(root.text()).toContain('1,234')
    expect(root.text()).toContain('addon 200')
  })

  it('auto probes once when no snapshot exists', async () => {
    queryQoderCredits.mockResolvedValue(creditsPayload())
    const wrapper = mount(QoderCreditsCell, { props: { account: makeAccount(8002) } })
    await flushPromises()

    expect(queryQoderCredits).toHaveBeenCalledTimes(1)
    expect(queryQoderCredits).toHaveBeenCalledWith(8002)
    expect(wrapper.get('[data-test="qoder-credits"]').text()).toContain('983')
  })

  it('keeps the snapshot and shows an error row when probing fails', async () => {
    queryQoderCredits.mockResolvedValue({ success: false, error: 'HTTP 502 upstream down' })
    const account = makeAccount(8003, {
      extra: { qoder_credits: { remain: 500, realm: 'cn', fetched_at: nowSeconds() - 3600 } }
    } as Partial<Account>)
    const wrapper = mount(QoderCreditsCell, { props: { account } })
    await flushPromises()

    const root = wrapper.get('[data-test="qoder-credits"]')
    expect(root.text()).toContain('500', '查询失败必须保留已渲染快照')
    expect(wrapper.get('[data-test="qoder-credits-error"]').text()).toContain('HTTP 502')
  })

  it('renders truncated transport errors', async () => {
    queryQoderCredits.mockRejectedValue(new Error('x'.repeat(200)))
    const wrapper = mount(QoderCreditsCell, { props: { account: makeAccount(8004) } })
    await flushPromises()

    const text = wrapper.get('[data-test="qoder-credits-error"]').text()
    expect(text.length).toBeLessThanOrEqual(83)
    expect(text.endsWith('...')).toBe(true)
  })

  it('claims the daily campaign and refreshes the balance from the receipt', async () => {
    queryQoderCredits.mockResolvedValue(creditsPayload())
    checkinQoderAccount.mockResolvedValue({
      success: true,
      status: 'ok',
      credits: 1083,
      amount: 100,
      grant_id: 'g-1',
      checked_at: nowSeconds()
    })
    const wrapper = mount(QoderCreditsCell, { props: { account: makeAccount(8005) } })
    await flushPromises()

    await wrapper.get('[data-test="qoder-checkin"]').trigger('click')
    await flushPromises()

    expect(checkinQoderAccount).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-test="qoder-credits"]').text()).toContain('1,083')
    // 领取后本轮视为已完成：状态标签出现且按钮禁用。
    expect(wrapper.text()).toContain('admin.accounts.qoder.claimDoneToday')
    expect((wrapper.get('[data-test="qoder-checkin"]').element as HTMLButtonElement).disabled).toBe(true)
  })

  it('treats already as success and falls back to a re-query when no credits in receipt', async () => {
    // 初始态必须是「可领」，否则按钮被禁用、点击不会发请求（jsdom 不向 disabled 按钮派发 click）。
    queryQoderCredits.mockResolvedValue(creditsPayload({ claimable: true }))
    checkinQoderAccount.mockResolvedValue({ success: true, status: 'already', checked_at: nowSeconds() })
    const wrapper = mount(QoderCreditsCell, { props: { account: makeAccount(8006) } })
    await flushPromises()
    expect(queryQoderCredits).toHaveBeenCalledTimes(1)

    await wrapper.get('[data-test="qoder-checkin"]').trigger('click')
    await flushPromises()

    expect(checkinQoderAccount).toHaveBeenCalledTimes(1)
    // already 无额度 → 回退再查一次；不得出现错误行。
    expect(queryQoderCredits).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[data-test="qoder-checkin-error"]').exists()).toBe(false)
  })

  it('surfaces fail and skipped states with detail', async () => {
    queryQoderCredits.mockResolvedValue(creditsPayload({ claimable: true }))
    checkinQoderAccount.mockResolvedValue({
      success: false,
      status: 'fail',
      detail: 'qoder openapi error: HTTP 403 forbidden',
      checked_at: nowSeconds()
    })
    const wrapper = mount(QoderCreditsCell, { props: { account: makeAccount(8007) } })
    await flushPromises()

    await wrapper.get('[data-test="qoder-checkin"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('admin.accounts.qoder.claimFailed')
    expect(wrapper.get('[data-test="qoder-checkin-error"]').text()).toContain('HTTP 403')
    // fail 仍可重试（不禁用按钮）
    expect((wrapper.get('[data-test="qoder-checkin"]').element as HTMLButtonElement).disabled).toBe(false)
  })

  it('ignores a check-in snapshot from a previous campaign round', async () => {
    // round 写成 2020-01-01：绝不得当作「本轮已领」渲染。
    const account = makeAccount(8008, {
      extra: {
        qoder_credits: { remain: 100, realm: 'cn', fetched_at: nowSeconds() },
        qoder_checkin: { round: '2020-01-01', status: 'ok' }
      }
    } as Partial<Account>)
    const wrapper = mount(QoderCreditsCell, { props: { account } })
    await flushPromises()

    expect(wrapper.text()).not.toContain('admin.accounts.qoder.claimDoneToday')
  })

  it('renders current-round check-in snapshot as claimed', async () => {
    // 用真实 10 点分界口径计算轮次，断言与后端一致。
    const now = new Date()
    const base = now.getHours() < 10 ? new Date(now.getTime() - 24 * 3600 * 1000) : now
    const round = `${base.getFullYear()}-${String(base.getMonth() + 1).padStart(2, '0')}-${String(
      base.getDate()
    ).padStart(2, '0')}`
    const account = makeAccount(8009, {
      extra: {
        qoder_credits: { remain: 100, realm: 'cn', fetched_at: nowSeconds() },
        qoder_checkin: { round, status: 'ok' }
      }
    } as Partial<Account>)
    const wrapper = mount(QoderCreditsCell, { props: { account } })
    await flushPromises()

    expect(wrapper.text()).toContain('admin.accounts.qoder.claimDoneToday')
    expect((wrapper.get('[data-test="qoder-checkin"]').element as HTMLButtonElement).disabled).toBe(true)
  })

  it('hides itself for non-qoder accounts', async () => {
    const wrapper = mount(QoderCreditsCell, {
      props: { account: makeAccount(8010, { platform: 'workbuddy' } as Partial<Account>) }
    })
    await flushPromises()
    expect(wrapper.find('[data-test="qoder-credits"]').exists()).toBe(false)
    expect(queryQoderCredits).not.toHaveBeenCalled()
  })

  it('resets local state when the account changes (list row reuse)', async () => {
    queryQoderCredits.mockResolvedValue(creditsPayload())
    const wrapper = mount(QoderCreditsCell, { props: { account: makeAccount(8011) } })
    await flushPromises()
    expect(wrapper.get('[data-test="qoder-credits"]').text()).toContain('983')

    await wrapper.setProps({ account: makeAccount(8012) })
    await flushPromises()
    // 切号后上一个账号的余额不得残留（列表行复用防串号）。
    const text = wrapper.get('[data-test="qoder-credits"]').text()
    expect(text).not.toContain('983')
    expect(text).toContain('-')
  })
})
