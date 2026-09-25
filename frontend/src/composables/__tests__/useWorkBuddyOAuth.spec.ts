import { afterEach, describe, expect, it, vi } from 'vitest'

const showErrorMock = vi.fn()

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: showErrorMock
  })
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => {
      const messages: Record<string, string> = {
        'admin.accounts.workbuddy.oauthFailedToGenerateUrl': '生成 WorkBuddy 授权链接失败',
        'admin.accounts.workbuddy.oauthFailedToExchange': 'WorkBuddy 授权码兑换失败',
        'admin.accounts.workbuddy.oauthMissingState': '缺少 OAuth state，请重新生成授权链接',
        'admin.accounts.workbuddy.oauthErrors.WORKBUDDY_OAUTH_INVALID_STATE':
          'WorkBuddy OAuth state 与当前会话不匹配'
      }
      return messages[key] ?? key
    }
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    workbuddy: {
      generateWorkBuddyAuthUrl: vi.fn(),
      exchangeWorkBuddyCode: vi.fn()
    }
  }
}))

import { useWorkBuddyOAuth } from '@/composables/useWorkBuddyOAuth'
import { adminAPI } from '@/api/admin'

describe('useWorkBuddyOAuth.generateAuthUrl', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.clearAllMocks()
  })

  it('saves authUrl/state on success and forwards the realm', async () => {
    vi.mocked(adminAPI.workbuddy.generateWorkBuddyAuthUrl).mockResolvedValueOnce({
      auth_url: 'https://copilot.tencent.com/oauth?state=abc',
      state: 'abc',
      realm: 'cn'
    })
    const oauth = useWorkBuddyOAuth()

    const ok = await oauth.generateAuthUrl('cn', 7)

    expect(ok).toBe(true)
    expect(oauth.authUrl.value).toBe('https://copilot.tencent.com/oauth?state=abc')
    expect(oauth.state.value).toBe('abc')
    expect(oauth.realm.value).toBe('cn')
    expect(adminAPI.workbuddy.generateWorkBuddyAuthUrl).toHaveBeenCalledWith({
      realm: 'cn',
      proxy_id: 7
    })
  })

  it('returns false and surfaces the error message on failure', async () => {
    vi.mocked(adminAPI.workbuddy.generateWorkBuddyAuthUrl).mockRejectedValueOnce({
      status: 500,
      message: 'boom'
    })
    const oauth = useWorkBuddyOAuth()

    const ok = await oauth.generateAuthUrl('global')

    expect(ok).toBe(false)
    expect(oauth.authUrl.value).toBe('')
    expect(oauth.error.value).toBe('boom')
    expect(showErrorMock).toHaveBeenCalledWith('boom')
    // proxy_id 未提供时不出现在 payload 中。
    expect(adminAPI.workbuddy.generateWorkBuddyAuthUrl).toHaveBeenCalledWith({ realm: 'global' })
  })
})

describe('useWorkBuddyOAuth.pollForToken', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.clearAllMocks()
  })

  it('returns pending while the user has not finished logging in', async () => {
    vi.mocked(adminAPI.workbuddy.generateWorkBuddyAuthUrl).mockResolvedValueOnce({
      auth_url: 'https://copilot.tencent.com/oauth?state=st-1',
      state: 'st-1',
      realm: 'cn'
    })
    vi.mocked(adminAPI.workbuddy.exchangeWorkBuddyCode).mockResolvedValueOnce({
      status: 'pending'
    })
    const oauth = useWorkBuddyOAuth()
    await oauth.generateAuthUrl('cn')

    const result = await oauth.pollForToken('cn')

    expect(result).toEqual({ status: 'pending' })
    expect(adminAPI.workbuddy.exchangeWorkBuddyCode).toHaveBeenCalledWith({
      state: 'st-1',
      realm: 'cn'
    })
  })

  it('returns the token payload when completed and keeps the realm consistent', async () => {
    vi.mocked(adminAPI.workbuddy.generateWorkBuddyAuthUrl).mockResolvedValueOnce({
      auth_url: 'https://workbuddy.ai/oauth?state=st-2',
      state: 'st-2',
      realm: 'global'
    })
    vi.mocked(adminAPI.workbuddy.exchangeWorkBuddyCode).mockResolvedValueOnce({
      status: 'completed',
      token: {
        access_token: 'at-1',
        refresh_token: 'rt-1',
        uid: 'u-1',
        enterprise_id: 'ent-1',
        domain: 'www.workbuddy.ai',
        nickname: 'tester'
      }
    })
    const oauth = useWorkBuddyOAuth()
    await oauth.generateAuthUrl('global')

    const result = await oauth.pollForToken('global')

    expect(result?.status).toBe('completed')
    if (result?.status !== 'completed') throw new Error('expected completed')
    expect(result.token.access_token).toBe('at-1')
    expect(result.token.refresh_token).toBe('rt-1')
    expect(result.token.uid).toBe('u-1')
    expect(result.token.enterprise_id).toBe('ent-1')
    // 后端未回传 realm 时沿用请求值，保证 realm 一致。
    expect(result.token.realm).toBe('global')
  })

  it('returns null and maps structured errors when the exchange fails', async () => {
    vi.mocked(adminAPI.workbuddy.generateWorkBuddyAuthUrl).mockResolvedValueOnce({
      auth_url: 'https://copilot.tencent.com/oauth?state=st-3',
      state: 'st-3',
      realm: 'cn'
    })
    vi.mocked(adminAPI.workbuddy.exchangeWorkBuddyCode).mockRejectedValueOnce({
      status: 400,
      reason: 'WORKBUDDY_OAUTH_INVALID_STATE',
      message: 'invalid oauth state'
    })
    const oauth = useWorkBuddyOAuth()
    await oauth.generateAuthUrl('cn')

    const result = await oauth.pollForToken('cn')

    expect(result).toBeNull()
    expect(oauth.error.value).toBe('WorkBuddy OAuth state 与当前会话不匹配')
  })

  it('rejects polling without a state', async () => {
    const oauth = useWorkBuddyOAuth()

    const result = await oauth.pollForToken('cn')

    expect(result).toBeNull()
    expect(oauth.error.value).toBe('缺少 OAuth state，请重新生成授权链接')
    expect(adminAPI.workbuddy.exchangeWorkBuddyCode).not.toHaveBeenCalled()
  })
})

describe('useWorkBuddyOAuth.startPolling', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.clearAllMocks()
  })

  async function seedAuthUrl(realm: 'cn' | 'global' = 'cn') {
    vi.mocked(adminAPI.workbuddy.generateWorkBuddyAuthUrl).mockResolvedValueOnce({
      auth_url: `https://example.com/oauth?state=st-poll`,
      state: 'st-poll',
      realm
    })
    const oauth = useWorkBuddyOAuth()
    await oauth.generateAuthUrl(realm)
    return oauth
  }

  it('keeps polling while pending and stops on completion', async () => {
    vi.useFakeTimers()
    const oauth = await seedAuthUrl('cn')
    vi.mocked(adminAPI.workbuddy.exchangeWorkBuddyCode)
      .mockResolvedValueOnce({ status: 'pending' })
      .mockResolvedValueOnce({ status: 'pending' })
      .mockResolvedValueOnce({
        status: 'completed',
        token: { access_token: 'at-final', refresh_token: 'rt-final', uid: 'u-final' }
      })

    const onCompleted = vi.fn()
    oauth.startPolling({ onCompleted }, { realm: 'cn', intervalMs: 1000 })

    expect(oauth.polling.value).toBe(true)

    await vi.advanceTimersByTimeAsync(1000)
    expect(adminAPI.workbuddy.exchangeWorkBuddyCode).toHaveBeenCalledTimes(1)

    await vi.advanceTimersByTimeAsync(1000)
    expect(adminAPI.workbuddy.exchangeWorkBuddyCode).toHaveBeenCalledTimes(2)

    await vi.advanceTimersByTimeAsync(1000)
    expect(adminAPI.workbuddy.exchangeWorkBuddyCode).toHaveBeenCalledTimes(3)
    expect(onCompleted).toHaveBeenCalledWith(
      expect.objectContaining({ access_token: 'at-final', refresh_token: 'rt-final' })
    )
    // 完成后自动停止，不再发起新请求。
    expect(oauth.polling.value).toBe(false)
    await vi.advanceTimersByTimeAsync(5000)
    expect(adminAPI.workbuddy.exchangeWorkBuddyCode).toHaveBeenCalledTimes(3)
  })

  it('stops polling after the timeout window elapses', async () => {
    vi.useFakeTimers()
    const oauth = await seedAuthUrl('cn')
    vi.mocked(adminAPI.workbuddy.exchangeWorkBuddyCode).mockResolvedValue({ status: 'pending' })

    const onCompleted = vi.fn()
    const onTimeout = vi.fn()
    oauth.startPolling({ onCompleted, onTimeout }, { realm: 'cn', intervalMs: 1000, timeoutMs: 3000 })

    await vi.advanceTimersByTimeAsync(3000)
    expect(onTimeout).toHaveBeenCalledTimes(1)
    expect(onCompleted).not.toHaveBeenCalled()
    expect(oauth.polling.value).toBe(false)

    const callsAtTimeout = vi.mocked(adminAPI.workbuddy.exchangeWorkBuddyCode).mock.calls.length
    await vi.advanceTimersByTimeAsync(5000)
    expect(vi.mocked(adminAPI.workbuddy.exchangeWorkBuddyCode).mock.calls.length).toBe(callsAtTimeout)
  })

  it('tolerates a transient request failure and continues polling', async () => {
    vi.useFakeTimers()
    const oauth = await seedAuthUrl('global')
    vi.mocked(adminAPI.workbuddy.exchangeWorkBuddyCode)
      .mockRejectedValueOnce({ status: 0, message: 'Network error' })
      .mockResolvedValueOnce({
        status: 'completed',
        token: { access_token: 'at-retry', realm: 'global' }
      })

    const onCompleted = vi.fn()
    const onError = vi.fn()
    oauth.startPolling(
      { onCompleted, onError },
      { realm: 'global', intervalMs: 1000, timeoutMs: 60_000 }
    )

    await vi.advanceTimersByTimeAsync(1000)
    expect(onError).toHaveBeenCalledTimes(1)

    await vi.advanceTimersByTimeAsync(1000)
    expect(onCompleted).toHaveBeenCalledWith(expect.objectContaining({ access_token: 'at-retry' }))
    expect(oauth.polling.value).toBe(false)
  })

  it('stopPolling clears pending timers so no further requests fire', async () => {
    vi.useFakeTimers()
    const oauth = await seedAuthUrl('cn')
    vi.mocked(adminAPI.workbuddy.exchangeWorkBuddyCode).mockResolvedValue({ status: 'pending' })

    oauth.startPolling({ onCompleted: vi.fn() }, { realm: 'cn', intervalMs: 1000 })
    await vi.advanceTimersByTimeAsync(1000)
    expect(adminAPI.workbuddy.exchangeWorkBuddyCode).toHaveBeenCalledTimes(1)

    oauth.stopPolling()
    expect(oauth.polling.value).toBe(false)
    await vi.advanceTimersByTimeAsync(10_000)
    expect(adminAPI.workbuddy.exchangeWorkBuddyCode).toHaveBeenCalledTimes(1)
  })
})

describe('useWorkBuddyOAuth.buildCredentials', () => {
  it('builds snake_case credentials and omits empty fields', () => {
    const oauth = useWorkBuddyOAuth()

    const credentials = oauth.buildCredentials({
      access_token: 'at',
      refresh_token: 'rt',
      uid: 'u-9',
      enterprise_id: 'ent-9',
      domain: 'www.workbuddy.ai',
      realm: 'global',
      nickname: 'nick',
      expires_at: 1_900_000_000
    })

    expect(credentials).toEqual({
      access_token: 'at',
      refresh_token: 'rt',
      uid: 'u-9',
      enterprise_id: 'ent-9',
      domain: 'www.workbuddy.ai',
      realm: 'global',
      nickname: 'nick',
      expires_at: 1_900_000_000
    })
    expect(oauth.buildCredentials({ access_token: 'at-only' })).toEqual({ access_token: 'at-only' })
  })
})
