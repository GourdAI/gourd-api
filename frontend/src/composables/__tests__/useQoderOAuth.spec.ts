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
        'admin.accounts.qoder.oauthFailed': 'OAuth 授权失败，请重试。'
      }
      return messages[key] ?? key
    }
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    qoder: {
      generateQoderAuthUrl: vi.fn(),
      exchangeQoderCode: vi.fn()
    }
  }
}))

import { useQoderOAuth } from '@/composables/useQoderOAuth'
import { adminAPI } from '@/api/admin'

describe('useQoderOAuth.generateAuthUrl', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.clearAllMocks()
  })

  it('saves loginUrl/loginId on success and forwards the realm', async () => {
    vi.mocked(adminAPI.qoder.generateQoderAuthUrl).mockResolvedValueOnce({
      login_url: 'https://qoder.com/oauth?nonce=n-1',
      login_id: 'lid-1'
    })
    const oauth = useQoderOAuth()

    const ok = await oauth.generateAuthUrl('cn')

    expect(ok).toBe(true)
    expect(oauth.loginUrl.value).toBe('https://qoder.com/oauth?nonce=n-1')
    expect(oauth.loginId.value).toBe('lid-1')
    expect(oauth.realm.value).toBe('cn')
    expect(adminAPI.qoder.generateQoderAuthUrl).toHaveBeenCalledWith('cn')
  })

  it('returns false and surfaces the error message on failure', async () => {
    vi.mocked(adminAPI.qoder.generateQoderAuthUrl).mockRejectedValueOnce({
      status: 500,
      message: 'boom'
    })
    const oauth = useQoderOAuth()

    const ok = await oauth.generateAuthUrl('global')

    expect(ok).toBe(false)
    expect(oauth.loginUrl.value).toBe('')
    expect(oauth.error.value).toBe('boom')
    expect(showErrorMock).toHaveBeenCalledWith('boom')
  })
})

describe('useQoderOAuth.pollForCredentials', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.clearAllMocks()
  })

  it('returns pending while credentials are still empty', async () => {
    vi.mocked(adminAPI.qoder.generateQoderAuthUrl).mockResolvedValueOnce({
      login_url: 'https://qoder.com/oauth?nonce=n-2',
      login_id: 'lid-2'
    })
    vi.mocked(adminAPI.qoder.exchangeQoderCode).mockResolvedValueOnce({
      credentials: {}
    })
    const oauth = useQoderOAuth()
    await oauth.generateAuthUrl('cn')

    const result = await oauth.pollForCredentials('cn')

    expect(result).toEqual({ status: 'pending' })
    expect(adminAPI.qoder.exchangeQoderCode).toHaveBeenCalledWith('lid-2', 'cn')
  })

  it('returns the credentials when completed', async () => {
    vi.mocked(adminAPI.qoder.generateQoderAuthUrl).mockResolvedValueOnce({
      login_url: 'https://qoder.com/oauth?nonce=n-3',
      login_id: 'lid-3'
    })
    vi.mocked(adminAPI.qoder.exchangeQoderCode).mockResolvedValueOnce({
      credentials: {
        access_token: 'at-1',
        refresh_token: 'rt-1',
        uid: 'u-1',
        nickname: 'tester',
        realm: 'global'
      },
      account_name: 'tester'
    })
    const oauth = useQoderOAuth()
    await oauth.generateAuthUrl('global')

    const result = await oauth.pollForCredentials('global')

    expect(result?.status).toBe('completed')
    if (result?.status !== 'completed') throw new Error('expected completed')
    expect(result.credentials.access_token).toBe('at-1')
    expect(result.credentials.refresh_token).toBe('rt-1')
    expect(result.credentials.uid).toBe('u-1')
    expect(result.accountName).toBe('tester')
  })

  it('returns null and maps structured errors when the exchange fails', async () => {
    vi.mocked(adminAPI.qoder.generateQoderAuthUrl).mockResolvedValueOnce({
      login_url: 'https://qoder.com/oauth?nonce=n-4',
      login_id: 'lid-4'
    })
    vi.mocked(adminAPI.qoder.exchangeQoderCode).mockRejectedValueOnce({
      status: 400,
      reason: 'QODER_OAUTH_LOGIN_ID_REQUIRED',
      message: 'login_id is required'
    })
    const oauth = useQoderOAuth()
    await oauth.generateAuthUrl('cn')

    const result = await oauth.pollForCredentials('cn')

    expect(result).toBeNull()
    expect(oauth.error.value).toBe('login_id is required')
  })

  it('rejects polling without a login_id', async () => {
    const oauth = useQoderOAuth()

    const result = await oauth.pollForCredentials('cn')

    expect(result).toBeNull()
    expect(oauth.error.value).toBe('OAuth 授权失败，请重试。')
    expect(adminAPI.qoder.exchangeQoderCode).not.toHaveBeenCalled()
  })
})

describe('useQoderOAuth.startPolling', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.clearAllMocks()
  })

  async function seedLoginUrl(realm: 'cn' | 'global' = 'cn') {
    vi.mocked(adminAPI.qoder.generateQoderAuthUrl).mockResolvedValueOnce({
      login_url: 'https://qoder.com/oauth?nonce=n-poll',
      login_id: 'lid-poll'
    })
    const oauth = useQoderOAuth()
    await oauth.generateAuthUrl(realm)
    return oauth
  }

  it('keeps polling while pending and stops on completion', async () => {
    vi.useFakeTimers()
    const oauth = await seedLoginUrl('cn')
    vi.mocked(adminAPI.qoder.exchangeQoderCode)
      .mockResolvedValueOnce({ credentials: {} })
      .mockResolvedValueOnce({ credentials: {} })
      .mockResolvedValueOnce({
        credentials: { access_token: 'at-final', refresh_token: 'rt-final' },
        account_name: 'final'
      })

    const onCompleted = vi.fn()
    oauth.startPolling({ onCompleted }, { realm: 'cn', intervalMs: 1000 })

    expect(oauth.polling.value).toBe(true)

    await vi.advanceTimersByTimeAsync(1000)
    expect(adminAPI.qoder.exchangeQoderCode).toHaveBeenCalledTimes(1)

    await vi.advanceTimersByTimeAsync(1000)
    expect(adminAPI.qoder.exchangeQoderCode).toHaveBeenCalledTimes(2)

    await vi.advanceTimersByTimeAsync(1000)
    expect(adminAPI.qoder.exchangeQoderCode).toHaveBeenCalledTimes(3)
    expect(onCompleted).toHaveBeenCalledWith(
      expect.objectContaining({ access_token: 'at-final', refresh_token: 'rt-final' }),
      'final'
    )
    // 完成后自动停止，不再发起新请求。
    expect(oauth.polling.value).toBe(false)
    await vi.advanceTimersByTimeAsync(5000)
    expect(adminAPI.qoder.exchangeQoderCode).toHaveBeenCalledTimes(3)
  })

  it('stops polling after the timeout window elapses', async () => {
    vi.useFakeTimers()
    const oauth = await seedLoginUrl('cn')
    vi.mocked(adminAPI.qoder.exchangeQoderCode).mockResolvedValue({ credentials: {} })

    const onCompleted = vi.fn()
    const onTimeout = vi.fn()
    oauth.startPolling({ onCompleted, onTimeout }, { realm: 'cn', intervalMs: 1000, timeoutMs: 3000 })

    await vi.advanceTimersByTimeAsync(3000)
    expect(onTimeout).toHaveBeenCalledTimes(1)
    expect(onCompleted).not.toHaveBeenCalled()
    expect(oauth.polling.value).toBe(false)

    const callsAtTimeout = vi.mocked(adminAPI.qoder.exchangeQoderCode).mock.calls.length
    await vi.advanceTimersByTimeAsync(5000)
    expect(vi.mocked(adminAPI.qoder.exchangeQoderCode).mock.calls.length).toBe(callsAtTimeout)
  })

  it('tolerates a transient request failure and continues polling', async () => {
    vi.useFakeTimers()
    const oauth = await seedLoginUrl('global')
    vi.mocked(adminAPI.qoder.exchangeQoderCode)
      .mockRejectedValueOnce({ status: 0, message: 'Network error' })
      .mockResolvedValueOnce({
        credentials: { access_token: 'at-retry', realm: 'global' }
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
    expect(onCompleted).toHaveBeenCalledWith(
      expect.objectContaining({ access_token: 'at-retry' }),
      ''
    )
    expect(oauth.polling.value).toBe(false)
  })

  it('stopPolling clears pending timers so no further requests fire', async () => {
    vi.useFakeTimers()
    const oauth = await seedLoginUrl('cn')
    vi.mocked(adminAPI.qoder.exchangeQoderCode).mockResolvedValue({ credentials: {} })

    oauth.startPolling({ onCompleted: vi.fn() }, { realm: 'cn', intervalMs: 1000 })
    await vi.advanceTimersByTimeAsync(1000)
    expect(adminAPI.qoder.exchangeQoderCode).toHaveBeenCalledTimes(1)

    oauth.stopPolling()
    expect(oauth.polling.value).toBe(false)
    await vi.advanceTimersByTimeAsync(10_000)
    expect(adminAPI.qoder.exchangeQoderCode).toHaveBeenCalledTimes(1)
  })
})
