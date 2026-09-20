# frozen_string_literal: true
# typed: strict

require_relative 'base_object'
require_relative '../projections'
require_relative 'entity'
require_relative '../enums/transaction_state_enum'
require_relative '../enums/transfer_state_enum'

module Types
  # GraphQL type for a Subscription: one Investor's fractional purchase, with
  # the state of the Transaction and its two legs as the projections last saw
  # them.
  class Subscription < BaseObject
    extend Projections

    description 'One Investor’s fractional purchase of a Security. State fields come from a read model that ' \
                'may lag the ledger; they are null until the first event for the Transaction has been consumed.'

    field :id, ID, null: false
    field :amount_minor_units, GraphQL::Types::BigInt, null: false
    field :currency, String, null: false
    field :created_at, GraphQL::Types::ISO8601DateTime, null: false
    field :state, TransactionStateEnum, null: true, description: 'The Transaction’s projected state.'
    field :reason, String, null: true, description: 'Why it is in that state, when Go gave a reason.'
    field :claim_leg_state, TransferStateEnum,
          null: true,
          description: 'Projected state of the Transfer moving claims out of the Supply. It runs first, so an ' \
                       'oversubscription is settled before any Investor money moves.'
    field :money_leg_state, TransferStateEnum,
          null: true,
          description: 'Projected state of the Transfer moving the Investor’s cleared cash into escrow. It ' \
                       'waits for the claim leg.'
    field :investor, Types::Entity, null: true

    PROJECTED_COLUMNS = T.let(
      [
        Sequel[:transaction_projections][:state].as(:state),
        Sequel[:transaction_projections][:reason].as(:reason),
        Sequel[:claim_leg][:state].as(:claim_leg_state),
        Sequel[:money_leg][:state].as(:money_leg_state)
      ].freeze,
      T::Array[Sequel::SQL::AliasedExpression]
    )

    PROJECTION_JOINS = T.let(
      {
        transaction_projections: %i[transaction_projections id],
        claim_leg: %i[transfer_projections claim_transfer_id],
        money_leg: %i[transfer_projections money_transfer_id]
      }.freeze,
      T::Hash[Symbol, T::Array[Symbol]]
    )

    T::Sig::WithoutRuntime.sig { returns(Models::Subscription::PrivateDataset) }
    def self.dataset
      projected(model: Models::Subscription, table: :subscriptions,
                joins: PROJECTION_JOINS, columns: PROJECTED_COLUMNS)
    end

    sig { returns(T.nilable(String)) }
    def state = object[:state]

    sig { returns(T.nilable(String)) }
    def reason = object[:reason]

    sig { returns(T.nilable(String)) }
    def claim_leg_state = object[:claim_leg_state]

    sig { returns(T.nilable(String)) }
    def money_leg_state = object[:money_leg_state]

    sig { returns(T.untyped) }
    def investor
      dataloader.with(Sources::EntitiesById).load(object.investor_entity_id)
    end
  end
end
