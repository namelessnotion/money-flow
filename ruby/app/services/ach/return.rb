# frozen_string_literal: true
# typed: strict

require_relative 'settlement_command'

module Services
  module Ach
    # The entry will never post — the provider returned it (an R-code), or
    # refused it at submission. The real leg is cancelled, and Go rolls back
    # whatever else it had done.
    #
    # The notice is recorded on the row first, under the row's lock, and the
    # first one wins: it is what Clear checks before it records a Clearing
    # (ruby/docs/adr/0011, decision 3).
    #
    # Cancelling the leg is the whole call, for the same reason Settle's posting
    # is: the cancellation is an event on the leg's own stream, and the
    # orchestrator follows it to the owning Transaction and starts the rollback
    # (go/docs/adr/0006).
    class Return < SettlementCommand
      sig { params(ach_transaction_id: String, reason: String).returns(Models::AchTransaction) }
      def call(ach_transaction_id:, reason:)
        ach = record_notice!(find!(ach_transaction_id), reason)
        @go.cancel_staged(ach.real_transfer_id, reason)
        ach
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
    end
  end
end
