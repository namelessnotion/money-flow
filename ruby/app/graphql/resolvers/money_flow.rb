# frozen_string_literal: true
# typed: strict

require_relative '../types/objects/money_flow'

module Resolvers
  # `Query.moneyFlow`: Services::MoneyFlow, narrowed by what the caller asks.
  class MoneyFlow < GraphQL::Schema::Resolver
    type Types::MoneyFlow, null: false
    description 'Every completed Movement of money and the Parties it ran between, oldest first. Not ' \
                'paginated: a graph of it needs every edge, so narrow it instead.'

    argument :name_prefix, String, required: false,
                                   description: 'Only Movements whose entity’s name starts with this, taken ' \
                                                'literally — how one simulation run is picked out.'
    argument :since, GraphQL::Types::ISO8601DateTime, required: false,
                                                      description: 'Only Movements completed at or after this.'

    sig { params(name_prefix: T.nilable(String), since: T.nilable(Time)).returns(Services::MoneyFlow::Flow) }
    def resolve(name_prefix: nil, since: nil)
      Services::MoneyFlow.of(name_prefix: name_prefix, since: since)
    end
  end
end
