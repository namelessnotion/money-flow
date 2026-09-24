# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::PurchaseShape do
  let(:world) { securities_world }
  let(:claim_id) { SecureRandom.uuid_v7 }
  let(:money_id) { SecureRandom.uuid_v7 }
  let(:cash_id) { Services::Securities::Leg.cash_id_for(money_id) }
  let(:request) do
    described_class
      .for(security: world.security, investor_entity_id: world.investor.id, accounts: world.accounts,
           claim_transfer_id: claim_id, money_transfer_id: money_id)
      .start_request(transaction_id: SecureRandom.uuid_v7, amount_minor_units: 25_000)
  end

  def leg(id) = request.transfers[id]

  it 'names the factory that built it' do
    expect([request.factory_name, request.factory_version]).to eq(%w[security_purchase 2])
  end

  it "moves claims out of the Security's supply into the Investor's claims" do
    expect(leg(claim_id).from_wallet_id).to eq(world.security_wallet('security_supply'))
    expect(leg(claim_id).to_wallet_id).to eq(world.wallet_of(world.investor, 'investment'))
  end

  it "moves the Investor's cleared cash into the Security's escrow" do
    expect(leg(money_id).from_wallet_id).to eq(world.wallet_of(world.investor, 'cleared_cash'))
    expect(leg(money_id).to_wallet_id).to eq(world.security_wallet('security_escrow'))
  end

  it "moves the Investor's cash into the Security's cash beside it, so the real money follows" do
    # Without this leg the Investor's cash still says they hold money they
    # have spent, and the Security holds none of the money it will draw.
    expect(leg(cash_id).from_wallet_id).to eq(world.wallet_of(world.investor, 'cash'))
    expect(leg(cash_id).to_wallet_id).to eq(world.security_wallet('security_cash'))
  end

  it 'sends exactly the claim, money and cash legs' do
    expect(request.transfers.keys).to contain_exactly(claim_id, money_id, cash_id)
  end

  it 'moves the same amount on every leg' do
    [claim_id, money_id, cash_id].each do |id|
      expect(leg(id).amount.minor_units).to eq(25_000)
      expect(leg(id).amount.currency).to eq('USD')
    end
  end

  it 'stages nothing and mints nothing: every wallet is inside the platform' do
    # The supply was minted once when the offering opened; a purchase only
    # moves what is already there.
    [claim_id, money_id, cash_id].each do |id|
      expect(leg(id).stage).to be false
      expect(leg(id).mint_source).to be false
      expect(leg(id).auto_process).to be true
    end
  end

  it 'leaves the claim leg a root and gates both money legs behind it, ' \
     'so oversubscription is settled before any investor money moves' do
    # security_supply is the hot wallet — every concurrent purchase of this
    # Security draws on it, and it is exactly where Go's accept-time pre-flight
    # can over-accept, because it checks every ready-at-zero child against one
    # unconsumed snapshot.
    #
    # Claim-leg-first buys two things. Normally the claim leg IS pre-flighted,
    # so an oversubscription is a clean TransactionRejected before anything is
    # written. And when the race does slip past, the claim leg fails at real
    # dispatch with the money leg never having run: nothing to reverse, and no
    # Investor money moved. Had the money leg been a root, every lost race
    # would reverse a committed payment instead.
    expect(request.transfer_dependency.keys).to contain_exactly(money_id, cash_id)
    [money_id, cash_id].each do |id|
      expect(request.transfer_dependency[id].transfer_id).to eq([claim_id])
    end
  end

  it 'refuses an investor with nowhere to hold claims' do
    # A Borrower banks, but holds no claims — it is not a party that can buy.
    expect do
      described_class.for(security: world.security, investor_entity_id: world.borrower.id,
                          accounts: world.accounts,
                          claim_transfer_id: claim_id, money_transfer_id: money_id)
    end.to raise_error(Services::Securities::MissingAccount, /investment/)
  end

  it 'refuses a security whose wallets were never opened' do
    unopened = create(:security, issuer: world.issuer, borrower: world.borrower)

    expect do
      described_class.for(security: unopened, investor_entity_id: world.investor.id,
                          accounts: world.accounts,
                          claim_transfer_id: claim_id, money_transfer_id: money_id)
    end.to raise_error(Services::Securities::MissingAccount, /security #{unopened.id}/)
  end
end
