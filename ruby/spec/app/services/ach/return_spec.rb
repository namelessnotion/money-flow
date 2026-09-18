# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Ach::Return do
  subject(:service) { described_class.new(gateway: go_gateway) }

  let(:ach) { create(:ach_transaction) }

  before { stub_go_happy_path }

  it 'cancels the real leg with the return reason, then resumes the Transaction so Go rolls it back' do
    service.call(ach_transaction_id: ach.id, reason: 'R01 insufficient funds')

    expect(transfer_client).to have_received(:cancel_staged_transfer) do |req|
      expect([req.id, req.reason]).to eq([ach.real_transfer_id, 'R01 insufficient funds'])
    end
    expect(transaction_client).to have_received(:resume_transaction) { |req| expect(req.id).to eq(ach.id) }
  end

  it 'raises when Go refuses to cancel, and does not resume' do
    allow(transfer_client).to receive(:cancel_staged_transfer) do |req|
      twirp_ok(Transfer::V1::CancelStagedTransferResponse.new(
                 id: req.id,
                 cancel_staged_transfer_rejected: Transfer::V1::CancelStagedTransferRejected.new(id: req.id,
                                                                                                 reason: 'committed')
               ))
    end

    expect { service.call(ach_transaction_id: ach.id, reason: 'R01') }
      .to raise_error(Services::Ach::Refused, 'committed')
    expect(transaction_client).not_to have_received(:resume_transaction)
  end
end
