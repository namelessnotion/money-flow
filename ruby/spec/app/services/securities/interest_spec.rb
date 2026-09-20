# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::Interest do
  it 'accrues a full year at the annual rate' do
    # $1,000.00 at 10.00% for 365 days is exactly $100.00.
    expect(described_class.accrued(principal_minor_units: 100_000, annual_rate_bps: 1000, days: 365))
      .to eq(10_000)
  end

  it 'accrues nothing on its first day' do
    expect(described_class.accrued(principal_minor_units: 100_000, annual_rate_bps: 1000, days: 0)).to eq(0)
  end

  it 'accrues actual days over a 365-day year' do
    # $1,000.00 at 10.00% for 37 days: 100000 * 1000 * 37 / (10000 * 365) = 1013.69...
    expect(described_class.accrued(principal_minor_units: 100_000, annual_rate_bps: 1000, days: 37))
      .to eq(1014)
  end

  it 'rounds once at the end rather than per day' do
    # Per-day integer division would truncate 30 times and land under by 30
    # minor units; one Rational rounded at the end does not.
    daily = described_class.accrued(principal_minor_units: 100_001, annual_rate_bps: 1, days: 1)
    expect(described_class.accrued(principal_minor_units: 100_001, annual_rate_bps: 1, days: 30))
      .not_to eq(daily * 30)
  end

  it 'rounds half away from zero, the way a servicer rounds a minor unit' do
    # 7300 * 1 * 1 / (10000 * 365) = 0.002 -> 0; 3_650_000 * 1 * 1 / 3_650_000 = 1.
    expect(described_class.accrued(principal_minor_units: 1_825_000, annual_rate_bps: 1, days: 1)).to eq(1)
  end

  it 'accrues nothing at a zero rate' do
    expect(described_class.accrued(principal_minor_units: 100_000, annual_rate_bps: 0, days: 365)).to eq(0)
  end

  it 'accrues nothing on nothing' do
    expect(described_class.accrued(principal_minor_units: 0, annual_rate_bps: 1000, days: 365)).to eq(0)
  end

  it 'refuses to accrue backwards' do
    expect { described_class.accrued(principal_minor_units: 100_000, annual_rate_bps: 1000, days: -1) }
      .to raise_error(Services::Securities::InvalidAmount, /days/)
  end

  it 'refuses a negative principal' do
    expect { described_class.accrued(principal_minor_units: -1, annual_rate_bps: 1000, days: 1) }
      .to raise_error(Services::Securities::InvalidAmount, /principal/)
  end

  it 'refuses a negative rate' do
    expect { described_class.accrued(principal_minor_units: 100_000, annual_rate_bps: -1, days: 1) }
      .to raise_error(Services::Securities::InvalidAmount, /rate/)
  end
end
