# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Ach::Settle do
  subject(:service) { described_class.new(gateway: go_gateway) }

  let(:ach) { create(:ach_transaction) }

  before { stub_go_happy_path }

  it 'posts the pending real leg, then resumes the Transaction so the shadow leg runs' do
    service.call(ach_transaction_id: ach.id)

    expect(transfer_client).to have_received(:post_pending_transfer) do |req|
      expect(req.id).to eq(ach.real_transfer_id)
    end
    expect(transaction_client).to have_received(:resume_transaction) { |req| expect(req.id).to eq(ach.id) }
  end

  it 'raises when Go refuses to post, and does not resume' do
    allow(transfer_client).to receive(:post_pending_transfer) do |req|
      twirp_ok(Transfer::V1::PostPendingTransferResponse.new(
                 id: req.id,
                 post_pending_transfer_rejected: Transfer::V1::PostPendingTransferRejected.new(id: req.id,
                                                                                               reason: 'not pending')
               ))
    end

    expect { service.call(ach_transaction_id: ach.id) }.to raise_error(Services::Ach::Refused, 'not pending')
    expect(transaction_client).not_to have_received(:resume_transaction)
  end

  it 'raises Unavailable once Go stays unreachable' do
    allow(transfer_client).to receive(:post_pending_transfer).and_raise(Faraday::ConnectionFailed, 'refused')

    expect { service.call(ach_transaction_id: ach.id) }.to raise_error(Services::Ach::Unavailable, /refused/)
  end

  it 'refuses an unknown ACH Transaction' do
    expect { service.call(ach_transaction_id: SecureRandom.uuid_v7) }.to raise_error(Services::Ach::NotFound)
  end
end
