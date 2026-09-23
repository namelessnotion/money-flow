# frozen_string_literal: true
# typed: strict

require_relative 'settlement_command'

module Services
  module Ach
    # The provider reports the entry posted: the real leg commits, and the
    # shadow leg that was waiting on it follows.
    #
    # Posting the leg is the whole call. It writes TransferCommitted to the
    # leg's own stream, and that event is the trigger the orchestrator follows
    # back to the owning Transaction to run the shadow leg (go/docs/adr/0006).
    # This used to call ResumeTransaction afterwards to do that driving itself;
    # keeping the call would only race the orchestrator for the same work and
    # imply it still mattered.
    class Settle < SettlementCommand
      sig { params(ach_transaction_id: String).returns(Models::AchTransaction) }
      def call(ach_transaction_id:)
        ach = find!(ach_transaction_id)
        @go.post_pending(ach.real_transfer_id)
        ach
      end
    end
  end
end
