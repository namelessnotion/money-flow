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
    expect([request.factory_name, request.factory_version]).to eq(%w[security_draw 2])
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

  it 'makes both legs roots and mints nothing, so a short escrow or short cash is refused before anything is written' do
    # Two roots on two different wallets: Go's pre-flight checks each against
    # its own balance, so there is no shared snapshot to over-accept against.
    [leg, cash_leg].each { |transfer| expect(transfer.mint_source).to be false }
    expect(request.transfer_dependency.keys).to be_empty
  end

  it 'never stages: it does not cross the bank boundary' do
    # Reaching a real bank is an ordinary ACH withdrawal, on rails that
    # already exist.
    [leg, cash_leg].each { |transfer| expect(transfer.stage).to be false }
  end
end
