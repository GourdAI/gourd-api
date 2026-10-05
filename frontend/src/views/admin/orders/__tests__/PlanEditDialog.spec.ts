import { describe, expect, it, vi } from 'vitest'
import { defineComponent, nextTick } from 'vue'
import { mount } from '@vue/test-utils'

import PlanEditDialog from '../PlanEditDialog.vue'
import { adminPaymentAPI } from '@/api/admin/payment'
import type { AdminGroup, SubscriptionPlan } from '@/types'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) => {
      if (key === 'payment.admin.subscriptionCnyPayPreview') return `preview ${params?.amount}`
      if (key === 'payment.admin.subscriptionCnyPayPreviewWithFee') return `fee ${params?.feeRate} ${params?.total}`
      return key
    },
  }),
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
  }),
}))

vi.mock('@/api/admin/payment', () => ({
  adminPaymentAPI: {
    createPlan: vi.fn(),
    updatePlan: vi.fn(),
  },
}))

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: {
    show: Boolean,
    title: String,
    width: String,
  },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
})

const SelectStub = defineComponent({
  name: 'SelectStub',
  props: {
    modelValue: [String, Number],
    options: {
      type: Array,
      default: () => [],
    },
    placeholder: String,
  },
  emits: ['update:modelValue'],
  setup(_props, { emit }) {
    const onChange = (event: Event) => {
      const value = (event.target as HTMLSelectElement).value
      emit('update:modelValue', value === '' ? null : Number(value))
    }
    return { onChange }
  },
  template: `
    <select
      :value="modelValue ?? ''"
      @change="onChange"
    >
      <option value="">{{ placeholder }}</option>
      <option
        v-for="option in options"
        :key="option.value"
        :value="option.value"
        :data-platform="option.platform"
      >
        {{ option.label }}
      </option>
    </select>
  `,
})

const groupFixture = (overrides: Partial<AdminGroup>): AdminGroup => ({
  id: 1,
  name: 'OpenAI',
  description: null,
  platform: 'openai',
  rate_multiplier: 1,
  rpm_limit: 0,
  is_exclusive: false,
  status: 'active',
  subscription_type: 'subscription',
  daily_limit_usd: null,
  weekly_limit_usd: null,
  monthly_limit_usd: null,
  allow_image_generation: false,
  image_rate_independent: false,
  image_rate_multiplier: 1,
  image_price_1k: null,
  image_price_2k: null,
  image_price_4k: null,
  peak_rate_enabled: false,
  peak_start: '',
  peak_end: '',
  peak_rate_multiplier: 1,
  claude_code_only: false,
  fallback_group_id: null,
  fallback_group_id_on_invalid_request: null,
  allow_messages_dispatch: false,
  require_oauth_only: false,
  require_privacy_set: false,
  created_at: '2026-07-01T00:00:00Z',
  updated_at: '2026-07-01T00:00:00Z',
  model_routing: null,
  model_routing_enabled: false,
  mcp_xml_inject: false,
  sort_order: 0,
  ...overrides,
})

function mountDialog({
  groups = [],
  paymentConfig = null,
}: {
  groups?: AdminGroup[]
  paymentConfig?: Record<string, unknown> | null
} = {}) {
  return mount(PlanEditDialog, {
    props: {
      show: true,
      plan: null,
      groups,
      paymentConfig,
    },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        Select: SelectStub,
        Icon: true,
        GroupBadge: true,
      },
    },
  })
}

describe('PlanEditDialog', () => {
  it('sends total_limit_usd in the plan payload and normalizes a cleared input to null', async () => {
    const createPlan = vi.mocked(adminPaymentAPI.createPlan)
    createPlan.mockReset()
    createPlan.mockResolvedValue({} as never)

    const wrapper = mountDialog({ groups: [groupFixture({ id: 10, name: 'OpenAI' })] })
    // 选中分组 + 填价格，凑齐提交前置校验
    await wrapper.findAll('select')[0]!.setValue('10')
    await wrapper.findAll('input[type="number"]')[0]!.setValue('9.99')

    const quotaInput = wrapper.get('[data-test="plan-total-limit-usd"]')
    await quotaInput.setValue('50')
    await wrapper.get('form').trigger('submit')
    expect(createPlan.mock.calls[0]![0]).toMatchObject({ total_limit_usd: 50 })

    // 清空输入：v-model.number 得到 ''，必须归一为 null（= 不限额），
    // 否则空串透到 Go 的 *float64 会直接 400。
    createPlan.mockClear()
    await quotaInput.setValue('')
    await wrapper.get('form').trigger('submit')
    expect(createPlan.mock.calls[0]![0]).toMatchObject({ total_limit_usd: null })

    // 输 0 同样按「不限额」上报（后端 <=0 归一为 nil）
    createPlan.mockClear()
    await quotaInput.setValue('0')
    await wrapper.get('form').trigger('submit')
    expect(createPlan.mock.calls[0]![0]).toMatchObject({ total_limit_usd: null })
  })

  // 编辑路径：后端 total_limit_usd 是三态（nil=不改 / >0=设值 / <=0=清空）。
  // 清空输入必须上报 0，否则被读成「不修改」，已设额度的套餐改不回不限额。
  it('reports 0 when the limit is cleared while editing, so it can go back to unlimited', async () => {
    const updatePlan = vi.mocked(adminPaymentAPI.updatePlan)
    updatePlan.mockReset()
    updatePlan.mockResolvedValue({} as never)

    const plan = {
      id: 7,
      group_id: 10,
      name: 'Pro',
      description: '',
      price: 9.9,
      validity_days: 30,
      validity_unit: 'days',
      for_sale: true,
      features: [],
      sort_order: 0,
      total_limit_usd: 50,
    } as unknown as SubscriptionPlan

    const wrapper = mount(PlanEditDialog, {
      props: {
        show: false,
        plan,
        groups: [groupFixture({ id: 10, name: 'OpenAI' })],
        paymentConfig: null,
      },
      global: {
        stubs: { BaseDialog: BaseDialogStub, Select: SelectStub, Icon: true, GroupBadge: true },
      },
    })
    // 表单由 watch(show) 填充：必须 false -> true 才会带入既有额度
    await wrapper.setProps({ show: true })
    await nextTick()

    const quotaInput = wrapper.get('[data-test="plan-total-limit-usd"]')
    expect((quotaInput.element as HTMLInputElement).value).toBe('50')

    await quotaInput.setValue('')
    await wrapper.get('form').trigger('submit')
    expect(updatePlan.mock.calls[0]![1]).toMatchObject({ total_limit_usd: 0 })

    // 未清空时按数值上报，不会被误判为清空
    updatePlan.mockClear()
    await quotaInput.setValue('80')
    await wrapper.get('form').trigger('submit')
    expect(updatePlan.mock.calls[0]![1]).toMatchObject({ total_limit_usd: 80 })
  })

  it('shows CNY channel charge using the configured subscription rate and fee', async () => {
    const wrapper = mountDialog({
      paymentConfig: {
        subscription_usd_to_cny_rate: 7.15,
        recharge_fee_rate: 2.5,
      },
    })

    await wrapper.find('input[type="number"]').setValue('9.99')

    expect(wrapper.text()).toContain('preview')
    expect(wrapper.text()).toContain('¥71.43')
    expect(wrapper.text()).toContain('fee 2.5')
    expect(wrapper.text()).toContain('¥73.22')
  })

  it('hides the preview when the subscription rate is not configured', async () => {
    const wrapper = mountDialog({
      paymentConfig: {
        subscription_usd_to_cny_rate: 0,
        recharge_fee_rate: 2.5,
      },
    })

    await wrapper.find('input[type="number"]').setValue('9.99')

    expect(wrapper.text()).not.toContain('preview')
    expect(wrapper.text()).not.toContain('¥71.43')
  })

  it('allows composite subscription groups for payment plans', () => {
    const wrapper = mountDialog({
      groups: [
        groupFixture({
          id: 10,
          name: 'OpenAI + Claude + Gemini + Grok',
          platform: 'composite',
          rate_multiplier: 1.2,
          subscription_type: 'subscription',
        }),
        groupFixture({
          id: 11,
          name: 'Standard OpenAI',
          platform: 'openai',
          subscription_type: 'standard',
        }),
      ],
    })

    const options = wrapper.findAll('option').map(option => option.text())

    expect(options).toContain('OpenAI + Claude + Gemini + Grok — composite (1.2x)')
    expect(options).not.toContain('Standard OpenAI — openai (1x)')
  })
})
