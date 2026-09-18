import { describe, expect, it, vi } from 'vitest'
import ClearingNotice from './ClearingNotice.vue'
import { CLEAR_ACH_MUTATION } from '../graphql/ach'
import { mountWithApollo } from '../test/mountWithApollo'
import { achDetail } from '../test/fixtures'

describe('ClearingNotice', () => {
  it('asks the backend to clear the deposit now', async () => {
    const cleared = achDetail({ state: 'COMPLETED', clearingState: 'INITIALIZED' })
    const request = vi.fn(() => ({ data: { clearAch: { __typename: 'ClearAchPayload', achTransaction: cleared } } }))
    const wrapper = await mountWithApollo(ClearingNotice, {
      props: { achTransactionId: cleared.id },
      mocks: [{ request: { query: CLEAR_ACH_MUTATION, variables: { achTransactionId: cleared.id } }, result: request }],
    })

    await wrapper.get('[data-test=clear]').trigger('click')

    await vi.waitFor(() => expect(request).toHaveBeenCalledOnce())
  })

  it('shows why the backend refused', async () => {
    const wrapper = await mountWithApollo(ClearingNotice, {
      props: { achTransactionId: 'tx-1' },
      mocks: [
        {
          request: { query: CLEAR_ACH_MUTATION, variables: { achTransactionId: 'tx-1' } },
          result: { errors: [{ message: 'tx-1 has not completed' }] },
        },
      ],
    })

    await wrapper.get('[data-test=clear]').trigger('click')

    await vi.waitFor(() => expect(wrapper.text()).toContain('Unable to clear: tx-1 has not completed'))
  })
})
