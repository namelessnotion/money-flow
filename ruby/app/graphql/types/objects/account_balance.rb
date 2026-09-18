# frozen_string_literal: true
# typed: strict

require_relative 'base_object'

module Types
  # GraphQL type for what an Account holds in one currency.
  class AccountBalance < BaseObject
    description 'What an Account holds in one currency, summed over its Tokens from a read model that may lag ' \
                'the ledger.'

    field :currency, String, null: false
    field :posted_minor_units, GraphQL::Types::BigInt,
          null: false, method: :posted,
          description: 'Settled on the ledger. Negative when an Account that may overdraw has.'
    field :pending_outgoing_minor_units, GraphQL::Types::BigInt,
          null: false, method: :pending_outgoing,
          description: 'Reserved by staged Transfers to leave the Account, not yet posted or voided.'
    field :pending_incoming_minor_units, GraphQL::Types::BigInt,
          null: false, method: :pending_incoming,
          description: 'Reserved by staged Transfers to arrive in the Account, not yet posted or voided.'
  end
end
