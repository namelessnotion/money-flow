import { ApolloClient, HttpLink, InMemoryCache } from '@apollo/client/core'
import { relayStylePagination } from '@apollo/client/utilities'

const uri = import.meta.env.VITE_GRAPHQL_URL ?? '/graphql'

export const apolloClient = new ApolloClient({
  link: new HttpLink({ uri }),
  cache: new InMemoryCache({
    typePolicies: {
      Query: {
        fields: {
          // Entities are always fetched in the same `id` order with no
          // filter args, so successive pages merge into one cached list
          // keyed only on the field name (`keyArgs: false`).
          entities: relayStylePagination(),
          // One list per entity; pages of the same entity merge.
          achTransactions: relayStylePagination(['entityId']),
        },
      },
    },
  }),
})
