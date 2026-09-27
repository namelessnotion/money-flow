# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Jobs::RecoverReceivables do
  let(:sweep) { Services::Ach::RecoverDue.new(logger: Logger.new(StringIO.new)) }

  before { allow(Services::Ach::RecoverDue).to receive(:new).and_return(sweep) }

  def result(failed: {}, refused: {})
    Services::Ach::RecoverDue::Result.new(sent: [], refused: refused, failed: failed)
  end

  it 'runs on the ach queue' do
    expect(Resque.queue_from_class(described_class)).to eq(:ach)
  end

  it 'sweeps for what entities owe' do
    allow(sweep).to receive(:call).and_return(result)

    described_class.perform

    expect(sweep).to have_received(:call)
  end

  # A refusal is a stale view of cleared cash, which the next run waits out.
  it 'passes a run whose only trouble was a refusal' do
    allow(sweep).to receive(:call).and_return(result(refused: { 7 => 'insufficient Token capacity' }))

    expect { described_class.perform }.not_to raise_error
  end

  it 'fails the run when any entity could not be looked at' do
    allow(sweep).to receive(:call).and_return(result(failed: { 7 => 'Services::Ach::Unavailable: down' }))

    expect { described_class.perform }.to raise_error(described_class::Incomplete, /7.*Unavailable: down/)
  end
end
