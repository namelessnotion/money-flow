# frozen_string_literal: true

require 'spec_helper'
require_relative '../../../lib/lending_simulation'

RSpec.describe LendingSimulation::Profile do
  let(:profile) { described_class.load(described_class::DEFAULT_PATH) }

  it 'loads the committed Groundfloor-like profile' do
    expect(profile.grades.map(&:name)).to eq(%w[A B C D E])
    expect(profile.term_months.keys).to include(12, 15)
  end

  it 'scales loan and position sizes as the profile says' do
    # $191,100.00 at the median, scaled by 0.01; $10.00 at the median, by 10.
    expect(profile.loan_amount.deciles.fetch(5)).to eq(191_100)
    expect(profile.position.deciles.fetch(5)).to eq(10_000)
  end

  describe 'drawing a loan' do
    it 'is the same loan for the same seed' do
      first, second = Array.new(2) { profile.loan(Random.new(42)).serialize }

      expect(first).to eq(second)
    end

    it 'stays inside what the profile allows, over many draws' do
      random = Random.new(7)
      loans = Array.new(500) { profile.loan(random) }

      expect(loans.map(&:grade)).to all(satisfy { |grade| %w[A B C D E].include?(grade) })
      expect(loans.map(&:annual_rate_bps)).to all(be_between(550, 1650))
      expect(loans.map(&:principal_minor_units)).to all(be_between(31_000, 1_000_000))
      expect(loans.map(&:term_days)).to all(satisfy { |days| [183, 274, 365, 456, 548, 639].include?(days) })
    end

    it 'prices a loan in whole dollars' do
      expect(profile.loan(Random.new(3)).principal_minor_units % 100).to eq(0)
    end
  end

  describe 'when a loan pays off' do
    def terms(term_days:, payoff_offset_days:)
      LendingSimulation::Profile::LoanTerms.new(principal_minor_units: 100_000, grade: 'C', annual_rate_bps: 1100,
                                                term_days: term_days, payoff_offset_days: payoff_offset_days)
    end

    it 'is its term after the Draw, moved by the payoff offset' do
      expect(terms(term_days: 365, payoff_offset_days: -40).days_to_payoff(minimum: 30)).to eq(325)
      expect(terms(term_days: 365, payoff_offset_days: 172).days_to_payoff(minimum: 30)).to eq(537)
    end

    it 'is never sooner than the minimum, however early the offset' do
      expect(terms(term_days: 183, payoff_offset_days: -365).days_to_payoff(minimum: 30)).to eq(30)
    end
  end
end
