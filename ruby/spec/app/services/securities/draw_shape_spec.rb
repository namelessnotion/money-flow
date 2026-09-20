# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::DrawShape do
  let(:world) { securities_world(principal_minor_units: 1_000_000) }
  let(:transfer_id) { SecureRandom.uuid_v7 }
  let(:request) do
    described_class
      .for(security: world.security, accounts: world.accounts, transfer_id: transfer_id)
      .start_request(transaction_id: SecureRandom.uuid_v7, amount_minor_units: 1_000_000)
  end

  def leg = request.transfers[transfer_id]

  it 'names the factory that built it' do
    expect([request.factory_name, request.factory_version]).to eq(%w[security_draw 1])
  end

  it "moves the Security's escrow into the Borrower's cleared cash" do
    expect(leg.from_wallet_id).to eq(world.security_wallet('security_escrow'))
    expect(leg.to_wallet_id).to eq(world.wallet_of(world.borrower, 'cleared_cash'))
    expect(leg.amount.minor_units).to eq(1_000_000)
  end

  it 'is a root and mints nothing, so a short escrow is refused before anything is written' do
    expect(leg.mint_source).to be false
    expect(request.transfer_dependency.keys).to be_empty
  end

  it 'never stages: it does not cross the bank boundary' do
    # Reaching a real bank is an ordinary ACH withdrawal from the Borrower's
    # own cleared cash, on rails that already exist.
    expect(leg.stage).to be false
  end
end
