# frozen_string_literal: true
# typed: strict

require_relative 'base_object'
require_relative 'ach_step'
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
    field :clearing_state, TransactionStateEnum,
          null: true,
          description: 'Projected state of the clearing Transaction that moves a settled deposit from uncleared ' \
                       'to cleared cash. Null until one has been originated and projected.'
    field :clearing_due_on, GraphQL::Types::ISO8601Date,
          null: true,
          description: 'For a completed deposit, the Federal Reserve business day its clearing becomes due.'
    field :steps, [AchStep], null: false,
                             description: 'The lifecycle steps — initiation, funding (a withdrawal), submission, ' \
                                          'settlement, completion, clearing (a deposit) and, while one is under ' \
                                          'way, rollback — and where each stands.'
    field :created_at, GraphQL::Types::ISO8601DateTime, null: false

    UUID = T.let(/\A\h{8}-\h{4}-\h{4}-\h{4}-\h{12}\z/, Regexp)

    # The projected columns, aliased onto each row under their field names.
    PROJECTED_COLUMNS = T.let(
      [
        Sequel[:transaction_projections][:state].as(:state),
        Sequel[:transaction_projections][:reason].as(:reason),
        Sequel[:real_leg][:state].as(:real_leg_state),
        Sequel[:shadow_leg][:state].as(:shadow_leg_state),
        Sequel[:clearing][:state].as(:clearing_state),
        Sequel.function(:coalesce, Sequel[:transaction_projections][:state_changed_at],
                        Sequel[:transaction_projections][:updated_at]).as(:state_changed_at)
      ].freeze,
      T::Array[Sequel::SQL::AliasedExpression]
    )

    # Each projection joined in, by alias: its table, and the ach_transactions
    # column holding the id of the aggregate it projects.
    PROJECTION_JOINS = T.let(
      {
        transaction_projections: %i[transaction_projections id],
        real_leg: %i[transfer_projections real_transfer_id],
        shadow_leg: %i[transfer_projections shadow_transfer_id],
        clearing: %i[transaction_projections clearing_transaction_id]
      }.freeze,
      T::Hash[Symbol, T::Array[Symbol]]
    )

    # Every ACH Transaction with its projected state, in one query: the
    # projections are joined in rather than loaded per row. Rows the projection
    # has not reached yet come back with null state columns.
    T::Sig::WithoutRuntime.sig { returns(Models::AchTransaction::PrivateDataset) }
    def self.dataset
      joined = PROJECTION_JOINS.reduce(Models::AchTransaction.dataset) do |dataset, (as, (table, key))|
        dataset.left_join(Sequel[table].as(as), aggregate_id: Sequel[:ach_transactions][key])
      end
      joined.select_all(:ach_transactions).select_append(*PROJECTED_COLUMNS)
    end

    # One ACH Transaction by id, with its projected state, or nil — including
    # for an id that is not a uuid, which Postgres would refuse to compare.
    sig { params(id: String).returns(T.nilable(Models::AchTransaction)) }
    def self.find(id)
      return nil unless UUID.match?(id)

      dataset.where(Sequel[:ach_transactions][:id] => id).first
    end

    sig { returns(T.nilable(String)) }
    def state = object[:state]

    sig { returns(T.nilable(String)) }
    def reason = object[:reason]

    sig { returns(T.nilable(String)) }
    def real_leg_state = object[:real_leg_state]

    sig { returns(T.nilable(String)) }
    def clearing_state = object[:clearing_state]

    sig { returns(T::Array[Services::Ach::Progress::Step]) }
    def steps
      Services::Ach::Progress.of(
        Services::Ach::Progress::Snapshot.new(
          direction: Types::Enums::AchDirection.deserialize(object.direction),
          submitted: !object.provider_reference.nil?,
          state: projected_transaction(:state), clearing_state: projected_transaction(:clearing_state),
          real_leg_state: projected_transfer(:real_leg_state), shadow_leg_state: projected_transfer(:shadow_leg_state)
        )
      )
    end

    sig { params(column: Symbol).returns(T.nilable(Types::Enums::TransactionState)) }
    def projected_transaction(column) = Types::Enums::TransactionState.try_deserialize(object[column])

    sig { params(column: Symbol).returns(T.nilable(Types::Enums::TransferState)) }
    def projected_transfer(column) = Types::Enums::TransferState.try_deserialize(object[column])

    sig { returns(T.nilable(Date)) }
    def clearing_due_on
      completed = object[:state] == Types::Enums::TransactionState::Completed.serialize
      deposit = object.direction == Types::Enums::AchDirection::Deposit.serialize
      return nil unless completed && deposit

      Services::Ach::ClearingPolicy.due_on(object[:state_changed_at])
    end
  end
end
