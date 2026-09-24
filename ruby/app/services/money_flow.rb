# frozen_string_literal: true
# typed: strict

require_relative 'money_flow/party'
require_relative 'money_flow/movement'
require_relative 'money_flow/legs'

module Services
  # Where money has gone: every Movement the read model has seen complete,
  # between the Parties it ran between, oldest first.
  #
  # Derived, never stored — the same reasoning as a Position. Each business row
  # (an ACH Transaction, a Subscription, a Draw, a Repayment, a Disbursement)
  # already says who paid whom and how much, and its projection says whether and
  # when it completed. A stored Movement would be a second answer to that
  # question, and the two would eventually disagree.
  #
  # It lags the ledger exactly as the projections do, and reads only the money
  # side: claims minted, bought and retired are not money, and are left out.
  module MoneyFlow
    # The Parties any Movement touches, and the Movements.
    class Flow < T::Struct
      const :parties, T::Array[Party]
      const :movements, T::Array[Movement]
    end

    # One row of the query, typed as it leaves Sequel: T::Struct checks each
    # field at runtime, so a column the pg driver decoded as something else
    # fails here rather than somewhere downstream.
    class Row < T::Struct
      const :kind, Movement::Kind
      const :entity_id, Integer
      const :entity_name, String
      const :entity_role, String
      const :security_id, T.nilable(String)
      const :security_name, T.nilable(String)
      const :amount_minor_units, Integer
      const :occurred_at, Time
      const :transaction_id, String
    end

    COMPLETED = T.let(Types::Enums::TransactionState::Completed.serialize, String)

    # Go's time where the projection carries it, else when Ruby last saw the
    # row change — the same coalesce Securities::Schedule uses for a draw.
    OCCURRED_AT = T.let(
      Sequel.function(:coalesce, Sequel[:transaction_projections][:state_changed_at],
                      Sequel[:transaction_projections][:updated_at]),
      Sequel::SQL::Function
    )

    MOVEMENTS = T.let(Sequel[:movements], Sequel::SQL::Identifier)
    ENTITIES = T.let(Sequel[:entities], Sequel::SQL::Identifier)

    # What each Row is read from: the union's own columns, and the names.
    COLUMNS = T.let(
      [MOVEMENTS.*, OCCURRED_AT.as(:occurred_at), ENTITIES[:name].as(:entity_name),
       ENTITIES[:role].as(:entity_role), Sequel[:securities][:name].as(:security_name)].freeze,
      T::Array[T.any(Sequel::SQL::ColumnAll, Sequel::SQL::AliasedExpression)]
    )

    # Oldest first; the id and kind break ties, so a Disbursement's two parts
    # always arrive the same way round.
    ORDER = T.let([Sequel.asc(:occurred_at), MOVEMENTS[:transaction_id], MOVEMENTS[:kind]].freeze,
                  T::Array[T.any(Sequel::SQL::OrderedExpression, Sequel::SQL::QualifiedIdentifier)])

    # Every Movement, optionally only those whose entity's name starts with
    # `name_prefix` (taken literally, not as a pattern) and that completed at
    # or after `since`.
    sig { params(name_prefix: T.nilable(String), since: T.nilable(Time)).returns(Flow) }
    def self.of(name_prefix: nil, since: nil)
      rows = narrowed(completed, name_prefix, since).all.map do |row|
        Row.new(**row, kind: Movement::Kind.deserialize(row.fetch(:kind)))
      end
      Flow.new(parties: rows.flat_map { |row| [entity_of(row), counterparty_of(row)] }.uniq(&:id),
               movements: rows.map { |row| movement_from(row) })
    end

    # One query: every kind's rows unioned, joined to the projections for state
    # and time, and to the entity and Security for the Parties' names.
    sig { returns(Sequel::Dataset) }
    def self.completed
      DB.from(Legs.union.as(:movements))
        .join(:transaction_projections, aggregate_id: MOVEMENTS[:transaction_id])
        .join(:entities, id: MOVEMENTS[:entity_id])
        .left_join(:securities, id: MOVEMENTS[:security_id])
        .where(Sequel[:transaction_projections][:state] => COMPLETED)
        .select(*COLUMNS).order(*ORDER)
    end
    private_class_method :completed

    sig do
      params(dataset: Sequel::Dataset, name_prefix: T.nilable(String), since: T.nilable(Time))
        .returns(Sequel::Dataset)
    end
    def self.narrowed(dataset, name_prefix, since)
      dataset = dataset.where(Sequel.function(:starts_with, ENTITIES[:name], name_prefix)) if name_prefix
      dataset = dataset.where(Sequel.expr(since) <= OCCURRED_AT) if since
      dataset
    end
    private_class_method :narrowed

    sig { params(row: Row).returns(Movement) }
    def self.movement_from(row)
      entity = Party.entity_id(row.entity_id)
      other = counterparty_of(row).id
      source, target = row.kind.into_entity? ? [other, entity] : [entity, other]

      Movement.new(kind: row.kind, source: source, target: target, amount_minor_units: row.amount_minor_units,
                   occurred_at: row.occurred_at, transaction_id: row.transaction_id)
    end
    private_class_method :movement_from

    sig { params(row: Row).returns(Party) }
    def self.entity_of(row)
      Party.new(id: Party.entity_id(row.entity_id), kind: Party::Kind.deserialize(row.entity_role),
                label: row.entity_name)
    end
    private_class_method :entity_of

    # The other end: the Bank for an ACH Transaction, else the Security.
    sig { params(row: Row).returns(Party) }
    def self.counterparty_of(row)
      return BANK if row.kind.across_bank_boundary?

      Party.new(id: Party.security_id(T.must(row.security_id)), kind: Party::Kind::Security,
                label: T.must(row.security_name))
    end
    private_class_method :counterparty_of
  end
end
