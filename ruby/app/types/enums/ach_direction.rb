# frozen_string_literal: true
# typed: strict

module Types
  module Enums
    # Which way an ACH Transaction moves money across the bank boundary.
    # Serialized values are explicit: they are persisted in
    # ach_transactions.direction.
    class AchDirection < T::Enum
      enums do
        Deposit = new('deposit')        # bank account -> platform
        Withdrawal = new('withdrawal')  # platform -> bank account
      end
    end
  end
end
