# frozen_string_literal: true
# typed: strict

require_relative 'base_object'

module Types
  # GraphQL type for one Services::Securities::Positions::Position.
  #
  # Not a table: a Position is the sum of an Investor's completed Subscriptions
  # less the principal already repaid to them, derived on read.
  class Position < BaseObject
    description 'What one Investor holds in one Security, as the read model last saw it. ' \
                'Derived from their Subscriptions and Disbursements rather than stored.'

    field :investor_entity_id, ID, null: false
    field :principal_minor_units, GraphQL::Types::BigInt, null: false,
                                                          description: 'Everything they bought, across every ' \
                                                                       'Subscription.'
    field :outstanding_principal_minor_units, GraphQL::Types::BigInt,
          null: false, description: 'What of it has not yet been repaid to them.'
    field :investor, Types::Entity, null: true, description: 'The Investor holding it.'

    sig { returns(String) }
    def investor_entity_id = object.investor_entity_id.to_s

    sig { returns(T.untyped) }
    def investor
      # Returns a Dataloader promise, not an Entity — graphql-ruby resolves it.
      # Untyped because `load` has no sig to narrow against.
      dataloader.with(Sources::EntitiesById).load(object.investor_entity_id)
    end
  end
end
