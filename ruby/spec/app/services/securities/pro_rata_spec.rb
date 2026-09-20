# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::ProRata do
  it 'splits in proportion when the split is exact' do
    expect(described_class.allocate(total: 900, weights: { 1 => 100, 2 => 200 }))
      .to eq({ 1 => 300, 2 => 600 })
  end

  it 'hands the units left over to the largest remainders' do
    # 100 over 3 equal holders: each floors to 33, and the 1 left over goes to
    # the first by the tie-break below.
    expect(described_class.allocate(total: 100, weights: { 1 => 1, 2 => 1, 3 => 1 }))
      .to eq({ 1 => 34, 2 => 33, 3 => 33 })
  end

  it 'gives a remainder to the larger fraction before the lower key' do
    # 10 over weights 1:2:2 -> 2.0, 4.0, 4.0 exact; shift to 11 -> 2.2, 4.4, 4.4
    # floors to 2, 4, 4 with 1 unit left and remainders .2, .4, .4 — key 2 wins
    # on fraction, not key 1 on order.
    expect(described_class.allocate(total: 11, weights: { 1 => 1, 2 => 2, 3 => 2 }))
      .to eq({ 1 => 2, 2 => 5, 3 => 4 })
  end

  it 'breaks a tie on the lower key, so the same inputs always split the same way' do
    expect(described_class.allocate(total: 2, weights: { 9 => 1, 3 => 1, 7 => 1 }))
      .to eq({ 3 => 1, 7 => 1, 9 => 0 })
  end

  it 'gives a zero weight nothing' do
    expect(described_class.allocate(total: 100, weights: { 1 => 1, 2 => 0 }))
      .to eq({ 1 => 100, 2 => 0 })
  end

  it 'gives a single holder everything' do
    expect(described_class.allocate(total: 7, weights: { 42 => 3 })).to eq({ 42 => 7 })
  end

  it 'splits nothing into nothing' do
    expect(described_class.allocate(total: 0, weights: { 1 => 1, 2 => 2 })).to eq({ 1 => 0, 2 => 0 })
  end

  it 'has nothing to split across nobody' do
    expect(described_class.allocate(total: 0, weights: {})).to eq({})
  end

  it 'refuses to split something across nobody' do
    expect { described_class.allocate(total: 1, weights: {}) }
      .to raise_error(Services::Securities::AllocationError, /no proportion/)
  end

  it 'refuses to split across weights that are all zero' do
    expect { described_class.allocate(total: 100, weights: { 1 => 0, 2 => 0 }) }
      .to raise_error(Services::Securities::AllocationError, /no proportion/)
  end

  it 'refuses a negative total' do
    expect { described_class.allocate(total: -1, weights: { 1 => 1 }) }
      .to raise_error(Services::Securities::AllocationError, /total/)
  end

  it 'refuses a negative weight' do
    expect { described_class.allocate(total: 100, weights: { 1 => -1, 2 => 2 }) }
      .to raise_error(Services::Securities::AllocationError, /weight/)
  end

  describe 'the invariant it exists to keep' do
    # The parts must sum to exactly the total, for every shape of input. A
    # rounding rule that loses a minor unit here is a Disbursement that can
    # never be reconciled against its Repayment, so this is checked over
    # randomised inputs rather than a handful of chosen ones. Seeded by RSpec's
    # own seed, so a failure reproduces with --seed.
    it 'always allocates exactly the total' do
      random = Random.new(RSpec.configuration.seed)

      200.times do
        weights = Array.new(random.rand(1..12)) { [random.rand(1..500), random.rand(0..1_000_000)] }.to_h
        next if weights.each_value.sum.zero?

        total = random.rand(0..10_000_000)
        parts = described_class.allocate(total: total, weights: weights)

        expect(parts.each_value.sum).to eq(total)
        expect(parts.keys).to match_array(weights.keys)
      end
    end

    it 'gives no holder more than one unit above their exact share' do
      parts = described_class.allocate(total: 1_000_003, weights: { 1 => 1, 2 => 1, 3 => 1, 4 => 1, 5 => 1 })

      expect(parts.each_value.max - parts.each_value.min).to eq(1)
    end

    it 'is a function of its inputs alone, because the Transaction id is derived and the amount is not' do
      # The same holders in a different insertion order: a sweep re-running
      # later reads its positions back from a fresh query, so the result must
      # not depend on the order they arrived in.
      in_one_order = described_class.allocate(total: 99_991, weights: { 11 => 317, 22 => 101, 33 => 582 })
      in_another = described_class.allocate(total: 99_991, weights: { 33 => 582, 11 => 317, 22 => 101 })

      expect(in_one_order).to eq(in_another)
      expect(in_one_order.each_value.sum).to eq(99_991)
    end
  end
end
