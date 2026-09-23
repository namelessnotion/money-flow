# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Ach::Settle do
  subject(:service) { described_class.new(gateway: go_gateway) }

  let(:ach) { create(:ach_transaction) }

  before { stub_go_happy_path }

  it 'posts the pending real leg, and leaves the Transaction to the orchestrator' do
    service.call(ach_transaction_id: ach.id)

    expect(transfer_client).to have_received(:post_pending_transfer) do |req|
      expect(req.id).to eq(ach.real_transfer_id)
    end
    # Posting writes TransferCommitted to the leg's own stream, and that event
    # is what wakes the owning Transaction. Asking Go to do it here as well
    # would be racing the orchestrator for the same work.
    expect(transaction_client).not_to have_received(:get_transaction_state)
  end

  it 'raises when Go refuses to post' do
    allow(transfer_client).to receive(:post_pending_transfer) do |req|
      twirp_ok(Transfer::V1::PostPendingTransferResponse.new(
                 id: req.id,
                 post_pending_transfer_rejected: Transfer::V1::PostPendingTransferRejected.new(id: req.id,
                                                                                               reason: 'not pending')
               ))
    end

    expect { service.call(ach_transaction_id: ach.id) }.to raise_error(Services::Ach::Refused, 'not pending')
  end

  it 'raises Unavailable once Go stays unreachable' do
    allow(transfer_client).to receive(:post_pending_transfer).and_raise(Faraday::ConnectionFailed, 'refused')

    expect { service.call(ach_transaction_id: ach.id) }.to raise_error(Services::Ach::Unavailable, /refused/)
  end

  it 'refuses an unknown ACH Transaction' do
    expect { service.call(ach_transaction_id: SecureRandom.uuid_v7) }.to raise_error(Services::Ach::NotFound)
  end
end
