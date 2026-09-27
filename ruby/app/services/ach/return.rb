# frozen_string_literal: true
# typed: strict

require_relative '../det_id'
require_relative 'late_return_shape'
require_relative 'settlement_command'

module Services
  module Ach
    # The provider's return notice: the network returned the entry (an
    # R-code), or the provider refused it at submission.
    #
    # The notice is recorded on the row first, under the row's lock, and the
    # first one wins. It is what Clear checks before it records a Clearing,
    # and what decides a late return's form (ruby/docs/adr/0011, decision 3).
    #
    # Then it is acted on, by what Go says of the ACH Transaction:
    #
    # - still running: the entry never posted. The real leg is cancelled, and
    #   the orchestrator follows the cancellation, an event on the leg's own
    #   stream, to the owning Transaction and starts its rollback
    #   (go/docs/adr/0006). If Go refuses, the leg has just posted, and
    #   ReturnDue acts on the notice once the Transaction completes;
    # - completed: a late return. The network took back money that had
    #   settled, and a LateReturnShape records it as a Transaction of its own;
    # - rolled back, rejected, or anything else: it never moved money, and
    #   there is nothing to take back. A failed rollback needs a person.
    class Return < SettlementCommand
      sig { params(ach_transaction_id: String, reason: String).returns(Models::AchTransaction) }
      def call(ach_transaction_id:, reason:)
        ach = record_notice!(find!(ach_transaction_id), reason)
        act_on_notice(ach)
        ach
      end

      # Acts on a notice already recorded. ReturnDue calls it for every notice
      # not yet seen through. A late return already recorded is sent again —
      # Go dedupes it by its derived id — without asking Go anything else.
      sig { params(ach: Models::AchTransaction).void }
      def act_on_notice(ach)
        return send_late_return(ach) if ach.return_transaction_id

        case @go.state(ach.id).state
        when :TRANSACTION_STATE_STARTED then cancel(ach)
        when :TRANSACTION_STATE_COMPLETED then return_late(ach)
        end
      end

      private

      sig { params(ach: Models::AchTransaction, reason: String).returns(Models::AchTransaction) }
      def record_notice!(ach, reason)
        perform do
          ach.lock!
          ach.update(returned_at: Time.now, return_reason: reason) if ach.returned_at.nil?
        end
        ach
      end

      sig { params(ach: Models::AchTransaction).void }
      def cancel(ach)
        @go.cancel_staged(ach.real_transfer_id, T.must(ach.return_reason))
      rescue Refused
        nil
      end

      # Recorded under the lock before Go is asked, like Clear's ids: a record
      # of what was sent, not the duplicate guard.
      sig { params(ach: Models::AchTransaction).void }
      def return_late(ach)
        perform do
          ach.lock!
          ach.update(return_transaction_id: DetId.for("#{ach.id}:return")) if ach.return_transaction_id.nil?
        end
        send_late_return(ach)
      end

      sig { params(ach: Models::AchTransaction).void }
      def send_late_return(ach)
        shape = LateReturnShape.for(ach: ach, accounts: Models::Account.where(entity_id: ach.entity_id).all)
        @go.start_transaction(shape.start_request(amount_minor_units: ach.amount_minor_units))
      end
    end
  end
end
