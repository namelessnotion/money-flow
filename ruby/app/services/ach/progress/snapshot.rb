# frozen_string_literal: true
# typed: strict

module Services
  module Ach
    module Progress
      # What Ruby last saw of an ACH Transaction: its record and the
      # projections of the Transaction, its two legs and its clearing. Each
      # state is nil until the projection has seen that aggregate.
      class Snapshot < T::Struct
        const :direction, Types::Enums::AchDirection
        # Whether the provider gave a reference for the entry.
        const :submitted, T::Boolean
        const :state, T.nilable(Types::Enums::TransactionState)
        const :real_leg_state, T.nilable(Types::Enums::TransferState)
        const :shadow_leg_state, T.nilable(Types::Enums::TransferState)
        const :clearing_state, T.nilable(Types::Enums::TransactionState)
        # Whether a return notice is recorded, whether Clear had recorded a
        # Clearing, whether a late return was sent, and where it stands.
        const :returned, T::Boolean, default: false
        const :clearing_recorded, T::Boolean, default: false
        const :late_return_sent, T::Boolean, default: false
        const :late_return_state, T.nilable(Types::Enums::TransactionState), default: nil

        # From an ACH Transaction's row, read with its projected states aliased
        # onto it under their field names (Types::AchTransaction.dataset).
        sig { params(row: Models::AchTransaction).returns(Snapshot) }
        def self.of(row)
          new(direction: Types::Enums::AchDirection.deserialize(row.direction),
              submitted: set?(row, :provider_reference),
              state: transaction_state(row, :state), clearing_state: transaction_state(row, :clearing_state),
              real_leg_state: transfer_state(row, :real_leg_state),
              shadow_leg_state: transfer_state(row, :shadow_leg_state),
              returned: set?(row, :returned_at), clearing_recorded: set?(row, :clearing_transaction_id),
              late_return_sent: set?(row, :return_transaction_id),
              late_return_state: transaction_state(row, :late_return_state))
        end

        sig { params(row: Models::AchTransaction, column: Symbol).returns(T::Boolean) }
        def self.set?(row, column) = !row[column].nil?
        private_class_method :set?

        sig { params(row: Models::AchTransaction, column: Symbol).returns(T.nilable(Types::Enums::TransactionState)) }
        def self.transaction_state(row, column) = Types::Enums::TransactionState.try_deserialize(row[column])
        private_class_method :transaction_state

        sig { params(row: Models::AchTransaction, column: Symbol).returns(T.nilable(Types::Enums::TransferState)) }
        def self.transfer_state(row, column) = Types::Enums::TransferState.try_deserialize(row[column])
        private_class_method :transfer_state

        sig { returns(T::Boolean) }
        def withdrawal? = direction == Types::Enums::AchDirection::Withdrawal

        # A notice for a Transaction that had completed: a late return, sent
        # or waiting to be.
        sig { returns(T::Boolean) }
        def late_return? = late_return_sent || (returned && state == Types::Enums::TransactionState::Completed)

        # A deposit clawed back before its Clearing was recorded: it never clears.
        sig { returns(T::Boolean) }
        def clawed_back? = late_return? && !withdrawal? && !clearing_recorded
      end
    end
  end
end
