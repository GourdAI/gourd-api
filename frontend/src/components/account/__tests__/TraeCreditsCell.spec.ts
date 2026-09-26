import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import TraeCreditsCell from '../TraeCreditsCell.vue'
import type { Account } from '@/types'

const { queryTraeCredits, checkinTraeAccount } = vi.hoisted(() => ({
  queryTraeCredits: vi.fn(),
  checkinTraeAccount: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    trae: {
      queryTraeCredits,
      checkinTraeAccount
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

const localToday = () => {
  const now = new Date()
  const month = String(now.getMonth() + 1).padStart(2, '0')
  const day = String(now.getDate()).padStart(2, '0')
  return `${now.getFullYear()}-${month}-${day}`
}

function makeAccount(id: number, overrides: Partial<Account> = {}): Account {
  return {
    id,
    platform: 'trae',
    type: 'apikey',
    ...overrides
  } as Account
}

describe('TraeCreditsCell', () => {
  beforeEach(() => {
    queryTraeCredits.mockReset()
    checkinTraeAccount.mockReset()
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('renders the float snapshot from extra without probing (thousands + 1 decimal)', async () => {
    const account = makeAccount(9101, {
      extra: {
        trae_credits: {
          credits: 2203.5264,
          remain: 2203.5264,
          used: 796.4736,
          size: 3000,
          packs: 1,
          realm: 'cn',
          checkable: true,
          checked_in: false,
          fetched_at: nowSeconds()
        }
      }
    } as Partial<Account>)
    const wrapper = mount(TraeCreditsCell, { props: { account } })
    await flushPromises()

    const root = wrapper.get('[data-test="trae-credits"]')
    expect(root.text()).toContain('admin.accounts.trae.creditsRemain')
    // 积分是浮点数：整数部分千分位 + 最多 1 位小数。
    expect(wrapper.get('[data-test="trae-credits-remain"]').text()).toContain('2,203.5')
    expect(root.text()).toContain('cn')
    // 新鲜快照：不自动探测，避免打开列表页就打上游。
    expect(queryTraeCredits).not.toHaveBeenCalled()
  })

  it('falls back to credits when the snapshot has no remain field', async () => {
    const account = makeAccount(9102, {
      extra: {
        trae_credits: { credits: 1234.987, realm: 'global', fetched_at: nowSeconds() }
      }
    } as Partial<Account>)
    const wrapper = mount(TraeCreditsCell, { props: { account } })
    await flushPromises()

    expect(wrapper.get('[data-test="trae-credits-remain"]').text()).toContain('1,235')
    expect(wrapper.text()).toContain('global')
    expect(queryTraeCredits).not.toHaveBeenCalled()
  })

  it('shows used / total and the active package count', async () => {
    const account = makeAccount(9103, {
      extra: {
        trae_credits: {
          credits: 100,
          remain: 100,
          used: 50.5,
          size: 150.5,
          packs: 3,
          realm: 'cn',
          fetched_at: nowSeconds(),
          packages: [
            { entitlement_id: 'e-1', kind: 'checkin', limit: 10, used: 0, remain: 10 },
            { entitlement_id: 'e-2', kind: 'plan', limit: 100, used: 50.5, remain: 49.5 },
            { entitlement_id: 'e-3', kind: 'other', limit: 5, used: 5, remain: 0, expired: true }
          ]
        }
      }
    } as Partial<Account>)
    const wrapper = mount(TraeCreditsCell, { props: { account } })
    await flushPromises()

    // 副行：已用 / 总额（浮点保留 1 位小数）
    expect(wrapper.get('[data-test="trae-credits-usage"]').text()).toContain('50.5')
    expect(wrapper.get('[data-test="trae-credits-usage"]').text()).toContain('150.5')
    // 已过期的包不计入展示：3 个包只算 2 个
    expect(wrapper.get('[data-test="trae-credits-packs"]').text()).toContain('2')
    const tooltip = wrapper.get('[data-test="trae-credits-remain"]').attributes('title') || ''
    expect(tooltip).toContain('e-1')
    expect(tooltip).toContain('e-2')
    expect(tooltip).not.toContain('e-3')
  })

  it('auto-probes once when the snapshot is stale', async () => {
    queryTraeCredits.mockResolvedValue({
      success: true,
      realm: 'cn',
      credits: 777.25,
      remain: 777.25,
      used: 0,
      size: 0,
      packs: 0,
      checkable: true,
      checked_in: false,
      fetched_at: nowSeconds()
    })
    const account = makeAccount(9104, {
      extra: { trae_credits: { remain: 900, realm: 'cn', fetched_at: 0 } }
    } as Partial<Account>)
    const wrapper = mount(TraeCreditsCell, { props: { account } })
    await flushPromises()

    expect(queryTraeCredits).toHaveBeenCalledTimes(1)
    expect(queryTraeCredits).toHaveBeenCalledWith(9104)
    expect(wrapper.get('[data-test="trae-credits-remain"]').text()).toContain('777.3')
  })

  it('refreshes credits via the refresh button and renders the query result', async () => {
    queryTraeCredits.mockResolvedValue({
      success: true,
      realm: 'cn',
      credits: 555.5,
      remain: 555.5,
      used: 44.5,
      size: 600,
      packs: 1,
      checkable: true,
      checked_in: false,
      fetched_at: nowSeconds()
    })
    const account = makeAccount(9105, {
      extra: {
        trae_credits: { remain: 900.4, realm: 'cn', fetched_at: nowSeconds() }
      }
    } as Partial<Account>)
    const wrapper = mount(TraeCreditsCell, { props: { account } })
    await flushPromises()

    await wrapper.get('[data-test="trae-credits-refresh"]').trigger('click')
    await flushPromises()

    expect(queryTraeCredits).toHaveBeenCalledWith(9105)
    expect(wrapper.get('[data-test="trae-credits-remain"]').text()).toContain('555.5')
  })

  it('keeps the snapshot and shows a truncated error when upstream returns success:false', async () => {
    queryTraeCredits.mockResolvedValue({
      success: false,
      realm: 'cn',
      credits: 0,
      remain: 0,
      used: 0,
      size: 0,
      packs: 0,
      checkable: false,
      checked_in: false,
      fetched_at: 0,
      error: 'x'.repeat(90)
    })
    const account = makeAccount(9106, {
      extra: { trae_credits: { remain: 900.4, realm: 'cn', fetched_at: nowSeconds() } }
    } as Partial<Account>)
    const wrapper = mount(TraeCreditsCell, { props: { account } })
    await flushPromises()

    await wrapper.get('[data-test="trae-credits-refresh"]').trigger('click')
    await flushPromises()

    // 失败保留快照数字（不被上游 0 覆盖），仅叠加错误行（80 字符截断）。
    expect(wrapper.get('[data-test="trae-credits-remain"]').text()).toContain('900.4')
    const errorRow = wrapper.get('[data-test="trae-credits-error"]')
    expect(errorRow.text()).toContain('x'.repeat(80) + '...')
  })

  it('shows the checked-in state and updated credits after a successful check-in', async () => {
    checkinTraeAccount.mockResolvedValue({
      success: true,
      status: 'ok',
      credits: 880.75,
      has_credits: true,
      realm: 'cn',
      checked_at: nowSeconds()
    })
    const account = makeAccount(9107, {
      extra: {
        trae_credits: { remain: 900.4, realm: 'cn', checkable: true, checked_in: false, fetched_at: nowSeconds() }
      }
    } as Partial<Account>)
    const wrapper = mount(TraeCreditsCell, { props: { account } })
    await flushPromises()

    const btn = wrapper.get('[data-test="trae-checkin"]')
    expect((btn.element as HTMLButtonElement).disabled).toBe(false)
    await btn.trigger('click')
    await flushPromises()

    expect(checkinTraeAccount).toHaveBeenCalledWith(9107)
    expect(wrapper.get('[data-test="trae-checkin-status"]').text()).toContain(
      'admin.accounts.trae.checkinDoneToday'
    )
    // 签到回执的浮点余额直接更新展示（无需再查询）。
    expect(wrapper.get('[data-test="trae-credits-remain"]').text()).toContain('880.8')
    expect(queryTraeCredits).not.toHaveBeenCalled()
    // 已签到后按钮禁用并提示今日已签到。
    const after = wrapper.get('[data-test="trae-checkin"]')
    expect((after.element as HTMLButtonElement).disabled).toBe(true)
    expect(after.attributes('title')).toBe('admin.accounts.trae.checkinDoneToday')
  })

  it('keeps the current balance when the check-in receipt carries no verified credits', async () => {
    // 回查 status 失败时后端 credits 恒为 0（has_credits 缺省）——
    // 不能拿它覆盖展示，否则「剩余 2203.5」会变成 0，与事实相反。
    checkinTraeAccount.mockResolvedValue({
      success: true,
      status: 'ok',
      credits: 0,
      realm: 'cn',
      checked_at: nowSeconds()
    })
    const account = makeAccount(9115, {
      extra: {
        trae_credits: {
          remain: 2203.5,
          realm: 'cn',
          checkable: true,
          checked_in: false,
          fetched_at: nowSeconds()
        }
      }
    } as Partial<Account>)
    const wrapper = mount(TraeCreditsCell, { props: { account } })
    await flushPromises()

    await wrapper.get('[data-test="trae-checkin"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-test="trae-credits-remain"]').text()).toContain('2,203.5')
    expect(wrapper.get('[data-test="trae-checkin-status"]').text()).toContain(
      'admin.accounts.trae.checkinDoneToday'
    )
  })

  it('disables the check-in button from the upstream checked_in reading', async () => {
    const account = makeAccount(9108, {
      extra: {
        trae_credits: { remain: 10, realm: 'cn', checkable: true, checked_in: true, fetched_at: nowSeconds() }
      }
    } as Partial<Account>)
    const wrapper = mount(TraeCreditsCell, { props: { account } })
    await flushPromises()

    const btn = wrapper.get('[data-test="trae-checkin"]')
    expect((btn.element as HTMLButtonElement).disabled).toBe(true)
    expect(wrapper.get('[data-test="trae-checkin-status"]').text()).toContain(
      'admin.accounts.trae.checkinDoneToday'
    )
  })

  it('disables the check-in button when the upstream marks it uncheckable', async () => {
    const account = makeAccount(9109, {
      extra: {
        trae_credits: { remain: 10, realm: 'cn', checkable: false, checked_in: false, fetched_at: nowSeconds() }
      }
    } as Partial<Account>)
    const wrapper = mount(TraeCreditsCell, { props: { account } })
    await flushPromises()

    const btn = wrapper.get('[data-test="trae-checkin"]')
    expect((btn.element as HTMLButtonElement).disabled).toBe(true)
    expect(btn.attributes('title')).toBe('admin.accounts.trae.creditsCheckinUnavailableTooltip')
  })

  it('treats already-checked-in as success and surfaces the failure detail', async () => {
    checkinTraeAccount.mockResolvedValue({
      success: true,
      status: 'already',
      credits: 0,
      realm: 'cn',
      checked_at: nowSeconds()
    })
    const account = makeAccount(9110, {
      extra: { trae_credits: { remain: 900.4, realm: 'cn', fetched_at: nowSeconds() } }
    } as Partial<Account>)
    const wrapper = mount(TraeCreditsCell, { props: { account } })
    await flushPromises()

    await wrapper.get('[data-test="trae-checkin"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-test="trae-checkin-status"]').text()).toContain(
      'admin.accounts.trae.checkinDoneToday'
    )

    checkinTraeAccount.mockResolvedValue({ success: false, status: 'fail', detail: 'boom-error', credits: 0, checked_at: nowSeconds() })
    const failing = makeAccount(9111, {
      extra: { trae_credits: { remain: 10, realm: 'cn', fetched_at: nowSeconds() } }
    } as Partial<Account>)
    const wrapper2 = mount(TraeCreditsCell, { props: { account: failing } })
    await flushPromises()
    await wrapper2.get('[data-test="trae-checkin"]').trigger('click')
    await flushPromises()

    expect(wrapper2.get('[data-test="trae-checkin-status"]').text()).toContain(
      'admin.accounts.trae.checkinFailed'
    )
    expect(wrapper2.get('[data-test="trae-checkin-error"]').text()).toContain('boom-error')
  })

  it('reads today\'s check-in snapshot from extra', async () => {
    const account = makeAccount(9112, {
      extra: {
        trae_credits: { remain: 20, realm: 'cn', fetched_at: nowSeconds() },
        trae_checkin: { date: localToday(), status: 'ok', checked_at: nowSeconds(), credits: 20 }
      }
    } as Partial<Account>)
    const wrapper = mount(TraeCreditsCell, { props: { account } })
    await flushPromises()

    expect(wrapper.get('[data-test="trae-checkin-status"]').text()).toContain(
      'admin.accounts.trae.checkinDoneToday'
    )
    expect((wrapper.get('[data-test="trae-checkin"]').element as HTMLButtonElement).disabled).toBe(true)
  })

  it('ignores a check-in snapshot from another day', async () => {
    const account = makeAccount(9113, {
      extra: {
        trae_credits: { remain: 20, realm: 'cn', fetched_at: nowSeconds() },
        trae_checkin: { date: '1999-01-01', status: 'ok', checked_at: 0, credits: 0 }
      }
    } as Partial<Account>)
    const wrapper = mount(TraeCreditsCell, { props: { account } })
    await flushPromises()

    expect(wrapper.find('[data-test="trae-checkin-status"]').exists()).toBe(false)
  })

  it('stays hidden for non-trae accounts', async () => {
    const account = makeAccount(9114, { platform: 'workbuddy' } as Partial<Account>)
    const wrapper = mount(TraeCreditsCell, { props: { account } })
    await flushPromises()

    expect(wrapper.find('[data-test="trae-credits"]').exists()).toBe(false)
    expect(queryTraeCredits).not.toHaveBeenCalled()
  })

  // 凭据到期告警：Trae 的 refreshToken 过期后无法自动续期（必须人工重登重取凭据），
  // 所以「还能用多久」必须常驻列表，否则账号会静默变成不可用。
  describe('token expiry badge', () => {
    const expiryBadge = (wrapper: ReturnType<typeof mount>) =>
      wrapper.find('[data-test="trae-token-expiry"]')

    it('shows nothing when no expiry info is available', async () => {
      const account = makeAccount(9120, {
        extra: { trae_credits: { remain: 10, realm: 'cn', fetched_at: nowSeconds() } }
      } as Partial<Account>)
      const wrapper = mount(TraeCreditsCell, { props: { account } })
      await flushPromises()
      expect(expiryBadge(wrapper).exists()).toBe(false)
    })

    it('warns when the refresh token is about to expire', async () => {
      const account = makeAccount(9121, {
        extra: {
          trae_credits: {
            remain: 10,
            realm: 'cn',
            fetched_at: nowSeconds(),
            token_expires_at: nowSeconds() + 3 * 86400,
            refresh_expires_at: nowSeconds() + 2 * 3600
          }
        }
      } as Partial<Account>)
      const wrapper = mount(TraeCreditsCell, { props: { account } })
      await flushPromises()
      expect(expiryBadge(wrapper).text()).toContain('admin.accounts.trae.tokenExpiringInHours')
    })

    it('marks expired credentials when the refresh token is gone', async () => {
      const account = makeAccount(9122, {
        extra: {
          trae_credits: {
            remain: 10,
            realm: 'cn',
            fetched_at: nowSeconds(),
            token_expires_at: nowSeconds() - 86400,
            refresh_expires_at: nowSeconds() - 3600
          }
        }
      } as Partial<Account>)
      const wrapper = mount(TraeCreditsCell, { props: { account } })
      await flushPromises()
      expect(expiryBadge(wrapper).text()).toContain('admin.accounts.trae.tokenExpired')
      expect(expiryBadge(wrapper).attributes('title')).toContain(
        'admin.accounts.trae.tokenExpiredHint'
      )
    })

    it('treats a past access token as renewable while the refresh token lives', async () => {
      const account = makeAccount(9123, {
        extra: {
          trae_credits: {
            remain: 10,
            realm: 'cn',
            fetched_at: nowSeconds(),
            token_expires_at: nowSeconds() - 600,
            refresh_expires_at: nowSeconds() + 30 * 86400
          }
        }
      } as Partial<Account>)
      const wrapper = mount(TraeCreditsCell, { props: { account } })
      await flushPromises()
      // 不得误报「已过期」——后端出站前会自动换票。
      expect(expiryBadge(wrapper).text()).toContain('admin.accounts.trae.tokenRenewable')
    })

    it('falls back to credentials.expires_at when no credit snapshot exists', async () => {
      // expires_at 不是敏感键，会随详情下发；无积分快照时也应能告警。
      const account = makeAccount(9124, {
        credentials: { expires_at: nowSeconds() - 10 }
      } as Partial<Account>)
      const wrapper = mount(TraeCreditsCell, { props: { account } })
      await flushPromises()
      expect(expiryBadge(wrapper).text()).toContain('admin.accounts.trae.tokenExpired')
    })

    it('reads camelCase credential aliases (traework2api auth file shape)', async () => {
      const account = makeAccount(9125, {
        credentials: { expiredAt: nowSeconds() - 10 }
      } as Partial<Account>)
      const wrapper = mount(TraeCreditsCell, { props: { account } })
      await flushPromises()
      expect(expiryBadge(wrapper).text()).toContain('admin.accounts.trae.tokenExpired')
    })

    it('normalizes millisecond epochs instead of reporting absurd expiry', async () => {
      const account = makeAccount(9126, {
        credentials: { expires_at: (nowSeconds() + 3600) * 1000 }
      } as Partial<Account>)
      const wrapper = mount(TraeCreditsCell, { props: { account } })
      await flushPromises()
      // 若不归一为秒，会被当成「距今 5 万年」而不告警。
      expect(expiryBadge(wrapper).text()).toContain('admin.accounts.trae.tokenExpiringInHours')
    })
  })
})
