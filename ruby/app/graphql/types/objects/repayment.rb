# frozen_string_literal: true
# typed: strict

require_relative 'base_object'
require_relative '../projections'
require_relative 'disbursement'
require_relative '../enums/transaction_state_enum'
require_relative '../enums/transfer_state_enum'

module Types
  # GraphQL type for a Repayment: one payment from the Borrower into the
  # Security, and what became of it.
  class Repayment < BaseObject
    extend Projections

    description 'One payment from the Borrower into a Security. The ledger moves principal and interest as ' \
                'one amount; the split is the business fact the allocation reads.'

    field :id, ID, null: false
    field :principal_minor_units, GraphQL::Types::BigInt, null: false
    field :interest_minor_units, GraphQL::Types::BigInt, null: false
    field :as_of, GraphQL::Types::ISO8601Date, null: false,
                                               description: 'The day interest was accrued to.'
    field :currency, String, null: false
    field :created_at, GraphQL::Types::ISO8601DateTime, null: false
    field :state, TransactionStateEnum, null: true
    field :reason, String, null: true
    field :collection_leg_state, TransferStateEnum,
          null: true,
          description: 'Projected state of the Transfer moving the Borrower’s cleared cash into the Security.'
    field :disbursements, [Disbursement], null: false,
                                          description: 'What each holder was paid of it. Empty until the sweep ' \
                                                       'has originated them.'

    UUID = T.let(/\A\h{8}-\h{4}-\h{4}-\h{4}-\h{12}\z/, Regexp)

    PROJECTED_COLUMNS = T.let(
      [
        Sequel[:transaction_projections][:state].as(:state),
        Sequel[:transaction_projections][:reason].as(:reason),
        Sequel[:collection_leg][:state].as(:collection_leg_state)
      ].freeze,
      T::Array[Sequel::SQL::AliasedExpression]
    )

    PROJECTION_JOINS = T.let(
      {
        transaction_projections: %i[transaction_projections id],
        collection_leg: %i[transfer_projections collection_transfer_id]
      }.freeze,
      T::Hash[Symbol, T::Array[Symbol]]
    )

    T::Sig::WithoutRuntime.sig { returns(Models::Repayment::PrivateDataset) }
    def self.dataset
      projected(model: Models::Repayment, table: :repayments,
                joins: PROJECTION_JOINS, columns: PROJECTED_COLUMNS)
    end

    sig { params(id: String).returns(T.nilable(Models::Repayment)) }
    def self.find(id)
      return nil unless UUID.match?(id)

      dataset.where(Sequel[:repayments][:id] => id).first
    end

    sig { returns(T.nilable(String)) }
    def state = object[:state]

    sig { returns(T.nilable(String)) }
    def reason = object[:reason]

    sig { returns(T.nilable(String)) }
    def collection_leg_state = object[:collection_leg_state]

    sig { returns(T.untyped) }
    def disbursements
      dataloader.with(Sources::RepaymentDisbursements).load(object.id)
    end
  end
end
