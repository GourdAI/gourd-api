import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

/**
 * EditAccountModal 的 WorkBuddy OAuth 回填与状态生命周期。
 *
 * 背景（真实故障）：用户在「编辑账号」弹窗里完成一次 OAuth 授权后，再打开/切换到
 * 另一个账号，弹窗里**仍然显示上一次的授权链接与「OAuth 授权成功，凭据已自动回填。」
 * 绿字**，而 Access Token / Refresh Token 输入框是空的。
 *
 * 后果有两个，都被用户直接看到：
 *   1) 「数据不回填」——绿字宣称成功，但输入框没有 token；
 *   2) 「换不了账号」——登录按钮的显示条件是 `v-if="!workbuddyOAuthUrl"`，残留的
 *      authUrl 让按钮永久隐藏，用户只剩一个「复制」按钮，无法为当前账号重新发起登录。
 *
 * WHY 只有 Trae 幸免：syncFormFromAccount 里对 Trae 显式调了 resetTraeOAuthState()，
 * 而 WorkBuddy / Qoder 的 reset*OAuthState() 只在「realm 变更」时调用——换账号不清。
 */

const { showErrorMock, showSuccessMock, generateWorkBuddyAuthUrlMock, exchangeWorkBuddyCodeMock } =
  vi.hoisted(() => ({
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
      update: vi.fn(),
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false }),
      syncUpstreamModels: vi.fn(),
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

import EditAccountModal from '../EditAccountModal.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
})

function buildWorkBuddyAccount(id: number, name: string) {
  return {
    id,
    name,
    notes: '',
    platform: 'workbuddy',
    type: 'apikey',
    credentials: {
      access_token: `wb-token-${id}`,
      refresh_token: `wb-refresh-${id}`,
      realm: 'cn',
      uid: `wb-uid-${id}`,
      base_url: 'https://copilot.tencent.com',
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

function mountModal(account = buildWorkBuddyAccount(1, 'WorkBuddy-hu')) {
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
        HelpTooltip: true,
        Toggle: true,
        GrokBaseUrlPresets: true,
        CnBaseUrlPresets: true,
        OpenCodeGoProtocolRulesEditor: true,
        HeaderOverrideEditor: true,
        OllamaCloudUsageSettings: true,
        UpstreamRequestIdHeaderField: true,
        ConfirmDialog: true,
      },
    },
  })
}

/** 点击 OAuth 登录并把轮询推进到第一次 completed。 */
async function completeOAuthOnce(wrapper: ReturnType<typeof mountModal>) {
  await wrapper.get('[data-testid="edit-workbuddy-oauth-login"]').trigger('click')
  await flushPromises()
  await vi.advanceTimersByTimeAsync(2500)
  await flushPromises()
}

describe('EditAccountModal WorkBuddy OAuth 状态生命周期', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    showErrorMock.mockReset()
    showSuccessMock.mockReset()
    generateWorkBuddyAuthUrlMock.mockReset().mockResolvedValue({
      auth_url: 'https://copilot.tencent.com/login?platform=CLI&state=st-1',
      state: 'st-1',
      realm: 'cn',
    })
    exchangeWorkBuddyCodeMock.mockReset().mockResolvedValue({
      status: 'completed',
      token: {
        access_token: 'brand-new-access',
        refresh_token: 'brand-new-refresh',
        uid: 'new-uid',
        enterprise_id: 'new-ent',
        realm: 'cn',
      },
    })
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('回填成功后确实写入两处 token 输入框', async () => {
    const wrapper = mountModal()
    await completeOAuthOnce(wrapper)

    const accessInput = wrapper.get<HTMLInputElement>(
      '[data-testid="edit-workbuddy-access-token"]'
    )
    expect(accessInput.element.value).toBe('brand-new-access')
    expect(wrapper.get<HTMLInputElement>('[data-testid="edit-workbuddy-refresh-token"]').element.value).toBe(
      'brand-new-refresh'
    )
    wrapper.unmount()
  })

  it('切换账号时必须清空用户已填入的 api_key，不得把它带到新账号', async () => {
    // 回归护栏：syncFormFromAccount 里 `editApiKey.value = ''` 那行曾被我误删（改 OAuth 重置
    // 时把相邻行当作锚点整段替换掉了）。editApiKey 本身不会被回填为非空值（后端响应脱敏），
    // 真正的危险路径是：**用户在账号 A 里手填了密钥但没点保存**，然后直接切到账号 B，
    // 残留的非空 editApiKey 会让保存分支（`if (editApiKey.value.trim())`）把 A 的密钥
    // 静默写进 B 的 credentials.api_key。
    const openaiAccount = {
      ...buildWorkBuddyAccount(9, 'plain-openai'),
      platform: 'openai',
      credentials: { base_url: 'https://api.openai.com' },
      credentials_status: { has_api_key: true },
    } as any
    const wrapper = mountModal(openaiAccount)
    await flushPromises()

    const keyInput = wrapper.get<HTMLInputElement>('input[placeholder="sk-proj-..."]')
    await keyInput.setValue('leaked-key-from-account-a')

    // 切到另一个账号：已手填的密钥必须被丢弃。
    await wrapper.setProps({ account: buildWorkBuddyAccount(10, 'WorkBuddy-10') })
    await flushPromises()

    // 切回一个普通 apikey 账号，确认密钥框是空的（没有被 A 的输入污染）。
    await wrapper.setProps({ account: openaiAccount })
    await flushPromises()

    expect(wrapper.get<HTMLInputElement>('input[placeholder="sk-proj-..."]').element.value).toBe('')
    wrapper.unmount()
  })

  it('切换到另一个账号后不得残留上一次的授权链接与成功提示', async () => {
    const wrapper = mountModal()
    await completeOAuthOnce(wrapper)

    // 授权完成的既成事实：链接在、绿字在。
    expect(wrapper.find('[data-testid="edit-workbuddy-oauth-url"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="edit-workbuddy-oauth-status"]').text()).toBe(
      'admin.accounts.workbuddy.oauthSuccess'
    )

    // 用户切到另一个 WorkBuddy 账号（列表里点编辑另一条）。
    await wrapper.setProps({ account: buildWorkBuddyAccount(2, 'WorkBuddy-2') })
    await flushPromises()

    // 断言：旧会话必须被丢弃，登录按钮回来，绿字消失。
    expect(wrapper.find('[data-testid="edit-workbuddy-oauth-url"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="edit-workbuddy-oauth-status"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="edit-workbuddy-oauth-login"]').exists()).toBe(true)

    wrapper.unmount()
  })

  it('关闭再打开另一个账号后同样不得残留（关闭即丢弃会话）', async () => {
    const wrapper = mountModal()
    await completeOAuthOnce(wrapper)

    await wrapper.setProps({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, account: buildWorkBuddyAccount(3, 'WorkBuddy-3') })
    await flushPromises()

    expect(wrapper.find('[data-testid="edit-workbuddy-oauth-url"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="edit-workbuddy-oauth-status"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="edit-workbuddy-oauth-login"]').exists()).toBe(true)

    wrapper.unmount()
  })

  it('残留成功后必须仍能重新发起登录（否则换号无从下手）', async () => {
    const wrapper = mountModal()
    await completeOAuthOnce(wrapper)

    await wrapper.setProps({ account: buildWorkBuddyAccount(4, 'WorkBuddy-4') })
    await flushPromises()

    // 按钮存在即可点击，点击后应重新生成一条授权链接。
    generateWorkBuddyAuthUrlMock.mockResolvedValue({
      auth_url: 'https://copilot.tencent.com/login?platform=CLI&state=st-2',
      state: 'st-2',
      realm: 'cn',
    })
    await wrapper.get('[data-testid="edit-workbuddy-oauth-login"]').trigger('click')
    await flushPromises()

    expect(generateWorkBuddyAuthUrlMock).toHaveBeenLastCalledWith({ realm: 'cn' })
    expect(
      wrapper.get<HTMLInputElement>('[data-testid="edit-workbuddy-oauth-url"]').element.value
    ).toContain('state=st-2')

    wrapper.unmount()
  })
})
