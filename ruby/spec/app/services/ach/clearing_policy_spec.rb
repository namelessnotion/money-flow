# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Ach::ClearingPolicy do
  # Completed Monday 2026-09-14, 11:00 Eastern.
  let(:completed_at) { Time.utc(2026, 9, 14, 15, 0) }

  it 'is due on the third business day after completion' do
    expect(described_class.due_on(completed_at)).to eq(Date.new(2026, 9, 17))
  end

  it 'is not due before that day' do
    expect(described_class.due?(completed_at, now: Time.utc(2026, 9, 17, 3, 59))).to be false # Wed 23:59 ET
  end

  it 'is due from the start of that day, Eastern time' do
    expect(described_class.due?(completed_at, now: Time.utc(2026, 9, 17, 4, 0))).to be true # Thu 00:00 ET
  end

  it 'stays due afterwards, so a missed sweep catches up' do
    expect(described_class.due?(completed_at, now: Time.utc(2026, 10, 1))).to be true
  end
end
