# frozen_string_literal: true
# typed: strict

require_relative 'settlement_command'

module Services
  module Ach
    # The entry will never post — the provider returned it (an R-code), or
    # refused it at submission. The real leg is cancelled, and resuming the
    # Transaction lets Go roll back whatever else it had done.
    class Return < SettlementCommand
      sig { params(ach_transaction_id: String, reason: String).returns(Models::AchTransaction) }
      def call(ach_transaction_id:, reason:)
        ach = find!(ach_transaction_id)
        @go.cancel_staged(ach.real_transfer_id, reason)
        @go.resume(ach.id)
        ach
      end
    end
  end
end
