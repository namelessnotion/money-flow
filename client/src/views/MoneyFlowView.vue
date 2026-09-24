<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useQuery } from '@vue/apollo-composable'
import MoneyFlowGraph from '../components/MoneyFlowGraph.vue'
import {
  MONEY_FLOW_QUERY,
  type MoneyFlowQueryResult,
  type MoneyFlowQueryVariables,
  type MovementKind,
} from '../graphql/moneyFlow'
import {
  ALL_KINDS,
  FLOW_COLOURS,
  buildGraph,
  layout,
  timeline,
  totalsUntil,
  type Flow,
  type GraphEdge,
} from '../lib/moneyFlowGraph'
import { formatMinorUnits } from '../lib/money'

// The money flow of one run (or everything), as a graph that replays over
// time: Investors → Securities → Borrowers, and back.

const REPLAY_MS = 20_000

const route = useRoute()
const router = useRouter()

const run = computed(() => (typeof route.query.run === 'string' && route.query.run !== '' ? route.query.run : null))
const runInput = ref(run.value ?? '')

const { result, loading, error } = useQuery<MoneyFlowQueryResult, MoneyFlowQueryVariables>(MONEY_FLOW_QUERY, {
  variables: () => ({ namePrefix: run.value }),
})

const parties = computed(() => result.value?.moneyFlow.parties ?? [])
const movements = computed(() => result.value?.moneyFlow.movements ?? [])

// The filters, by which way the money is going.
const FILTERS: { flow: Flow; label: string; kinds: MovementKind[] }[] = [
  { flow: 'out', label: 'Investing', kinds: ['SUBSCRIPTION', 'DRAW'] },
  { flow: 'back', label: 'Repaying', kinds: ['REPAYMENT', 'DISBURSEMENT_PRINCIPAL'] },
  { flow: 'interest', label: 'Interest', kinds: ['DISBURSEMENT_INTEREST'] },
  { flow: 'bank', label: 'Bank deposits & withdrawals', kinds: ['DEPOSIT', 'WITHDRAWAL'] },
]
const shownFlows = ref<Set<Flow>>(new Set(['out', 'back', 'interest']))
const kinds = computed(() => new Set(FILTERS.filter((f) => shownFlows.value.has(f.flow)).flatMap((f) => f.kinds)))

function toggleFlow(flow: Flow) {
  const next = new Set(shownFlows.value)
  if (next.has(flow)) next.delete(flow)
  else next.add(flow)
  shownFlows.value = next
}

const collapseSecurities = ref(false)
const graphView = ref<InstanceType<typeof MoneyFlowGraph> | null>(null)
watch(collapseSecurities, () => requestAnimationFrame(() => graphView.value?.fit()))

// The replay: everything up to `cutoff` is drawn.
const span = computed(() => timeline(movements.value))
const cutoff = ref(Infinity)
watch(span, (next) => {
  cutoff.value = next?.end ?? Infinity
})

const playing = ref(false)
let frame = 0
let lastTick = 0

function play() {
  const current = span.value
  if (!current) return
  if (cutoff.value >= current.end) cutoff.value = current.start
  playing.value = true
  lastTick = performance.now()
  frame = requestAnimationFrame(tick)
}

function pause() {
  playing.value = false
  cancelAnimationFrame(frame)
}

function tick(now: number) {
  const current = span.value
  if (!playing.value || !current) return
  cutoff.value = Math.min(current.end, cutoff.value + ((current.end - current.start) * (now - lastTick)) / REPLAY_MS)
  lastTick = now
  if (cutoff.value >= current.end) pause()
  else frame = requestAnimationFrame(tick)
}

onBeforeUnmount(pause)

const positions = computed(() => layout(parties.value, movements.value, collapseSecurities.value))
const graph = computed(() =>
  buildGraph(parties.value, movements.value, {
    until: cutoff.value,
    kinds: kinds.value,
    collapseSecurities: collapseSecurities.value,
  }),
)
// Widths are scaled against the whole run with every kind shown, so they mean
// the same at every point of the replay.
const largest = computed(() =>
  buildGraph(parties.value, movements.value, {
    until: Infinity,
    kinds: new Set(ALL_KINDS),
    collapseSecurities: collapseSecurities.value,
  }).edges.reduce((max, edge) => (edge.amountMinorUnits > max ? edge.amountMinorUnits : max), 0n),
)

const totals = computed(() => totalsUntil(movements.value, cutoff.value))
const TOTALS: { label: string; kind: MovementKind }[] = [
  { label: 'Invested', kind: 'SUBSCRIPTION' },
  { label: 'Drawn', kind: 'DRAW' },
  { label: 'Repaid', kind: 'REPAYMENT' },
  { label: 'Principal returned', kind: 'DISBURSEMENT_PRINCIPAL' },
  { label: 'Interest paid', kind: 'DISBURSEMENT_INTEREST' },
]

const labels = computed(() => new Map(parties.value.map((p) => [p.id, p.label])))
const selected = ref<string | null>(null)
const hovered = ref<GraphEdge | null>(null)

const KIND_NAMES: Record<MovementKind, string> = {
  DEPOSIT: 'Deposit',
  WITHDRAWAL: 'Withdrawal',
  SUBSCRIPTION: 'Subscription',
  DRAW: 'Draw',
  REPAYMENT: 'Repayment',
  DISBURSEMENT_PRINCIPAL: 'Principal returned',
  DISBURSEMENT_INTEREST: 'Interest paid',
}

function describe(edge: GraphEdge): string {
  const times = edge.count === 1 ? 'once' : `${edge.count} times`
  return `${labels.value.get(edge.source)} → ${labels.value.get(edge.target)} · ${KIND_NAMES[edge.kind]} · ${formatMinorUnits(edge.amountMinorUnits.toString(), 'USD')} (${times})`
}

const cutoffLabel = computed(() => {
  const current = span.value
  if (!current || !Number.isFinite(cutoff.value)) return ''
  return new Date(Math.min(cutoff.value, current.end)).toLocaleString()
})

function loadRun() {
  pause()
  selected.value = null
  const trimmed = runInput.value.trim()
  void router.replace({ query: trimmed ? { run: trimmed } : {} })
}
</script>

<template>
  <header class="mb-6">
    <p class="text-sm font-bold uppercase tracking-wider text-slate-500">Money flow</p>
    <h1 class="text-4xl font-bold tracking-tight text-slate-900">Investors → Borrowers → Investors</h1>
    <p class="mt-1 text-sm text-slate-600">
      Every completed Movement of money, replayed in the order Go completed it. Click a party to follow its money.
    </p>
  </header>

  <form class="mb-4 flex flex-wrap items-end gap-3" @submit.prevent="loadRun">
    <label class="flex flex-col text-sm font-semibold text-slate-700">
      Run (entity name prefix)
      <input
        v-model="runInput"
        name="run"
        placeholder="sim-42"
        class="mt-1 w-64 rounded-md border border-slate-300 px-3 py-2 font-normal"
      />
    </label>
    <button type="submit" class="rounded-md bg-slate-900 px-4 py-2 text-sm font-bold text-white">Show</button>
  </form>

  <div v-if="error" class="rounded-lg bg-rose-50 p-4 text-rose-700">Unable to load the money flow: {{ error.message }}</div>
  <div v-else-if="loading && !result" class="rounded-lg bg-slate-100 p-4 text-slate-600">Loading the money flow…</div>
  <div v-else-if="movements.length === 0" class="rounded-lg bg-slate-100 p-4 text-slate-600">
    No money has moved{{ run ? ` for ${run}` : '' }} yet.
  </div>

  <template v-else>
    <section class="mb-4 flex flex-wrap items-center gap-x-6 gap-y-2 text-sm">
      <label v-for="filter in FILTERS" :key="filter.flow" class="flex items-center gap-2">
        <input
          type="checkbox"
          :checked="shownFlows.has(filter.flow)"
          :data-testid="`flow-${filter.flow}`"
          @change="toggleFlow(filter.flow)"
        />
        <span class="inline-block h-1 w-5 rounded" :style="{ background: FLOW_COLOURS[filter.flow] }" />
        {{ filter.label }}
      </label>
      <label class="flex items-center gap-2 font-semibold">
        <input v-model="collapseSecurities" type="checkbox" data-testid="collapse" />
        Collapse Securities into their Borrowers
      </label>
    </section>

    <section class="mb-4 flex items-center gap-3">
      <button
        type="button"
        class="w-20 rounded-md border border-slate-300 bg-white px-3 py-1.5 text-sm font-bold"
        @click="playing ? pause() : play()"
      >
        {{ playing ? 'Pause' : 'Play' }}
      </button>
      <input
        v-if="span"
        v-model.number="cutoff"
        type="range"
        :min="span.start"
        :max="span.end"
        :step="Math.max(1, Math.floor((span.end - span.start) / 1000))"
        aria-label="Replay position"
        class="flex-1"
        @input="pause"
      />
      <span class="w-48 text-right text-sm tabular-nums text-slate-600">{{ cutoffLabel }}</span>
    </section>

    <dl class="mb-4 grid grid-cols-2 gap-3 sm:grid-cols-5">
      <div v-for="total in TOTALS" :key="total.kind" class="rounded-lg bg-white p-3 shadow-sm">
        <dt class="text-xs font-bold uppercase tracking-wider text-slate-500">{{ total.label }}</dt>
        <dd class="text-lg font-bold tabular-nums text-slate-900" :data-testid="`total-${total.kind}`">
          {{ formatMinorUnits(totals[total.kind].toString(), 'USD') }}
        </dd>
      </div>
    </dl>

    <p class="mb-2 h-5 truncate text-sm text-slate-700" data-testid="detail">
      <template v-if="hovered">{{ describe(hovered) }}</template>
      <template v-else-if="selected">Following {{ labels.get(selected) }}</template>
    </p>

    <div class="h-[70vh] rounded-lg border border-slate-200 bg-white">
      <MoneyFlowGraph
        ref="graphView"
        :nodes="graph.nodes"
        :edges="graph.edges"
        :positions="positions"
        :largest-minor-units="largest"
        :selected="selected"
        @select="selected = $event"
        @hover="hovered = $event"
      />
    </div>

    <p class="mt-2 text-xs text-slate-500">
      ● Investors · ■ Securities · ◆ Borrowers — {{ graph.nodes.length }} parties, {{ graph.edges.length }} edges,
      {{ movements.length }} movements in all.
    </p>
  </template>
</template>
