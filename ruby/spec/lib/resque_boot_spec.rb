# frozen_string_literal: true

require 'spec_helper'
require 'fugit'
require_relative '../../lib/resque_boot'

RSpec.describe ResqueBoot do
  let(:schedule) { described_class.schedule }

  it 'schedules the ACH clearing sweep hourly on weekdays, Eastern time' do
    entry = schedule.fetch('clear_ach_deposits')

    expect(entry.fetch('class')).to eq('Jobs::ClearAchDeposits')
    expect(entry.fetch('cron')).to eq('0 * * * 1-5 America/New_York')
  end

  # A typo here fails silently at runtime: the scheduler logs and never runs it.
  it 'names only job classes that exist, on the queue each job declares, with cron it can parse' do
    schedule.each_value do |entry|
      job = Object.const_get(entry.fetch('class'))
      expect(entry.fetch('queue')).to eq(Resque.queue_from_class(job).to_s)
      expect(Fugit.parse_cron(entry.fetch('cron'))).to be_a(Fugit::Cron)
    end
  end

  it 'connects to Redis at REDIS_URL' do
    expect(described_class.redis_url({ 'REDIS_URL' => 'redis://redis:6379/3' })).to eq('redis://redis:6379/3')
    expect(described_class.redis_url({})).to eq('redis://localhost:6379/0')
  end
end
