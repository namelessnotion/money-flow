import { describe, expect, it, vi } from 'vitest'
import InitiateAchForm from './InitiateAchForm.vue'
import {
  ACH_PAGE_SIZE,
  ACH_TRANSACTIONS_QUERY,
  INITIATE_ACH_DEPOSIT_MUTATION,
  INITIATE_ACH_WITHDRAWAL_MUTATION,
} from '../graphql/ach'
import { mountWithApollo } from '../test/mountWithApollo'
import { achSummary } from '../test/fixtures'

const emptyList = {
  request: { query: ACH_TRANSACTIONS_QUERY, variables: { entityId: '7', first: ACH_PAGE_SIZE } },
  result: {
    data: {
      achTransactions: {
        __typename: 'AchTransactionConnection',
        edges: [],
        pageInfo: { __typename: 'PageInfo', hasNextPage: false, endCursor: null },
      },
    },
  },
}

function mountForm(mocks: Parameters<typeof mountWithApollo>[1]['mocks']) {
  return mountWithApollo(InitiateAchForm, { mocks, props: { entityId: '7' } })
}

describe('InitiateAchForm', () => {
  it('enables both buttons only for a valid amount', async () => {
    const wrapper = await mountForm([])
    const buttons = () => ['deposit', 'withdrawal'].map((b) => wrapper.get(`[data-test=${b}]`).attributes('disabled'))

    expect(buttons()).toEqual(['', ''])

    await wrapper.get('input').setValue('1.234')
    expect(buttons()).toEqual(['', ''])
    expect(wrapper.text()).toContain('at most two decimal places')

    await wrapper.get('input').setValue('125.50')
    expect(buttons()).toEqual([undefined, undefined])
  })

  it('initiates a deposit in cents and clears the amount', async () => {
    const wrapper = await mountForm([
      {
        request: { query: INITIATE_ACH_DEPOSIT_MUTATION, variables: { entityId: '7', amountMinorUnits: '12550' } },
        result: {
          data: {
            initiateAchDeposit: {
              __typename: 'InitiateAchDepositPayload',
              achTransaction: achSummary({ amountMinorUnits: '12550', state: null }),
            },
          },
        },
      },
      emptyList,
    ])

    await wrapper.get('input').setValue('125.50')
    await wrapper.get('[data-test=deposit]').trigger('click')

    await vi.waitFor(() => expect((wrapper.get('input').element as HTMLInputElement).value).toBe(''))
    expect(wrapper.find('.text-rose-700').exists()).toBe(false)
  })

  it('shows why a withdrawal was refused and keeps the amount', async () => {
    const wrapper = await mountForm([
      {
        request: { query: INITIATE_ACH_WITHDRAWAL_MUTATION, variables: { entityId: '7', amountMinorUnits: '4000' } },
        result: { errors: [{ message: 'entity 7 has no bank account' }] },
      },
    ])

    await wrapper.get('input').setValue('40')
    await wrapper.get('[data-test=withdrawal]').trigger('click')

    await vi.waitFor(() => expect(wrapper.text()).toContain('entity 7 has no bank account'))
    expect((wrapper.get('input').element as HTMLInputElement).value).toBe('40')
  })
})
