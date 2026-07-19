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
