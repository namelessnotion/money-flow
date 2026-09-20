# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::OfferingShape do
  let(:world) { securities_world(principal_minor_units: 1_000_000) }
  let(:transfer_id) { SecureRandom.uuid_v7 }
  let(:request) do
    described_class
      .for(security: world.security, accounts: world.accounts, supply_transfer_id: transfer_id)
      .start_request(transaction_id: SecureRandom.uuid_v7, amount_minor_units: 1_000_000)
  end

  def leg = request.transfers[transfer_id]

  it 'names the factory that built it' do
    expect([request.factory_name, request.factory_version]).to eq(%w[security_offering 1])
  end

  it "mints the offering size into the Security's supply from the Issuer's control wallet" do
    expect(leg.from_wallet_id).to eq(world.wallet_of(world.issuer, 'issuer_control'))
    expect(leg.to_wallet_id).to eq(world.security_wallet('security_supply'))
    expect(leg.amount.minor_units).to eq(1_000_000)
  end

  it 'mints at the source, which is why issuer_control has to allow onramp' do
    # Go's validateMintSource refuses mint_source from any wallet narrower than
    # ALLOWS_ONRAMP, and this is the only way claims enter the ledger at all.
    expect(leg.mint_source).to be true
  end

  it 'is a root, so there is nothing for Go to order it against' do
    expect(request.transfer_dependency.keys).to be_empty
  end

  it 'refuses an issuer with nowhere to mint from' do
    # Only the issuer role opens an issuer_control wallet.
    borrower_issued = create(:security, issuer: world.borrower, borrower: world.investor)
    open_security_wallets(world.borrower, borrower_issued)

    expect do
      described_class.for(security: borrower_issued, accounts: world.accounts, supply_transfer_id: transfer_id)
    end.to raise_error(Services::Securities::MissingAccount, /issuer_control/)
  end
end
