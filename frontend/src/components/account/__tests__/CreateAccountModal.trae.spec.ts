import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const {
  createAccountMock,
  showErrorMock,
  showSuccessMock,
  exchangeTraeTokenMock,
  startTraeOAuthLoginMock,
  submitTraeOAuthCallbackMock,
  cancelTraeOAuthLoginMock,
} = vi.hoisted(() => ({
  createAccountMock: vi.fn(),
  showErrorMock: vi.fn(),
  showSuccessMock: vi.fn(),
  exchangeTraeTokenMock: vi.fn(),
  startTraeOAuthLoginMock: vi.fn(),
  submitTraeOAuthCallbackMock: vi.fn(),
  cancelTraeOAuthLoginMock: vi.fn(),
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: showErrorMock,
    showSuccess: showSuccessMock,
    showWarning: vi.fn(),
  }),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    get isSimpleMode() {
      return true
    },
  }),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      create: createAccountMock,
      probeUpstreamBilling: vi.fn(),
      syncUpstreamModels: vi.fn(),
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false }),
      importCodexSession: vi.fn(),
      createOpenAICodexPAT: vi.fn(),
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({}),
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([]),
    },
    trae: {
      exchangeTraeToken: exchangeTraeTokenMock,
      queryTraeCredits: vi.fn(),
      checkinTraeAccount: vi.fn(),
      startTraeOAuthLogin: startTraeOAuthLoginMock,
      submitTraeOAuthCallback: submitTraeOAuthCallbackMock,
      cancelTraeOAuthLogin: cancelTraeOAuthLoginMock,
    },
  },
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn().mockResolvedValue([]),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

import CreateAccountModal from '../CreateAccountModal.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
})

function mountModal() {
  return mount(CreateAccountModal, {
    props: { show: true, proxies: [], groups: [] },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        OAuthAuthorizationFlow: true,
        ConfirmDialog: true,
        Select: true,
        Icon: true,
        PlatformIcon: true,
        ProxySelector: true,
        ProxyAdBanner: true,
        GroupSelector: true,
        ModelWhitelistSelector: true,
        QuotaLimitCard: true,
      },
    },
  })
}

async function selectTrae(wrapper: ReturnType<typeof mountModal>) {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes('Trae'))
  expect(button).toBeDefined()
  await button?.trigger('click')
}

describe('CreateAccountModal Trae', () => {
  beforeEach(() => {
    createAccountMock.mockReset().mockResolvedValue({ id: 1 })
    showErrorMock.mockReset()
    showSuccessMock.mockReset()
    exchangeTraeTokenMock.mockReset()
    startTraeOAuthLoginMock.mockReset()
    submitTraeOAuthCallbackMock.mockReset()
    cancelTraeOAuthLoginMock.mockReset().mockResolvedValue(undefined)
  })

  it('renders the Trae credential form with the CN realm default endpoint', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await selectTrae(wrapper)

    expect(wrapper.find('[data-testid="trae-realm"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="trae-access-token"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="trae-ide-version-code"]').exists()).toBe(true)
    // Trae 凭据为 token，无 api_key：通用 API Key 输入与倍率探测开关都不渲染
    expect(wrapper.find('[data-testid="upstream-billing-auto-probe"]').exists()).toBe(false)
  })

  it('submits Trae credentials with the CN default endpoint', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await selectTrae(wrapper)
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Trae account')
    await wrapper.get('[data-testid="trae-access-token"]').setValue('trae-access-token')
    await wrapper.get('[data-testid="trae-realm"]').setValue('global')
    await wrapper.get('[data-testid="trae-ide-version-code"]').setValue('20261001')

    await wrapper.get('form#create-account-form').trigger('submit')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    const payload = createAccountMock.mock.calls[0][0]
    expect(payload.platform).toBe('trae')
    expect(payload.type).toBe('apikey')
    expect(payload.credentials).toMatchObject({
      access_token: 'trae-access-token',
      realm: 'global',
      base_url: 'https://a0ai-api-sg.byteintlapi.com',
      ide_version_code: '20261001',
    })
    // 固定协议平台：不带 account_mode / api_protocol；无探测资格：不带 probe 开关
    expect(payload.credentials.account_mode).toBeUndefined()
    expect(payload.credentials.api_protocol).toBeUndefined()
    expect(payload.upstream_billing_probe_enabled).toBeUndefined()
  })

  it('requires an access or refresh token before submitting', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await selectTrae(wrapper)
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Trae account')

    await wrapper.get('form#create-account-form').trigger('submit')
    await flushPromises()

    expect(showErrorMock).toHaveBeenCalledWith('admin.accounts.trae.credentialsRequired')
    expect(createAccountMock).not.toHaveBeenCalled()
  })

  it('exchanges the refresh token and refills both tokens plus the uid', async () => {
    exchangeTraeTokenMock.mockResolvedValue({
      success: true,
      realm: 'cn',
      access_token: 'rotated-access',
      refresh_token: 'rotated-refresh',
      uid: 'u-42',
    })
    const wrapper = mountModal()
    await flushPromises()
    await selectTrae(wrapper)
    await wrapper.get('[data-testid="trae-refresh-token"]').setValue('old-refresh')

    await wrapper.get('[data-testid="trae-exchange-token"]').trigger('click')
    await flushPromises()

    expect(exchangeTraeTokenMock).toHaveBeenCalledWith({
      realm: 'cn',
      refresh_token: 'old-refresh',
      proxy_id: undefined,
    })
    expect(wrapper.get<HTMLTextAreaElement>('[data-testid="trae-access-token"]').element.value).toBe(
      'rotated-access'
    )
    // 轮换后的 refresh_token 必须覆盖旧值
    expect(wrapper.get<HTMLInputElement>('[data-testid="trae-refresh-token"]').element.value).toBe(
      'rotated-refresh'
    )
    expect(wrapper.get<HTMLInputElement>('[data-testid="trae-uid"]').element.value).toBe('u-42')
    expect(showSuccessMock).toHaveBeenCalledWith('admin.accounts.trae.exchangeTokenSuccess')
  })

  it('surfaces the upstream error for a failed exchange', async () => {
    exchangeTraeTokenMock.mockResolvedValue({ success: false, realm: 'cn', error: 'bad token' })
    const wrapper = mountModal()
    await flushPromises()
    await selectTrae(wrapper)
    await wrapper.get('[data-testid="trae-refresh-token"]').setValue('stale-refresh')

    await wrapper.get('[data-testid="trae-exchange-token"]').trigger('click')
    await flushPromises()

    expect(showErrorMock).toHaveBeenCalledWith('bad token')
    expect(showSuccessMock).not.toHaveBeenCalled()
  })

  it('renders the browser login block alongside the refresh-token exchange entry', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await selectTrae(wrapper)

    // 浏览器登录入口与粘贴 refreshToken 换票入口并存，互不干扰
    expect(wrapper.find('[data-testid="trae-oauth-login"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="trae-exchange-token"]').exists()).toBe(true)
    // 尚未发起会话时：粘贴框 / 提交按钮 / 倒计时都不渲染
    expect(wrapper.find('[data-testid="trae-oauth-callback"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="trae-oauth-submit"]').exists()).toBe(false)
  })

  it('opens the login URL and reveals the paste-and-submit flow after login is started', async () => {
    startTraeOAuthLoginMock.mockResolvedValue({
      login_id: 'lid-1',
      login_url: 'https://grow.dev/authorize?state=abc',
      callback_url_prefix: 'http://127.0.0.1:53489/authorize',
      expires_at: Math.floor(Date.now() / 1000) + 900,
      needs_paste: true,
    })
    const openSpy = vi.spyOn(window, 'open').mockReturnValue({ opener: {} } as unknown as Window)
    const wrapper = mountModal()
    await flushPromises()
    await selectTrae(wrapper)

    await wrapper.get('[data-testid="trae-oauth-login"]').trigger('click')
    await flushPromises()

    expect(startTraeOAuthLoginMock).toHaveBeenCalledWith({ realm: 'cn' })
    // 不能带 noopener feature：规范要求此时 window.open 恒返回 null，会把成功打开误报为被拦截。
    expect(openSpy).toHaveBeenCalledWith('https://grow.dev/authorize?state=abc', '_blank')
    expect(openSpy.mock.results[0]?.value?.opener).toBeNull()
    expect(wrapper.get<HTMLInputElement>('[data-testid="trae-oauth-url"]').element.value).toBe(
      'https://grow.dev/authorize?state=abc'
    )
    // 降级可点链接 + 倒计时 + 粘贴框 + 提交按钮均可见
    expect(wrapper.find('[data-testid="trae-oauth-open"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="trae-oauth-countdown"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="trae-oauth-callback"]').exists()).toBe(true)

    const submit = wrapper.get<HTMLButtonElement>('[data-testid="trae-oauth-submit"]')
    // 粘贴框为空时提交禁用
    expect(submit.element.disabled).toBe(true)

    await wrapper.get('[data-testid="trae-oauth-callback"]').setValue(
      'http://127.0.0.1:53489/authorize?code=c-1'
    )
    expect(wrapper.get<HTMLButtonElement>('[data-testid="trae-oauth-submit"]').element.disabled).toBe(
      false
    )

    submitTraeOAuthCallbackMock.mockResolvedValue({
      status: 'completed',
      credentials: {
        access_token: 'at-new',
        refresh_token: 'rt-new',
        uid: 'u-9',
        device_id: 'd-9',
        machine_id: 'm-9',
        expires_at: '1893456000',
        nickname: 'trae-user',
        device_public_key: 'pk-9',
      },
      account_name: 'trae-user',
    })

    await wrapper.get('[data-testid="trae-oauth-submit"]').trigger('click')
    await flushPromises()

    expect(submitTraeOAuthCallbackMock).toHaveBeenCalledWith({
      login_id: 'lid-1',
      callback_url: 'http://127.0.0.1:53489/authorize?code=c-1',
    })
    expect(wrapper.get<HTMLTextAreaElement>('[data-testid="trae-access-token"]').element.value).toBe(
      'at-new'
    )
    expect(wrapper.get<HTMLInputElement>('[data-testid="trae-refresh-token"]').element.value).toBe(
      'rt-new'
    )
    expect(wrapper.get<HTMLInputElement>('[data-testid="trae-uid"]').element.value).toBe('u-9')
    expect(wrapper.get('[data-testid="trae-oauth-status"]').text()).toBe(
      'admin.accounts.trae.oauth.success'
    )
    expect(showSuccessMock).toHaveBeenCalledWith('admin.accounts.trae.oauth.success')
    // 账号名为空时用后端回传的 account_name 自动回填
    expect(wrapper.get<HTMLInputElement>('form#create-account-form input[type="text"]').element.value).toBe(
      'trae-user'
    )

    // 凭据提交：已知键走表单字段，未知键（device_public_key）原样透传合并
    await wrapper.get('form#create-account-form').trigger('submit')
    await flushPromises()
    const payload = createAccountMock.mock.calls[0][0]
    expect(payload.credentials).toMatchObject({
      access_token: 'at-new',
      refresh_token: 'rt-new',
      uid: 'u-9',
      device_public_key: 'pk-9',
      expires_at: 1893456000,
    })

    openSpy.mockRestore()
  })

  it('reports a blocked popup only when window.open really returns null', async () => {
    startTraeOAuthLoginMock.mockResolvedValue({
      login_id: 'lid-2',
      login_url: 'https://grow.dev/authorize?state=xyz',
      callback_url_prefix: 'http://127.0.0.1:53490/authorize',
      expires_at: Math.floor(Date.now() / 1000) + 900,
      needs_paste: true,
    })
    const openSpy = vi.spyOn(window, 'open').mockReturnValue(null as unknown as Window)
    const wrapper = mountModal()
    await flushPromises()
    await selectTrae(wrapper)

    await wrapper.get('[data-testid="trae-oauth-login"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="trae-oauth-status"]').text()).toBe(
      'admin.accounts.trae.oauth.popupBlocked'
    )
    openSpy.mockRestore()
  })

  it('clears a leftover browser-login session when the modal is reopened', async () => {
    startTraeOAuthLoginMock.mockResolvedValue({
      login_id: 'lid-3',
      login_url: 'https://grow.dev/authorize?state=old',
      callback_url_prefix: 'http://127.0.0.1:53491/authorize',
      expires_at: Math.floor(Date.now() / 1000) + 900,
      needs_paste: true,
    })
    submitTraeOAuthCallbackMock.mockResolvedValue({
      status: 'completed',
      credentials: {
        access_token: 'at-old',
        refresh_token: 'rt-old',
        device_public_key: 'pk-old',
        device_private_key: 'sk-old',
        device_id: 'd-old',
        machine_id: 'm-old',
      },
      account_name: 'old-user',
    })
    const openSpy = vi.spyOn(window, 'open').mockReturnValue({ opener: {} } as unknown as Window)
    const wrapper = mountModal()
    await flushPromises()
    await selectTrae(wrapper)

    await wrapper.get('[data-testid="trae-oauth-login"]').trigger('click')
    await flushPromises()
    await wrapper
      .get('[data-testid="trae-oauth-callback"]')
      .setValue('http://127.0.0.1:53491/authorize?code=c-old')
    await wrapper.get('[data-testid="trae-oauth-submit"]').trigger('click')
    await flushPromises()

    // 关闭再打开：表单被重置（平台回落 anthropic），旧会话状态一并清理
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await flushPromises()

    // 重新选 Trae：不得残留旧授权链接 / 旧粘贴框 / 旧「已成功」提示
    await selectTrae(wrapper)
    await flushPromises()
    expect(wrapper.find('[data-testid="trae-oauth-url"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="trae-oauth-callback"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="trae-oauth-status"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="trae-oauth-login"]').exists()).toBe(true)
    expect(wrapper.get<HTMLTextAreaElement>('[data-testid="trae-access-token"]').element.value).toBe('')

    // 重新建号：上一次登录的 extras（含设备私钥）不得混入新账号凭据
    await wrapper.get('form#create-account-form input[type="text"]').setValue('fresh trae')
    await wrapper.get('[data-testid="trae-access-token"]').setValue('manual-token')
    await wrapper.get('form#create-account-form').trigger('submit')
    await flushPromises()

    const payload = createAccountMock.mock.calls[0][0]
    expect(payload.credentials.access_token).toBe('manual-token')
    expect(payload.credentials.device_private_key).toBeUndefined()
    expect(payload.credentials.device_public_key).toBeUndefined()
    expect(payload.credentials.device_id).toBeUndefined()
    openSpy.mockRestore()
  })
})
