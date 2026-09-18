import { describe, expect, it, vi } from 'vitest'
import EntityView from './EntityView.vue'
import { ENTITY_QUERY } from '../graphql/entity'
import { ACH_PAGE_SIZE, ACH_TRANSACTIONS_QUERY } from '../graphql/ach'
import { mountWithApollo } from '../test/mountWithApollo'
import { achSummary } from '../test/fixtures'

const entity = {
  __typename: 'Entity',
  id: '7',
  name: 'Shining Knight Industries',
  holderUuid: '018f4d2e-0000-7000-8000-000000000000',
  createdAt: '2026-08-27T00:00:00Z',
  accounts: [
    {
      __typename: 'Account',
      id: '1',
      name: 'Bank',
      type: 'bank',
      walletUuid: 'w-1',
      balances: [
        {
          __typename: 'AccountBalance',
          currency: 'USD',
          postedMinorUnits: '-5000',
          pendingOutgoingMinorUnits: '0',
          pendingIncomingMinorUnits: '0',
        },
      ],
    },
    { __typename: 'Account', id: '2', name: 'Cash', type: 'cash', walletUuid: 'w-2', balances: [] },
  ],
}

const transactions = {
  request: { query: ACH_TRANSACTIONS_QUERY, variables: { entityId: '7', first: ACH_PAGE_SIZE } },
  result: {
    data: {
      achTransactions: {
        __typename: 'AchTransactionConnection',
        edges: [
          { __typename: 'AchTransactionEdge', cursor: 'c1', node: achSummary() },
          {
            __typename: 'AchTransactionEdge',
            cursor: 'c2',
            node: achSummary({ id: 'tx-2', direction: 'WITHDRAWAL', amountMinorUnits: '4000', state: null }),
          },
        ],
        pageInfo: { __typename: 'PageInfo', hasNextPage: false, endCursor: 'c2' },
      },
    },
  },
}

describe('EntityView', () => {
  it("lists the entity's accounts and ACH transactions, each linking to its view", async () => {
    const wrapper = await mountWithApollo(EntityView, {
      path: '/entities/7',
      props: { entityId: '7' },
      mocks: [{ request: { query: ENTITY_QUERY, variables: { id: '7' } }, result: { data: { entity } } }, transactions],
    })

    await vi.waitFor(() => expect(wrapper.text()).toContain('ACH Withdrawal · $40.00'))

    expect(wrapper.get('h1').text()).toBe('Shining Knight Industries')
    expect(wrapper.text()).toContain('Bank')
    expect(wrapper.text()).toContain('-$50.00')
    expect(wrapper.text()).toContain('ACH Deposit · $125.00')
    expect(wrapper.text()).toContain('Awaiting ledger')
    expect(wrapper.findAll('a').map((a) => a.attributes('href'))).toContain(
      '/entities/7/ach-transactions/tx-2',
    )
  })

  it('says so when there is no such entity', async () => {
    const wrapper = await mountWithApollo(EntityView, {
      path: '/entities/99',
      props: { entityId: '99' },
      mocks: [{ request: { query: ENTITY_QUERY, variables: { id: '99' } }, result: { data: { entity: null } } }],
    })

    await vi.waitFor(() => expect(wrapper.text()).toContain('No entity 99.'))
  })
})
