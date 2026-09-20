# frozen_string_literal: true
# typed: strict

require_relative 'base_object'
require_relative 'security_holdings'
require_relative '../projections'
require_relative 'entity'
require_relative 'position'
require_relative 'security_stage'
require_relative '../enums/security_stage_name_enum'
require_relative '../enums/transaction_state_enum'

module Types
  # GraphQL type for a Security: its terms, alongside the state of the
  # Transactions behind it as the projections last saw them.
  class Security < BaseObject
    extend Projections
    include SecurityHoldings

    description 'A claim on one Borrower’s debt obligation, offered by an Issuer and bought in fractions. ' \
                'State fields come from a read model that may lag the ledger; they are null until the first ' \
                'event for the Transaction has been consumed.'

    field :id, ID, null: false
    field :name, String, null: false
    field :principal_minor_units, GraphQL::Types::BigInt, null: false, description: 'The offering size.'
    field :annual_rate_bps, Integer, null: false, description: 'Simple interest rate, in basis points.'
    field :term_days, Integer, null: false
    field :currency, String, null: false
    field :created_at, GraphQL::Types::ISO8601DateTime, null: false

    field :offering_state, TransactionStateEnum, null: true,
                                                 description: 'Projected state of the Transaction that minted ' \
                                                              'the Supply.'
    field :draw_state, TransactionStateEnum, null: true,
                                             description: 'Projected state of the Transaction that moved the ' \
                                                          'escrow to the Borrower. Null until one is originated.'
    field :drawn_on, GraphQL::Types::ISO8601Date, null: true,
                                                  description: 'The day the money reached the Borrower, by Go’s ' \
                                                               'clock. Interest accrues from it.'

    field :stage, SecurityStageNameEnum, null: false, description: 'The furthest phase it has reached.'
    field :stages, [SecurityStage], null: false,
                                    description: 'Every phase — offering, funded, drawn, repaying, repaid — and ' \
                                                 'where each stands. Clients render this rather than re-deriving ' \
                                                 'it from raw states.'

    field :subscribed_minor_units, GraphQL::Types::BigInt, null: false,
                                                           description: 'What has been sold, as the read model ' \
                                                                        'last saw it.'
    field :remaining_minor_units, GraphQL::Types::BigInt,
          null: false,
          description: 'What is left to sell, as the read model last saw it. Advisory: the Supply wallet is ' \
                       'what actually refuses an oversubscription, and it may refuse one this did not foresee.'
    field :outstanding_principal_minor_units, GraphQL::Types::BigInt, null: false,
                                                                      description: 'What holders are still owed.'

    field :positions, [Position], null: false, description: 'What each Investor holds in it.'
    field :issuer, Types::Entity, null: true
    field :borrower, Types::Entity, null: true

    UUID = T.let(/\A\h{8}-\h{4}-\h{4}-\h{4}-\h{12}\z/, Regexp)

    # DB column backing each scalar field, keyed by the field's Ruby name.
    COLUMNS_BY_FIELD = T.let({
      id: :id,
      name: :name,
      principal_minor_units: :principal_minor_units,
      annual_rate_bps: :annual_rate_bps,
      term_days: :term_days,
      currency: :currency,
      created_at: :created_at
    }.freeze, T::Hash[Symbol, Symbol])

    # The projected columns, aliased onto each row under their field names.
    PROJECTED_COLUMNS = T.let(
      [
        Sequel[:offering][:state].as(:offering_state),
        Sequel[:draw][:state].as(:draw_state),
        Sequel.function(:coalesce, Sequel[:draw][:state_changed_at],
                        Sequel[:draw][:updated_at]).as(:draw_changed_at)
      ].freeze,
      T::Array[Sequel::SQL::AliasedExpression]
    )

    # Each projection joined in, by alias: its table, and the securities column
    # holding the id of the aggregate it projects.
    PROJECTION_JOINS = T.let(
      {
        offering: %i[transaction_projections offering_transaction_id],
        draw: %i[transaction_projections draw_transaction_id]
      }.freeze,
      T::Hash[Symbol, T::Array[Symbol]]
    )

    # Every Security with its projected state, in one query: the projections
    # are joined in rather than loaded per row. Rows the projection has not
    # reached yet come back with null state columns.
    T::Sig::WithoutRuntime.sig { returns(Models::Security::PrivateDataset) }
    def self.dataset
      projected(model: Models::Security, table: :securities,
                joins: PROJECTION_JOINS, columns: PROJECTED_COLUMNS)
    end

    # One Security by id, or nil — including for an id that is not a uuid,
    # which Postgres would refuse to compare.
    sig { params(id: String).returns(T.nilable(Models::Security)) }
    def self.find(id)
      return nil unless UUID.match?(id)

      dataset.where(Sequel[:securities][:id] => id).first
    end

    sig { returns(T.nilable(String)) }
    def offering_state = object[:offering_state]

    sig { returns(T.nilable(String)) }
    def draw_state = object[:draw_state]

    sig { returns(T.nilable(Date)) }
    def drawn_on
      return nil unless object[:draw_state] == Types::Enums::TransactionState::Completed.serialize

      object[:draw_changed_at]&.to_date
    end

    # Both go through the batching source rather than the model's own
    # association: left to graphql-ruby, `object.issuer` resolves through
    # Sequel one row at a time, which a page of Securities pays for once per
    # row. The security_spec query-count example is what holds that line.
    sig { returns(T.untyped) }
    def issuer
      dataloader.with(Sources::EntitiesById).load(object.issuer_entity_id)
    end

    sig { returns(T.untyped) }
    def borrower
      dataloader.with(Sources::EntitiesById).load(object.borrower_entity_id)
    end
  end
end
