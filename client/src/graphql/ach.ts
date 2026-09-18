import { gql } from '@apollo/client/core'

export const ACH_PAGE_SIZE = 100

export type AchDirection = 'DEPOSIT' | 'WITHDRAWAL'

export type TransactionState =
  | 'INITIALIZED'
  | 'REJECTED'
  | 'STARTED'
  | 'ROLLBACK_STARTED'
  | 'COMPLETED'
  | 'ROLLED_BACK'
  | 'ROLLBACK_FAILED'

export type TransferState =
  | 'ACCEPTED'
  | 'REJECTED'
  | 'PREPARED'
  | 'STAGED'
  | 'PENDING'
  | 'COMMITTED'
  | 'FAILED'
  | 'CANCELLED'

export type AchStepName =
  | 'INITIATION'
  | 'FUNDING'
  | 'SUBMISSION'
  | 'SETTLEMENT'
  | 'COMPLETION'
  | 'CLEARING'
  | 'ROLLBACK'

export type AchStepStatus = 'DONE' | 'WAITING' | 'FAILED' | 'SKIPPED'

export interface AchStep {
  name: AchStepName
  status: AchStepStatus
}

// Every state field comes from Ruby's read model, which lags the ledger: they
// are null until the consumer has seen the Transaction.
export interface AchTransactionSummary {
  id: string
  direction: AchDirection
  amountMinorUnits: string
  currency: string
  state: TransactionState | null
  createdAt: string
}

export interface AchTransactionDetail extends AchTransactionSummary {
  providerReference: string | null
  reason: string | null
  realLegState: TransferState | null
  clearingState: TransactionState | null
  clearingDueOn: string | null
  steps: AchStep[]
}

const SUMMARY_FIELDS = gql`
  fragment AchTransactionSummaryFields on AchTransaction {
    id
    direction
    amountMinorUnits
    currency
    state
    createdAt
  }
`

const DETAIL_FIELDS = gql`
  fragment AchTransactionDetailFields on AchTransaction {
    ...AchTransactionSummaryFields
    providerReference
    reason
    realLegState
    clearingState
    clearingDueOn
    steps {
      name
      status
    }
  }
  ${SUMMARY_FIELDS}
`

export const ACH_TRANSACTIONS_QUERY = gql`
  query AchTransactions($entityId: ID!, $first: Int, $after: String) {
    achTransactions(entityId: $entityId, first: $first, after: $after) {
      edges {
        cursor
        node {
          ...AchTransactionSummaryFields
        }
      }
      pageInfo {
        hasNextPage
        endCursor
      }
    }
  }
  ${SUMMARY_FIELDS}
`

export interface AchTransactionsQueryResult {
  achTransactions: {
    edges: { cursor: string; node: AchTransactionSummary | null }[]
    pageInfo: { hasNextPage: boolean; endCursor: string | null }
  }
}

export interface AchTransactionsQueryVariables {
  entityId: string
  first?: number
  after?: string | null
}

export const ACH_TRANSACTION_QUERY = gql`
  query AchTransaction($id: ID!) {
    achTransaction(id: $id) {
      ...AchTransactionDetailFields
    }
  }
  ${DETAIL_FIELDS}
`

export interface AchTransactionQueryResult {
  achTransaction: AchTransactionDetail | null
}

export interface AchTransactionQueryVariables {
  id: string
}

export const INITIATE_ACH_DEPOSIT_MUTATION = gql`
  mutation InitiateAchDeposit($entityId: ID!, $amountMinorUnits: BigInt!) {
    initiateAchDeposit(entityId: $entityId, amountMinorUnits: $amountMinorUnits) {
      achTransaction {
        ...AchTransactionSummaryFields
      }
    }
  }
  ${SUMMARY_FIELDS}
`

export const INITIATE_ACH_WITHDRAWAL_MUTATION = gql`
  mutation InitiateAchWithdrawal($entityId: ID!, $amountMinorUnits: BigInt!) {
    initiateAchWithdrawal(entityId: $entityId, amountMinorUnits: $amountMinorUnits) {
      achTransaction {
        ...AchTransactionSummaryFields
      }
    }
  }
  ${SUMMARY_FIELDS}
`

export interface InitiateAchMutationVariables {
  entityId: string
  amountMinorUnits: string
}

export interface InitiateAchDepositMutationResult {
  initiateAchDeposit: { achTransaction: AchTransactionSummary | null } | null
}

export interface InitiateAchWithdrawalMutationResult {
  initiateAchWithdrawal: { achTransaction: AchTransactionSummary | null } | null
}

// Stand-ins for the ACH provider's settlement and return notices; there is no
// real provider yet (docs/ach-transactions.md).
export const SETTLE_ACH_MUTATION = gql`
  mutation SettleAch($achTransactionId: ID!) {
    settleAch(achTransactionId: $achTransactionId) {
      achTransaction {
        ...AchTransactionDetailFields
      }
    }
  }
  ${DETAIL_FIELDS}
`

export const RETURN_ACH_MUTATION = gql`
  mutation ReturnAch($achTransactionId: ID!, $reason: String!) {
    returnAch(achTransactionId: $achTransactionId, reason: $reason) {
      achTransaction {
        ...AchTransactionDetailFields
      }
    }
  }
  ${DETAIL_FIELDS}
`

// Stand-in for the scheduled clearing sweep: clears a completed deposit now
// rather than after its return window.
export const CLEAR_ACH_MUTATION = gql`
  mutation ClearAch($achTransactionId: ID!) {
    clearAch(achTransactionId: $achTransactionId) {
      achTransaction {
        ...AchTransactionDetailFields
      }
    }
  }
  ${DETAIL_FIELDS}
`

export interface ClearAchMutationVariables {
  achTransactionId: string
}

export interface SettleAchMutationVariables {
  achTransactionId: string
}

export interface ReturnAchMutationVariables {
  achTransactionId: string
  reason: string
}
