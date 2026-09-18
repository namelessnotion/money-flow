# frozen_string_literal: true
# typed: strict

module Types
  # GraphQL face of Types::Enums::TransactionState, whose values it is built
  # from.
  class TransactionStateEnum < GraphQL::Schema::Enum
    graphql_name 'TransactionState'
    description "A Transaction's state as Ruby's read model last saw it. It may lag the ledger."

    Types::Enums::TransactionState.each_value { |state| value(state.serialize.upcase, value: state.serialize) }
  end
end
