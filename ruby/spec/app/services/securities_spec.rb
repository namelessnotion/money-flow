# frozen_string_literal: true

require 'spec_helper'

# The capability end to end, at the service layer: issue an offering, sell it
# to two Investors, and draw it to the Borrower. Go is stubbed, so what this
# proves is the sequence Ruby asks for — which factories, in which order, for
# which amounts, between which wallets — rather than what the ledger does with
# it. That sequence is the part Ruby owns.
#
# It runs two Issuers on purpose. Nothing in this capability may treat any
# entity as the house, and a query that quietly assumed a single Issuer would
# pass every other spec in the suite.
RSpec.describe Services::Securities do
  # Built as methods rather than lets: every gateway here shares the one
  # stubbed transaction client, so there is nothing to memoize for isolation.
  def issue = @issue ||= Services::Securities::IssueOffering.new(gateway: securities_gateway)
  def purchase = @purchase ||= Services::Securities::Purchase.new(gateway: securities_gateway)
  def draw = @draw ||= Services::Securities::Draw.new(gateway: securities_gateway)

  let(:sent) { [] }

  before do
    stub_go_securities_happy_path
    allow(transaction_client).to receive(:start_initializing_transaction) do |req|
      sent << req
      transaction_initialized(req.id)
    end
  end

  # Go completed it, and the read model has caught up.
  def observed(transaction_id)
    create(:transaction_projection, aggregate_id: transaction_id, state: 'completed')
  end

  def issuer = @issuer ||= create_provisioned_entity(role: Types::Enums::EntityRole::Issuer)
  def borrower = @borrower ||= create_provisioned_entity(role: Types::Enums::EntityRole::Borrower)

  def open_offering(principal:, issued_by: issuer, owed_by: borrower)
    security = issue.call(request: Services::Securities::IssueOffering::Request.new(
      issuer_entity_id: issued_by.id, borrower_entity_id: owed_by.id, name: 'Elm Street rehab',
      principal_minor_units: principal, annual_rate_bps: 1000, term_days: 365
    ))
    observed(security.offering_transaction_id)
    security
  end

  def buy(security, investor, amount)
    subscription = purchase.call(security_id: security.id, investor_entity_id: investor.id,
                                 amount_minor_units: amount)
    observed(subscription.id)
    subscription
  end

  describe 'the whole walk' do
    let(:alice) { create_provisioned_entity }
    let(:bob) { create_provisioned_entity }
    let(:security) { open_offering(principal: 100_000) }

    before do
      buy(security, alice, 75_000)
      buy(security, bob, 25_000)
      draw.call(security_id: security.id)
    end

    it 'asks Go for each factory in the order a Security lives them' do
      expect(sent.map(&:factory_name))
        .to eq(%w[security_offering security_purchase security_purchase security_draw])
    end

    it 'mints exactly the offering size, sells it in fractions, and draws the whole of it' do
      amounts = sent.map { |req| req.transfers.values.map { |t| t.amount.minor_units }.max }

      expect(amounts).to eq([100_000, 75_000, 25_000, 100_000])
    end

    it 'mints only once, at the offering, and never again' do
      minting = sent.select { |req| req.transfers.values.any?(&:mint_source) }

      expect(minting.map(&:factory_name)).to eq(['security_offering'])
    end

    it 'stages nothing: no part of this crosses the bank boundary' do
      expect(sent.flat_map { |req| req.transfers.values.map(&:stage) }.uniq).to eq([false])
    end

    it 'gates the money leg of every purchase behind its claim leg' do
      purchases = sent.select { |req| req.factory_name == 'security_purchase' }

      purchases.each do |req|
        gated = req.transfer_dependency.keys
        expect(gated.size).to eq(1)
        # The gated leg is the one leaving the investor's cleared cash.
        expect(req.transfers[gated.first].to_wallet_id).to eq(escrow_wallet(security))
      end
    end

    it 'leaves each holder their fraction, and the Security fully subscribed' do
      positions = Services::Securities::Positions.of(security)

      expect(positions.map(&:principal_minor_units)).to contain_exactly(75_000, 25_000)
      expect(Services::Securities::Positions.subscribed(security)).to eq(100_000)
    end

    it 'reads as drawn once the read model has seen the draw complete' do
      observed(security.reload.draw_transaction_id)

      snapshot = Services::Securities::Stage.snapshot_of(security.reload)
      expect(Services::Securities::Stage.current(snapshot)).to eq(Services::Securities::Stage::Name::Drawn)
    end
  end

  describe 'a second Issuer running its own offering' do
    let(:other_issuer) { create_provisioned_entity(role: Types::Enums::EntityRole::Issuer) }
    let(:other_borrower) { create_provisioned_entity(role: Types::Enums::EntityRole::Borrower) }
    let(:alice) { create_provisioned_entity }

    it 'keeps each offering on its own Issuer, and one Investor can hold both' do
      mine = open_offering(principal: 40_000)
      theirs = open_offering(principal: 60_000, issued_by: other_issuer, owed_by: other_borrower)

      buy(mine, alice, 40_000)
      buy(theirs, alice, 60_000)

      expect(Services::Securities::Positions.subscribed(mine)).to eq(40_000)
      expect(Services::Securities::Positions.subscribed(theirs)).to eq(60_000)
      expect(supply_wallet(mine)).not_to eq(supply_wallet(theirs))
    end

    it "mints each Issuer's supply from its own control wallet" do
      open_offering(principal: 40_000)
      open_offering(principal: 60_000, issued_by: other_issuer, owed_by: other_borrower)

      minted_from = sent.select { |req| req.factory_name == 'security_offering' }
                        .map { |req| req.transfers.values.first.from_wallet_id }

      expect(minted_from).to eq([control_wallet(issuer), control_wallet(other_issuer)])
    end
  end

  def supply_wallet(security)
    Models::Account.where(security_id: security.id, type: 'security_supply').first.wallet_uuid
  end

  def escrow_wallet(security)
    Models::Account.where(security_id: security.id, type: 'security_escrow').first.wallet_uuid
  end

  def control_wallet(entity)
    Models::Account.where(entity_id: entity.id, type: 'issuer_control', security_id: nil).first.wallet_uuid
  end
end
