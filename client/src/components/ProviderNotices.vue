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
// return notices (docs/ach-transactions.md).
const props = defineProps<{ achTransactionId: string }>()

const reason = ref('R01 insufficient funds')

const settle = useMutation<unknown, SettleAchMutationVariables>(SETTLE_ACH_MUTATION, { throws: 'never' })
const giveBack = useMutation<unknown, ReturnAchMutationVariables>(RETURN_ACH_MUTATION, { throws: 'never' })
</script>

<template>
  <section class="mt-8 rounded-lg border border-dashed border-slate-300 p-4">
    <h2 class="font-bold text-slate-900">Simulate the ACH provider</h2>
    <p class="mb-3 text-sm text-slate-500">No real provider is connected, so its notices are sent from here.</p>
    <div class="flex flex-wrap items-start gap-3">
      <button
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
