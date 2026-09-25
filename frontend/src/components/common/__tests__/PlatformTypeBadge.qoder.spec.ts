import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import PlatformTypeBadge from '../PlatformTypeBadge.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

describe('PlatformTypeBadge Qoder', () => {
  it('labels Qoder API keys as Qoder with the purple accent', () => {
    const wrapper = mount(PlatformTypeBadge, {
      props: {
        platform: 'qoder',
        type: 'apikey',
      },
    })

    expect(wrapper.text()).toContain('Qoder')
    expect(wrapper.text()).toContain('Key')
    expect(wrapper.html()).toContain('bg-purple-100')
    expect(wrapper.html()).not.toContain('bg-violet-100')
  })

  it('renders the Qoder platform icon mark (gradient rounded square with Q glyph)', () => {
    const wrapper = mount(PlatformTypeBadge, {
      props: {
        platform: 'qoder',
        type: 'apikey',
      },
    })

    const html = wrapper.html()
    expect(html).toContain('qoder-icon-gradient')
    // The Q glyph cutout is drawn with a white fill on the purple tile.
    expect(html).toContain('fill="#ffffff"')
  })

  it('falls back to shared labels for other platforms', () => {
    const wrapper = mount(PlatformTypeBadge, {
      props: {
        platform: 'workbuddy',
        type: 'apikey',
      },
    })

    expect(wrapper.text()).toContain('WorkBuddy')
    expect(wrapper.html()).not.toContain('qoder-icon-gradient')
  })
})
