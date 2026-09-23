# frozen_string_literal: true
# typed: strict

module Services
  module Securities
    # What each Investor holds in one Security.
    #
    # Derived, never stored. A Position is the sum of an Investor's completed
    # Subscriptions less the principal already disbursed back to them, and both
    # of those are already recorded — a stored Position would be a second
    # answer to a question the Subscriptions and the projections answer
    # together, and the two would eventually disagree.
    #
    # "Completed" means the read model has seen the Transaction complete. It
    # lags the ledger, so a Position lags with it: this is what the UI shows
    # and what an allocation is computed from, not an authority over what the
    # ledger did.
    module Positions
      # One Investor's holding in one Security.
      class Position < T::Struct
        const :investor_entity_id, Integer
        # Everything they ever bought, across all their Subscriptions.
        const :principal_minor_units, Integer
        # What of it has not yet been repaid to them.
        const :outstanding_principal_minor_units, Integer
      end

      COMPLETED = T.let(Types::Enums::TransactionState::Completed.serialize, String)

      # Every holder of `security`, ordered by entity id.
      #
      # The ordering is not cosmetic: ProRata breaks ties by key, so an
      # allocation is only reproducible if the positions it was computed from
      # always arrive the same way round. It belongs here rather than in each
      # caller, where forgetting it would be silent.
      sig { params(security: Models::Security).returns(T::Array[Position]) }
      def self.of(security)
        for_securities([security.id]).fetch(security.id, [])
      end

      # The same, for many Securities at once, in two queries rather than two
      # per Security — what Sources::SecurityPositions batches a page through.
      sig { params(security_ids: T::Array[String]).returns(T::Hash[String, T::Array[Position]]) }
      def self.for_securities(security_ids)
        return {} if security_ids.empty?

        repaid = principal_repaid(security_ids)

        bought(security_ids).to_h do |security_id, by_investor|
          settled = repaid.fetch(security_id, {})
          [security_id, holdings(by_investor, settled)]
        end
      end

      sig do
        params(by_investor: T::Hash[Integer, Integer], settled: T::Hash[Integer, Integer])
          .returns(T::Array[Position])
      end
      def self.holdings(by_investor, settled)
        by_investor.map do |investor_entity_id, principal|
          Position.new(investor_entity_id: investor_entity_id, principal_minor_units: principal,
                       outstanding_principal_minor_units: principal - settled.fetch(investor_entity_id, 0))
        end
      end
      private_class_method :holdings

      # What the Security has sold, as the read model last saw it — the figure
      # "remaining" is measured against, and never an authority: the Supply
      # wallet is what actually refuses an oversubscription.
      sig { params(security: Models::Security).returns(Integer) }
      def self.subscribed(security)
        of(security).sum(&:principal_minor_units)
      end

      # What is still owed across every holder.
      sig { params(security: Models::Security).returns(Integer) }
      def self.outstanding_principal(security)
        of(security).sum(&:outstanding_principal_minor_units)
      end

      # Only what the read model has seen complete counts. In flight, rejected
      # or rolled back is not held.
      COMPLETE = T.let({ Sequel[:transaction_projections][:state] => COMPLETED }.freeze, T::Hash[T.untyped, String])

      SECURITY = T.let(Sequel[:subscriptions][:security_id], Sequel::SQL::QualifiedIdentifier)
      SUBSCRIBER = T.let(Sequel[:subscriptions][:investor_entity_id], Sequel::SQL::QualifiedIdentifier)
      REPAID_SECURITY = T.let(Sequel[:repayments][:security_id], Sequel::SQL::QualifiedIdentifier)
      HOLDER = T.let(Sequel[:disbursements][:investor_entity_id], Sequel::SQL::QualifiedIdentifier)

      # Principal bought, by Security then investor, each ordered by entity id.
      sig { params(security_ids: T::Array[String]).returns(T::Hash[String, T::Hash[Integer, Integer]]) }
      def self.bought(security_ids)
        grouped(
          DB[:subscriptions]
            .join(:transaction_projections, aggregate_id: :id)
            .where(COMPLETE).where(SECURITY => security_ids)
            .group(SECURITY, SUBSCRIBER).order(SECURITY, SUBSCRIBER)
            .select(SECURITY.as(:security_id), SUBSCRIBER.as(:entity_id),
                    summed(Sequel[:subscriptions][:amount_minor_units]))
        )
      end
      private_class_method :bought

      # Principal already disbursed, by Security then investor. An in-flight
      # Disbursement has not reduced anybody's holding yet.
      sig { params(security_ids: T::Array[String]).returns(T::Hash[String, T::Hash[Integer, Integer]]) }
      def self.principal_repaid(security_ids)
        grouped(
          DB[:disbursements]
            .join(:repayments, id: :repayment_id)
            .join(:transaction_projections, aggregate_id: Sequel[:disbursements][:id])
            .where(COMPLETE).where(REPAID_SECURITY => security_ids)
            .group(REPAID_SECURITY, HOLDER)
            .select(REPAID_SECURITY.as(:security_id), HOLDER.as(:entity_id),
                    summed(Sequel[:disbursements][:principal_minor_units]))
        )
      end
      private_class_method :principal_repaid

      # Written out rather than using Sequel's virtual-row block form: a bare
      # `sum(...)` inside `select { }` is resolved against this module by
      # Sorbet, which cannot see that Sequel instance_evals it.
      sig { params(column: T.untyped).returns(T.untyped) }
      def self.summed(column)
        Sequel.function(:sum, column).as(:principal)
      end
      private_class_method :summed

      # Security id => investor id => total. Postgres sums a bigint as
      # numeric, which pg hands back as BigDecimal.
      sig { params(rows: T.untyped).returns(T::Hash[String, T::Hash[Integer, Integer]]) }
      def self.grouped(rows)
        rows.each_with_object({}) do |row, by_security|
          totals = by_security[row.fetch(:security_id)] ||= {}
          totals[Integer(row.fetch(:entity_id))] = Integer(row.fetch(:principal))
        end
      end
      private_class_method :grouped
    end
  end
end
