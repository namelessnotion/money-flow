# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::Allocation do
  let(:world) { securities_world(principal_minor_units: 100_000) }

  def holds(amount, investor: create_provisioned_entity)
    subscription = create(:subscription, security: world.security, investor: investor,
                                         amount_minor_units: amount)
    create(:transaction_projection, aggregate_id: subscription.id, state: 'completed')
    investor
  end

  def repayment(principal:, interest:)
    create(:repayment, security: world.security,
                       principal_minor_units: principal, interest_minor_units: interest)
  end

  it 'splits a repayment in proportion to what each holder is owed' do
    alice = holds(75_000)
    bob = holds(25_000)

    shares = described_class.for(repayment(principal: 40_000, interest: 4_000))
                            .to_h { |s| [s.investor_entity_id, [s.principal_minor_units, s.interest_minor_units]] }

    expect(shares.fetch(alice.id)).to eq([30_000, 3_000])
    expect(shares.fetch(bob.id)).to eq([10_000, 1_000])
  end

  it 'allocates each bucket exactly, even when neither divides evenly' do
    holds(33_333)
    holds(33_333)
    holds(33_334)
    payment = repayment(principal: 10_000, interest: 777)

    shares = described_class.for(payment)

    expect(shares.sum(&:principal_minor_units)).to eq(10_000)
    expect(shares.sum(&:interest_minor_units)).to eq(777)
  end

  it 'splits principal and interest separately, not one total carved back up' do
    # A tiny principal alongside a large interest payment. Allocating the
    # combined total and splitting it back would hand the small holders a
    # principal share out of the interest money — above what they hold — and
    # their retirement leg would then be refused by the investment wallet,
    # stranding the Disbursement. Split separately, nobody can exceed
    # their holding.
    small_holders = [holds(1), holds(1)]
    holds(99_998)
    payment = repayment(principal: 3, interest: 99_999)

    shares = described_class.for(payment)
                            .to_h { |s| [s.investor_entity_id, s.principal_minor_units] }

    expect(shares.values.sum).to eq(3)
    small_holders.each { |investor| expect(shares.fetch(investor.id, 0)).to be <= 1 }
  end

  it 'never allocates a holder more principal than they hold' do
    alice = holds(60_000)
    holds(40_000)

    shares = described_class.for(repayment(principal: 100_000, interest: 0))

    alice_share = shares.find { |s| s.investor_entity_id == alice.id }
    expect(alice_share.principal_minor_units).to be <= 60_000
  end

  it 'shrinks a holder\'s share as their principal is repaid' do
    alice = holds(50_000)
    bob = holds(50_000)
    paid = repayment(principal: 50_000, interest: 0)
    disbursement = create(:disbursement, repayment: paid, investor: alice,
                                         principal_minor_units: 50_000, interest_minor_units: 1)
    create(:transaction_projection, aggregate_id: disbursement.id, state: 'completed')

    # Alice now holds nothing outstanding, so the next repayment is all Bob's.
    shares = described_class.for(repayment(principal: 50_000, interest: 0))

    expect(shares.map(&:investor_entity_id)).to eq([bob.id])
    expect(shares.first.principal_minor_units).to eq(50_000)
  end

  it 'drops a holder owed nothing, because there would be no leg to send them' do
    alice = holds(99_999)
    holds(1)

    # One minor unit of principal cannot reach the second holder.
    shares = described_class.for(repayment(principal: 1, interest: 0))

    expect(shares.map(&:investor_entity_id)).to eq([alice.id])
  end

  it 'refuses to allocate a repayment nobody can receive' do
    # The Borrower's money would sit in the repayment wallet with nobody owed
    # it. Unreachable through the services — a Repayment needs a drawn
    # Security, which needs a fully subscribed one — so it is worth saying so
    # rather than quietly disbursing nothing.
    expect { described_class.for(repayment(principal: 0, interest: 1)) }
      .to raise_error(Services::Securities::AllocationError, /allocated 0 of 1 interest/)
  end

  it 'orders shares by entity id, so the same repayment always splits the same way' do
    ids = [holds(30_000), holds(30_000), holds(40_000)].map(&:id)

    shares = described_class.for(repayment(principal: 10_000, interest: 1_000))

    expect(shares.map(&:investor_entity_id)).to eq(ids.sort)
  end
end
