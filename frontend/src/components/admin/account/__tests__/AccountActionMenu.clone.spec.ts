import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import AccountActionMenu from '../AccountActionMenu.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

function makeAccount(overrides: Record<string, unknown> = {}) {
  return {
    id: 1,
    name: 'src',
    platform: 'anthropic',
    type: 'apikey',
    parent_account_id: null,
    ...overrides
  } as any
}

// AccountActionMenu uses <Teleport to="body">; content is rendered in document.body, not in wrapper.
const getBodyButtons = () => Array.from(document.body.querySelectorAll('button'))
const cloneButtons = () => getBodyButtons().filter(b => b.textContent?.includes('cloneAccount'))

describe('AccountActionMenu - clone', () => {
  it('hides clone button for oauth accounts', () => {
    const wrapper = mount(AccountActionMenu, {
      props: { show: true, account: makeAccount({ type: 'oauth' }), position: { top: 0, left: 0 } },
      attachTo: document.body
    })
    expect(cloneButtons().length).toBe(0)
    wrapper.unmount()
  })

  it('hides clone button for setup-token accounts', () => {
    const wrapper = mount(AccountActionMenu, {
      props: { show: true, account: makeAccount({ type: 'setup-token' }), position: { top: 0, left: 0 } },
      attachTo: document.body
    })
    expect(cloneButtons().length).toBe(0)
    wrapper.unmount()
  })

  it('hides clone button for shadow accounts (parent_account_id set)', () => {
    const wrapper = mount(AccountActionMenu, {
      props: { show: true, account: makeAccount({ parent_account_id: 99 }), position: { top: 0, left: 0 } },
      attachTo: document.body
    })
    expect(cloneButtons().length).toBe(0)
    wrapper.unmount()
  })

  it('shows clone button for apikey/upstream/bedrock/service_account', () => {
    for (const type of ['apikey', 'upstream', 'bedrock', 'service_account']) {
      const wrapper = mount(AccountActionMenu, {
        props: { show: true, account: makeAccount({ type }), position: { top: 0, left: 0 } },
        attachTo: document.body
      })
      expect(cloneButtons().length, `type=${type}`).toBe(1)
      wrapper.unmount()
    }
  })

  it('emits clone event with the account when clone button is clicked', async () => {
    const account = makeAccount({ id: 42 })
    const wrapper = mount(AccountActionMenu, {
      props: { show: true, account, position: { top: 0, left: 0 } },
      attachTo: document.body
    })
    const cloneBtn = cloneButtons()[0]
    expect(cloneBtn).toBeDefined()
    cloneBtn.click()
    await wrapper.vm.$nextTick()
    const emitted = wrapper.emitted('clone')
    expect(emitted).toBeTruthy()
    expect(emitted![0][0]).toEqual(account)
    expect(wrapper.emitted('close')).toBeTruthy()
    wrapper.unmount()
  })
})
