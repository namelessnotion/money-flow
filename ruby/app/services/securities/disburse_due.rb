# frozen_string_literal: true
# typed: strict

require 'logger'
require_relative 'allocation'
require_relative 'disburse'

module Services
  module Securities
    # The sweep behind the scheduled job: disburses every completed Repayment
    # to the holders of its Security, one Transaction per holder.
    #
    # A (Repayment, holder) pair is a candidate once the read model has seen
    # the Repayment's own Transaction complete — before that the money is not
    # in the repayment wallet — and has not yet seen a Disbursement for that
    # pair. One it has seen, in any state, is left alone: in flight it needs
    # nothing, and a rejected or rolled-back one needs a person, not a retry.
    # One sent but not yet seen is sent again, which Go dedupes by its derived
    # id.
    #
    # Being a sweep, it catches up on its own after any outage: nothing is
    # scheduled per Repayment that could be lost.
    #
    # **Why not one fan-out Transaction across every holder.** Every payout leg
    # would draw on the same repayment wallet, which is exactly the case where
    # Go's accept-time pre-flight evaluates siblings against one unconsumed
    # snapshot and can over-accept — and that window is wider now that the real
    # dispatch happens a CDC round trip later. And the Transaction is the unit
    # of rollback: one holder's leg failing would reverse every holder already
    # paid. Per-holder Transactions invert both (ruby/docs/adr/0007).
    #
    # There used to be a third reason, that Go dispatched a Transaction's
    # children serially and in-process inside the RPC that started it, so a
    # wide fan-out was that many Transfer sagas in one HTTP call. That is no
    # longer true (go/docs/adr/0006) and the ADR records it as withdrawn. Go
    # also now refuses an over-wide Transaction outright, so this is a decision
    # about shape rather than the only thing standing between here and a
    # two-hundred-leg DAG.
    class DisburseDue
      # What a sweep did.
      class Result < T::Struct
        # Disbursement (Transaction) ids sent this run.
        const :disbursed, T::Array[String]
        # Disbursement id => the failure, for each one that could not be sent.
        const :failed, T::Hash[String, String]
      end

      COMPLETED = T.let(Types::Enums::TransactionState::Completed.serialize, String)

      sig { params(logger: ::Logger, disburse: Disburse).void }
      def initialize(logger:, disburse: Disburse.new)
        @logger = logger
        @disburse = disburse
      end

      # `repayment_id` limits the sweep to one Repayment, which is what the
      # demo mutation uses; omitted, it sweeps everything due.
      sig { params(repayment_id: T.nilable(String)).returns(Result) }
      def call(repayment_id: nil)
        disbursed = T.let([], T::Array[String])
        failed = T.let({}, T::Hash[String, String])

        due(repayment_id).each { |repayment| disburse_all(repayment, disbursed, failed) }

        @logger.info("securities disbursement: #{disbursed.size} sent, #{failed.size} failed")
        Result.new(disbursed: disbursed, failed: failed)
      end

      private

      # Repayments the read model has seen complete.
      sig { params(repayment_id: T.nilable(String)).returns(T::Array[Models::Repayment]) }
      def due(repayment_id)
        seen = DB[:transaction_projections].where(state: COMPLETED).select(:aggregate_id)
        scope = Models::Repayment.where(id: seen)
        scope = scope.where(id: repayment_id) if repayment_id
        scope.order(:created_at).all
      end

      sig do
        params(repayment: Models::Repayment, disbursed: T::Array[String], failed: T::Hash[String, String]).void
      end
      def disburse_all(repayment, disbursed, failed)
        Allocation.for(repayment).each do |share|
          next if already_seen?(repayment, share)

          attempt(repayment, share, disbursed, failed)
        end
      rescue StandardError => e
        # An allocation that will not add up is one Repayment's problem, not
        # the sweep's: the rest still go out.
        failed[repayment.id] = "#{e.class}: #{e.message}"
        @logger.error("securities disbursement failed to allocate #{repayment.id}: #{failed[repayment.id]}")
      end

      # Seen in any state is left alone. Rejected or rolled back needs a
      # person; in flight needs nothing.
      sig { params(repayment: Models::Repayment, share: Allocation::Share).returns(T::Boolean) }
      def already_seen?(repayment, share)
        id = Disburse.transaction_id(repayment.id, share.investor_entity_id)
        !Models::TransactionProjection[id].nil?
      end

      sig do
        params(repayment: Models::Repayment, share: Allocation::Share, disbursed: T::Array[String],
               failed: T::Hash[String, String]).void
      end
      def attempt(repayment, share, disbursed, failed)
        disbursement = @disburse.call(repayment: repayment, share: share)
        disbursed << disbursement.id
      rescue StandardError => e
        id = Disburse.transaction_id(repayment.id, share.investor_entity_id)
        failed[id] = "#{e.class}: #{e.message}"
        @logger.error("securities disbursement failed for #{id}: #{failed[id]}")
      end
    end
  end
end
