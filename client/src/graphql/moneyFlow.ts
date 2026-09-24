import { gql } from '@apollo/client/core'

export const MONEY_FLOW_QUERY = gql`
  query MoneyFlow($namePrefix: String) {
    moneyFlow(namePrefix: $namePrefix) {
      parties {
        id
        kind
        label
      }
      movements {
        kind
        source
        target
        amountMinorUnits
        occurredAt
        transactionId
      }
    }
  }
`

export type PartyKind = 'INVESTOR' | 'BORROWER' | 'ISSUER' | 'SECURITY' | 'BANK'

export type MovementKind =
  | 'DEPOSIT'
  | 'WITHDRAWAL'
  | 'SUBSCRIPTION'
  | 'DRAW'
  | 'REPAYMENT'
  | 'DISBURSEMENT_PRINCIPAL'
  | 'DISBURSEMENT_INTEREST'

// Somewhere money moves between. The id is stable (`entity:<id>`,
// `security:<id>`, `bank`) and is how Movements name their ends.
export interface MoneyFlowParty {
  id: string
  kind: PartyKind
  label: string
}

// One completed Transaction's money, from one Party to another. The amount is
// a BigInt string of minor units.
export interface Movement {
  kind: MovementKind
  source: string
  target: string
  amountMinorUnits: string
  occurredAt: string
  transactionId: string
}

export interface MoneyFlowQueryResult {
  moneyFlow: { parties: MoneyFlowParty[]; movements: Movement[] }
}

export interface MoneyFlowQueryVariables {
  namePrefix: string | null
}
