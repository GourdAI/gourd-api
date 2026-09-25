import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { defineComponent, ref, nextTick } from 'vue'
import MultiGroupSelector from '../MultiGroupSelector.vue'
import type { Group } from '@/types'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        `${key}${params?.count != null ? `:${params.count}` : ''}`
    })
  }
})

const groups = [
  { id: 1, name: 'Alpha', platform: 'anthropic', rate_multiplier: 1, subscription_type: 'standard' },
  { id: 2, name: 'Beta', platform: 'openai', rate_multiplier: 2, subscription_type: 'standard' },
  { id: 3, name: 'Gamma', platform: 'gemini', rate_multiplier: 3, subscription_type: 'standard' }
] as unknown as Group[]

/**
 * Host that wires the selector to a real `v-model`, mirroring how the dialogs
 * use it (the component is fully controlled and never mutates its prop).
 */
const Host = defineComponent({
  components: { MultiGroupSelector },
  props: {
    initial: { type: Array as () => number[], default: () => [] },
    groups: { type: Array as () => Group[], default: () => [] },
    searchable: { type: [Boolean, String] as unknown as () => boolean | 'auto', default: true },
    disabled: { type: Boolean, default: false }
  },
  setup(props) {
    const selected = ref<number[]>([...props.initial])
    return { selected }
  },
  template: `
    <MultiGroupSelector
      v-model="selected"
      :groups="groups"
      :searchable="searchable"
      :disabled="disabled"
      test-id="mg"
    />
  `
})

const mountSelector = (props: Record<string, unknown> = {}) =>
  mount(Host, {
    props: { groups, ...props },
    global: { stubs: { GroupBadge: true, Icon: true } }
  })

const option = (wrapper: VueWrapper, id: number) => wrapper.get(`input[data-test="mg-checkbox-${id}"]`)
const selectedIds = (wrapper: VueWrapper) =>
  wrapper.findComponent(MultiGroupSelector).props('modelValue') as number[]
const badgeNames = (wrapper: VueWrapper, id: number) =>
  wrapper.findAll(`label[data-test="mg-option-${id}"] group-badge-stub`).map((badge) => badge.attributes('name'))

describe('MultiGroupSelector', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('appends newly checked groups so the first selection stays primary', async () => {
    const wrapper = mountSelector()
    await option(wrapper, 1).setValue(true)
    await option(wrapper, 3).setValue(true)

    expect(selectedIds(wrapper)).toEqual([1, 3])
  })

  it('removes unchecked groups without touching the rest', async () => {
    const wrapper = mountSelector({ initial: [1, 2, 3] })
    await option(wrapper, 2).setValue(false)

    expect(selectedIds(wrapper)).toEqual([1, 3])
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
  })

  it('renders every available group with its checkbox state', () => {
    const wrapper = mountSelector({ initial: [2] })
    expect(wrapper.findAll('input[type="checkbox"]').map((input) => (input.element as HTMLInputElement).checked))
      .toEqual([false, true, false])
    expect(badgeNames(wrapper, 2)).toEqual(['Beta'])
  })

  it('marks the head of the selection as the primary group only', async () => {
    const wrapper = mountSelector({ initial: [2, 3] })
    expect(wrapper.get('[data-test="mg-option-2"]').text()).toContain('keys.primaryGroupBadge')
    expect(wrapper.get('[data-test="mg-option-3"]').text()).not.toContain('keys.primaryGroupBadge')

    // Re-ordering the same set moves the primary marker to the new head.
    await option(wrapper, 2).setValue(false)
    await option(wrapper, 2).setValue(true)
    expect(selectedIds(wrapper)).toEqual([3, 2])
    expect(wrapper.get('[data-test="mg-option-3"]').text()).toContain('keys.primaryGroupBadge')
    expect(wrapper.get('[data-test="mg-option-2"]').text()).not.toContain('keys.primaryGroupBadge')
  })

  it('filters by name and reports an empty result', async () => {
    const wrapper = mountSelector()
    await wrapper.get('input[type="text"]').setValue('gam')

    expect(wrapper.findAll('input[type="checkbox"]')).toHaveLength(1)
    expect(badgeNames(wrapper, 3)).toEqual(['Gamma'])

    await wrapper.get('input[type="text"]').setValue('zzz')
    expect(wrapper.findAll('input[type="checkbox"]')).toHaveLength(0)
    expect(wrapper.get('[data-test="mg-empty"]').text()).toBe('keys.noGroupsFound')
  })

  it('shows the no-groups message when nothing is selectable', () => {
    const wrapper = mountSelector({ groups: [], initial: [] })
    expect(wrapper.get('[data-test="mg-empty"]').text()).toBe('common.noGroupsAvailable')
  })

  it('disables every checkbox while submitting', async () => {
    const wrapper = mountSelector({ initial: [1], disabled: true })
    expect(wrapper.findAll('input[type="checkbox"]').every((input) => input.attributes('disabled') !== undefined))
      .toBe(true)
  })

  it('never mutates the array it receives from the parent', async () => {
    const modelValue = [1]
    const wrapper = mountSelector({ initial: modelValue })
    await option(wrapper, 2).setValue(true)
    await nextTick()

    expect(modelValue).toEqual([1])
    expect(selectedIds(wrapper)).toEqual([1, 2])
    await flushPromises()
  })

  /**
   * 后端禁止一个密钥混绑订阅型与标准型分组（MIXED_GROUP_SUBSCRIPTION_TYPE），
   * 因此选择器必须把约束前移到 UI：已有选中后，类型不同的分组不可再选。
   */
  describe('subscription type lock', () => {
    const mixedGroups = [
      { id: 1, name: 'Std A', platform: 'anthropic', rate_multiplier: 1, subscription_type: 'standard' },
      { id: 2, name: 'Std B', platform: 'openai', rate_multiplier: 2, subscription_type: 'standard' },
      { id: 3, name: 'Sub C', platform: 'gemini', rate_multiplier: 3, subscription_type: 'subscription' }
    ] as unknown as Group[]

    it('locks opposite-type groups once a standard group is selected', () => {
      const wrapper = mountSelector({ groups: mixedGroups, initial: [1] })

      expect(option(wrapper, 3).attributes('disabled')).not.toBeUndefined()
      expect(wrapper.get('[data-test="mg-option-3"]').attributes('data-type-locked')).toBe('true')
      // 同类型仍可选。
      expect(option(wrapper, 2).attributes('disabled')).toBeUndefined()
      expect(wrapper.get('[data-test="mg-option-2"]').attributes('data-type-locked')).toBeUndefined()
    })

    it('locks standard groups once a subscription group is selected', () => {
      const wrapper = mountSelector({ groups: mixedGroups, initial: [3] })

      expect(option(wrapper, 1).attributes('disabled')).not.toBeUndefined()
      expect(option(wrapper, 2).attributes('disabled')).not.toBeUndefined()
    })

    it('unlocks everything when the selection becomes empty', async () => {
      const wrapper = mountSelector({ groups: mixedGroups, initial: [1] })
      await option(wrapper, 1).setValue(false)

      expect(option(wrapper, 3).attributes('disabled')).toBeUndefined()
      expect(wrapper.get('[data-test="mg-option-3"]').attributes('data-type-locked')).toBeUndefined()
    })

    it('never locks an already-selected group', () => {
      const wrapper = mountSelector({ groups: mixedGroups, initial: [1, 2] })

      expect(option(wrapper, 1).attributes('disabled')).toBeUndefined()
      expect(option(wrapper, 2).attributes('disabled')).toBeUndefined()
    })

    it('locks nothing while there is no selection', () => {
      const wrapper = mountSelector({ groups: mixedGroups })

      expect(option(wrapper, 1).attributes('disabled')).toBeUndefined()
      expect(option(wrapper, 3).attributes('disabled')).toBeUndefined()
    })

    it('treats a missing subscription_type as standard', () => {
      const legacyGroups = [
        { id: 1, name: 'Std', platform: 'anthropic', rate_multiplier: 1 },
        { id: 2, name: 'Sub', platform: 'gemini', rate_multiplier: 2, subscription_type: 'subscription' }
      ] as unknown as Group[]
      const wrapper = mountSelector({ groups: legacyGroups, initial: [1] })

      expect(option(wrapper, 2).attributes('disabled')).not.toBeUndefined()
    })
  })
})
