import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import SubscriptionsView from '../SubscriptionsView.vue'

/**
 * 订阅额度「逐字段增量」语义的前端锚点测试。
 *
 * 后端契约（总额度只有一份总额池）：键缺失 = 保持原值；>0 = 设为该额度；0 = 改回不限额。
 * 其中「输 0 要真的把 0 提交出去」是最容易被静默改坏的一条：
 * 任何 `if (value)` 之类的真值判断都会把 0 当成「没填」，
 * 结果是管理员想收紧额度却提交成「不改动」，用户的闸门凭空消失。
 */
const { listSubscriptions, assignSubscription, bulkAssign, getAllGroups, listUsers, searchUsageUsers, showError, showSuccess } =
  vi.hoisted(() => ({
    listSubscriptions: vi.fn(),
    assignSubscription: vi.fn(),
    bulkAssign: vi.fn(),
    getAllGroups: vi.fn(),
    listUsers: vi.fn(),
    searchUsageUsers: vi.fn(),
    showError: vi.fn(),
    showSuccess: vi.fn()
  }))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    subscriptions: { list: listSubscriptions, assign: assignSubscription, bulkAssign },
    groups: { getAll: getAllGroups },
    users: { list: listUsers },
    usage: { searchUsers: searchUsageUsers }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string, params?: { id?: number }) =>
      key === 'admin.redeem.userPrefix' ? `User #${params?.id}` : key })
  }
})

const mountView = () =>
  mount(SubscriptionsView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /></div>' },
        DataTable: { props: ['data'], template: '<div />' },
        RouterLink: true,
        Pagination: true,
        BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' },
        ConfirmDialog: true,
        EmptyState: true,
        Select: true,
        GroupBadge: true,
        GroupOptionItem: true,
        Icon: true,
        Teleport: true
      }
    }
  })

describe('admin subscription quota assignment', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    localStorage.clear()
    listSubscriptions.mockResolvedValue({ items: [], total: 0, pages: 1 })
    assignSubscription.mockResolvedValue({})
    bulkAssign.mockResolvedValue({
      success_count: 0,
      failed_count: 0,
      created_count: 0,
      reused_count: 0,
      subscriptions: [],
      errors: [],
      statuses: {}
    })
    getAllGroups.mockResolvedValue([])
    listUsers.mockResolvedValue({ items: [{ id: 84, email: 'another@example.com' }], total: 1, pages: 1 })
    searchUsageUsers.mockResolvedValue([])
  })

  /** 打开分配弹窗并选中一个用户（手工发放，无套餐、不绑分组）。 */
  const openDialogWithUser = async (wrapper: ReturnType<typeof mountView>) => {
    await flushPromises()
    const openButton = wrapper
      .findAll('button')
      .find((button) => button.text() === 'admin.subscriptions.assignSubscription')!
    await openButton.trigger('click')

    const search = wrapper.get('[data-assign-user-search] input')
    await search.trigger('focus')
    await search.setValue('another')
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()
    await wrapper.get('[data-assign-user-search] button').trigger('click')
    await flushPromises()
    return wrapper.get('#assign-subscription-form')
  }

  it('submits an explicit 0 so the quota resets to unlimited (0 must not be dropped)', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const wrapper = mountView()
    try {
      const form = await openDialogWithUser(wrapper)
      await form.get('[data-test="assign-quota-total"]').setValue('0')
      await form.trigger('submit')
      await flushPromises()

      expect(assignSubscription).toHaveBeenCalledTimes(1)
      const payload = assignSubscription.mock.calls[0][0]
      expect(payload).toMatchObject({ user_id: 84, validity_days: 30 })
      // 0 必须原样出现在上线报文里：键存在且值为 0（被 undefined 化就变成“不改动”）
      expect(payload.total_limit_usd).toBe(0)
      const wire = JSON.parse(JSON.stringify(payload))
      expect(wire).toHaveProperty('total_limit_usd', 0)
    } finally {
      wrapper.unmount()
      vi.useRealTimers()
    }
  })

  it('omits blank quotas so existing limits stay unchanged', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const wrapper = mountView()
    try {
      const form = await openDialogWithUser(wrapper)
      await form.trigger('submit')
      await flushPromises()

      const payload = assignSubscription.mock.calls[0][0]
      // 中间对象里是 undefined；真正决定语义的是上线报文：JSON.stringify 会丢弃 undefined 键，
      // 后端因而是「键缺失 = 保持原值」而不是「收到 0 = 改回不限额」。
      expect(payload.total_limit_usd).toBeUndefined()
      const wire = JSON.parse(JSON.stringify(payload))
      expect(Object.keys(wire)).not.toContain('total_limit_usd')
    } finally {
      wrapper.unmount()
      vi.useRealTimers()
    }
  })

  it('treats a cleared input as "unchanged", not as 0 (empty string must not reach Go as a number)', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const wrapper = mountView()
    try {
      const form = await openDialogWithUser(wrapper)
      const total = form.get('[data-test="assign-quota-total"]')
      await total.setValue('25')
      await total.setValue('')
      await form.trigger('submit')
      await flushPromises()

      const payload = assignSubscription.mock.calls[0][0]
      expect(payload.total_limit_usd).toBeUndefined()
      // 空串若透传到 Go，*float64 反序列化会直接 400
      expect(JSON.parse(JSON.stringify(payload))).not.toHaveProperty('total_limit_usd')
    } finally {
      wrapper.unmount()
      vi.useRealTimers()
    }
  })

  it('submits a positive total quota without any group fields', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const wrapper = mountView()
    try {
      const form = await openDialogWithUser(wrapper)
      await form.get('[data-test="assign-quota-total"]').setValue('120')
      await form.trigger('submit')
      await flushPromises()

      const payload = assignSubscription.mock.calls[0][0]
      expect(payload.total_limit_usd).toBe(120)
      const wire = JSON.parse(JSON.stringify(payload))
      expect(wire.total_limit_usd).toBe(120)
      // 订阅不绑分组：报文里不得再出现分组与日/周/月三档字段
      expect(Object.keys(wire)).not.toContain('group_id')
      expect(Object.keys(wire)).not.toContain('daily_limit_usd')
      expect(Object.keys(wire)).not.toContain('weekly_limit_usd')
      expect(Object.keys(wire)).not.toContain('monthly_limit_usd')
    } finally {
      wrapper.unmount()
      vi.useRealTimers()
    }
  })

  it('rejects a negative quota without submitting', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const wrapper = mountView()
    try {
      const form = await openDialogWithUser(wrapper)
      await form.get('[data-test="assign-quota-total"]').setValue('-1')
      await form.trigger('submit')
      await flushPromises()

      expect(assignSubscription).not.toHaveBeenCalled()
      expect(showError).toHaveBeenCalledWith('admin.subscriptions.quotaInvalid')
    } finally {
      wrapper.unmount()
      vi.useRealTimers()
    }
  })
})
