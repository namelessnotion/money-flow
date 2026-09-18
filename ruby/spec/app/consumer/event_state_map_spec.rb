# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Consumer::EventStateMap do
  # Messages in the proto packages that are never appended to an event stream:
  # the Start*/*Started/Complete* triplets Go deliberately does not emit
  # (go/internal/transfer/saga.go, currentState), and value types nested in
  # other messages. Requests and responses are excluded by name below. Anything
  # else in the package is an event, and must be mapped.
  let(:not_events) do
    %w[
      transaction.v1.Transfer
      transaction.v1.TransferIdList
      transaction.v1.StartProcessingTransferRejected
      transfer.v1.TransferDestination
      transfer.v1.TransferLeg
      transfer.v1.StartPreparingTransfer
      transfer.v1.PreparingTransferStarted
      transfer.v1.CompletePreparingTransfer
      transfer.v1.StartCancellingPreparedTransfer
      transfer.v1.CancellingPreparedTransferStarted
      transfer.v1.CompleteCancellingPreparedTransfer
      transfer.v1.StartCompensatingFailedTransfer
      transfer.v1.FailedTransferCompensationStarted
      transfer.v1.CompleteCompensatingFailedTransfer
      transfer.v1.StartCommittingTransfer
      transfer.v1.TransferCommittingStarted
      transfer.v1.CommitTransfer
      transfer.v1.StartStagingTransfer
      transfer.v1.StagingTransferStarted
      transfer.v1.CompleteStagingTransfer
      transfer.v1.StartCancellingStagedTransfer
      transfer.v1.CancellingStagedTransferStarted
      transfer.v1.CompleteCancellingStagedTransfer
    ]
  end

  # Every message class the generated code defines in `package` (a module
  # such as Transfer::V1), by full proto name.
  def events_in(package)
    package.constants.map { |name| package.const_get(name) }
           .select { |klass| klass.respond_to?(:descriptor) && klass.descriptor.is_a?(Google::Protobuf::Descriptor) }
           .map { |klass| klass.descriptor.name }
           .reject { |name| name.end_with?('Request', 'Response') || not_events.include?(name) }
  end

  describe 'completeness' do
    # A new event in the proto with no entry here would otherwise be dropped by
    # the projector — indistinguishable from lag (ruby/docs/adr/0001).
    it 'maps every Transaction event in the published proto' do
      expect(described_class::TRANSACTION.keys)
        .to match_array(events_in(Transaction::V1))
    end

    it 'maps every Transfer event in the published proto' do
      expect(described_class::TRANSFER.keys)
        .to match_array(events_in(Transfer::V1))
    end
  end

  describe '.transition' do
    it 'names the Transaction state an event moves to' do
      state = described_class.transition(aggregate_type: 'transaction',
                                         event_type: 'transaction.v1.TransactionCompleted')
      expect(state).to eq(Types::Enums::TransactionState::Completed)
    end

    it 'projects every terminal state, not only the happy path' do
      transitions = {
        'transaction.v1.TransactionRejected' => Types::Enums::TransactionState::Rejected,
        'transaction.v1.TransactionRollbackFailed' => Types::Enums::TransactionState::RollbackFailed,
        'transaction.v1.TransactionRolledBack' => Types::Enums::TransactionState::RolledBack
      }
      transitions.each do |event_type, state|
        expect(described_class.transition(aggregate_type: 'transaction', event_type: event_type)).to eq(state)
      end
      expect(described_class.transition(aggregate_type: 'transfer', event_type: 'transfer.v1.TransferRequestRejected'))
        .to eq(Types::Enums::TransferState::Rejected)
    end

    it 'treats a Reversal as a Transfer' do
      expect(described_class.transition(aggregate_type: 'transfer', event_type: 'transfer.v1.ReversalRequestAccepted'))
        .to eq(Types::Enums::TransferState::Accepted)
    end

    it 'knows step events that leave the state unchanged' do
      expect(
        described_class.transition(aggregate_type: 'transaction',
                                   event_type: 'transaction.v1.TransferRequestedWithinTransaction')
      ).to be_nil
    end

    it 'fails loudly on an unmapped event type rather than skipping it' do
      expect { described_class.transition(aggregate_type: 'transfer', event_type: 'transfer.v1.SomethingNew') }
        .to raise_error(described_class::UnmappedEvent, /transfer\.v1\.SomethingNew/)
    end

    it 'fails loudly on an unknown aggregate type' do
      expect { described_class.transition(aggregate_type: 'wallet', event_type: 'wallet.v1.WalletOpened') }
        .to raise_error(described_class::UnmappedEvent, /wallet/)
    end
  end
end
