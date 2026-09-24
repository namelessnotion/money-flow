# frozen_string_literal: true
# typed: strict

require_relative 'base_object'
require_relative 'money_flow_party'
require_relative 'movement'

module Types
  # GraphQL type for a Services::MoneyFlow::Flow.
  class MoneyFlow < BaseObject
    description 'Where money has gone: every Movement the read model has seen complete, oldest first, and ' \
                'the Parties they ran between. Derived from the business rows and projections, never stored.'

    field :parties, [MoneyFlowParty], null: false, description: 'Every Party a Movement touches, and no other.'
    field :movements, [Movement], null: false, description: 'Oldest first.'
  end
end
