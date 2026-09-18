<script setup lang="ts">
import { computed } from 'vue'
import { useQuery } from '@vue/apollo-composable'
import AchStepList from '../components/AchStepList.vue'
import ClearingNotice from '../components/ClearingNotice.vue'
import ProviderNotices from '../components/ProviderNotices.vue'
import {
  ACH_TRANSACTION_QUERY,
  type AchTransactionQueryResult,
  type AchTransactionQueryVariables,
} from '../graphql/ach'
import { DIRECTION_LABELS, stateLabel, stateTone } from '../lib/ach'
import { formatDateTime } from '../lib/dates'
import { formatMinorUnits } from '../lib/money'

const props = defineProps<{ entityId: string; id: string }>()

// Polls for the same reason as the list: the read model moves on its own.
const { result, loading, error } = useQuery<AchTransactionQueryResult, AchTransactionQueryVariables>(
  ACH_TRANSACTION_QUERY,
  { variables: () => ({ id: props.id }), pollInterval: 5000 },
)

const transaction = computed(() => result.value?.achTransaction)

// The provider's notices only make sense for an entry it holds and has not
// yet settled or returned.
function stepStatus(name: string) {
  return transaction.value?.steps.find((step) => step.name === name)?.status
}

const awaitingProvider = computed(() => stepStatus('SUBMISSION') === 'DONE' && stepStatus('SETTLEMENT') === 'WAITING')

// A completed deposit waits for the sweep to clear it; until a clearing is
// under way, it can be cleared early from here.
const awaitingClearing = computed(
  () => stepStatus('COMPLETION') === 'DONE' && stepStatus('CLEARING') === 'WAITING' && !transaction.value?.clearingState,
)
</script>

<template>
  <RouterLink :to="{ name: 'entity', params: { entityId } }" class="text-sm font-bold text-blue-700 hover:underline">
    ← Back to entity
  </RouterLink>

  <div v-if="error" class="mt-6 rounded-lg bg-rose-50 p-4 text-rose-700">
    Unable to load transaction: {{ error.message }}
  </div>
  <div v-else-if="loading && !transaction" class="mt-6 rounded-lg bg-slate-100 p-4 text-slate-600">
    Loading transaction…
  </div>
  <div v-else-if="!transaction" class="mt-6 rounded-lg bg-slate-100 p-4 text-slate-600">No transaction {{ id }}.</div>

  <template v-else>
    <header class="mt-4 mb-8 flex items-start justify-between gap-4">
      <div>
        <p class="text-sm font-bold uppercase tracking-wider text-slate-500">
          {{ DIRECTION_LABELS[transaction.direction] }}
        </p>
        <h1 class="text-4xl font-bold tracking-tight text-slate-900">
          {{ formatMinorUnits(transaction.amountMinorUnits, transaction.currency) }}
        </h1>
        <p class="mt-1 text-sm text-slate-500">Started {{ formatDateTime(transaction.createdAt) }}</p>
      </div>
      <span class="mt-2 shrink-0 rounded-full px-2.5 py-0.5 text-xs font-bold" :class="stateTone(transaction.state)">
        {{ stateLabel(transaction.state) }}
      </span>
    </header>

    <section class="rounded-lg border border-slate-200 bg-white p-5 shadow-sm">
      <h2 class="mb-4 text-lg font-bold text-slate-900">Progress</h2>
      <AchStepList :steps="transaction.steps" :reason="transaction.reason" :clearing-due-on="transaction.clearingDueOn" />
    </section>

    <dl class="mt-6 grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2 text-sm">
      <dt class="text-slate-500">Transaction id</dt>
      <dd class="truncate font-mono text-slate-900">{{ transaction.id }}</dd>
      <dt class="text-slate-500">Provider reference</dt>
      <dd class="truncate font-mono text-slate-900">{{ transaction.providerReference ?? '—' }}</dd>
    </dl>

    <p class="mt-6 text-xs text-slate-500">
      States come from a read model that can lag the ledger by a few seconds; this page refreshes itself.
    </p>

    <ProviderNotices v-if="awaitingProvider" :ach-transaction-id="transaction.id" />
    <ClearingNotice v-if="awaitingClearing" :ach-transaction-id="transaction.id" />
  </template>
</template>
