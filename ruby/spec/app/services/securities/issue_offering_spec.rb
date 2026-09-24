# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::IssueOffering do
  subject(:service) { described_class.new(gateway: securities_gateway) }

  let(:issuer) { create_provisioned_entity(role: Types::Enums::EntityRole::Issuer) }
  let(:borrower) { create_provisioned_entity(role: Types::Enums::EntityRole::Borrower) }

  def terms
    { issuer_entity_id: issuer.id, borrower_entity_id: borrower.id, name: 'Elm Street rehab',
      principal_minor_units: 1_000_000, annual_rate_bps: 1000, term_days: 365 }
  end

  def issue(**overrides)
    attributes = terms.merge(overrides)
    service.call(request: described_class::Request.new(**attributes))
  end

  def sent_request
    request = nil
    expect(transaction_client).to have_received(:start_initializing_transaction) { |req| request = req }
    request
  end

  before { stub_go_securities_happy_path }

  it 'records the terms it was asked for' do
    security = issue

    expect(security.name).to eq('Elm Street rehab')
    expect(security.principal_minor_units).to eq(1_000_000)
    expect(security.annual_rate_bps).to eq(1000)
    expect(security.currency).to eq('USD')
    expect(security.issuer_entity_id).to eq(issuer.id)
  end

  it "opens the Security's four wallets on its Issuer's holder" do
    security = issue
    accounts = Models::Account.where(security_id: security.id).all

    expect(accounts.map(&:type))
      .to contain_exactly('security_supply', 'security_escrow', 'security_repayment', 'security_cash')
    expect(accounts.map(&:entity_id).uniq).to eq([issuer.id])
  end

  it 'provisions the wallets before writing anything locally' do
    # A local row must never name a Wallet that was never opened.
    rows_at_provision_time = nil
    allow(holder_client).to receive(:provision) do
      rows_at_provision_time = Models::Security.count
      holder_provisioned
    end

    issue

    expect(rows_at_provision_time).to eq(Models::Security.count - 1)
  end

  it 'asks Go for wallets with the policy each type carries' do
    sent = nil
    allow(holder_client).to receive(:provision) do |req|
      sent = req
      holder_provisioned
    end

    security = issue

    expect(sent.id).to eq(issuer.holder_uuid)
    allows = sent.wallets.to_h { |spec| [spec.name, spec.allows] }
    # ALLOWS_NONE is what gives the supply Token debits_must_not_exceed_credits,
    # which is the oversubscription control.
    expect(allows).to eq('security_supply' => :ALLOWS_NONE, 'security_escrow' => :ALLOWS_NONE,
                         'security_repayment' => :ALLOWS_NONE, 'security_cash' => :ALLOWS_NONE)
    expect(sent.wallets.map(&:wallet_id))
      .to match_array(Models::Account.where(security_id: security.id).all.map(&:wallet_uuid))
  end

  it "mints the offering size into the Security's supply" do
    security = issue
    leg = sent_request.transfers[security.offering_transfer_id]

    expect(sent_request.id).to eq(security.offering_transaction_id)
    expect(sent_request.factory_name).to eq('security_offering')
    expect(leg.amount.minor_units).to eq(1_000_000)
    expect(leg.mint_source).to be true
  end

  it 'derives the mint ids from the Security, so a re-send converges rather than minting twice' do
    security = issue

    expect(security.offering_transaction_id).to eq(Services::DetId.for("security:#{security.id}:offering"))
    expect(security.offering_transfer_id).to eq(Services::DetId.for("security:#{security.id}:offering:supply"))
  end

  describe '#ensure_open' do
    it 'sends exactly the ids it sent the first time' do
      # The remedy for a crash between recording the Security and minting its
      # supply: Go returns the decision it already recorded.
      sent = []
      allow(transaction_client).to receive(:start_initializing_transaction) do |req|
        sent << req
        transaction_initialized(req.id)
      end

      security = issue
      service.ensure_open(security)

      expect(sent.map(&:id).uniq).to eq([security.offering_transaction_id])
      expect(sent.map { |req| req.transfers.keys }.uniq).to eq([[security.offering_transfer_id]])
    end
  end

  describe 'what it refuses' do
    it 'refuses an issuer that is not one' do
      expect { issue(issuer_entity_id: borrower.id) }
        .to raise_error(Services::Securities::WrongRole, /is a borrower, not issuer/)
    end

    it 'refuses a borrower that is not one' do
      expect { issue(borrower_entity_id: issuer.id) }
        .to raise_error(Services::Securities::WrongRole, /is a issuer, not borrower/)
    end

    it 'refuses an unknown party' do
      expect { issue(borrower_entity_id: 0) }.to raise_error(Services::Securities::NotFound, /no entity 0/)
    end

    it 'refuses an offering of nothing, which could have no supply leg' do
      expect { issue(principal_minor_units: 0) }
        .to raise_error(Services::Securities::InvalidAmount, /principal/)
    end

    it 'refuses a term of no days' do
      expect { issue(term_days: 0) }.to raise_error(Services::Securities::InvalidAmount, /term/)
    end

    it 'writes nothing when it refuses' do
      before_count = Models::Security.count

      expect { issue(principal_minor_units: -1) }.to raise_error(Services::Securities::InvalidAmount)
      expect(Models::Security.count).to eq(before_count)
      expect(holder_client).not_to have_received(:provision)
    end
  end

  it 'lets a second Issuer run its own offering: nothing here is the house' do
    other_issuer = create_provisioned_entity(role: Types::Enums::EntityRole::Issuer)
    other_borrower = create_provisioned_entity(role: Types::Enums::EntityRole::Borrower)

    mine = issue
    theirs = issue(issuer_entity_id: other_issuer.id, borrower_entity_id: other_borrower.id)

    expect(theirs.issuer_entity_id).to eq(other_issuer.id)
    expect(theirs.offering_transaction_id).not_to eq(mine.offering_transaction_id)
    expect(Models::Account.where(security_id: theirs.id).all.map(&:entity_id).uniq).to eq([other_issuer.id])
  end
end
