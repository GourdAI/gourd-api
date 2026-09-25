import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const {
  showErrorMock,
  showSuccessMock,
  createAccountMock,
  generateQoderAuthUrlMock,
  exchangeQoderCodeMock,
  windowOpenMock,
} = vi.hoisted(() => ({
  showErrorMock: vi.fn(),
  showSuccessMock: vi.fn(),
  createAccountMock: vi.fn(),
  generateQoderAuthUrlMock: vi.fn(),
  exchangeQoderCodeMock: vi.fn(),
  windowOpenMock: vi.fn(),
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
    qoder: {
      generateQoderAuthUrl: generateQoderAuthUrlMock,
      exchangeQoderCode: exchangeQoderCodeMock,
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

async function selectQoder(wrapper: ReturnType<typeof mountModal>) {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes('Qoder'))
  expect(button).toBeDefined()
  await button?.trigger('click')
}

describe('CreateAccountModal Qoder OAuth', () => {
  beforeEach(() => {
    vi.stubGlobal('open', windowOpenMock)
    showErrorMock.mockReset()
    showSuccessMock.mockReset()
    windowOpenMock.mockReset()
    createAccountMock.mockReset().mockResolvedValue({ id: 1 })
    generateQoderAuthUrlMock.mockReset().mockResolvedValue({
      login_url: 'https://qoder.com/oauth?nonce=n-1',
      login_id: 'lid-1',
    })
    exchangeQoderCodeMock.mockReset().mockResolvedValue({ credentials: {} })
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it('selecting Qoder shows the credential form and generates the login URL', async () => {
    vi.useFakeTimers()
    const wrapper = mountModal()
    await selectQoder(wrapper)

    // qoder 凭据表单可见（realm/baseUrl/access_token），API Key 输入不可见。
    expect(wrapper.find('[data-testid="qoder-realm"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="qoder-access-token"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="qoder-oauth-login"]').exists()).toBe(true)

    await wrapper.get('[data-testid="qoder-oauth-login"]').trigger('click')
    await flushPromises()

    expect(generateQoderAuthUrlMock).toHaveBeenCalledWith('cn')
    expect(windowOpenMock).toHaveBeenCalledWith(
      'https://qoder.com/oauth?nonce=n-1',
      '_blank',
      'noopener,noreferrer'
    )
    const urlInput = wrapper.get<HTMLInputElement>('[data-testid="qoder-oauth-url"]')
    expect(urlInput.element.value).toBe('https://qoder.com/oauth?nonce=n-1')
    // 轮询指示可见（startPolling 已启动）。
    expect(wrapper.find('[data-testid="qoder-oauth-polling"]').exists()).toBe(true)

    await vi.advanceTimersByTimeAsync(2500)
    expect(exchangeQoderCodeMock).toHaveBeenCalledWith('lid-1', 'cn')

    wrapper.unmount()
  })

  it('back-fills credentials and shows success once the exchange completes', async () => {
    vi.useFakeTimers()
    exchangeQoderCodeMock
      .mockResolvedValueOnce({ credentials: {} })
      .mockResolvedValueOnce({
        credentials: {
          access_token: 'qd-access',
          refresh_token: 'qd-refresh',
          uid: 'u-42',
          realm: 'cn',
        },
        account_name: 'qd-user',
      })

    const wrapper = mountModal()
    await selectQoder(wrapper)
    await wrapper.get('[data-testid="qoder-oauth-login"]').trigger('click')
    await flushPromises()

    await vi.advanceTimersByTimeAsync(2500)
    await vi.advanceTimersByTimeAsync(2500)
    await flushPromises()

    expect(wrapper.get<HTMLInputElement>('[data-testid="qoder-access-token"]').element.value).toBe(
      'qd-access'
    )
    expect(
      wrapper.get<HTMLInputElement>('[data-testid="qoder-refresh-token"]').element.value
    ).toBe('qd-refresh')
    expect(wrapper.get<HTMLInputElement>('[data-testid="qoder-uid"]').element.value).toBe('u-42')
    // 成功提示 + toast；轮询已停止。
    expect(wrapper.get('[data-testid="qoder-oauth-status"]').text()).toBe(
      'admin.accounts.qoder.oauthSuccess'
    )
    expect(showSuccessMock).toHaveBeenCalledWith('admin.accounts.qoder.oauthSuccess')
    expect(wrapper.find('[data-testid="qoder-oauth-polling"]').exists()).toBe(false)

    // 完成后不再发起新请求。
    await vi.advanceTimersByTimeAsync(10_000)
    expect(exchangeQoderCodeMock).toHaveBeenCalledTimes(2)

    wrapper.unmount()
  })

  it('reports a timeout message and offers retry after the timeout window', async () => {
    vi.useFakeTimers()
    exchangeQoderCodeMock.mockResolvedValue({ credentials: {} })

    const wrapper = mountModal()
    await selectQoder(wrapper)
    await wrapper.get('[data-testid="qoder-oauth-login"]').trigger('click')
    await flushPromises()

    // 推进到 5 分钟超时窗口之后。
    await vi.advanceTimersByTimeAsync(5 * 60 * 1000)
    await flushPromises()

    expect(wrapper.get('[data-testid="qoder-oauth-status"]').text()).toBe(
      'admin.accounts.qoder.oauthTimeout'
    )
    expect(wrapper.find('[data-testid="qoder-oauth-polling"]').exists()).toBe(false)
    // 超时后重试按钮可见。
    expect(wrapper.find('[data-testid="qoder-oauth-retry"]').exists()).toBe(true)

    const callsAtTimeout = exchangeQoderCodeMock.mock.calls.length
    await vi.advanceTimersByTimeAsync(10_000)
    expect(exchangeQoderCodeMock.mock.calls.length).toBe(callsAtTimeout)

    wrapper.unmount()
  })

  it('stops polling and clears the login link when the realm changes', async () => {
    vi.useFakeTimers()
    const wrapper = mountModal()
    await selectQoder(wrapper)
    await wrapper.get('[data-testid="qoder-oauth-login"]').trigger('click')
    await flushPromises()

    await vi.advanceTimersByTimeAsync(2500)
    expect(exchangeQoderCodeMock).toHaveBeenCalledTimes(1)

    await wrapper.get('[data-testid="qoder-realm"]').setValue('global')
    await flushPromises()

    // 授权链接被清空（等待重新生成），轮询停止。
    expect(wrapper.find('[data-testid="qoder-oauth-url"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="qoder-oauth-polling"]').exists()).toBe(false)

    const callsAtRealmSwitch = exchangeQoderCodeMock.mock.calls.length
    await vi.advanceTimersByTimeAsync(30_000)
    expect(exchangeQoderCodeMock.mock.calls.length).toBe(callsAtRealmSwitch)

    wrapper.unmount()
  })

  it('submits qoder credentials without the probe flag and hides the probe toggle', async () => {
    const wrapper = mountModal()
    await selectQoder(wrapper)
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Qoder account')
    await wrapper.get('[data-testid="qoder-access-token"]').setValue('qd-access-token')
    await wrapper.get('[data-testid="qoder-refresh-token"]').setValue('qd-refresh-token')
    await wrapper.get('[data-testid="qoder-uid"]').setValue('u-7')
    await wrapper.get('[data-testid="qoder-nickname"]').setValue('nick-7')

    // qoder 凭据为 token 无 api_key，后端不具备探测资格：开关隐藏。
    expect(wrapper.find('[data-testid="upstream-billing-auto-probe"]').exists()).toBe(false)

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    const credentials = createAccountMock.mock.calls[0]?.[0]?.credentials
    expect(credentials).toMatchObject({
      realm: 'cn',
      base_url: 'https://gateway.qoder.com.cn',
      access_token: 'qd-access-token',
      refresh_token: 'qd-refresh-token',
      uid: 'u-7',
      nickname: 'nick-7'
    })
    // qoder 无探测资格：不提交 probe 字段，也不得携带 api_key。
    expect(createAccountMock.mock.calls[0]?.[0]?.upstream_billing_probe_enabled).toBeUndefined()
    expect(credentials?.api_key).toBeUndefined()
  })

  it('requires an access token before submitting qoder credentials', async () => {
    const wrapper = mountModal()
    await selectQoder(wrapper)
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Qoder account')

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).not.toHaveBeenCalled()
  })
})
