<script setup lang="ts">
import { computed, ref } from 'vue'
import type { ErrorLike } from '@apollo/client'
import { useMutation } from '@vue/apollo-composable'
import {
  ACH_PAGE_SIZE,
  ACH_TRANSACTIONS_QUERY,
  INITIATE_ACH_DEPOSIT_MUTATION,
  INITIATE_ACH_WITHDRAWAL_MUTATION,
  type AchDirection,
  type InitiateAchDepositMutationResult,
  type InitiateAchMutationVariables,
  type InitiateAchWithdrawalMutationResult,
} from '../graphql/ach'
import { dollarsToCents } from '../lib/money'

const props = defineProps<{ entityId: string }>()

const amount = ref('')
const cents = computed(() => dollarsToCents(amount.value))
const showInvalid = computed(() => amount.value.trim() !== '' && cents.value === null)

const refetchQueries = computed(() => [
  { query: ACH_TRANSACTIONS_QUERY, variables: { entityId: props.entityId, first: ACH_PAGE_SIZE } },
])

const deposit = useMutation<InitiateAchDepositMutationResult, InitiateAchMutationVariables>(
  INITIATE_ACH_DEPOSIT_MUTATION,
  () => ({ throws: 'never', refetchQueries: refetchQueries.value }),
)
const withdrawal = useMutation<InitiateAchWithdrawalMutationResult, InitiateAchMutationVariables>(
  INITIATE_ACH_WITHDRAWAL_MUTATION,
  () => ({ throws: 'never', refetchQueries: refetchQueries.value }),
)

const loading = computed(() => deposit.loading.value || withdrawal.loading.value)
const error = ref<ErrorLike | null>(null)

async function initiate(direction: AchDirection): Promise<void> {
  if (cents.value === null) return

  error.value = null
  const variables = { entityId: props.entityId, amountMinorUnits: cents.value }
  if (direction === 'DEPOSIT') {
    const result = await deposit.mutate({ variables })
    error.value = deposit.error.value ?? null
    if (result?.data?.initiateAchDeposit?.achTransaction) amount.value = ''
  } else {
    const result = await withdrawal.mutate({ variables })
    error.value = withdrawal.error.value ?? null
    if (result?.data?.initiateAchWithdrawal?.achTransaction) amount.value = ''
  }
}
</script>

<template>
  <section class="mb-8">
    <h2 class="mb-3 text-lg font-bold text-slate-900">Move money</h2>
    <form class="flex flex-wrap items-start gap-3" @submit.prevent>
      <div class="min-w-40 flex-1">
        <label for="ach-amount" class="sr-only">Amount in US dollars</label>
        <input
          id="ach-amount"
          v-model="amount"
          type="text"
          inputmode="decimal"
          placeholder="Amount (USD)"
          :aria-invalid="showInvalid"
          class="w-full rounded-lg border border-slate-300 px-3 py-2 text-slate-900 focus:border-blue-700 focus:outline-none"
        />
      </div>
      <button
        type="button"
        data-test="deposit"
        :disabled="loading || cents === null"
        class="shrink-0 rounded-lg bg-blue-700 px-4 py-2 font-bold text-white hover:bg-blue-800 disabled:cursor-not-allowed disabled:opacity-60"
        @click="initiate('DEPOSIT')"
      >
        ACH Deposit
      </button>
      <button
        type="button"
        data-test="withdrawal"
        :disabled="loading || cents === null"
        class="shrink-0 rounded-lg border border-blue-700 px-4 py-2 font-bold text-blue-700 hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-60"
        @click="initiate('WITHDRAWAL')"
      >
        ACH Withdrawal
      </button>
    </form>
    <p v-if="showInvalid" class="mt-2 text-sm text-slate-500">
      Enter a positive amount with at most two decimal places.
    </p>
    <p v-if="error" class="mt-2 text-sm text-rose-700">Unable to start the transaction: {{ error.message }}</p>
  </section>
</template>
