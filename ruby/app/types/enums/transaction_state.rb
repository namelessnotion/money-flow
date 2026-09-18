# frozen_string_literal: true
# typed: strict

module Types
  module Enums
    # A Go Transaction's top-level state as Ruby's read model last saw it —
    # the same vocabulary as go/internal/transaction's topLevelState. Persisted
    # in transaction_projections.state.
    class TransactionState < T::Enum
      enums do
        Initialized = new('initialized')
        Rejected = new('rejected')
        Started = new('started')
        RollbackStarted = new('rollback_started')
        Completed = new('completed')
        RolledBack = new('rolled_back')
        RollbackFailed = new('rollback_failed')
      end
    end
  end
end
