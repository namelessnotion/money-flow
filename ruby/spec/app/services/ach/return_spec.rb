# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Ach::Return do
  subject(:service) { described_class.new(gateway: go_gateway) }

  let(:entity) { create_provisioned_entity }
  let(:ach) { create(:ach_transaction, entity: entity) }

  before do
    stub_go_happy_path
    wallet_uuids_of(entity)
  end

  def go_says(state)
    allow(transaction_client).to receive(:get_transaction_state) { |req| transaction_state(req.id, state) }
  end

  def go_refuses_to_cancel
    allow(transfer_client).to receive(:cancel_staged_transfer) do |req|
      twirp_ok(Transfer::V1::CancelStagedTransferResponse.new(
                 id: req.id,
                 cancel_staged_transfer_rejected: Transfer::V1::CancelStagedTransferRejected.new(id: req.id,
                                                                                                 reason: 'committed')
               ))
    end
  end

  def started_requests
    requests = []
    allow(transaction_client).to receive(:start_initializing_transaction) do |req|
      requests << req
      transaction_initialized(req.id)
    end
    requests
  end

  it 'records the notice before acting on it' do
    service.call(ach_transaction_id: ach.id, reason: 'R01 insufficient funds')

    expect(ach.reload.return_reason).to eq('R01 insufficient funds')
    expect(ach.returned_at).not_to be_nil
  end

  it 'keeps the first notice when a second arrives' do
    service.call(ach_transaction_id: ach.id, reason: 'R01 insufficient funds')
    first_at = ach.reload.returned_at

    service.call(ach_transaction_id: ach.id, reason: 'R10 unauthorized')

    expect([ach.reload.return_reason, ach.returned_at]).to eq(['R01 insufficient funds', first_at])
  end

  it 'raises NotFound for an unknown id' do
    expect { service.call(ach_transaction_id: SecureRandom.uuid_v7, reason: 'R01') }
      .to raise_error(Services::Ach::NotFound)
  end

  context 'when the ACH Transaction is still running' do
    before { go_says(:TRANSACTION_STATE_STARTED) }

    it 'cancels the real leg with the return reason, and leaves the rollback to the orchestrator' do
      service.call(ach_transaction_id: ach.id, reason: 'R01 insufficient funds')

      expect(transfer_client).to have_received(:cancel_staged_transfer) do |req|
        expect([req.id, req.reason]).to eq([ach.real_transfer_id, 'R01 insufficient funds'])
      end
      expect(transaction_client).not_to have_received(:start_initializing_transaction)
    end

    # The leg posted between the notice and the cancel, and the Transaction
    # hasn't completed yet. The notice is on record, so ReturnDue acts on it
    # once it has.
    it 'leaves the notice for the sweep when Go refuses to cancel' do
      go_refuses_to_cancel

      expect(service.call(ach_transaction_id: ach.id, reason: 'R01').id).to eq(ach.id)
      expect(ach.reload.return_transaction_id).to be_nil
    end
  end

  context 'when the ACH Transaction has completed' do
    before { go_says(:TRANSACTION_STATE_COMPLETED) }

    it 'claws back a deposit whose Clearing was never recorded' do
      requests = started_requests

      service.call(ach_transaction_id: ach.id, reason: 'R01')

      expect(requests.map(&:factory_name)).to eq(['ach_deposit_clawback'])
      expect(transfer_client).not_to have_received(:cancel_staged_transfer)
    end

    it 'records a deposit whose Clearing was recorded as owed' do
      ach.update(clearing_transaction_id: Services::DetId.for("#{ach.id}:clearing"))
      requests = started_requests

      service.call(ach_transaction_id: ach.id, reason: 'R10 unauthorized')

      expect(requests.map(&:factory_name)).to eq(['ach_deposit_return'])
      expect(requests.first.transfers.values.map(&:amount).map(&:minor_units)).to eq([ach.amount_minor_units])
    end

    it 'puts a withdrawal back' do
      withdrawal = create(:ach_transaction, entity: entity, direction: 'withdrawal')
      requests = started_requests

      service.call(ach_transaction_id: withdrawal.id, reason: 'R02 account closed')

      expect(requests.map(&:factory_name)).to eq(['ach_withdrawal_return'])
    end

    it 'records the late return before asking Go for it' do
      recorded = nil
      allow(transaction_client).to receive(:start_initializing_transaction) do |req|
        recorded = Models::AchTransaction[ach.id].return_transaction_id
        transaction_initialized(req.id)
      end

      service.call(ach_transaction_id: ach.id, reason: 'R01')

      expect(recorded).to eq(Services::DetId.for("#{ach.id}:return"))
    end

    # Its id is derived from the ACH Transaction's, so Go dedupes it.
    it 'sends the same late return again, without asking Go where things stand' do
      requests = started_requests
      service.call(ach_transaction_id: ach.id, reason: 'R01')

      service.call(ach_transaction_id: ach.id, reason: 'R01')

      expect(requests.map(&:id).uniq).to eq([Services::DetId.for("#{ach.id}:return")])
      expect(requests.size).to eq(2)
      expect(transaction_client).to have_received(:get_transaction_state).once
    end

    it "raises Go's reason when it rejects the late return" do
      allow(transaction_client).to receive(:start_initializing_transaction) do |req|
        transaction_rejected(req.id, 'insufficient Token capacity')
      end

      expect { service.call(ach_transaction_id: ach.id, reason: 'R01') }
        .to raise_error(Services::Ach::Refused, 'insufficient Token capacity')
    end
  end

  # Rolled back or rejected, it never moved money, so there is nothing to take
  # back. Rollback failed needs a person.
  %i[TRANSACTION_STATE_ROLLED_BACK TRANSACTION_STATE_REJECTED TRANSACTION_STATE_ROLLBACK_FAILED].each do |state|
    it "does nothing more for a Transaction that is #{state}" do
      go_says(state)

      service.call(ach_transaction_id: ach.id, reason: 'R01')

      expect(transfer_client).not_to have_received(:cancel_staged_transfer)
      expect(transaction_client).not_to have_received(:start_initializing_transaction)
    end
  end
end
