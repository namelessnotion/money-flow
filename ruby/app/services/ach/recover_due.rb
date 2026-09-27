# frozen_string_literal: true
# typed: strict

require 'logger'
require_relative 'go_gateway'
require_relative 'owed'
require_relative 'recovery_shape'

module Services
  module Ach
    # The sweep behind the scheduled job: collects what entities owe from their
    # cleared cash, one Recovery per entity at a time (ruby/docs/adr/0011,
    # decision 5).
    #
    # For each entity that owes something recoverable, it looks at its latest
    # Recovery first:
    #
    # - recorded but not yet seen by the read model: sent again, since Go
    #   may never have received it, and nothing new is started;
    # - in flight: nothing, until it concludes. What it collects isn't known
    #   until then, and a second one sized meanwhile could collect the same
    #   debt twice;
    # - rolled back with a failure: nothing, ever. That needs a person;
    # - refused or rolled back: another only once the entity's cleared cash
    #   has moved on in the event log since that one was sized. A stale view
    #   of cleared cash is what gets a Recovery refused, and retrying on the
    #   same view would be refused again, for ever.
    #
    # Otherwise it records the next Recovery, for what is owed or for the
    # cleared cash that isn't reserved, whichever is less, and asks Go for it.
    # Recording it takes the next sequence under a unique index, so of two
    # sweeps only one gets to start it.
    #
    # Each Recovery that completes lowers what is owed by at least one minor
    # unit, and debt only grows by a notice, so the chain ends.
    class RecoverDue
      # What a sweep did.
      class Result < T::Struct
        # Recovery ids sent to Go, new or again.
        const :sent, T::Array[String]
        # Entity id => Go's reason, for each Recovery it refused. Not a failure
        # of the run: the next Recovery waits for a fresher view.
        const :refused, T::Hash[Integer, String]
        # Entity id => the failure, for each entity that could not be looked at.
        const :failed, T::Hash[Integer, String]
      end

      STATE = Types::Enums::TransactionState
      IN_FLIGHT = T.let(
        [STATE::Initialized, STATE::Started, STATE::RollbackStarted].map(&:serialize).freeze, T::Array[String]
      )
      REFUSED = T.let([STATE::Rejected, STATE::RolledBack].map(&:serialize).freeze, T::Array[String])

      sig { params(logger: ::Logger, gateway: GoGateway).void }
      def initialize(logger:, gateway: GoGateway.new)
        @logger = logger
        @go = gateway
      end

      sig { returns(Result) }
      def call
        result = Result.new(sent: [], refused: {}, failed: {})
        Owed.recoverable_entity_ids.each { |entity_id| attempt(entity_id, result) }
        @logger.info("Recovery: #{result.sent.size} sent, #{result.refused.size} refused, " \
                     "#{result.failed.size} failed")
        result
      end

      private

      sig { params(entity_id: Integer, result: Result).void }
      def attempt(entity_id, result)
        recovery = next_recovery(entity_id)
        return if recovery.nil?

        send_recovery(recovery)
        result.sent << recovery.id
      rescue Refused => e
        result.refused[entity_id] = e.message
      rescue StandardError => e
        result.failed[entity_id] = "#{e.class}: #{e.message}"
        @logger.error("Recovery failed for entity #{entity_id}: #{result.failed[entity_id]}")
      end

      # The Recovery to send for the entity now, if any: its latest one again,
      # or a new one recorded here.
      sig { params(entity_id: Integer).returns(T.nilable(Models::ReceivableRecovery)) }
      def next_recovery(entity_id)
        latest = Models::ReceivableRecovery.where(entity_id: entity_id).order(Sequel.desc(:sequence)).first
        cleared = ClearedCash.of(entity_id)
        if latest
          state = Models::TransactionProjection[latest.id]&.state
          return latest if state.nil?
          return unless may_follow?(state, latest, cleared)
        end

        record(entity_id, (latest&.sequence || 0) + 1, cleared)
      end

      sig { params(state: String, latest: Models::ReceivableRecovery, cleared: ClearedCash).returns(T::Boolean) }
      def may_follow?(state, latest, cleared)
        return false if IN_FLIGHT.include?(state) || state == STATE::RollbackFailed.serialize
        return true unless REFUSED.include?(state)

        cleared.global_seq > latest.cleared_global_seq
      end

      sig do
        params(entity_id: Integer, sequence: Integer, cleared: ClearedCash).returns(T.nilable(Models::ReceivableRecovery))
      end
      def record(entity_id, sequence, cleared)
        amount = [Owed.for(entity_id).for_recovery, cleared.available].min
        return unless amount.positive?

        Models::ReceivableRecovery.create(
          id: DetId.for("#{entity_id}:recovery:#{sequence}"), entity_id: entity_id, sequence: sequence,
          amount_minor_units: amount, currency: TransactionShape::CURRENCY, cleared_global_seq: cleared.global_seq
        )
      rescue Sequel::UniqueConstraintViolation
        nil
      end

      sig { params(recovery: Models::ReceivableRecovery).void }
      def send_recovery(recovery)
        shape = RecoveryShape.for(entity_id: recovery.entity_id, sequence: recovery.sequence,
                                  accounts: Models::Account.where(entity_id: recovery.entity_id).all)
        @go.start_transaction(shape.start_request(amount_minor_units: recovery.amount_minor_units))
      end

      # An entity's cleared cash as the read model last saw it: what isn't
      # reserved by a staged Transfer, and how far into the event log that view
      # reaches.
      class ClearedCash < T::Struct
        const :available, Integer
        const :global_seq, Integer

        sig { params(entity_id: Integer).returns(ClearedCash) }
        def self.of(entity_id)
          wallet = Models::Account.first(entity_id: entity_id,
                                         type: Types::Enums::AccountType::ClearedCash.serialize)&.wallet_uuid
          return new(available: 0, global_seq: 0) if wallet.nil?

          new(available: available_in(wallet),
              global_seq: Integer(Models::TokenBalanceProjection.where(wallet_uuid: wallet).max(:last_global_seq) || 0))
        end

        sig { params(wallet: String).returns(Integer) }
        def self.available_in(wallet)
          balance = Models::TokenBalanceProjection.totals_for([wallet]).fetch(wallet, [])
                                                  .find { |each| each.currency == TransactionShape::CURRENCY }
          balance ? balance.posted - balance.pending_outgoing : 0
        end
        private_class_method :available_in
      end
    end
  end
end
