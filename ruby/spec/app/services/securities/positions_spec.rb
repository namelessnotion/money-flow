# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::Positions do
  let(:world) { securities_world(principal_minor_units: 1_000_000) }
  let(:security) { world.security }

  def completed(aggregate_id)
    create(:transaction_projection, aggregate_id: aggregate_id, state: 'completed')
  end

  def bought(investor, amount, state: 'completed')
    subscription = create(:subscription, security: security, investor: investor, amount_minor_units: amount)
    create(:transaction_projection, aggregate_id: subscription.id, state: state) if state
    subscription
  end

  it 'counts nothing for a Security nobody has bought into' do
    expect(described_class.of(security)).to be_empty
    expect(described_class.subscribed(security)).to eq(0)
  end

  it "sums an Investor's purchases into one position" do
    bought(world.investor, 30_000)
    bought(world.investor, 20_000)

    position = described_class.of(security).first

    expect(position.investor_entity_id).to eq(world.investor.id)
    expect(position.principal_minor_units).to eq(50_000)
    expect(position.outstanding_principal_minor_units).to eq(50_000)
  end

  it 'counts only purchases the read model has seen complete' do
    # In flight, or rejected, is not held.
    bought(world.investor, 30_000)
    bought(world.investor, 70_000, state: 'rejected')
    bought(world.investor, 90_000, state: nil)

    expect(described_class.subscribed(security)).to eq(30_000)
  end

  it 'orders holders by entity id, because the allocation breaks ties by key' do
    # ProRata is only reproducible if the positions it is given always arrive
    # the same way round; a sweep re-running must reach the same amounts.
    second = create_provisioned_entity
    bought(second, 10_000)
    bought(world.investor, 10_000)

    ids = described_class.of(security).map(&:investor_entity_id)

    expect(ids).to eq(ids.sort)
  end

  describe 'once principal has been repaid' do
    let(:repayment) { create(:repayment, security: security) }

    def disbursed(investor, principal, state: 'completed')
      disbursement = create(:disbursement, repayment: repayment, investor: investor,
                                           principal_minor_units: principal, interest_minor_units: 1)
      create(:transaction_projection, aggregate_id: disbursement.id, state: state) if state
      disbursement
    end

    it 'leaves the purchase intact and shrinks what is outstanding' do
      bought(world.investor, 50_000)
      disbursed(world.investor, 20_000)

      position = described_class.of(security).first

      expect(position.principal_minor_units).to eq(50_000)
      expect(position.outstanding_principal_minor_units).to eq(30_000)
    end

    it 'ignores a disbursement still in flight: nobody has been repaid yet' do
      bought(world.investor, 50_000)
      disbursed(world.investor, 20_000, state: nil)

      expect(described_class.outstanding_principal(security)).to eq(50_000)
    end

    it 'sums what is still owed across every holder' do
      second = create_provisioned_entity
      bought(world.investor, 50_000)
      bought(second, 30_000)
      disbursed(world.investor, 20_000)

      expect(described_class.outstanding_principal(security)).to eq(60_000)
    end
  end

  it "never counts another Security's purchases" do
    other = create(:security, issuer: world.issuer, borrower: world.borrower)
    subscription = create(:subscription, security: other, investor: world.investor, amount_minor_units: 99_000)
    completed(subscription.id)
    bought(world.investor, 10_000)

    expect(described_class.subscribed(security)).to eq(10_000)
  end
end
