import { gql } from '@apollo/client/core'

export const ENTITY_QUERY = gql`
  query Entity($id: ID!) {
    entity(id: $id) {
      id
      name
      holderUuid
      createdAt
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
  accounts: AccountNode[]
}

export interface EntityQueryResult {
  entity: EntityDetail | null
}

export interface EntityQueryVariables {
  id: string
}
