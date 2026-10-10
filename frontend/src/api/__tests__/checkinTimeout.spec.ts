import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import type { AxiosInstance, InternalAxiosRequestConfig } from 'axios'

// 需要在导入 client 之前设置 mock（与 client.spec.ts 同口径）
vi.mock('@/i18n', () => ({
  getLocale: () => 'zh-CN',
}))

/**
 * 回归规格：手动签到是长耗时管理动作（Trae 撞 9074 限流时后端会等 60s 换新设备号
 * 重试、编排总上限 420s；qoder/workbuddy 编排上限 60s），必须覆盖全局 30s 默认超时，
 * 否则前端必然本地 abort 并把「服务端还在重试」误报成网络错误。
 */
describe('签到长耗时超时覆盖', () => {
  let apiClient: AxiosInstance
  let longTimeout: number
  let checkinTraeAccount: (id: number) => Promise<unknown>
  let checkinQoderAccount: (id: number) => Promise<unknown>
  let checkinWorkBuddyAccount: (id: number) => Promise<unknown>
  let captured: InternalAxiosRequestConfig | undefined

  /** 回显适配器：记录最终合并后的请求 config，并返回标准成功包。 */
  const installEchoAdapter = () => {
    captured = undefined
    apiClient.defaults.adapter = vi
      .fn()
      .mockImplementation(async (config: InternalAxiosRequestConfig) => {
        captured = config
        return {
          data: { code: 0, data: { success: true, status: 'ok' } },
          status: 200,
          statusText: 'OK',
          headers: {},
          config,
        }
      })
  }

  beforeEach(async () => {
    localStorage.clear()
    sessionStorage.clear()
    vi.resetModules()
    const client = await import('@/api/client')
    apiClient = client.apiClient
    longTimeout = client.LONG_RUNNING_ACTION_TIMEOUT_MS
    checkinTraeAccount = (await import('@/api/admin/trae')).checkinTraeAccount
    checkinQoderAccount = (await import('@/api/admin/qoder')).checkinQoderAccount
    checkinWorkBuddyAccount = (await import('@/api/admin/workbuddy')).checkinWorkBuddyAccount
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('长耗时上限显著大于全局 30s 默认，且不低于 Trae 编排上限 420s', () => {
    expect(apiClient.defaults.timeout).toBe(30000)
    expect(longTimeout).toBeGreaterThanOrEqual(420000)
  })

  it('trae 签到覆盖全局 30s 超时', async () => {
    installEchoAdapter()
    await checkinTraeAccount(9107)
    expect(captured?.url).toBe('/admin/trae/accounts/9107/checkin')
    expect(captured?.timeout).toBe(longTimeout)
  })

  it('qoder 签到覆盖全局 30s 超时', async () => {
    installEchoAdapter()
    await checkinQoderAccount(42)
    expect(captured?.url).toBe('/admin/qoder/accounts/42/checkin')
    expect(captured?.timeout).toBe(longTimeout)
  })

  it('workbuddy 签到覆盖全局 30s 超时', async () => {
    installEchoAdapter()
    await checkinWorkBuddyAccount(7)
    expect(captured?.url).toBe('/admin/workbuddy/accounts/7/checkin')
    expect(captured?.timeout).toBe(longTimeout)
  })

  it('普通请求仍保持全局 30s 默认', async () => {
    installEchoAdapter()
    await apiClient.get('/admin/trae/accounts/9107/credits')
    expect(captured?.timeout).toBe(30000)
  })
})
