import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'

const {
  updateAccountMock,
  checkMixedChannelRiskMock,
  startTraeOAuthLoginMock,
  submitTraeOAuthCallbackMock,
  cancelTraeOAuthLoginMock,
} = vi.hoisted(() => ({
  updateAccountMock: vi.fn(),
  checkMixedChannelRiskMock: vi.fn(),
  startTraeOAuthLoginMock: vi.fn(),
  submitTraeOAuthCallbackMock: vi.fn(),
  cancelTraeOAuthLoginMock: vi.fn(),
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showInfo: vi.fn(),
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
      update: updateAccountMock,
      checkMixedChannelRisk: checkMixedChannelRiskMock,
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({}),
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([]),
    },
    trae: {
      startTraeOAuthLogin: startTraeOAuthLoginMock,
      submitTraeOAuthCallback: submitTraeOAuthCallbackMock,
      cancelTraeOAuthLogin: cancelTraeOAuthLoginMock,
      exchangeTraeToken: vi.fn(),
      queryTraeCredits: vi.fn(),
      checkinTraeAccount: vi.fn(),
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

import EditAccountModal from '../EditAccountModal.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
})

function buildTraeAccount(id: number, uid: string) {
  return {
    id,
    name: `Trae ${uid}`,
    notes: '',
    platform: 'trae',
    type: 'apikey',
    credentials: {
      realm: 'cn',
      uid,
      device_id: '1234567890123456',
      machine_id: 'a'.repeat(32),
      base_url: 'https://trae-api-cn.mchost.guru',
    },
    credentials_status: { has_access_token: true, has_refresh_token: true },
    extra: {},
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false,
  } as any
}

function mountModal(account: ReturnType<typeof buildTraeAccount>) {
  return mount(EditAccountModal, {
    props: { show: true, account, proxies: [], groups: [] },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        Select: true,
        Icon: true,
        ProxySelector: true,
        GroupSelector: true,
        ModelWhitelistSelector: true,
        QuotaLimitCard: true,
      },
    },
  })
}

describe('EditAccountModal Trae browser login', () => {
  beforeEach(() => {
    updateAccountMock.mockReset().mockResolvedValue({})
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })
    startTraeOAuthLoginMock.mockReset().mockResolvedValue({
      login_id: 'lid-edit-1',
      login_url: 'https://grow.dev/authorize?state=edit',
      callback_url_prefix: 'http://127.0.0.1:53489/authorize',
      expires_at: Math.floor(Date.now() / 1000) + 900,
      needs_paste: true,
    })
    submitTraeOAuthCallbackMock.mockReset().mockResolvedValue({
      status: 'completed',
      credentials: {
        access_token: 'at-edit',
        refresh_token: 'rt-edit',
        uid: 'u-edit',
        device_id: '9999999999999999',
        machine_id: 'b'.repeat(32),
        device_public_key: 'pk-edit',
        device_private_key: 'sk-edit',
      },
      account_name: 'edited-user',
    })
    cancelTraeOAuthLoginMock.mockReset().mockResolvedValue(undefined)
  })

  it("does not leak a previous trae account's browser-login state into the next account", async () => {
    const openSpy = vi
      .spyOn(window, 'open')
      .mockReturnValue({ opener: {} } as unknown as Window)
    const wrapper = mountModal(buildTraeAccount(4201, 'u-a'))
    await flushPromises()

    // 账号 A：发起浏览器登录（保持 pending，不提交）
    await wrapper.get('[data-testid="edit-trae-oauth-login"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="edit-trae-oauth-url"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="edit-trae-oauth-callback"]').exists()).toBe(true)

    // 切到另一个 trae 账号：pending 会话必须被取消，旧 UI 状态不得残留
    await wrapper.setProps({ account: buildTraeAccount(4202, 'u-b') })
    await flushPromises()

    expect(cancelTraeOAuthLoginMock).toHaveBeenCalledWith('lid-edit-1')
    expect(wrapper.find('[data-testid="edit-trae-oauth-url"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="edit-trae-oauth-callback"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="edit-trae-oauth-status"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="edit-trae-oauth-login"]').exists()).toBe(true)

    openSpy.mockRestore()
  })

  it("does not merge the previous account's oauth extras into this account's save payload", async () => {
    const openSpy = vi
      .spyOn(window, 'open')
      .mockReturnValue({ opener: {} } as unknown as Window)
    const wrapper = mountModal(buildTraeAccount(4203, 'u-c'))
    await flushPromises()

    await wrapper.get('[data-testid="edit-trae-oauth-login"]').trigger('click')
    await flushPromises()
    await wrapper
      .get('[data-testid="edit-trae-oauth-callback"]')
      .setValue('http://127.0.0.1:53489/authorize?code=c-edit')
    await wrapper.get('[data-testid="edit-trae-oauth-submit"]').trigger('click')
    await flushPromises()

    // 切换到账号 D 后直接保存（不重新发起登录）
    await wrapper.setProps({ account: buildTraeAccount(4204, 'u-d') })
    await flushPromises()
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await flushPromises()

    const payload = updateAccountMock.mock.calls[0]?.[1]
    expect(payload).toBeDefined()
    // 账号 C 登录产生的设备密钥/公钥/did/nickname 不得混入账号 D 的保存
    expect(payload.credentials.device_private_key).toBeUndefined()
    expect(payload.credentials.device_public_key).toBeUndefined()
    expect(payload.credentials.nickname).toBeUndefined()
    expect(payload.credentials.uid).toBe('u-d')
    expect(payload.credentials.device_id).toBe('1234567890123456')

    openSpy.mockRestore()
  })
})
