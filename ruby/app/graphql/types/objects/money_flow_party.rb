# frozen_string_literal: true
# typed: strict

require_relative 'base_object'
require_relative '../enums/money_flow_party_kind_enum'

module Types
  # GraphQL type for one Services::MoneyFlow::Party.
  class MoneyFlowParty < BaseObject
    description 'Somewhere money moves between: an entity, a Security, or the Bank outside the platform.'

    field :id, ID, null: false,
                   description: 'Stable across queries: `entity:<id>`, `security:<id>`, or `bank`. Movements ' \
                                'name their ends by it.'
    field :kind, MoneyFlowPartyKindEnum, null: false
    field :label, String, null: false, description: 'The entity’s or Security’s name.'
  end
end
