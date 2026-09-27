# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Ach::Return do
  subject(:service) { described_class.new(gateway: go_gateway) }

  let(:ach) { create(:ach_transaction) }

  before { stub_go_happy_path }

  it 'cancels the real leg with the return reason, and leaves the rollback to the orchestrator' do
    service.call(ach_transaction_id: ach.id, reason: 'R01 insufficient funds')

    expect(transfer_client).to have_received(:cancel_staged_transfer) do |req|
      expect([req.id, req.reason]).to eq([ach.real_transfer_id, 'R01 insufficient funds'])
    end
    # The cancellation is an event on the leg's own stream, and following it to
    # the owning Transaction is the orchestrator's job.
    expect(transaction_client).not_to have_received(:get_transaction_state)
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

  it 'raises when Go refuses to cancel' do
    allow(transfer_client).to receive(:cancel_staged_transfer) do |req|
      twirp_ok(Transfer::V1::CancelStagedTransferResponse.new(
                 id: req.id,
                 cancel_staged_transfer_rejected: Transfer::V1::CancelStagedTransferRejected.new(id: req.id,
                                                                                                 reason: 'committed')
               ))
    end

    expect { service.call(ach_transaction_id: ach.id, reason: 'R01') }
      .to raise_error(Services::Ach::Refused, 'committed')
  end
end
