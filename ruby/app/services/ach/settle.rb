# frozen_string_literal: true
# typed: strict

require_relative 'settlement_command'

module Services
  module Ach
    # The provider reports the entry posted: the real leg commits, and resuming
    # the Transaction runs the shadow leg that was waiting on it.
    class Settle < SettlementCommand
      sig { params(ach_transaction_id: String).returns(Models::AchTransaction) }
      def call(ach_transaction_id:)
        ach = find!(ach_transaction_id)
        @go.post_pending(ach.real_transfer_id)
        @go.resume(ach.id)
        ach
      end
    end
  end
end
