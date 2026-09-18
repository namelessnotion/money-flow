<script setup lang="ts">
import { useMutation } from '@vue/apollo-composable'
import { CLEAR_ACH_MUTATION, type ClearAchMutationVariables } from '../graphql/ach'

// A completed deposit is cleared by a scheduled sweep once its return window
// (three business days) has passed. For demonstrations, this clears it now.
const props = defineProps<{ achTransactionId: string }>()

const clear = useMutation<unknown, ClearAchMutationVariables>(CLEAR_ACH_MUTATION, { throws: 'never' })
</script>

<template>
  <section class="mt-8 rounded-lg border border-dashed border-slate-300 p-4">
    <h2 class="font-bold text-slate-900">Simulate the clearing sweep</h2>
    <p class="mb-3 text-sm text-slate-500">
      The sweep clears a deposit once its return window has passed. Clear it now instead of waiting.
    </p>
    <button
      type="button"
      data-test="clear"
      :disabled="clear.loading.value"
      class="rounded-lg bg-blue-700 px-4 py-2 font-bold text-white hover:bg-blue-800 disabled:opacity-60"
      @click="clear.mutate({ variables: { achTransactionId: props.achTransactionId } })"
    >
      Clear now
    </button>
    <p v-if="clear.error.value" class="mt-2 text-sm text-rose-700">Unable to clear: {{ clear.error.value.message }}</p>
  </section>
</template>
