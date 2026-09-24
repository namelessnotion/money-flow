# frozen_string_literal: true
# typed: strict

module Types
  # GraphQL face of Services::MoneyFlow::Party::Kind, whose values it is built
  # from.
  class MoneyFlowPartyKindEnum < GraphQL::Schema::Enum
    graphql_name 'MoneyFlowPartyKind'
    description 'What a Party in the money flow is: an entity by its role, a Security, or the Bank.'

    Services::MoneyFlow::Party::Kind.each_value { |kind| value(kind.serialize.upcase, value: kind) }
  end
end
