# frozen_string_literal: true
# typed: strict

module Types
  module Enums
    # A Go Transfer's (or Reversal's) state as Ruby's read model last saw it —
    # go/internal/transfer's currentState, plus `rejected` for a request that
    # never started a saga. Persisted in transfer_projections.state.
    class TransferState < T::Enum
      enums do
        Accepted = new('accepted')
        Rejected = new('rejected')
        Prepared = new('prepared')
        Staged = new('staged')
        Pending = new('pending')
        Committed = new('committed')
        Failed = new('failed')
        Cancelled = new('cancelled')
      end
    end
  end
end
