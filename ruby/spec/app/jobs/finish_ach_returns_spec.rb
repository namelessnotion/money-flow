# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Jobs::FinishAchReturns do
  let(:sweep) { Services::Ach::ReturnDue.new(logger: Logger.new(StringIO.new)) }

  before { allow(Services::Ach::ReturnDue).to receive(:new).and_return(sweep) }

  it 'runs on the ach queue' do
    expect(Resque.queue_from_class(described_class)).to eq(:ach)
  end

  it 'sweeps for return notices not yet seen through' do
    allow(sweep).to receive(:call).and_return(Services::Ach::ReturnDue::Result.new(acted_on: ['a'], failed: {}))

    described_class.perform

    expect(sweep).to have_received(:call)
  end

  it 'fails the run when any notice could not be acted on' do
    failed = Services::Ach::ReturnDue::Result.new(acted_on: [], failed: { 'abc' => 'Services::Ach::Refused: no' })
    allow(sweep).to receive(:call).and_return(failed)

    expect { described_class.perform }.to raise_error(described_class::Incomplete, /abc.*Refused: no/)
  end
end
