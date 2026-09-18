# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Consumer::Projector do
  subject(:projector) { described_class.new }

  let(:transaction_id) { SecureRandom.uuid_v7 }
  let(:transfer_id) { SecureRandom.uuid_v7 }

  def txn_event(message, sequence)
    envelope_for(message, aggregate_id: transaction_id, sequence: sequence)
  end

  def transfer_event(message, sequence, aggregate_id: transfer_id)
    envelope_for(message, aggregate_id: aggregate_id, sequence: sequence)
  end

  def transaction_row
    Models::TransactionProjection[transaction_id]
  end

  def initialized = Transaction::V1::TransactionInitialized.new(id: transaction_id)
  def started = Transaction::V1::TransactionStarted.new(id: transaction_id)
  def step = Transaction::V1::TransferRequestedWithinTransaction.new(id: transaction_id, transfer_id: transfer_id)
  def completed = Transaction::V1::TransactionCompleted.new(id: transaction_id)

  describe 'a Transaction' do
    it 'creates the row the first time the aggregate is seen' do
      expect(projector.apply(txn_event(initialized, 1))).to be true

      expect(transaction_row.state).to eq('initialized')
      expect(transaction_row.last_sequence).to eq(1)
    end

    it 'follows the stream in order to its terminal state' do
      [initialized, started, step, completed].each.with_index(1) do |event, sequence|
        projector.apply(txn_event(event, sequence))
      end

      expect(transaction_row.state).to eq('completed')
      expect(transaction_row.last_sequence).to eq(4)
    end

    it 'ignores a duplicate delivery' do
      projector.apply(txn_event(initialized, 1))
      projector.apply(txn_event(started, 2))

      expect(projector.apply(txn_event(started, 2))).to be false
      expect(transaction_row.state).to eq('started')
    end

    # The failure the monotonic guard exists for (ruby/docs/adr/0001): two of
    # one aggregate's messages arrive swapped, and last-write-wins would walk
    # the read model backwards and never recover.
    it 'never walks backwards when messages arrive out of order' do
      projector.apply(txn_event(initialized, 1))
      projector.apply(txn_event(completed, 3))

      expect(projector.apply(txn_event(started, 2))).to be false
      expect(transaction_row.state).to eq('completed')
      expect(transaction_row.last_sequence).to eq(3)
    end

    it 'advances the high-water mark on a step event without changing the state' do
      projector.apply(txn_event(initialized, 1))
      projector.apply(txn_event(started, 2))
      projector.apply(txn_event(step, 3))

      expect(transaction_row.state).to eq('started')
      expect(transaction_row.last_sequence).to eq(3)
    end

    it 'records the reason a terminal state carries' do
      projector.apply(txn_event(Transaction::V1::TransactionRejected.new(id: transaction_id, reason: 'cycle'), 1))

      expect(transaction_row.state).to eq('rejected')
      expect(transaction_row.reason).to eq('cycle')
    end

    it 'projects a rollback that failed' do
      projector.apply(txn_event(initialized, 1))
      rollback_started = Transaction::V1::TransactionRollbackStarted.new(id: transaction_id, reason: 'returned')
      projector.apply(txn_event(rollback_started, 2))
      projector.apply(txn_event(Transaction::V1::TransactionRollbackFailed.new(id: transaction_id, reason: 'stuck'), 3))

      expect(transaction_row.state).to eq('rollback_failed')
      expect(transaction_row.reason).to eq('stuck')
    end
  end

  describe 'a Transfer' do
    it 'records the owning Transaction from its opening event' do
      accepted = Transfer::V1::TransferRequestAccepted.new(id: transfer_id, transaction_id: transaction_id)
      projector.apply(transfer_event(accepted, 1))
      projector.apply(transfer_event(Transfer::V1::TransferStaged.new(id: transfer_id), 2))

      row = Models::TransferProjection[transfer_id]
      expect(row.state).to eq('staged')
      expect(row.transaction_id).to eq(transaction_id)
    end

    it 'creates a row for a Reversal it has never heard of' do
      reversal_id = SecureRandom.uuid_v7
      accepted = Transfer::V1::ReversalRequestAccepted.new(id: reversal_id, transaction_id: transaction_id)

      projector.apply(transfer_event(accepted, 1, aggregate_id: reversal_id))

      expect(Models::TransferProjection[reversal_id].state).to eq('accepted')
    end

    it 'keeps the state when a settlement command was refused' do
      projector.apply(transfer_event(Transfer::V1::TransferRequestAccepted.new(id: transfer_id), 1))
      refused = Transfer::V1::PostPendingTransferRejected.new(id: transfer_id, reason: 'not pending')
      projector.apply(transfer_event(refused, 2))

      row = Models::TransferProjection[transfer_id]
      expect(row.state).to eq('accepted')
      expect(row.reason).to be_nil
      expect(row.last_sequence).to eq(2)
    end
  end

  # With snapshot.mode=no_data, the first event Ruby sees for an aggregate that
  # predates the connector can be a mid-stream step. There is no state to
  # record yet, so nothing is — the next state-changing event creates the row.
  it 'does not invent a row from a step event on an unseen aggregate' do
    expect(projector.apply(txn_event(step, 5))).to be false
    expect(transaction_row).to be_nil
  end

  it 'fails loudly on an unmapped event without writing anything' do
    envelope = Consumer::Envelope.new(
      aggregate_id: transfer_id, aggregate_type: 'transfer', event_type: 'transfer.v1.SomethingNew',
      sequence: 1, global_seq: 1, payload: ''
    )

    expect { projector.apply(envelope) }.to raise_error(Consumer::EventStateMap::UnmappedEvent)
    expect(Models::TransferProjection[transfer_id]).to be_nil
  end
end
