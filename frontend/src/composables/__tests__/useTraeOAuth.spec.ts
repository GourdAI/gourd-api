import { afterEach, describe, expect, it, vi } from 'vitest'

const {
  showErrorMock,
  startTraeOAuthLoginMock,
  submitTraeOAuthCallbackMock,
  cancelTraeOAuthLoginMock,
} = vi.hoisted(() => ({
  showErrorMock: vi.fn(),
  startTraeOAuthLoginMock: vi.fn(),
  submitTraeOAuthCallbackMock: vi.fn(),
  cancelTraeOAuthLoginMock: vi.fn(),
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: showErrorMock
  })
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => {
      const messages: Record<string, string> = {
        'admin.accounts.trae.oauth.startFailed': '生成 Trae 登录链接失败，请重试。',
        'admin.accounts.trae.oauth.expired': '登录会话已过期，请重新发起',
        'admin.accounts.trae.oauth.callbackMissing': '请先把浏览器地址栏整串链接粘回输入框',
        'admin.accounts.trae.oauth.sessionMissing': '尚未发起浏览器登录，请先点「使用浏览器登录」',
        'admin.accounts.trae.oauth.submitFailed': 'Trae 登录提交失败，请重新发起登录。',
        'admin.accounts.trae.oauth.errors.TRAE_LOGIN_NOT_FOUND':
          '登录会话不存在（服务可能已重启），请重新发起登录。',
        'admin.accounts.trae.oauth.errors.TRAE_LOGIN_EXPIRED':
          '登录会话已过期，请重新发起登录并尽快完成操作。'
      }
      // extractI18nErrorMessage 会先查 `<namespace>.<REASON>`：命中返回译文，
      // 未命中返回 key 本身，以此驱动回落到 err.message。
      if (key.startsWith('admin.accounts.trae.oauth.errors.')) {
        return messages[key] ?? key
      }
      return messages[key] ?? key
    }
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    trae: {
      startTraeOAuthLogin: startTraeOAuthLoginMock,
      submitTraeOAuthCallback: submitTraeOAuthCallbackMock,
      cancelTraeOAuthLogin: cancelTraeOAuthLoginMock
    }
  }
}))

import { useTraeOAuth } from '@/composables/useTraeOAuth'

const nowSeconds = () => Math.floor(Date.now() / 1000)

function authUrlPayload(overrides: Record<string, unknown> = {}) {
  return {
    login_id: 'lid-1',
    login_url: 'https://grow.dev/authorize?state=abc',
    callback_url_prefix: 'http://127.0.0.1:53489/authorize',
    expires_at: nowSeconds() + 15 * 60,
    needs_paste: true,
    ...overrides
  }
}

describe('useTraeOAuth.startLogin', () => {
  afterEach(() => {
    vi.clearAllMocks()
  })

  it('stores the session, forwards the realm and starts the countdown', async () => {
    startTraeOAuthLoginMock.mockResolvedValueOnce(authUrlPayload())
    const oauth = useTraeOAuth()

    const ok = await oauth.startLogin('cn')

    expect(ok).toBe(true)
    expect(oauth.loginUrl.value).toBe('https://grow.dev/authorize?state=abc')
    expect(oauth.loginId.value).toBe('lid-1')
    expect(oauth.callbackUrlPrefix.value).toBe('http://127.0.0.1:53489/authorize')
    expect(oauth.realm.value).toBe('cn')
    expect(oauth.sessionActive.value).toBe(true)
    expect(oauth.expired.value).toBe(false)
    // 倒计时读数形如 mm:ss（15 分钟窗口内）
    expect(oauth.countdownText.value).toMatch(/^\d{2}:\d{2}$/)
    expect(oauth.remainingMs.value).toBeGreaterThan(13 * 60 * 1000)
    expect(startTraeOAuthLoginMock).toHaveBeenCalledWith({ realm: 'cn' })
  })

  it('attaches proxy_id when one is selected', async () => {
    startTraeOAuthLoginMock.mockResolvedValueOnce(authUrlPayload())
    const oauth = useTraeOAuth()

    await oauth.startLogin('global', 7)

    expect(startTraeOAuthLoginMock).toHaveBeenCalledWith({ realm: 'global', proxy_id: 7 })
    expect(oauth.realm.value).toBe('global')
  })

  it('surfaces the API error and returns false when the link cannot be generated', async () => {
    startTraeOAuthLoginMock.mockRejectedValueOnce({ status: 500, message: 'boom' })
    const oauth = useTraeOAuth()

    const ok = await oauth.startLogin('cn')

    expect(ok).toBe(false)
    expect(oauth.loginId.value).toBe('')
    expect(oauth.error.value).toBe('boom')
    expect(showErrorMock).toHaveBeenCalledWith('boom')
  })

  it('falls back to the 15-minute TTL when expires_at is missing', async () => {
    startTraeOAuthLoginMock.mockResolvedValueOnce(authUrlPayload({ expires_at: 0 }))
    const oauth = useTraeOAuth()

    await oauth.startLogin('cn')

    // 兜底后仍有有效倒计时，不会把用户直接挡在门外
    expect(oauth.sessionActive.value).toBe(true)
    expect(oauth.remainingMs.value).toBeGreaterThan(14 * 60 * 1000)
  })

  it('ignores a concurrent start while the first link is still being generated', async () => {
    let resolveLogin: (value: unknown) => void = () => {}
    startTraeOAuthLoginMock.mockReturnValueOnce(
      new Promise((resolve) => {
        resolveLogin = resolve
      })
    )
    const oauth = useTraeOAuth()

    const first = oauth.startLogin('cn')
    const second = await oauth.startLogin('cn')

    // 第二次点击必须短路：并发两个会话会互相覆盖 login_id，后粘回者必报 TRACE_MISMATCH。
    expect(second).toBe(false)
    expect(startTraeOAuthLoginMock).toHaveBeenCalledTimes(1)

    resolveLogin(authUrlPayload())
    await expect(first).resolves.toBe(true)
    expect(oauth.loginId.value).toBe('lid-1')
  })
})

describe('useTraeOAuth.submitCallback', () => {
  afterEach(() => {
    vi.clearAllMocks()
  })

  it('returns credentials and account name on success and invalidates the session', async () => {
    startTraeOAuthLoginMock.mockResolvedValueOnce(authUrlPayload())
    submitTraeOAuthCallbackMock.mockResolvedValueOnce({
      status: 'completed',
      credentials: {
        access_token: 'at-1',
        refresh_token: 'rt-1',
        uid: 'u-1',
        nickname: 'tester',
        device_id: 'd-1',
        device_public_key: 'pk-1'
      },
      account_name: 'tester'
    })
    const oauth = useTraeOAuth()
    await oauth.startLogin('cn')

    const result = await oauth.submitCallback('http://127.0.0.1:53489/authorize?code=c-1')

    expect(result).not.toBeNull()
    expect(result?.accountName).toBe('tester')
    expect(result?.credentials.access_token).toBe('at-1')
    expect(result?.credentials.device_public_key).toBe('pk-1')
    expect(submitTraeOAuthCallbackMock).toHaveBeenCalledWith({
      login_id: 'lid-1',
      callback_url: 'http://127.0.0.1:53489/authorize?code=c-1'
    })
    // 换票后会话即作废：不再允许 cancel（避免多余请求）
    expect(oauth.loginId.value).toBe('')
  })

  it('rejects an empty paste without calling the backend', async () => {
    startTraeOAuthLoginMock.mockResolvedValueOnce(authUrlPayload())
    const oauth = useTraeOAuth()
    await oauth.startLogin('cn')

    const result = await oauth.submitCallback('   ')

    expect(result).toBeNull()
    expect(oauth.error.value).toBe('请先把浏览器地址栏整串链接粘回输入框')
    expect(submitTraeOAuthCallbackMock).not.toHaveBeenCalled()
  })

  it('rejects submitting before a session exists', async () => {
    const oauth = useTraeOAuth()

    const result = await oauth.submitCallback('http://127.0.0.1:1/authorize?code=x')

    expect(result).toBeNull()
    expect(oauth.error.value).toBe('尚未发起浏览器登录，请先点「使用浏览器登录」')
    expect(submitTraeOAuthCallbackMock).not.toHaveBeenCalled()
  })

  it('maps the 410 expired session and clears the pasted URL', async () => {
    startTraeOAuthLoginMock.mockResolvedValueOnce(authUrlPayload())
    submitTraeOAuthCallbackMock.mockRejectedValueOnce({
      status: 410,
      reason: 'TRAE_LOGIN_EXPIRED',
      message: 'login session expired, restart login'
    })
    const oauth = useTraeOAuth()
    await oauth.startLogin('cn')
    oauth.pastedUrl.value = 'http://127.0.0.1:53489/authorize?code=stale'

    const result = await oauth.submitCallback()

    expect(result).toBeNull()
    expect(oauth.error.value).toBe('登录会话已过期，请重新发起登录并尽快完成操作。')
    expect(oauth.pastedUrl.value).toBe('')
    expect(oauth.expired.value).toBe(true)
    expect(oauth.sessionActive.value).toBe(false)
  })

  it('treats a 404 missing session as gone and clears the pasted URL', async () => {
    startTraeOAuthLoginMock.mockResolvedValueOnce(authUrlPayload())
    submitTraeOAuthCallbackMock.mockRejectedValueOnce({
      status: 404,
      reason: 'TRAE_LOGIN_NOT_FOUND',
      message: 'login session not found'
    })
    const oauth = useTraeOAuth()
    await oauth.startLogin('cn')
    oauth.pastedUrl.value = 'http://127.0.0.1:53489/authorize?code=stale'

    const result = await oauth.submitCallback()

    expect(result).toBeNull()
    expect(oauth.error.value).toBe('登录会话不存在（服务可能已重启），请重新发起登录。')
    expect(oauth.pastedUrl.value).toBe('')
    expect(oauth.expired.value).toBe(true)
  })

  it('ignores a concurrent submit while one is already in flight', async () => {
    startTraeOAuthLoginMock.mockResolvedValueOnce(authUrlPayload())
    let resolveSubmit: (value: unknown) => void = () => {}
    submitTraeOAuthCallbackMock.mockReturnValueOnce(
      new Promise((resolve) => {
        resolveSubmit = resolve
      })
    )
    const oauth = useTraeOAuth()
    await oauth.startLogin('cn')

    const first = oauth.submitCallback('http://127.0.0.1:53489/authorize?code=c-1')
    const second = await oauth.submitCallback('http://127.0.0.1:53489/authorize?code=c-1')

    // 第二次点击必须直接短路：重复提交会因会话一次性消费拿到 404，覆盖首次的成功状态。
    expect(second).toBeNull()
    expect(submitTraeOAuthCallbackMock).toHaveBeenCalledTimes(1)

    resolveSubmit({
      status: 'completed',
      credentials: { access_token: 'at-1' },
      account_name: 'tester'
    })
    const result = await first
    expect(result?.credentials.access_token).toBe('at-1')
  })

  it('keeps the pasted URL for a 400 malformed callback', async () => {
    startTraeOAuthLoginMock.mockResolvedValueOnce(authUrlPayload())
    submitTraeOAuthCallbackMock.mockRejectedValueOnce({
      status: 400,
      reason: 'TRAE_LOGIN_CALLBACK_INVALID',
      message: 'callback url invalid'
    })
    const oauth = useTraeOAuth()
    await oauth.startLogin('cn')
    // 用户自己粘回的链接粘错了：400 不能清空，否则用户得从头再找一次地址栏
    oauth.pastedUrl.value = 'http://127.0.0.1:53489/authorize'

    const result = await oauth.submitCallback()

    expect(result).toBeNull()
    expect(oauth.error.value).toBe('callback url invalid')
    expect(oauth.pastedUrl.value).toBe('http://127.0.0.1:53489/authorize')
    expect(oauth.expired.value).toBe(false)
  })

  it('refuses to submit once the countdown has elapsed', async () => {
    vi.useFakeTimers()
    try {
      startTraeOAuthLoginMock.mockResolvedValueOnce(
        authUrlPayload({ expires_at: Math.floor(Date.now() / 1000) + 2 })
      )
      const oauth = useTraeOAuth()
      await oauth.startLogin('cn')

      await vi.advanceTimersByTimeAsync(3000)
      expect(oauth.expired.value).toBe(true)

      const result = await oauth.submitCallback('http://127.0.0.1:53489/authorize?code=c-1')

      expect(result).toBeNull()
      expect(oauth.error.value).toBe('登录会话已过期，请重新发起')
      expect(submitTraeOAuthCallbackMock).not.toHaveBeenCalled()
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('useTraeOAuth.cancelLogin', () => {
  afterEach(() => {
    vi.clearAllMocks()
  })

  it('cancels the active session once and is idempotent', async () => {
    startTraeOAuthLoginMock.mockResolvedValueOnce(authUrlPayload())
    cancelTraeOAuthLoginMock.mockResolvedValueOnce(undefined)
    const oauth = useTraeOAuth()
    await oauth.startLogin('cn')

    await oauth.cancelLogin()
    await oauth.cancelLogin()

    expect(cancelTraeOAuthLoginMock).toHaveBeenCalledTimes(1)
    expect(cancelTraeOAuthLoginMock).toHaveBeenCalledWith('lid-1')
  })

  it('swallows a backend failure silently', async () => {
    startTraeOAuthLoginMock.mockResolvedValueOnce(authUrlPayload())
    cancelTraeOAuthLoginMock.mockRejectedValueOnce({ status: 500, message: 'boom' })
    const oauth = useTraeOAuth()
    await oauth.startLogin('cn')

    await expect(oauth.cancelLogin()).resolves.toBeUndefined()
    expect(showErrorMock).not.toHaveBeenCalled()
  })

  it('does nothing when no session was started', async () => {
    const oauth = useTraeOAuth()

    await oauth.cancelLogin()

    expect(cancelTraeOAuthLoginMock).not.toHaveBeenCalled()
  })
})
