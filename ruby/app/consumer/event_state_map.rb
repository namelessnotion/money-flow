# frozen_string_literal: true
# typed: strict

require_relative '../../gen/proto/transaction/v1/transaction_pb'
require_relative '../../gen/proto/transfer/v1/transfer_pb'

module Consumer
  # The single owner of how each published event moves Ruby's read model: for
  # every event type Go appends to a Transaction or Transfer stream, the state
  # it leaves the aggregate in — or nil for an event Go records without
  # changing the aggregate's state (a saga step, a refused command).
  #
  # Every event Go emits is listed, including the nil ones, so that "known but
  # state-preserving" is distinguishable from "never heard of": an unlisted
  # event raises UnmappedEvent rather than being skipped (ruby/docs/adr/0001,
  # decision 3). The spec checks this list against the generated proto classes.
  module EventStateMap
    class UnmappedEvent < StandardError; end

    TransactionState = Types::Enums::TransactionState
    TransferState = Types::Enums::TransferState

    # Mirrors go/internal/transaction's topLevelState.
    TRANSACTION = T.let(
      {
        'transaction.v1.TransactionInitialized' => TransactionState::Initialized,
        'transaction.v1.TransactionRejected' => TransactionState::Rejected,
        'transaction.v1.TransactionStarted' => TransactionState::Started,
        'transaction.v1.TransactionRollbackStarted' => TransactionState::RollbackStarted,
        'transaction.v1.TransactionCompleted' => TransactionState::Completed,
        'transaction.v1.TransactionRolledBack' => TransactionState::RolledBack,
        'transaction.v1.TransactionRollbackFailed' => TransactionState::RollbackFailed,
        # Per-child saga steps: they move a child Transfer, not the Transaction.
        'transaction.v1.TransferRequestedWithinTransaction' => nil,
        'transaction.v1.TransferGatedWithinTransaction' => nil,
        'transaction.v1.TransferCompletedWithinTransaction' => nil,
        'transaction.v1.TransferFailedWithinTransaction' => nil,
        'transaction.v1.TransferReversalRequestedWithinTransaction' => nil,
        'transaction.v1.TransferRolledBackWithinTransaction' => nil,
        'transaction.v1.TransferRollbackFailedWithinTransaction' => nil
      }.freeze,
      T::Hash[String, T.nilable(TransactionState)]
    )

    # Mirrors go/internal/transfer's currentState, plus the rejections that
    # open a stream and never start a saga (rejectedRequest). A Reversal is the
    # same aggregate type and shares this map.
    TRANSFER = T.let(
      {
        'transfer.v1.TransferRequestAccepted' => TransferState::Accepted,
        'transfer.v1.ReversalRequestAccepted' => TransferState::Accepted,
        'transfer.v1.TransferRequestRejected' => TransferState::Rejected,
        'transfer.v1.ReversalRequestRejected' => TransferState::Rejected,
        'transfer.v1.TransferPrepared' => TransferState::Prepared,
        'transfer.v1.TransferStaged' => TransferState::Staged,
        'transfer.v1.TransferPending' => TransferState::Pending,
        'transfer.v1.TransferCommitted' => TransferState::Committed,
        'transfer.v1.TransferFailed' => TransferState::Failed,
        'transfer.v1.TransferCancelled' => TransferState::Cancelled,
        'transfer.v1.AcceptedTransferCancelled' => TransferState::Cancelled,
        'transfer.v1.PreparedTransferCancelled' => TransferState::Cancelled,
        # A settlement command refused against the wrong state is recorded on
        # the stream, but leaves the Transfer where it was.
        'transfer.v1.ConfirmStagedTransferRejected' => nil,
        'transfer.v1.CancelStagedTransferRejected' => nil,
        'transfer.v1.PostPendingTransferRejected' => nil,
        # Dispatch claim markers (go/docs/adr/0005): appended before a
        # side-effecting saga step so only one caller performs it. Go's own
        # currentState ignores them; the outcome event that follows moves the
        # state.
        'transfer.v1.StagingTransferStarted' => nil,
        'transfer.v1.TransferCommittingStarted' => nil,
        'transfer.v1.CancellingStagedTransferStarted' => nil,
        'transfer.v1.CancellingPreparedTransferStarted' => nil
      }.freeze,
      T::Hash[String, T.nilable(TransferState)]
    )

    # The state `event_type` moves an `aggregate_type` aggregate to, or nil
    # when it leaves the state as it was.
    sig { params(aggregate_type: String, event_type: String).returns(T.nilable(T::Enum)) }
    def self.transition(aggregate_type:, event_type:)
      map =
        case aggregate_type
        when 'transaction' then TRANSACTION
        when 'transfer' then TRANSFER
        else raise UnmappedEvent, "no projection for aggregate type #{aggregate_type.inspect}"
        end

      map.fetch(event_type) { raise UnmappedEvent, "no projection for #{aggregate_type} event #{event_type.inspect}" }
    end
  end
end
