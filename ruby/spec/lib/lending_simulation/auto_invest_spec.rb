# frozen_string_literal: true

require 'spec_helper'
require_relative '../../../lib/lending_simulation'

RSpec.describe LendingSimulation::AutoInvest do
  let(:tens) { LendingSimulation::Quantiles.of([1_000] * 11) }

  def offering(security_id, remaining)
    LendingSimulation::AutoInvest::Offering.new(security_id: security_id, remaining_minor_units: remaining)
  end

  def plan(offerings:, cash:, buyers: 10, seed: 1)
    described_class.plan(offerings: offerings, cash: cash, position: tens, buyers_per_offering: buyers,
                         random: Random.new(seed))
  end

  def total_for(purchases, security_id)
    purchases.select { |p| p.security_id == security_id }.sum(&:amount_minor_units)
  end

  it 'buys a position of the drawn size into an open offering' do
    purchases = plan(offerings: [offering('s1', 50_000)], cash: { 1 => 9_000 })

    expect(purchases.map { |p| [p.investor_entity_id, p.security_id, p.amount_minor_units] })
      .to eq([[1, 's1', 1_000]])
  end

  it 'has the last buyer take exactly what remains, so the offering fills' do
    purchases = plan(offerings: [offering('s1', 2_500)],
                     cash: { 1 => 9_000, 2 => 9_000, 3 => 9_000 })

    expect(total_for(purchases, 's1')).to eq(2_500)
    expect(purchases.map(&:amount_minor_units).sort).to eq([500, 1_000, 1_000])
  end

  it 'never spends more than an Investor has' do
    purchases = plan(offerings: [offering('s1', 50_000),
                                 offering('s2', 50_000)],
                     cash: { 1 => 1_500 })

    expect(purchases.sum(&:amount_minor_units)).to eq(1_500)
  end

  it 'lets at most the given number of Investors into one offering a day' do
    cash = (1..20).to_h { |id| [id, 9_000] }

    purchases = plan(offerings: [offering('s1', 1_000_000)],
                     cash: cash, buyers: 4)

    expect(purchases.map(&:investor_entity_id).uniq.size).to eq(4)
  end

  it 'fills offerings oldest first' do
    purchases = plan(offerings: [offering('old', 1_000),
                                 offering('new', 1_000)],
                     cash: { 1 => 1_000 })

    expect(purchases.map(&:security_id)).to eq(['old'])
  end

  it 'plans the same purchases for the same seed' do
    offerings = [offering('s1', 30_000)]
    cash = (1..8).to_h { |id| [id, 5_000] }

    first, second = Array.new(2) { plan(offerings: offerings, cash: cash, seed: 9).map(&:serialize) }

    expect(first).to eq(second)
  end
end
