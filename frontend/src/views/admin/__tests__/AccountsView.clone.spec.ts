import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountsView from '../AccountsView.vue'
import AccountActionMenu from '@/components/admin/account/AccountActionMenu.vue'

const {
  listAccounts,
  listWithEtag,
  getBatchTodayStats,
  getAllProxies,
  getAllGroups,
  getAccountById,
  showError
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  getAccountById: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      listWithEtag,
      getBatchTodayStats,
      getById: getAccountById,
      duplicate: vi.fn(),
      createSparkShadow: vi.fn(),
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      toggleSchedulable: vi.fn()
    },
    proxies: { getAll: getAllProxies },
    groups: { getAll: getAllGroups }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess: vi.fn(), showInfo: vi.fn() })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ token: 'test-token' })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const mountView = () =>
  mount(AccountsView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: {
          template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>'
        },
        DataTable: true,
        Pagination: true,
        ConfirmDialog: true,
        AccountTableActions: { template: '<div><slot name="beforeCreate" /><slot name="after" /></div>' },
        AccountTableFilters: { template: '<div></div>' },
        AccountBulkActionsBar: true,
        AccountActionMenu: true,
        ImportDataModal: true,
        ReAuthAccountModal: true,
        AccountTestModal: true,
        AccountStatsModal: true,
        ScheduledTestsPanel: true,
        SyncFromCrsModal: true,
        TempUnschedStatusModal: true,
        ErrorPassthroughRulesModal: true,
        TLSFingerprintProfilesModal: true,
        CreateAccountModal: {
          props: ['show', 'cloneFrom'],
          template: '<div v-if="show">create-modal-stub (cloneFrom id={{ cloneFrom?.id }})</div>'
        },
        EditAccountModal: true,
        BulkEditAccountModal: true,
        PlatformTypeBadge: true,
        AccountCapacityCell: true,
        AccountStatusIndicator: true,
        AccountTodayStatsCell: true,
        AccountGroupsCell: true,
        AccountUsageCell: true,
        Icon: true
      }
    }
  })

describe('admin AccountsView - clone wiring', () => {
  beforeEach(() => {
    localStorage.clear()
    for (const fn of [
      listAccounts,
      listWithEtag,
      getBatchTodayStats,
      getAllProxies,
      getAllGroups,
      getAccountById,
      showError
    ]) {
      fn.mockReset()
    }
    listAccounts.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20, pages: 0 })
    listWithEtag.mockResolvedValue({ notModified: true, etag: null, data: null })
    getBatchTodayStats.mockResolvedValue({ stats: {} })
    getAllProxies.mockResolvedValue([])
    getAllGroups.mockResolvedValue([])
    getAccountById.mockResolvedValue({ id: 42, name: 'src', platform: 'anthropic', type: 'apikey' })
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('emitting clone fetches the source account and opens CreateAccountModal with cloneFrom', async () => {
    const wrapper = mountView()
    await flushPromises()

    wrapper
      .findComponent(AccountActionMenu)
      .vm.$emit('clone', { id: 42, name: 'src', platform: 'anthropic', type: 'apikey' })
    await flushPromises()

    expect(getAccountById).toHaveBeenCalledWith(42)
    expect(wrapper.html()).toContain('cloneFrom id=42')
    wrapper.unmount()
  })

  it('shows error toast and does not open modal when getById fails', async () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {})
    getAccountById.mockRejectedValueOnce(new Error('network error'))

    const wrapper = mountView()
    await flushPromises()

    wrapper
      .findComponent(AccountActionMenu)
      .vm.$emit('clone', { id: 42, name: 'src', platform: 'anthropic', type: 'apikey' })
    await flushPromises()

    expect(showError).toHaveBeenCalled()
    expect(wrapper.html()).not.toContain('create-modal-stub')
    consoleError.mockRestore()
    wrapper.unmount()
  })

  it('debounces duplicate clone clicks on the same account', async () => {
    let resolveFetch!: (acc: unknown) => void
    getAccountById.mockImplementationOnce(
      () => new Promise<unknown>((r) => {
        resolveFetch = r
      })
    )

    const wrapper = mountView()
    await flushPromises()

    const menu = wrapper.findComponent(AccountActionMenu)
    const src = { id: 42, name: 'src', platform: 'anthropic', type: 'apikey' }
    menu.vm.$emit('clone', src)
    menu.vm.$emit('clone', src)
    await flushPromises()

    expect(getAccountById).toHaveBeenCalledTimes(1)
    resolveFetch({ id: 42, name: 'src' })
    await flushPromises()
    wrapper.unmount()
  })
})
