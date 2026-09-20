# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::RepaymentShape do
  let(:world) { securities_world }
  let(:transfer_id) { SecureRandom.uuid_v7 }
  let(:request) do
    described_class
      .for(security: world.security, accounts: world.accounts, transfer_id: transfer_id)
      .start_request(transaction_id: SecureRandom.uuid_v7, amount_minor_units: 110_000)
  end

  def leg = request.transfers[transfer_id]

  it 'names the factory that built it' do
    expect([request.factory_name, request.factory_version]).to eq(%w[security_repayment 1])
  end

  it "moves the Borrower's cleared cash into the Security's repayment wallet" do
    expect(leg.from_wallet_id).to eq(world.wallet_of(world.borrower, 'cleared_cash'))
    expect(leg.to_wallet_id).to eq(world.security_wallet('security_repayment'))
  end

  it 'collects principal and interest as one amount' do
    # Which part is principal is a business fact, recorded on the repayments
    # row where the allocation reads it, not something the ledger tracks.
    expect(leg.amount.minor_units).to eq(110_000)
  end

  it 'is a root and mints nothing, so an underfunded Borrower is refused at accept time' do
    expect(leg.mint_source).to be false
    expect(request.transfer_dependency.keys).to be_empty
  end
end
