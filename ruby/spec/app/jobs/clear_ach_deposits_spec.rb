# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Jobs::ClearAchDeposits do
  let(:sweep) { Services::Ach::ClearDue.new(logger: Logger.new(StringIO.new)) }

  before { allow(Services::Ach::ClearDue).to receive(:new).and_return(sweep) }

  it 'runs on the ach queue' do
    expect(Resque.queue_from_class(described_class)).to eq(:ach)
  end

  it 'sweeps for due deposits' do
    allow(sweep).to receive(:call).and_return(Services::Ach::ClearDue::Result.new(cleared: ['a'], failed: {}))

    described_class.perform

    expect(sweep).to have_received(:call)
  end

  # Raising lands the run in Resque's failed queue, where a person sees it —
  # after every other due deposit has already been cleared.
  it 'fails the run when any deposit could not be cleared' do
    failed = Services::Ach::ClearDue::Result.new(cleared: [], failed: { 'abc' => 'Services::Ach::Refused: no' })
    allow(sweep).to receive(:call).and_return(failed)

    expect { described_class.perform }.to raise_error(described_class::Incomplete, /abc.*Refused: no/)
  end
end
