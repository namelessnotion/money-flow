import { gql } from '@apollo/client/core'

export const ENTITY_QUERY = gql`
  query Entity($id: ID!) {
    entity(id: $id) {
      id
      name
      holderUuid
      createdAt
      owedMinorUnits
      accounts {
        id
        name
        type
        walletUuid
        balances {
          currency
          postedMinorUnits
          pendingOutgoingMinorUnits
          pendingIncomingMinorUnits
        }
      }
    }
  }
`

// What an Account holds in one currency. Amounts are BigInt strings of minor
// units; posted is negative for an overdrawn Account.
export interface AccountBalance {
  currency: string
  postedMinorUnits: string
  pendingOutgoingMinorUnits: string
  pendingIncomingMinorUnits: string
}

export interface AccountNode {
  id: string
  name: string
  type: string
  walletUuid: string
  // One per currency; empty until the Account's first ledger activity.
  balances: AccountBalance[]
}

export interface EntityDetail {
  id: string
  name: string
  holderUuid: string
  createdAt: string
  // What it owes for late ACH deposit returns, as a BigInt string of minor
  // units. While it is above zero the entity can't withdraw.
  owedMinorUnits: string
  accounts: AccountNode[]
}

export interface EntityQueryResult {
  entity: EntityDetail | null
}

export interface EntityQueryVariables {
  id: string
}
