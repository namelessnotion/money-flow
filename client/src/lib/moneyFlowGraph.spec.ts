import { describe, expect, it } from 'vitest'
import { buildGraph, layout, ALL_KINDS, flowOf, timeline, totalsUntil } from './moneyFlowGraph'
import type { MoneyFlowParty, Movement, MovementKind } from '../graphql/moneyFlow'

const parties: MoneyFlowParty[] = [
  { id: 'bank', kind: 'BANK', label: 'Bank' },
  { id: 'entity:1', kind: 'INVESTOR', label: 'investor 01' },
  { id: 'entity:2', kind: 'INVESTOR', label: 'investor 02' },
  { id: 'security:s', kind: 'SECURITY', label: '12 Oak Street' },
  { id: 'entity:9', kind: 'BORROWER', label: 'Keystone Homes' },
]

let sequence = 0
function move(kind: MovementKind, source: string, target: string, amount: number, at: string): Movement {
  sequence += 1
  return {
    kind,
    source,
    target,
    amountMinorUnits: String(amount),
    occurredAt: at,
    transactionId: `t${sequence}`,
  }
}

const movements: Movement[] = [
  move('DEPOSIT', 'bank', 'entity:1', 50_000, '2026-09-01T00:00:00Z'),
  move('SUBSCRIPTION', 'entity:1', 'security:s', 3_000, '2026-09-02T00:00:00Z'),
  move('SUBSCRIPTION', 'entity:1', 'security:s', 2_000, '2026-09-02T00:00:01Z'),
  move('SUBSCRIPTION', 'entity:2', 'security:s', 5_000, '2026-09-02T00:00:02Z'),
  move('DRAW', 'security:s', 'entity:9', 10_000, '2026-09-03T00:00:00Z'),
  move('REPAYMENT', 'entity:9', 'security:s', 10_900, '2026-09-10T00:00:00Z'),
  move('DISBURSEMENT_PRINCIPAL', 'security:s', 'entity:1', 5_000, '2026-09-10T00:00:01Z'),
  move('DISBURSEMENT_INTEREST', 'security:s', 'entity:1', 450, '2026-09-10T00:00:01Z'),
  move('DISBURSEMENT_PRINCIPAL', 'security:s', 'entity:2', 5_000, '2026-09-10T00:00:02Z'),
  move('DISBURSEMENT_INTEREST', 'security:s', 'entity:2', 450, '2026-09-10T00:00:02Z'),
]

const everything = { until: Infinity, kinds: new Set(ALL_KINDS), collapseSecurities: false }

function edge(graph: ReturnType<typeof buildGraph>, source: string, target: string, kind: MovementKind) {
  return graph.edges.find((e) => e.source === source && e.target === target && e.kind === kind)
}

describe('buildGraph', () => {
  it('sums the Movements between two Parties of one kind into one edge', () => {
    const graph = buildGraph(parties, movements, everything)

    const bought = edge(graph, 'entity:1', 'security:s', 'SUBSCRIPTION')
    expect(bought?.amountMinorUnits).toBe(5_000n)
    expect(bought?.count).toBe(2)
  })

  it('keeps principal and interest back to an Investor as separate edges', () => {
    const graph = buildGraph(parties, movements, everything)

    expect(edge(graph, 'security:s', 'entity:1', 'DISBURSEMENT_PRINCIPAL')?.amountMinorUnits).toBe(5_000n)
    expect(edge(graph, 'security:s', 'entity:1', 'DISBURSEMENT_INTEREST')?.amountMinorUnits).toBe(450n)
  })

  it('shows only the Parties some visible edge touches', () => {
    const graph = buildGraph(parties, movements, { ...everything, kinds: new Set<MovementKind>(['SUBSCRIPTION']) })

    expect(graph.nodes.map((n) => n.id).sort()).toEqual(['entity:1', 'entity:2', 'security:s'])
  })

  it('shows only what had happened by the cutoff', () => {
    const graph = buildGraph(parties, movements, { ...everything, until: Date.parse('2026-09-03T00:00:00Z') })

    expect(graph.edges.map((e) => e.kind).sort()).toEqual(['DEPOSIT', 'DRAW', 'SUBSCRIPTION', 'SUBSCRIPTION'])
  })

  describe('with Securities collapsed', () => {
    const collapsed = { ...everything, collapseSecurities: true }

    it('runs money straight from Investor to Borrower and back', () => {
      const graph = buildGraph(parties, movements, collapsed)

      expect(edge(graph, 'entity:1', 'entity:9', 'SUBSCRIPTION')?.amountMinorUnits).toBe(5_000n)
      expect(edge(graph, 'entity:9', 'entity:2', 'DISBURSEMENT_INTEREST')?.amountMinorUnits).toBe(450n)
      expect(graph.nodes.some((n) => n.kind === 'SECURITY')).toBe(false)
    })

    it('drops the Draw and the Repayment, which would run from the Borrower to itself', () => {
      const graph = buildGraph(parties, movements, collapsed)

      expect(graph.edges.some((e) => e.kind === 'DRAW' || e.kind === 'REPAYMENT')).toBe(false)
    })

    it('leaves a Security nobody has drawn yet in place, since it has no Borrower to stand for', () => {
      const undrawn = [move('SUBSCRIPTION', 'entity:1', 'security:new', 1_000, '2026-09-11T00:00:00Z')]
      const graph = buildGraph(
        [...parties, { id: 'security:new', kind: 'SECURITY', label: '9 Elm Lane' }],
        [...movements, ...undrawn],
        collapsed,
      )

      expect(edge(graph, 'entity:1', 'security:new', 'SUBSCRIPTION')?.amountMinorUnits).toBe(1_000n)
    })
  })
})

describe('layout', () => {
  it('puts the Bank, Investors, Securities and Borrowers in columns, left to right', () => {
    const positions = layout(parties, movements, false)
    const x = (id: string) => positions.get(id)?.x ?? NaN

    expect(x('bank')).toBeLessThan(x('entity:1'))
    expect(x('entity:1')).toBe(x('entity:2'))
    expect(x('entity:1')).toBeLessThan(x('security:s'))
    expect(x('security:s')).toBeLessThan(x('entity:9'))
  })

  it('places every Party the same wherever the cutoff is, so the replay never jumps', () => {
    expect(layout(parties, movements, false)).toEqual(layout(parties, movements, false))
  })
})

describe('flowOf', () => {
  it('groups kinds by which way the money is going', () => {
    expect(flowOf('SUBSCRIPTION')).toBe('out')
    expect(flowOf('DRAW')).toBe('out')
    expect(flowOf('REPAYMENT')).toBe('back')
    expect(flowOf('DISBURSEMENT_PRINCIPAL')).toBe('back')
    expect(flowOf('DISBURSEMENT_INTEREST')).toBe('interest')
    expect(flowOf('DEPOSIT')).toBe('bank')
  })
})

describe('timeline', () => {
  it('spans the first Movement to the last', () => {
    expect(timeline(movements)).toEqual({
      start: Date.parse('2026-09-01T00:00:00Z'),
      end: Date.parse('2026-09-10T00:00:02Z'),
    })
  })

  it('is empty when nothing has moved', () => {
    expect(timeline([])).toBeNull()
  })
})

describe('totalsUntil', () => {
  it('adds up what had moved by the cutoff, by kind', () => {
    const totals = totalsUntil(movements, Date.parse('2026-09-03T00:00:00Z'))

    expect(totals.SUBSCRIPTION).toBe(10_000n)
    expect(totals.DRAW).toBe(10_000n)
    expect(totals.DISBURSEMENT_INTEREST).toBe(0n)
  })
})
