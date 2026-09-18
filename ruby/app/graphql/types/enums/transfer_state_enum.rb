# frozen_string_literal: true
# typed: strict

module Types
  # GraphQL face of Types::Enums::TransferState, whose values it is built from.
  class TransferStateEnum < GraphQL::Schema::Enum
    graphql_name 'TransferState'
    description "A Transfer's state as Ruby's read model last saw it. It may lag the ledger."

    Types::Enums::TransferState.each_value { |state| value(state.serialize.upcase, value: state.serialize) }
  end
end
