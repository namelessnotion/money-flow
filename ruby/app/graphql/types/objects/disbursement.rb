# frozen_string_literal: true
# typed: strict

require_relative 'base_object'
require_relative '../projections'
require_relative 'entity'
require_relative '../enums/transaction_state_enum'
require_relative '../enums/transfer_state_enum'

module Types
  # GraphQL type for a Disbursement: one holder's share of one Repayment.
  class Disbursement < BaseObject
    extend Projections

    description 'One holder’s share of one Repayment: the Transaction that retires that much of their claim ' \
                'and pays them the money. One per holder, never a fan-out across all of them.'

    field :id, ID, null: false
    field :principal_minor_units, GraphQL::Types::BigInt, null: false
    field :interest_minor_units, GraphQL::Types::BigInt, null: false
    field :currency, String, null: false
    field :created_at, GraphQL::Types::ISO8601DateTime, null: false
    field :state, TransactionStateEnum, null: true
    field :reason, String, null: true
    field :retirement_leg_state, TransferStateEnum,
          null: true,
          description: 'Projected state of the Transfer returning the repaid claim to the Issuer. It runs ' \
                       'first, so no holder is paid principal they do not hold. Null on an interest-only ' \
                       'Repayment, which retires nothing and so has no such leg.'
    field :payout_leg_state, TransferStateEnum, null: true,
                                                description: 'Projected state of the Transfer paying the holder.'
    field :investor, Types::Entity, null: true

    PROJECTED_COLUMNS = T.let(
      [
        Sequel[:transaction_projections][:state].as(:state),
        Sequel[:transaction_projections][:reason].as(:reason),
        Sequel[:retirement_leg][:state].as(:retirement_leg_state),
        Sequel[:payout_leg][:state].as(:payout_leg_state)
      ].freeze,
      T::Array[Sequel::SQL::AliasedExpression]
    )

    PROJECTION_JOINS = T.let(
      {
        transaction_projections: %i[transaction_projections id],
        retirement_leg: %i[transfer_projections retirement_transfer_id],
        payout_leg: %i[transfer_projections payout_transfer_id]
      }.freeze,
      T::Hash[Symbol, T::Array[Symbol]]
    )

    T::Sig::WithoutRuntime.sig { returns(Models::Disbursement::PrivateDataset) }
    def self.dataset
      projected(model: Models::Disbursement, table: :disbursements,
                joins: PROJECTION_JOINS, columns: PROJECTED_COLUMNS)
    end

    sig { returns(T.nilable(String)) }
    def state = object[:state]

    sig { returns(T.nilable(String)) }
    def reason = object[:reason]

    sig { returns(T.nilable(String)) }
    def retirement_leg_state = object[:retirement_leg_state]

    sig { returns(T.nilable(String)) }
    def payout_leg_state = object[:payout_leg_state]

    sig { returns(T.untyped) }
    def investor
      dataloader.with(Sources::EntitiesById).load(object.investor_entity_id)
    end
  end
end
