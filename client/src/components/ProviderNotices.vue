<script setup lang="ts">
import { ref } from 'vue'
import { useMutation } from '@vue/apollo-composable'
import {
  RETURN_ACH_MUTATION,
  SETTLE_ACH_MUTATION,
  type ReturnAchMutationVariables,
  type SettleAchMutationVariables,
} from '../graphql/ach'

// There is no real ACH provider yet: these stand in for its settlement and
// return notices (docs/ach-transactions.md). Once the entry has settled only a
// late return is left to send — an unauthorized debit (R10) can come back up
// to 60 days later (ruby/docs/adr/0011).
const props = withDefaults(defineProps<{ achTransactionId: string; late?: boolean }>(), { late: false })

const reason = ref(props.late ? 'R10 customer advises not authorized' : 'R01 insufficient funds')

const settle = useMutation<unknown, SettleAchMutationVariables>(SETTLE_ACH_MUTATION, { throws: 'never' })
const giveBack = useMutation<unknown, ReturnAchMutationVariables>(RETURN_ACH_MUTATION, { throws: 'never' })
</script>

<template>
  <section class="mt-8 rounded-lg border border-dashed border-slate-300 p-4">
    <h2 class="font-bold text-slate-900">{{ props.late ? 'Simulate a late return' : 'Simulate the ACH provider' }}</h2>
    <p class="mb-3 text-sm text-slate-500">
      <template v-if="props.late">
        The entry has settled, but the network can still take it back. A late return is recorded in the ledger.
      </template>
      <template v-else>No real provider is connected, so its notices are sent from here.</template>
    </p>
    <div class="flex flex-wrap items-start gap-3">
      <button
        v-if="!props.late"
        type="button"
        data-test="settle"
        :disabled="settle.loading.value || giveBack.loading.value"
        class="rounded-lg bg-emerald-700 px-4 py-2 font-bold text-white hover:bg-emerald-800 disabled:opacity-60"
        @click="settle.mutate({ variables: { achTransactionId: props.achTransactionId } })"
      >
        Settle
      </button>
      <label for="return-reason" class="sr-only">Return reason</label>
      <input
        id="return-reason"
        v-model="reason"
        type="text"
        class="min-w-40 flex-1 rounded-lg border border-slate-300 px-3 py-2 text-slate-900 focus:border-blue-700 focus:outline-none"
      />
      <button
        type="button"
        data-test="return"
        :disabled="settle.loading.value || giveBack.loading.value || !reason.trim()"
        class="rounded-lg bg-rose-700 px-4 py-2 font-bold text-white hover:bg-rose-800 disabled:opacity-60"
        @click="giveBack.mutate({ variables: { achTransactionId: props.achTransactionId, reason: reason.trim() } })"
      >
        Return
      </button>
    </div>
    <p v-if="settle.error.value" class="mt-2 text-sm text-rose-700">Unable to settle: {{ settle.error.value.message }}</p>
    <p v-if="giveBack.error.value" class="mt-2 text-sm text-rose-700">
      Unable to return: {{ giveBack.error.value.message }}
    </p>
  </section>
</template>
