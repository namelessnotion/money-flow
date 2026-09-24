import type { MoneyFlowParty, Movement, MovementKind, PartyKind } from '../graphql/moneyFlow'

// Turns the money flow (Parties and the Movements between them) into what a
// graph draws: a node per Party, and an edge per (source, target, kind) summing
// every Movement along it. Pure, so the replay can call it once per frame.

export const ALL_KINDS: readonly MovementKind[] = [
  'DEPOSIT',
  'SUBSCRIPTION',
  'DRAW',
  'REPAYMENT',
  'DISBURSEMENT_PRINCIPAL',
  'DISBURSEMENT_INTEREST',
  'WITHDRAWAL',
]

// Which way money is going, which is what an edge is coloured by: out to the
// Borrower, back towards the Investors, interest earned, or across the bank
// boundary. A Repayment carries principal and interest together, and counts as
// money coming back.
export type Flow = 'out' | 'back' | 'interest' | 'bank'

const FLOWS: Record<MovementKind, Flow> = {
  DEPOSIT: 'bank',
  WITHDRAWAL: 'bank',
  SUBSCRIPTION: 'out',
  DRAW: 'out',
  REPAYMENT: 'back',
  DISBURSEMENT_PRINCIPAL: 'back',
  DISBURSEMENT_INTEREST: 'interest',
}

export function flowOf(kind: MovementKind): Flow {
  return FLOWS[kind]
}

// The one place a Flow's colour is decided: the graph's edges and the
// filters' swatches both read it.
export const FLOW_COLOURS: Record<Flow, string> = {
  out: '#2563eb',
  back: '#059669',
  interest: '#d97706',
  bank: '#94a3b8',
}

export interface GraphOptions {
  // Epoch milliseconds; only Movements that had completed by then are drawn.
  until: number
  kinds: ReadonlySet<MovementKind>
  // Draw each Security as its Borrower, so money runs Investor ↔ Borrower.
  collapseSecurities: boolean
}

export interface GraphNode {
  id: string
  kind: PartyKind
  label: string
}

export interface GraphEdge {
  id: string
  source: string
  target: string
  kind: MovementKind
  amountMinorUnits: bigint
  count: number
}

export interface Graph {
  nodes: GraphNode[]
  edges: GraphEdge[]
}

export function buildGraph(parties: MoneyFlowParty[], movements: Movement[], options: GraphOptions): Graph {
  const standIn = options.collapseSecurities ? borrowerOf(movements) : new Map<string, string>()
  const endOf = (id: string) => standIn.get(id) ?? id
  const edges = new Map<string, GraphEdge>()

  for (const movement of movements) {
    if (!options.kinds.has(movement.kind) || Date.parse(movement.occurredAt) > options.until) continue

    const source = endOf(movement.source)
    const target = endOf(movement.target)
    if (source === target) continue

    const id = `${source}|${target}|${movement.kind}`
    const edge = edges.get(id) ?? { id, source, target, kind: movement.kind, amountMinorUnits: 0n, count: 0 }
    edge.amountMinorUnits += BigInt(movement.amountMinorUnits)
    edge.count += 1
    edges.set(id, edge)
  }

  const touched = new Set([...edges.values()].flatMap((e) => [e.source, e.target]))
  const nodes = parties.filter((p) => touched.has(p.id)).map(({ id, kind, label }) => ({ id, kind, label }))
  return { nodes, edges: [...edges.values()] }
}

// Security id => its Borrower's id, learned from the Draw or a Repayment —
// the only Movements that name both. A Security nobody has drawn has no
// Borrower yet, so it has no stand-in and stays as itself.
function borrowerOf(movements: Movement[]): Map<string, string> {
  const borrowers = new Map<string, string>()
  for (const m of movements) {
    if (m.kind === 'DRAW') borrowers.set(m.source, m.target)
    if (m.kind === 'REPAYMENT') borrowers.set(m.target, m.source)
  }
  return borrowers
}

const COLUMN: Record<PartyKind, number> = { BANK: 0, ISSUER: 0, INVESTOR: 1, SECURITY: 2, BORROWER: 3 }
const ROW_GAP = 44
// Width over height of the whole layout, roughly the canvas's own shape, so a
// fit fills it instead of leaving a tall thin column of dots.
const ASPECT = 1.6

export interface Position {
  x: number
  y: number
}

// A fixed place for every Party, whatever the cutoff, so nothing moves while
// the replay runs: the Bank, then Investors, Securities and Borrowers in
// columns, left to right. Every column spreads over the same height, so eight
// Borrowers face forty Investors instead of bunching in the middle. Securities
// are ordered by when money first reached them; everyone else by name.
export function layout(parties: MoneyFlowParty[], movements: Movement[], collapseSecurities: boolean) {
  const firstSeen = new Map<string, number>()
  for (const m of movements) {
    const at = Date.parse(m.occurredAt)
    for (const id of [m.source, m.target]) if (!firstSeen.has(id)) firstSeen.set(id, at)
  }

  const columns = new Map<number, MoneyFlowParty[]>()
  for (const party of parties) {
    if (collapseSecurities && party.kind === 'SECURITY') continue
    const column = collapseSecurities && party.kind === 'BORROWER' ? 2 : COLUMN[party.kind]
    columns.set(column, [...(columns.get(column) ?? []), party])
  }

  const height = Math.max(1, ...[...columns.values()].map((members) => members.length)) * ROW_GAP
  const columnGap = (height * ASPECT) / Math.max(1, columns.size - 1)
  const positions = new Map<string, Position>()
  for (const [column, members] of columns) {
    const ordered = [...members].sort((a, b) =>
      a.kind === 'SECURITY'
        ? (firstSeen.get(a.id) ?? 0) - (firstSeen.get(b.id) ?? 0)
        : a.label.localeCompare(b.label),
    )
    const row = height / ordered.length
    ordered.forEach((party, i) =>
      positions.set(party.id, { x: column * columnGap, y: -height / 2 + (i + 0.5) * row }),
    )
  }
  return positions
}

// The span the replay runs over, in epoch milliseconds.
export function timeline(movements: Movement[]): { start: number; end: number } | null {
  if (movements.length === 0) return null

  const times = movements.map((m) => Date.parse(m.occurredAt))
  return { start: Math.min(...times), end: Math.max(...times) }
}

// What had moved by the cutoff, by kind.
export function totalsUntil(movements: Movement[], until: number): Record<MovementKind, bigint> {
  const totals = Object.fromEntries(ALL_KINDS.map((kind) => [kind, 0n])) as Record<MovementKind, bigint>
  for (const m of movements) {
    if (Date.parse(m.occurredAt) <= until) totals[m.kind] += BigInt(m.amountMinorUnits)
  }
  return totals
}
