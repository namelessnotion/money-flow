# frozen_string_literal: true

require 'spec_helper'
require_relative '../../../lib/lending_simulation'

RSpec.describe LendingSimulation::Quantiles do
  # A Random that always lands on the same point, so a spec can say where.
  def landing_at(point)
    Class.new(Random) do
      define_method(:rand) { |*| point }
    end.new
  end

  let(:tenths) { described_class.of([0, 10, 20, 30, 40, 50, 60, 70, 80, 90, 100]) }

  it 'interpolates between the deciles either side of a uniform draw' do
    random = landing_at(0.25)

    expect(tenths.sample(random)).to eq(25)
  end

  it 'reaches both ends of the table' do
    expect(tenths.sample(landing_at(0.0))).to eq(0)
    expect(tenths.sample(landing_at(0.999_999))).to eq(100)
  end

  it 'never leaves the table, over many draws' do
    random = Random.new(1)
    table = described_class.of([-365, -259, -169, -108, -81, -40, 9, 83, 172, 363, 540])

    draws = Array.new(2_000) { table.sample(random) }

    expect(draws.minmax.first).to be >= -365
    expect(draws.minmax.last).to be <= 540
  end

  it 'scales every decile, rounding to a whole unit' do
    expect(described_class.of([0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10]).scaled(2.5).deciles)
      .to eq([0, 3, 5, 8, 10, 13, 15, 18, 20, 23, 25])
  end

  it 'refuses a table that is not eleven ascending values' do
    expect { described_class.of([1, 2, 3]) }.to raise_error(ArgumentError, /eleven/)
    expect { described_class.of([0, 10, 20, 30, 40, 50, 60, 70, 80, 100, 90]) }
      .to raise_error(ArgumentError, /ascending/)
  end
end
