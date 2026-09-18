# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Ach::BusinessCalendar do
  def date(iso) = Date.iso8601(iso)

  describe '.business_day?' do
    it 'is false on weekends' do
      expect(described_class.business_day?(date('2026-09-19'))).to be false # Saturday
      expect(described_class.business_day?(date('2026-09-20'))).to be false # Sunday
    end

    it 'is false on a Federal Reserve holiday' do
      expect(described_class.business_day?(date('2026-11-26'))).to be false # Thanksgiving
    end

    # The Fed does not close the Friday before a Saturday holiday, unlike
    # federal offices — the case a generic US calendar gets wrong.
    it 'is true on the Friday before a Saturday holiday' do
      expect(described_class.business_day?(date('2026-07-03'))).to be true # July 4 is a Saturday
    end

    it 'is false on the Monday after a Sunday holiday' do
      expect(described_class.business_day?(date('2027-07-05'))).to be false # July 4 is a Sunday
    end
  end

  describe '.add_business_days' do
    it 'counts weekdays forward' do
      expect(described_class.add_business_days(date('2026-09-14'), 3)).to eq(date('2026-09-17')) # Mon -> Thu
    end

    it 'skips a weekend' do
      expect(described_class.add_business_days(date('2026-09-17'), 3)).to eq(date('2026-09-22')) # Thu -> Tue
    end

    it 'skips a holiday' do
      # Tue -> Mon, over Thanksgiving and the weekend
      expect(described_class.add_business_days(date('2026-11-24'), 3)).to eq(date('2026-11-30'))
    end

    it 'counts from a non-business day as if from the next one' do
      expect(described_class.add_business_days(date('2026-09-19'), 3)).to eq(date('2026-09-24')) # Sat, as Mon -> Thu
    end
  end

  describe '.date_of' do
    # The Fed keeps Eastern time: 02:00 UTC on Tuesday is still Monday there.
    it 'reads the calendar date in Eastern time' do
      expect(described_class.date_of(Time.utc(2026, 9, 15, 2, 0))).to eq(date('2026-09-14'))
    end
  end
end
