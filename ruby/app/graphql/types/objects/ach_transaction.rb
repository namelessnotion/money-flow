# frozen_string_literal: true
# typed: strict

require_relative 'base_object'
require_relative '../enums/ach_direction_enum'
require_relative '../enums/transaction_state_enum'
require_relative '../enums/transfer_state_enum'

module Types
  # GraphQL type for an ACH Transaction: Ruby's record of what it asked for,
  # alongside the Transaction's state as the projection last saw it.
  class AchTransaction < BaseObject
    description 'An ACH deposit or withdrawal. State fields come from a read model that may lag the ledger; ' \
                'they are null until the first event for the Transaction has been consumed.'

    field :id, ID, null: false
    field :direction, AchDirectionEnum, null: false
    field :amount_minor_units, GraphQL::Types::BigInt, null: false
    field :currency, String, null: false
    field :provider_reference, String, null: true
    field :state, TransactionStateEnum, null: true, description: "The Transaction's projected state."
    field :reason, String, null: true, description: 'Why the Transaction is in its state, when Go gave a reason.'
    field :real_leg_state, TransferStateEnum, null: true,
                                              description: 'Projected state of the Transfer that crosses the bank ' \
                                                           'boundary — pending while ACH settles.'
    field :created_at, GraphQL::Types::ISO8601DateTime, null: false

    # The projected columns, aliased onto each row under their field names.
    PROJECTED_COLUMNS = T.let(
      [
        Sequel[:transaction_projections][:state].as(:state),
        Sequel[:transaction_projections][:reason].as(:reason),
        Sequel[:real_leg][:state].as(:real_leg_state)
      ].freeze,
      T::Array[Sequel::SQL::AliasedExpression]
    )

    # Every ACH Transaction with its projected state, in one query: the
    # projections are joined in rather than loaded per row. Rows the projection
    # has not reached yet come back with null state columns.
    T::Sig::WithoutRuntime.sig { returns(Models::AchTransaction::PrivateDataset) }
    def self.dataset
      Models::AchTransaction.dataset
                            .left_join(:transaction_projections, aggregate_id: Sequel[:ach_transactions][:id])
                            .left_join(Sequel[:transfer_projections].as(:real_leg),
                                       aggregate_id: Sequel[:ach_transactions][:real_transfer_id])
                            .select_all(:ach_transactions)
                            .select_append(*PROJECTED_COLUMNS)
    end

    # One ACH Transaction by id, with its projected state, or nil.
    sig { params(id: String).returns(T.nilable(Models::AchTransaction)) }
    def self.find(id)
      dataset.where(Sequel[:ach_transactions][:id] => id).first
    end

    sig { returns(T.nilable(String)) }
    def state = object[:state]

    sig { returns(T.nilable(String)) }
    def reason = object[:reason]

    sig { returns(T.nilable(String)) }
    def real_leg_state = object[:real_leg_state]
  end
end
