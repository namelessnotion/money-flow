<script setup lang="ts">
import { computed } from 'vue'
import { useQuery } from '@vue/apollo-composable'
import {
  ACH_PAGE_SIZE,
  ACH_TRANSACTIONS_QUERY,
  type AchTransactionsQueryResult,
  type AchTransactionsQueryVariables,
} from '../graphql/ach'
import { DIRECTION_LABELS, stateLabel, stateTone } from '../lib/ach'
import { formatDateTime } from '../lib/dates'
import { formatMinorUnits } from '../lib/money'

const props = defineProps<{ entityId: string }>()

// The read model catches up with the ledger in the background, so the list
// polls rather than showing a state that has already moved on.
const { result, loading, error, fetchMore } = useQuery<AchTransactionsQueryResult, AchTransactionsQueryVariables>(
  ACH_TRANSACTIONS_QUERY,
  { variables: () => ({ entityId: props.entityId, first: ACH_PAGE_SIZE }), pollInterval: 5000 },
)

const transactions = computed(
  () => result.value?.achTransactions.edges.flatMap((edge) => (edge.node ? [edge.node] : [])) ?? [],
)
const pageInfo = computed(() => result.value?.achTransactions.pageInfo)

async function loadMore(): Promise<void> {
  const after = pageInfo.value?.endCursor
  if (!after) return

  await fetchMore({ variables: { entityId: props.entityId, first: ACH_PAGE_SIZE, after } })
}
</script>

<template>
  <section>
    <h2 class="mb-3 text-lg font-bold text-slate-900">Transactions</h2>

    <div v-if="error" class="rounded-lg bg-rose-50 p-4 text-rose-700">
      Unable to load transactions: {{ error.message }}
    </div>
    <div v-else-if="loading && transactions.length === 0" class="rounded-lg bg-slate-100 p-4 text-slate-600">
      Loading transactions…
    </div>
    <div v-else-if="transactions.length === 0" class="rounded-lg bg-slate-100 p-4 text-slate-600">
      No ACH transactions yet.
    </div>

    <template v-else>
      <ul class="divide-y divide-slate-200 overflow-hidden rounded-lg border border-slate-200 bg-white shadow-sm">
        <li v-for="transaction in transactions" :key="transaction.id">
          <RouterLink
            :to="{ name: 'ach-transaction', params: { entityId, id: transaction.id } }"
            class="flex items-center justify-between gap-4 px-4 py-3 hover:bg-slate-50"
          >
            <div class="min-w-0">
              <p class="font-semibold text-slate-900">
                {{ DIRECTION_LABELS[transaction.direction] }}
                · {{ formatMinorUnits(transaction.amountMinorUnits, transaction.currency) }}
              </p>
              <p class="text-sm text-slate-500">{{ formatDateTime(transaction.createdAt) }}</p>
            </div>
            <span
              class="shrink-0 rounded-full px-2.5 py-0.5 text-xs font-bold"
              :class="stateTone(transaction.state)"
            >
              {{ stateLabel(transaction.state) }}
            </span>
          </RouterLink>
        </li>
      </ul>
      <div v-if="pageInfo?.hasNextPage" class="mt-4 flex justify-center">
        <button
          type="button"
          class="rounded-lg bg-blue-700 px-4 py-2 font-bold text-white hover:bg-blue-800"
          @click="loadMore"
        >
          Load more
        </button>
      </div>
    </template>
  </section>
</template>
