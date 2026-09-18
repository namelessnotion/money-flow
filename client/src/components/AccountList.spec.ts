import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import AccountList from './AccountList.vue'
import type { AccountBalance, AccountNode } from '../graphql/entity'

function account(balances: AccountBalance[]): AccountNode {
  return { id: '1', name: 'Cash', type: 'cash', walletUuid: 'w-1', balances }
}

function balance(overrides: Partial<AccountBalance> = {}): AccountBalance {
  return {
    currency: 'USD',
    postedMinorUnits: '0',
    pendingOutgoingMinorUnits: '0',
    pendingIncomingMinorUnits: '0',
    ...overrides,
  }
}

describe('AccountList', () => {
  it("shows each Account's posted balance", () => {
    const wrapper = mount(AccountList, { props: { accounts: [account([balance({ postedMinorUnits: '125050' })])] } })

    expect(wrapper.get('[data-balance=posted]').text()).toBe('$1,250.50')
    expect(wrapper.find('[data-balance=pending-out]').exists()).toBe(false)
    expect(wrapper.find('[data-balance=pending-in]').exists()).toBe(false)
  })

  it('shows an overdrawn Account as negative', () => {
    const wrapper = mount(AccountList, { props: { accounts: [account([balance({ postedMinorUnits: '-5000' })])] } })

    expect(wrapper.get('[data-balance=posted]').text()).toBe('-$50.00')
  })

  it('shows money on its way out and in while a Transfer is staged', () => {
    const staged = balance({ postedMinorUnits: '10000', pendingOutgoingMinorUnits: '5000', pendingIncomingMinorUnits: '2000' })
    const wrapper = mount(AccountList, { props: { accounts: [account([staged])] } })

    expect(wrapper.get('[data-balance=pending-out]').text()).toBe('−$50.00 pending out')
    expect(wrapper.get('[data-balance=pending-in]').text()).toBe('+$20.00 pending in')
  })

  it('shows $0.00 for an Account with no ledger activity yet', () => {
    const wrapper = mount(AccountList, { props: { accounts: [account([])] } })

    expect(wrapper.get('[data-balance=posted]').text()).toBe('$0.00')
  })
})
