import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const { createAccountMock } = vi.hoisted(() => ({ createAccountMock: vi.fn() }))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showWarning: vi.fn() })
}))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ isSimpleMode: true }) }))
vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: { create: createAccountMock, checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false }) },
    settings: { getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }), getSettings: vi.fn().mockResolvedValue({}) },
    tlsFingerprintProfiles: { list: vi.fn().mockResolvedValue([]) }
  }
}))
vi.mock('@/api/admin/accounts', () => ({ getAntigravityDefaultModelMapping: vi.fn().mockResolvedValue([]) }))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string, params?: Record<string, unknown>) => params?.name ? `${key}:${params.name}` : key }) }
})

import CreateAccountModal from '../CreateAccountModal.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false }, title: { type: String, default: '' } },
  template: '<div v-if="show" :data-title="title"><slot /><slot name="footer" /></div>'
})

function mountModal(props: Record<string, unknown> = {}) {
  return mount(CreateAccountModal, {
    props: { show: false, proxies: [], groups: [], ...props },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        OAuthAuthorizationFlow: { template: '<div />' },
        ConfirmDialog: true, Select: true, Icon: true, PlatformIcon: true,
        ProxySelector: true, ProxyAdBanner: true, GroupSelector: true,
        ModelWhitelistSelector: true, QuotaLimitCard: true,
        HeaderOverrideEditor: { props: ['rows'], emits: ['update:rows'], template: '<div />' }
      }
    }
  })
}

const baseSource = (overrides: Record<string, unknown> = {}) => ({
  id: 42, name: 'src', notes: null,
  platform: 'anthropic' as const, type: 'apikey' as const,
  credentials: {}, proxy_id: null, concurrency: 10, priority: 1,
  rate_multiplier: 1, group_ids: [], expires_at: null, auto_pause_on_expired: true,
  extra: {},
  ...overrides
})

beforeEach(() => { createAccountMock.mockReset() })

describe('CreateAccountModal - clone prop', () => {
  it('does not change title when cloneFrom is absent', async () => {
    const wrapper = mountModal({ show: true })
    await flushPromises()
    const dialog = wrapper.find('[data-title]')
    expect(dialog.exists()).toBe(true)
    expect(dialog.attributes('data-title')).toBe('admin.accounts.createAccount')
  })

  it('switches title to cloneAccount when cloneFrom is provided', async () => {
    const cloneFrom = baseSource()
    const wrapper = mountModal({ show: true, cloneFrom })
    await flushPromises()
    const dialog = wrapper.find('[data-title]')
    expect(dialog.attributes('data-title')).toBe('admin.accounts.cloneAccount')
  })
})

describe('CreateAccountModal - basic field mapping', () => {
  it('prefills name, notes, platform, type, proxy, concurrency, priority, rate_multiplier, group_ids, expires_at, auto_pause_on_expired', async () => {
    const cloneFrom = baseSource({
      name: 'src',
      notes: 'my notes',
      platform: 'openai',
      type: 'apikey',
      proxy_id: 7,
      concurrency: 25,
      load_factor: 1.5,
      priority: 9,
      rate_multiplier: 1.8,
      group_ids: [3, 4],
      expires_at: 1735689600,
      auto_pause_on_expired: false
    })
    const wrapper = mountModal({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.form.name).toBe('src (Copy)')
    expect(vm.form.notes).toBe('my notes')
    expect(vm.form.platform).toBe('openai')
    expect(vm.form.type).toBe('apikey')
    expect(vm.form.proxy_id).toBe(7)
    expect(vm.form.concurrency).toBe(25)
    expect(vm.form.load_factor).toBe(1.5)
    expect(vm.form.priority).toBe(9)
    expect(vm.form.rate_multiplier).toBe(1.8)
    expect(vm.form.group_ids).toEqual([3, 4])
    expect(vm.form.expires_at).toBe(1735689600)
    expect(vm.autoPauseOnExpired).toBe(false)
  })

  it('clears API key and other sensitive inputs', async () => {
    const cloneFrom = baseSource({ platform: 'anthropic', type: 'apikey' })
    const wrapper = mountModal({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    // Other sensitive refs such as upstreamApiKey cannot be prefilled externally; tasks 6-8 cover them through end-to-end tests.
    expect(vm.apiKeyValue).toBe('')
  })

  it('keeps source base_url across platform watcher reset (openai apikey)', async () => {
    const cloneFrom = baseSource({
      platform: 'openai', type: 'apikey',
      credentials: { base_url: 'https://custom-openai.example.com' }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    await wrapper.vm.$nextTick()
    await wrapper.vm.$nextTick()
    const vm = wrapper.vm as any
    expect(vm.apiKeyBaseUrl).toBe('https://custom-openai.example.com')
    expect(vm.form.platform).toBe('openai')
  })

  it('keeps source base_url for grok (form.credentials path)', async () => {
    const cloneFrom = baseSource({
      platform: 'grok', type: 'apikey',
      credentials: { base_url: 'https://custom-grok.example.com' }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.form.credentials.base_url).toBe('https://custom-grok.example.com')
  })
})
