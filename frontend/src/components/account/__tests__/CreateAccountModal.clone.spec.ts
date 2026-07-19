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

describe('CreateAccountModal - non-sensitive credential fields', () => {
  it('prefills mixed model_mapping without dropping whitelist entries', async () => {
    const cloneFrom = baseSource({
      platform: 'openai', type: 'apikey',
      credentials: { model_mapping: { 'gpt-4': 'gpt-4', 'claude-*': 'claude-sonnet-4' } }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    await wrapper.vm.$nextTick()
    await wrapper.vm.$nextTick()
    const vm = wrapper.vm as any
    expect(vm.modelRestrictionMode).toBe('combined')
    expect(vm.allowedModels).toContain('gpt-4')
    expect(vm.modelMappings.find((m: any) => m.from === 'claude-*' && m.to === 'claude-sonnet-4')).toBeTruthy()
  })

  it('simulates AccountsView flow: set cloneFrom first, then show', async () => {
    const cloneFrom = baseSource({
      platform: 'openai', type: 'apikey',
      credentials: { model_mapping: { 'gpt-4': 'gpt-4', 'gpt-3.5': 'gpt-4-turbo', 'claude-*': 'claude-sonnet-4' } }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    // Simulate AccountsView: cloneFrom set first, then show=true (separate reactivity ticks)
    await wrapper.setProps({ cloneFrom })
    await flushPromises()
    await wrapper.setProps({ show: true })
    await flushPromises()
    await wrapper.vm.$nextTick()
    await wrapper.vm.$nextTick()
    await wrapper.vm.$nextTick()
    const vm = wrapper.vm as any
    expect(vm.modelRestrictionMode).toBe('combined')
    expect(vm.allowedModels).toEqual(['gpt-4'])
    expect(vm.modelMappings).toEqual([
      { from: 'gpt-3.5', to: 'gpt-4-turbo' },
      { from: 'claude-*', to: 'claude-sonnet-4' }
    ])
  })

  it('prefills whitelist-only model_mapping', async () => {
    const cloneFrom = baseSource({
      platform: 'openai', type: 'apikey',
      credentials: { model_mapping: { 'custom-model': 'custom-model' } }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    const vm = wrapper.vm as any
    vm.modelRestrictionMode = 'mapping'
    await wrapper.vm.$nextTick()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    await wrapper.vm.$nextTick()
    expect(vm.modelRestrictionMode).toBe('whitelist')
    expect(vm.allowedModels).toEqual(['custom-model'])
    expect(vm.modelMappings).toEqual([])
  })

  it('prefills header override rows and enables headerOverrideEnabled', async () => {
    const cloneFrom = baseSource({
      credentials: { custom_headers: { 'X-Custom': 'val1', 'X-Other': 'val2' } }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.headerOverrideEnabled).toBe(true)
    expect(vm.headerOverrideRows).toEqual([
      { name: 'X-Custom', value: 'val1' },
      { name: 'X-Other', value: 'val2' }
    ])
  })

  it('prefills OpenAI specific flags', async () => {
    const cloneFrom = baseSource({
      platform: 'openai', type: 'apikey',
      credentials: { openai_ws_mode_v2: 'ctx_pool', codex_cli_only_enabled: true, codex_app_server_only_enabled: true }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.openaiAPIKeyResponsesWebSocketV2Mode).toBe('ctx_pool')
    expect(vm.codexCLIOnlyEnabled).toBe(true)
    expect(vm.codexCLIOnlyAppServerEnabled).toBe(true)
  })

  it('prefills Anthropic API key auth scheme', async () => {
    const cloneFrom = baseSource({
      platform: 'anthropic', type: 'apikey',
      credentials: { anthropic_api_key_auth_scheme: 'authorization_bearer' }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.anthropicAPIKeyAuthScheme).toBe('authorization_bearer')
  })

  it('prefills Antigravity project id', async () => {
    const cloneFrom = baseSource({
      platform: 'antigravity', type: 'upstream',
      credentials: { antigravity_project_id: 'proj-123' }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.antigravityProjectId).toBe('proj-123')
  })
})

describe('CreateAccountModal - extra fields', () => {
  it('prefills quota limits and reset config', async () => {
    const cloneFrom = baseSource({
      platform: 'anthropic', type: 'apikey',
      extra: { quota_limit: 100, quota_daily_limit: 10, quota_weekly_limit: 50,
               quota_daily_reset_mode: 'fixed', quota_daily_reset_hour: 8,
               quota_weekly_reset_mode: 'fixed', quota_weekly_reset_day: 1, quota_weekly_reset_hour: 9,
               quota_reset_timezone: 'Asia/Shanghai' }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.editQuotaLimit).toBe(100)
    expect(vm.editQuotaDailyLimit).toBe(10)
    expect(vm.editQuotaWeeklyLimit).toBe(50)
    expect(vm.editDailyResetMode).toBe('fixed')
    expect(vm.editDailyResetHour).toBe(8)
    expect(vm.editWeeklyResetMode).toBe('fixed')
    expect(vm.editWeeklyResetDay).toBe(1)
    expect(vm.editWeeklyResetHour).toBe(9)
    expect(vm.editResetTimezone).toBe('Asia/Shanghai')
  })

  it('prefills TLS fingerprint, session masking, cache TTL, custom base URL', async () => {
    const cloneFrom = baseSource({
      extra: { enable_tls_fingerprint: true, tls_fingerprint_profile_id: 5,
               session_id_masking_enabled: true,
               cache_ttl_override_enabled: true, cache_ttl_override_target: '1h',
               custom_base_url_enabled: true, custom_base_url: 'https://proxy.example.com' }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.tlsFingerprintEnabled).toBe(true)
    expect(vm.tlsFingerprintProfileId).toBe(5)
    expect(vm.sessionIdMaskingEnabled).toBe(true)
    expect(vm.cacheTTLOverrideEnabled).toBe(true)
    expect(vm.cacheTTLOverrideTarget).toBe('1h')
    expect(vm.customBaseUrlEnabled).toBe(true)
    expect(vm.customBaseUrl).toBe('https://proxy.example.com')
  })

  it('prefills RPM limit and queue mode', async () => {
    const cloneFrom = baseSource({
      extra: { base_rpm: 60, rpm_strategy: 'sticky_exempt', rpm_sticky_buffer: 5, user_msg_queue_mode: 'serialize' }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.rpmLimitEnabled).toBe(true)
    expect(vm.baseRpm).toBe(60)
    expect(vm.rpmStrategy).toBe('sticky_exempt')
    expect(vm.rpmStickyBuffer).toBe(5)
    expect(vm.userMsgQueueMode).toBe('serialize')
  })

  it('prefills window cost and session limit', async () => {
    const cloneFrom = baseSource({
      extra: { window_cost_limit: 5.5, window_cost_sticky_reserve: 0.5,
               max_sessions: 3, session_idle_timeout_minutes: 15 }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.windowCostEnabled).toBe(true)
    expect(vm.windowCostLimit).toBe(5.5)
    expect(vm.windowCostStickyReserve).toBe(0.5)
    expect(vm.sessionLimitEnabled).toBe(true)
    expect(vm.maxSessions).toBe(3)
    expect(vm.sessionIdleTimeout).toBe(15)
  })

  it('prefills pool mode and intercept warmup', async () => {
    const cloneFrom = baseSource({
      extra: { pool_mode_enabled: true, pool_mode_retry_count: 3, pool_mode_retry_status_codes: [429, 500],
               intercept_warmup_requests: true }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.poolModeEnabled).toBe(true)
    expect(vm.poolModeRetryCount).toBe(3)
    expect(vm.poolModeRetryStatusCodesInput).toBe('429,500')
    expect(vm.interceptWarmupRequests).toBe(true)
  })

  it('prefills custom error codes', async () => {
    const cloneFrom = baseSource({
      extra: { custom_error_codes_enabled: true, custom_error_codes: [400, 403] }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.customErrorCodesEnabled).toBe(true)
    expect(vm.selectedErrorCodes).toEqual([400, 403])
  })

  it('prefills OpenAI-specific extra', async () => {
    const cloneFrom = baseSource({
      platform: 'openai', type: 'apikey',
      extra: { openai_passthrough_enabled: true, openai_long_context_billing_enabled: true,
               openai_compact_mode: 'on', openai_responses_mode: 'responses',
               openai_endpoint_capabilities: ['chat_completions', 'responses'] }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.openaiPassthroughEnabled).toBe(true)
    expect(vm.openAILongContextBillingEnabled).toBe(true)
    expect(vm.openAICompactMode).toBe('on')
    expect(vm.openAIResponsesMode).toBe('responses')
    expect(vm.openAIEndpointCapabilities).toEqual(['chat_completions', 'responses'])
  })

  it('prefills Antigravity mixed scheduling and overages', async () => {
    const cloneFrom = baseSource({
      platform: 'antigravity', type: 'upstream',
      extra: { antigravity_mixed_scheduling_enabled: true, antigravity_allow_overages: true }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.mixedScheduling).toBe(true)
    expect(vm.allowOverages).toBe(true)
  })

  it('prefills web search and anthropic passthrough', async () => {
    const cloneFrom = baseSource({
      platform: 'anthropic', type: 'apikey',
      extra: { web_search_emulation_mode: 'claude', web_search_global_enabled: true,
               anthropic_passthrough_enabled: true }
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.webSearchEmulationMode).toBe('claude')
    expect(vm.webSearchGlobalEnabled).toBe(true)
    expect(vm.anthropicPassthroughEnabled).toBe(true)
  })
})

describe('CreateAccountModal - state selectors and submission', () => {
  it('sets accountCategory=apikey for anthropic apikey', async () => {
    const cloneFrom = baseSource({ platform: 'anthropic', type: 'apikey' })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    expect((wrapper.vm as any).accountCategory).toBe('apikey')
  })

  it('sets accountCategory=bedrock for anthropic bedrock', async () => {
    const cloneFrom = baseSource({ platform: 'anthropic', type: 'bedrock', extra: { bedrock_auth_mode: 'apikey' } })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.accountCategory).toBe('bedrock')
    expect(vm.bedrockAuthMode).toBe('apikey')
  })

  it('sets accountCategory=service_account for service_account', async () => {
    const cloneFrom = baseSource({ platform: 'gemini', type: 'service_account' })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    expect((wrapper.vm as any).accountCategory).toBe('service_account')
  })

  it('sets antigravityAccountType=upstream for antigravity upstream', async () => {
    const cloneFrom = baseSource({ platform: 'antigravity', type: 'upstream' })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.antigravityAccountType).toBe('upstream')
    expect(vm.accountCategory).toBe('apikey')
  })

  it('regression: opens as fresh new-account modal when cloneFrom is null', async () => {
    const wrapper = mountModal({ show: true })
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.form.name).toBe('')
    expect(vm.form.platform).toBe('anthropic')
    expect(vm.apiKeyValue).toBe('')
    const dialog = wrapper.find('[data-title]')
    expect(dialog.attributes('data-title')).toBe('admin.accounts.createAccount')
  })

  it('regression: closing modal and reopening without cloneFrom resets state', async () => {
    const wrapper = mountModal({ show: true, cloneFrom: baseSource({ name: 'first' }) })
    await flushPromises()
    expect((wrapper.vm as any).form.name).toBe('first (Copy)')
    await wrapper.setProps({ show: false, cloneFrom: null })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom: null })
    await flushPromises()
    expect((wrapper.vm as any).form.name).toBe('')
  })

  it('submits via create API with cloneFrom values', async () => {
    createAccountMock.mockResolvedValue({ id: 999, name: 'src (Copy)' })
    const cloneFrom = baseSource({
      name: 'src', platform: 'anthropic', type: 'apikey',
      concurrency: 15, priority: 3, group_ids: [1, 2]
    })
    const wrapper = mountModal({ show: false })
    await flushPromises()
    await wrapper.setProps({ show: true, cloneFrom })
    await flushPromises()
    await wrapper.get('form#create-account-form input[type="password"]').setValue('new-api-key')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()
    expect(createAccountMock).toHaveBeenCalledTimes(1)
    const payload = createAccountMock.mock.calls[0][0]
    expect(payload.name).toBe('src (Copy)')
    expect(payload.platform).toBe('anthropic')
    expect(payload.type).toBe('apikey')
    expect(payload.concurrency).toBe(15)
    expect(payload.priority).toBe(3)
    expect(payload.group_ids).toEqual([1, 2])
  })
})
