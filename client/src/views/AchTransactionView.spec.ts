import { describe, expect, it, vi } from 'vitest'
import AchTransactionView from './AchTransactionView.vue'
import { ACH_TRANSACTION_QUERY } from '../graphql/ach'
import { mountWithApollo } from '../test/mountWithApollo'
import { achDetail } from '../test/fixtures'

function mountView(transaction: ReturnType<typeof achDetail>) {
  return mountWithApollo(AchTransactionView, {
    path: `/entities/7/ach-transactions/${transaction.id}`,
    props: { entityId: '7', id: transaction.id },
    mocks: [
      {
        request: { query: ACH_TRANSACTION_QUERY, variables: { id: transaction.id } },
        result: { data: { achTransaction: transaction } },
      },
    ],
  })
}

describe('AchTransactionView', () => {
  it('shows the amount and each step, and offers the provider notices while settlement waits', async () => {
    const wrapper = await mountView(achDetail())

    await vi.waitFor(() => expect(wrapper.find('h1').exists()).toBe(true))

    expect(wrapper.get('h1').text()).toBe('$125.00')
    expect(wrapper.findAll('[data-step]').map((li) => li.attributes('data-status'))).toEqual([
      'DONE',
      'DONE',
      'WAITING',
      'WAITING',
      'WAITING',
    ])
    expect(wrapper.find('[data-test=settle]').exists()).toBe(true)
  })

  it('offers no provider notices once the entry was returned', async () => {
    const wrapper = await mountView(
      achDetail({
        state: 'ROLLED_BACK',
        realLegState: 'CANCELLED',
        reason: 'R01 insufficient funds',
        steps: [
          { name: 'INITIATION', status: 'DONE' },
          { name: 'SUBMISSION', status: 'DONE' },
          { name: 'SETTLEMENT', status: 'FAILED' },
          { name: 'COMPLETION', status: 'SKIPPED' },
          { name: 'CLEARING', status: 'SKIPPED' },
        ],
      }),
    )

    await vi.waitFor(() => expect(wrapper.find('h1').exists()).toBe(true))

    expect(wrapper.text()).toContain('R01 insufficient funds')
    expect(wrapper.find('[data-test=settle]').exists()).toBe(false)
  })

  const completedDeposit = (overrides: Parameters<typeof achDetail>[0] = {}) =>
    achDetail({
      state: 'COMPLETED',
      realLegState: 'COMMITTED',
      clearingDueOn: '2026-09-23',
      steps: [
        { name: 'INITIATION', status: 'DONE' },
        { name: 'SUBMISSION', status: 'DONE' },
        { name: 'SETTLEMENT', status: 'DONE' },
        { name: 'COMPLETION', status: 'DONE' },
        { name: 'CLEARING', status: 'WAITING' },
      ],
      ...overrides,
    })

  it('offers to clear a completed deposit now instead of waiting for the sweep', async () => {
    const wrapper = await mountView(completedDeposit())

    await vi.waitFor(() => expect(wrapper.find('h1').exists()).toBe(true))

    expect(wrapper.find('[data-test=clear]').exists()).toBe(true)
    expect(wrapper.find('[data-test=settle]').exists()).toBe(false)
  })

  it('offers no clearing once one is under way', async () => {
    const wrapper = await mountView(completedDeposit({ clearingState: 'STARTED' }))

    await vi.waitFor(() => expect(wrapper.find('h1').exists()).toBe(true))

    expect(wrapper.find('[data-test=clear]').exists()).toBe(false)
  })
})
