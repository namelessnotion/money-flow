# frozen_string_literal: true
# typed: strict

require_relative 'clear'
require_relative 'errors'

module Services
  module Ach
    # Clears one deposit immediately, skipping the return window that
    # ClearingPolicy makes the scheduled sweep (ClearDue) wait out. It exists
    # for demonstrations, alongside the simulated provider notices: in
    # production nothing should clear a deposit the network could still return.
    #
    # It still requires what the sweep requires: the deposit's Transaction has
    # completed, as the read model last saw it — before that there is no
    # uncleared cash to move. Asking twice is safe; Clear's ids are derived
    # from the deposit, so Go dedupes the second.
    class ClearNow
      sig { params(clear: Clear).void }
      def initialize(clear: Clear.new)
        @clear = clear
      end

      sig { params(ach_transaction_id: String).returns(Models::AchTransaction) }
      def call(ach_transaction_id:)
        ach = Models::AchTransaction[ach_transaction_id] || raise(NotFound, "no ACH transaction #{ach_transaction_id}")
        ensure_completed!(ach)
        @clear.call(ach_transaction_id: ach.id)
      end

      private

      sig { params(ach: Models::AchTransaction).void }
      def ensure_completed!(ach)
        state = Models::TransactionProjection[ach.id]&.state
        return if state == Types::Enums::TransactionState::Completed.serialize

        raise NotClearable, "#{ach.id} has not completed (#{state || 'not yet seen'}); only a settled deposit clears"
      end
    end
  end
end
