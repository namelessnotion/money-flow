<script setup lang="ts">
import type { AchStep, AchStepStatus } from '../graphql/ach'
import { STATUS_LABELS, STEP_LABELS } from '../lib/ach'
import { formatDate } from '../lib/dates'

defineProps<{
  steps: AchStep[]
  // Why the Transaction is in its state; shown against the step that failed.
  reason: string | null
  clearingDueOn: string | null
}>()

const MARKERS: Record<AchStepStatus, { symbol: string; tone: string }> = {
  DONE: { symbol: '✓', tone: 'bg-emerald-600 text-white' },
  WAITING: { symbol: '…', tone: 'border-2 border-amber-500 bg-white text-amber-600' },
  FAILED: { symbol: '✕', tone: 'bg-rose-600 text-white' },
  SKIPPED: { symbol: '–', tone: 'bg-slate-200 text-slate-500' },
}
</script>

<template>
  <ol class="space-y-4">
    <li
      v-for="step in steps"
      :key="step.name"
      :data-step="step.name"
      :data-status="step.status"
      class="flex gap-3"
    >
      <span
        class="mt-0.5 flex h-7 w-7 shrink-0 items-center justify-center rounded-full text-sm font-bold"
        :class="MARKERS[step.status].tone"
        aria-hidden="true"
      >
        {{ MARKERS[step.status].symbol }}
      </span>
      <div class="min-w-0">
        <p class="font-semibold" :class="step.status === 'SKIPPED' ? 'text-slate-400' : 'text-slate-900'">
          {{ STEP_LABELS[step.name].title }}
          <span class="ml-1 text-sm font-normal text-slate-500">{{ STATUS_LABELS[step.status] }}</span>
        </p>
        <p class="text-sm text-slate-500">{{ STEP_LABELS[step.name].detail }}</p>
        <p v-if="step.status === 'FAILED' && reason" class="text-sm text-rose-700">{{ reason }}</p>
        <p v-if="step.name === 'CLEARING' && step.status === 'WAITING' && clearingDueOn" class="text-sm text-slate-500">
          Due {{ formatDate(clearingDueOn) }}
        </p>
      </div>
    </li>
  </ol>
</template>
