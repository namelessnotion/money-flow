import type { AchDirection, AchStepName, AchStepStatus, TransactionState } from '../graphql/ach'

// Wording only: which step is done, waiting or failed is decided by the API
// (AchTransaction.steps), never re-derived here from raw states.

export const STEP_LABELS: Record<AchStepName, { title: string; detail: string }> = {
  INITIATION: { title: 'Initiated', detail: 'The ledger accepted the transaction.' },
  FUNDING: { title: 'Funded', detail: 'Cleared cash moved to bank control, before any money left.' },
  SUBMISSION: { title: 'Submitted', detail: 'The entry was handed to the ACH provider.' },
  SETTLEMENT: { title: 'Settled', detail: 'The ACH network posted the entry.' },
  COMPLETION: { title: 'Completed', detail: 'The clearing accounts recorded the movement.' },
  CLEARING: { title: 'Cleared', detail: 'The deposit moved from uncleared to cleared cash.' },
  ROLLBACK: {
    title: 'Rolled back',
    detail: 'Whatever had moved was put back. Reversing a settled ACH entry can take days.',
  },
}

export const STATUS_LABELS: Record<AchStepStatus, string> = {
  DONE: 'Done',
  WAITING: 'Waiting',
  FAILED: 'Failed',
  SKIPPED: 'Skipped',
}

export const DIRECTION_LABELS: Record<AchDirection, string> = {
  DEPOSIT: 'ACH Deposit',
  WITHDRAWAL: 'ACH Withdrawal',
}

// A null state means the read model has not caught up with the ledger yet.
export function stateLabel(state: TransactionState | null): string {
  if (state === null) return 'Awaiting ledger'
  const words = state.toLowerCase().replaceAll('_', ' ')
  return words.charAt(0).toUpperCase() + words.slice(1)
}

export function stateTone(state: TransactionState | null): string {
  switch (state) {
    case 'COMPLETED':
      return 'bg-emerald-50 text-emerald-700'
    case 'REJECTED':
    case 'ROLLED_BACK':
    case 'ROLLBACK_FAILED':
    case 'ROLLBACK_STARTED':
      return 'bg-rose-50 text-rose-700'
    default:
      return 'bg-amber-50 text-amber-700'
  }
}
