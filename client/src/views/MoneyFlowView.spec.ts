import { describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import MoneyFlowView from './MoneyFlowView.vue'
import MoneyFlowGraph from '../components/MoneyFlowGraph.vue'
import { MONEY_FLOW_QUERY } from '../graphql/moneyFlow'
import { mountWithApollo } from '../test/mountWithApollo'

const moneyFlow = {
  __typename: 'MoneyFlow',
  parties: [
    { __typename: 'MoneyFlowParty', id: 'entity:1', kind: 'INVESTOR', label: 'sim-3 investor 01' },
    { __typename: 'MoneyFlowParty', id: 'security:s', kind: 'SECURITY', label: '12 Oak Street (C, 11.00%)' },
    { __typename: 'MoneyFlowParty', id: 'entity:9', kind: 'BORROWER', label: 'sim-3 Keystone Homes 1' },
    { __typename: 'MoneyFlowParty', id: 'bank', kind: 'BANK', label: 'Bank' },
  ],
  movements: [
    ['DEPOSIT', 'bank', 'entity:1', '100000', '2026-09-01T00:00:00+00:00'],
    ['SUBSCRIPTION', 'entity:1', 'security:s', '50000', '2026-09-02T00:00:00+00:00'],
    ['DRAW', 'security:s', 'entity:9', '50000', '2026-09-03T00:00:00+00:00'],
    ['REPAYMENT', 'entity:9', 'security:s', '52500', '2026-09-10T00:00:00+00:00'],
    ['DISBURSEMENT_PRINCIPAL', 'security:s', 'entity:1', '50000', '2026-09-10T00:00:01+00:00'],
    ['DISBURSEMENT_INTEREST', 'security:s', 'entity:1', '2500', '2026-09-10T00:00:01+00:00'],
  ].map(([kind, source, target, amountMinorUnits, occurredAt], i) => ({
    __typename: 'Movement',
    kind,
    source,
    target,
    amountMinorUnits,
    occurredAt,
    transactionId: `t${i}`,
  })),
}

// jsdom cannot draw a canvas; the graph is stood in for by its props.
const GraphStub = defineComponent({
  name: 'MoneyFlowGraph',
  props: ['nodes', 'edges', 'positions', 'largestMinorUnits', 'selected'],
  template: '<div data-testid="graph-stub" />',
})

async function mountView(run: string | null = 'sim-3') {
  const wrapper = await mountWithApollo(MoneyFlowView, {
    path: run ? `/money-flow?run=${run}` : '/money-flow',
    mocks: [{ request: { query: MONEY_FLOW_QUERY, variables: { namePrefix: run } }, result: { data: { moneyFlow } } }],
    stubs: { MoneyFlowGraph: GraphStub },
  })
  await vi.waitFor(() => expect(wrapper.find('[data-testid="total-SUBSCRIPTION"]').exists()).toBe(true))
  return wrapper
}

describe('MoneyFlowView', () => {
  it('asks for the run named in the URL and totals what moved', async () => {
    const wrapper = await mountView()

    expect(wrapper.get('[data-testid="total-SUBSCRIPTION"]').text()).toBe('$500.00')
    expect(wrapper.get('[data-testid="total-DISBURSEMENT_INTEREST"]').text()).toBe('$25.00')
  })

  it('draws Investor → Security → Borrower and back, leaving the bank out by default', async () => {
    const wrapper = await mountView()
    const graph = wrapper.findComponent(MoneyFlowGraph)

    const edges = graph.props('edges') as { kind: string }[]
    expect(edges.map((e) => e.kind).sort()).toEqual([
      'DISBURSEMENT_INTEREST',
      'DISBURSEMENT_PRINCIPAL',
      'DRAW',
      'REPAYMENT',
      'SUBSCRIPTION',
    ])
  })

  it('shows the bank once its flows are switched on', async () => {
    const wrapper = await mountView()

    await wrapper.get('[data-testid="flow-bank"]').trigger('change')

    const nodes = wrapper.findComponent(MoneyFlowGraph).props('nodes') as { id: string }[]
    expect(nodes.map((n) => n.id)).toContain('bank')
  })

  it('collapses Securities into their Borrowers on request', async () => {
    const wrapper = await mountView()

    await wrapper.get('[data-testid="collapse"]').setValue(true)

    const nodes = wrapper.findComponent(MoneyFlowGraph).props('nodes') as { kind: string }[]
    expect(nodes.map((n) => n.kind)).not.toContain('SECURITY')
  })

  it('replays: dragging back in time hides what had not happened yet', async () => {
    const wrapper = await mountView()

    await wrapper.get('input[type="range"]').setValue(String(Date.parse('2026-09-02T12:00:00Z')))

    expect(wrapper.get('[data-testid="total-DRAW"]').text()).toBe('$0.00')
    const edges = wrapper.findComponent(MoneyFlowGraph).props('edges') as { kind: string }[]
    expect(edges.map((e) => e.kind)).toEqual(['SUBSCRIPTION'])
  })

  it('says so when nothing has moved', async () => {
    const wrapper = await mountWithApollo(MoneyFlowView, {
      path: '/money-flow?run=nobody',
      mocks: [
        {
          request: { query: MONEY_FLOW_QUERY, variables: { namePrefix: 'nobody' } },
          result: { data: { moneyFlow: { __typename: 'MoneyFlow', parties: [], movements: [] } } },
        },
      ],
      stubs: { MoneyFlowGraph: GraphStub },
    })
    await vi.waitFor(() => expect(wrapper.text()).toContain('No money has moved for nobody yet.'))
  })
})

