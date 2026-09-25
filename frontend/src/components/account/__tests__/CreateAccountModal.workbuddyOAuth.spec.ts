import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const {
  showErrorMock,
  showSuccessMock,
  generateWorkBuddyAuthUrlMock,
  exchangeWorkBuddyCodeMock,
} = vi.hoisted(() => ({
  showErrorMock: vi.fn(),
  showSuccessMock: vi.fn(),
  generateWorkBuddyAuthUrlMock: vi.fn(),
  exchangeWorkBuddyCodeMock: vi.fn(),
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
      create: vi.fn(),
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
    workbuddy: {
      generateWorkBuddyAuthUrl: generateWorkBuddyAuthUrlMock,
      exchangeWorkBuddyCode: exchangeWorkBuddyCodeMock,
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

async function selectWorkBuddy(wrapper: ReturnType<typeof mountModal>) {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes('WorkBuddy'))
  expect(button).toBeDefined()
  await button?.trigger('click')
}

describe('CreateAccountModal WorkBuddy OAuth', () => {
  beforeEach(() => {
    showErrorMock.mockReset()
    showSuccessMock.mockReset()
    generateWorkBuddyAuthUrlMock.mockReset().mockResolvedValue({
      auth_url: 'https://copilot.tencent.com/oauth?state=st-1',
      state: 'st-1',
      realm: 'cn',
    })
    exchangeWorkBuddyCodeMock.mockReset().mockResolvedValue({ status: 'pending' })
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('generates the auth URL with the selected realm and starts polling', async () => {
    vi.useFakeTimers()
    const wrapper = mountModal()
    await selectWorkBuddy(wrapper)

    expect(wrapper.find('[data-testid="workbuddy-oauth-login"]').exists()).toBe(true)
    await wrapper.get('[data-testid="workbuddy-oauth-login"]').trigger('click')
    await flushPromises()

    expect(generateWorkBuddyAuthUrlMock).toHaveBeenCalledWith({ realm: 'cn' })
    const urlInput = wrapper.get<HTMLInputElement>('[data-testid="workbuddy-oauth-url"]')
    expect(urlInput.element.value).toBe('https://copilot.tencent.com/oauth?state=st-1')
    // 轮询指示可见（startPolling 已启动）。
    expect(wrapper.find('[data-testid="workbuddy-oauth-polling"]').exists()).toBe(true)

    await vi.advanceTimersByTimeAsync(2500)
    expect(exchangeWorkBuddyCodeMock).toHaveBeenCalledWith({ state: 'st-1', realm: 'cn' })

    wrapper.unmount()
  })

  it('back-fills credentials and shows success once the exchange completes', async () => {
    vi.useFakeTimers()
    exchangeWorkBuddyCodeMock
      .mockResolvedValueOnce({ status: 'pending' })
      .mockResolvedValueOnce({
        status: 'completed',
        token: {
          access_token: 'wb-access',
          refresh_token: 'wb-refresh',
          uid: 'u-42',
          enterprise_id: 'ent-42',
          realm: 'cn',
        },
      })

    const wrapper = mountModal()
    await selectWorkBuddy(wrapper)
    await wrapper.get('[data-testid="workbuddy-oauth-login"]').trigger('click')
    await flushPromises()

    await vi.advanceTimersByTimeAsync(2500)
    await vi.advanceTimersByTimeAsync(2500)
    await flushPromises()

    expect(wrapper.get<HTMLInputElement>('[data-testid="workbuddy-access-token"]').element.value).toBe(
      'wb-access'
    )
    expect(
      wrapper.get<HTMLInputElement>('[data-testid="workbuddy-refresh-token"]').element.value
    ).toBe('wb-refresh')
    expect(wrapper.get<HTMLInputElement>('[data-testid="workbuddy-uid"]').element.value).toBe('u-42')
    expect(
      wrapper.get<HTMLInputElement>('[data-testid="workbuddy-enterprise-id"]').element.value
    ).toBe('ent-42')
    // 成功提示 + toast；轮询已停止。
    expect(wrapper.get('[data-testid="workbuddy-oauth-status"]').text()).toBe(
      'admin.accounts.workbuddy.oauthSuccess'
    )
    expect(showSuccessMock).toHaveBeenCalledWith('admin.accounts.workbuddy.oauthSuccess')
    expect(wrapper.find('[data-testid="workbuddy-oauth-polling"]').exists()).toBe(false)

    // 完成后不再发起新请求。
    await vi.advanceTimersByTimeAsync(10_000)
    expect(exchangeWorkBuddyCodeMock).toHaveBeenCalledTimes(2)

    wrapper.unmount()
  })

  it('keeps polling while pending and reports a timeout message', async () => {
    vi.useFakeTimers()
    exchangeWorkBuddyCodeMock.mockResolvedValue({ status: 'pending' })

    const wrapper = mountModal()
    await selectWorkBuddy(wrapper)
    await wrapper.get('[data-testid="workbuddy-oauth-login"]').trigger('click')
    await flushPromises()

    await vi.advanceTimersByTimeAsync(2500)
    expect(exchangeWorkBuddyCodeMock).toHaveBeenCalledTimes(1)

    // 推进到 5 分钟超时窗口之后。
    await vi.advanceTimersByTimeAsync(5 * 60 * 1000)
    await flushPromises()

    expect(wrapper.get('[data-testid="workbuddy-oauth-status"]').text()).toBe(
      'admin.accounts.workbuddy.oauthTimeout'
    )
    expect(wrapper.find('[data-testid="workbuddy-oauth-polling"]').exists()).toBe(false)
    // 超时后重试按钮可见。
    expect(wrapper.find('[data-testid="workbuddy-oauth-retry"]').exists()).toBe(true)

    const callsAtTimeout = exchangeWorkBuddyCodeMock.mock.calls.length
    await vi.advanceTimersByTimeAsync(10_000)
    expect(exchangeWorkBuddyCodeMock.mock.calls.length).toBe(callsAtTimeout)

    wrapper.unmount()
  })

  it('stops polling when the modal is closed', async () => {
    vi.useFakeTimers()
    const wrapper = mountModal()
    await selectWorkBuddy(wrapper)
    await wrapper.get('[data-testid="workbuddy-oauth-login"]').trigger('click')
    await flushPromises()

    await vi.advanceTimersByTimeAsync(2500)
    expect(exchangeWorkBuddyCodeMock).toHaveBeenCalledTimes(1)

    // 关闭弹窗（触发 BaseDialog 的 close 事件路径）。
    await wrapper.setProps({ show: false })
    await flushPromises()

    const callsAtClose = exchangeWorkBuddyCodeMock.mock.calls.length
    await vi.advanceTimersByTimeAsync(30_000)
    expect(exchangeWorkBuddyCodeMock.mock.calls.length).toBe(callsAtClose)

    wrapper.unmount()
  })

  it('stops polling and clears the auth link when the realm changes', async () => {
    vi.useFakeTimers()
    const wrapper = mountModal()
    await selectWorkBuddy(wrapper)
    await wrapper.get('[data-testid="workbuddy-oauth-login"]').trigger('click')
    await flushPromises()

    await vi.advanceTimersByTimeAsync(2500)
    expect(exchangeWorkBuddyCodeMock).toHaveBeenCalledTimes(1)

    await wrapper.get('[data-testid="workbuddy-realm"]').setValue('global')
    await flushPromises()

    // 授权链接被清空（等待重新生成），轮询停止。
    expect(wrapper.find('[data-testid="workbuddy-oauth-url"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="workbuddy-oauth-polling"]').exists()).toBe(false)

    const callsAtRealmSwitch = exchangeWorkBuddyCodeMock.mock.calls.length
    await vi.advanceTimersByTimeAsync(30_000)
    expect(exchangeWorkBuddyCodeMock.mock.calls.length).toBe(callsAtRealmSwitch)

    wrapper.unmount()
  })
})
