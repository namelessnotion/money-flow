# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::DrawShape do
  let(:world) { securities_world(principal_minor_units: 1_000_000) }
  let(:transfer_id) { SecureRandom.uuid_v7 }
  let(:cash_id) { Services::Securities::Leg.cash_id_for(transfer_id) }
  let(:request) do
    described_class
      .for(security: world.security, accounts: world.accounts, transfer_id: transfer_id)
      .start_request(transaction_id: SecureRandom.uuid_v7, amount_minor_units: 1_000_000)
  end

  def leg = request.transfers[transfer_id]
  def cash_leg = request.transfers[cash_id]

  it 'names the factory that built it' do
    expect([request.factory_name, request.factory_version]).to eq(%w[security_draw 3])
  end

  it "moves the Security's escrow into the Borrower's cleared cash" do
    expect(leg.from_wallet_id).to eq(world.security_wallet('security_escrow'))
    expect(leg.to_wallet_id).to eq(world.wallet_of(world.borrower, 'cleared_cash'))
    expect(leg.amount.minor_units).to eq(1_000_000)
  end

  it "moves the Security's cash into the Borrower's cash, so the Borrower can take it to a bank" do
    # An ACH withdrawal's real leg draws on `cash`. Before this leg existed a
    # Borrower could spend drawn money on the platform but never withdraw it.
    expect(cash_leg.from_wallet_id).to eq(world.security_wallet('security_cash'))
    expect(cash_leg.to_wallet_id).to eq(world.wallet_of(world.borrower, 'cash'))
    expect(cash_leg.amount.minor_units).to eq(1_000_000)
  end

  it 'sends exactly the two legs' do
    expect(request.transfers.keys).to contain_exactly(transfer_id, cash_id)
  end

  it 'makes the cash leg the root and mints nothing, so short cash is refused before anything is written' do
    [leg, cash_leg].each { |transfer| expect(transfer.mint_source).to be false }
    expect(request.transfer_dependency.keys).to eq([transfer_id])
  end

  it "credits the Borrower's cleared cash only once their cash has arrived, so their cash always covers it" do
    # Otherwise the Borrower could fund a withdrawal from drawn cleared cash
    # whose cash hasn't landed, and have it refused
    # (ruby/docs/adr/0010, namelessnotion/money_flow#7).
    expect(request.transfer_dependency[transfer_id].transfer_id).to eq([cash_id])
  end

  it 'never stages: it does not cross the bank boundary' do
    # Reaching a real bank is an ordinary ACH withdrawal, on rails that
    # already exist.
    [leg, cash_leg].each { |transfer| expect(transfer.stage).to be false }
  end
end
