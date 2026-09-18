# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Ach::Clear do
  subject(:service) { described_class.new(gateway: go_gateway) }

  let(:entity) { create(:entity) }
  let(:ach) { create(:ach_transaction, entity: entity, amount_minor_units: 12_500) }
  let(:wallets) do
    Types::Enums::AccountType.values.to_h do |type|
      [type.serialize, create(:account, entity: entity, type: type.serialize).wallet_uuid]
    end
  end

  before do
    wallets
    stub_go_happy_path
  end

  def sent_request
    request = nil
    expect(transaction_client).to have_received(:start_initializing_transaction) { |req| request = req }
    request
  end

  it 'originates one Transfer moving the deposit from uncleared to cleared cash' do
    service.call(ach_transaction_id: ach.id)

    transfer = sent_request.transfers.values.first
    expect(sent_request.transfers.size).to eq(1)
    expect([transfer.from_wallet_id, transfer.to_wallet_id]).to eq([wallets['uncleared_cash'], wallets['cleared_cash']])
    expect([transfer.amount.minor_units, transfer.amount.currency]).to eq([12_500, 'USD'])
    expect([transfer.stage, transfer.mint_source, transfer.auto_process]).to eq([false, false, true])
  end

  # Derived from the deposit, so a second sweep — or a Ruby that forgot it
  # ever sent one — asks Go for the same Transaction, which Go dedupes.
  it 'derives the clearing ids from the deposit' do
    service.call(ach_transaction_id: ach.id)

    expect(sent_request.id).to eq(Services::DetId.for("#{ach.id}:clearing"))
    expect(sent_request.factory_name).to eq('ach_clearing')
    expect(ach.reload.clearing_transaction_id).to eq(sent_request.id)
    expect(ach.clearing_transfer_id).to eq(sent_request.transfers.keys.first)
  end

  it 'sends the same ids every time' do
    ids = []
    allow(transaction_client).to receive(:start_initializing_transaction) do |req|
      ids << [req.id, req.transfers.keys]
      transaction_initialized(req.id)
    end

    2.times { service.call(ach_transaction_id: ach.id) }

    expect(ids.size).to eq(2)
    expect(ids.uniq.size).to eq(1)
  end

  it 'records the ids before calling Go' do
    recorded = nil
    allow(transaction_client).to receive(:start_initializing_transaction) do |req|
      recorded = Models::AchTransaction[ach.id].clearing_transaction_id
      transaction_initialized(req.id)
    end

    service.call(ach_transaction_id: ach.id)

    expect(recorded).to eq(Services::DetId.for("#{ach.id}:clearing"))
  end

  it 'refuses a withdrawal, which has nothing to clear' do
    withdrawal = create(:ach_transaction, entity: entity, direction: 'withdrawal')

    expect { service.call(ach_transaction_id: withdrawal.id) }.to raise_error(Services::Ach::NotClearable)
    expect(transaction_client).not_to have_received(:start_initializing_transaction)
  end

  it 'raises Go\'s reason when it rejects the clearing' do
    allow(transaction_client).to receive(:start_initializing_transaction) do |req|
      rejected = Transaction::V1::TransactionRejected.new(id: req.id, reason: 'insufficient capacity')
      twirp_ok(Transaction::V1::StartInitializingTransactionResponse.new(id: req.id, transaction_rejected: rejected))
    end

    expect { service.call(ach_transaction_id: ach.id) }.to raise_error(Services::Ach::Refused, 'insufficient capacity')
  end
end
