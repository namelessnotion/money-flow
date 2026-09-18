# frozen_string_literal: true

require 'spec_helper'
require 'logger'
require 'stringio'

RSpec.describe Consumer::Runner do
  subject(:runner) do
    described_class.new(
      source: source, projector: projector, logger: Logger.new(StringIO.new), sleeper: ->(s) { slept << s }
    )
  end

  let(:source) { FakeSource.new(messages) }
  let(:projector) { Consumer::Projector.new }
  let(:slept) { [] }
  let(:transaction_id) { SecureRandom.uuid_v7 }

  def message(event, sequence:, offset:)
    raw = kafka_message_for(event, aggregate_id: transaction_id, sequence: sequence)
    Consumer::Message.new(topic: 'transaction-events', partition: 0, offset: offset, key: raw[:key], value: raw[:value])
  end

  def initialized = message(Transaction::V1::TransactionInitialized.new(id: transaction_id), sequence: 1, offset: 10)
  def started = message(Transaction::V1::TransactionStarted.new(id: transaction_id), sequence: 2, offset: 11)

  context 'when every message projects' do
    let(:messages) { [initialized, started] }

    it 'projects each message and commits its offset after it' do
      runner.run

      expect(Models::TransactionProjection[transaction_id].state).to eq('started')
      expect(source.committed).to eq([10, 11])
    end
  end

  context 'when a message is not a well-formed event' do
    let(:messages) do
      garbage = Consumer::Message.new(topic: 'transaction-events', partition: 0, offset: 11, key: nil, value: '{}')
      [initialized, garbage, started]
    end

    it 'halts at once without committing it or reading past it' do
      expect { runner.run }.to raise_error(described_class::Halted, /transaction-events\[0\]@11/)

      expect(source.committed).to eq([10])
      expect(slept).to be_empty
    end
  end

  context 'when an event has no mapping' do
    let(:messages) do
      unmapped = Consumer::Message.new(
        topic: 'transaction-events', partition: 0, offset: 11, key: transaction_id,
        value: { payload: '', aggregate_type: 'transaction', event_type: 'transaction.v1.New',
                 sequence: 2, global_seq: 2 }.to_json
      )
      [initialized, unmapped]
    end

    it 'halts at once rather than skipping it' do
      expect { runner.run }.to raise_error(described_class::Halted) do |error|
        expect(error.cause).to be_a(Consumer::EventStateMap::UnmappedEvent)
      end
      expect(source.committed).to eq([10])
    end
  end

  context 'when projecting fails transiently' do
    let(:messages) { [initialized] }

    it 'retries with doubling backoff and commits once it succeeds' do
      attempts = 0
      allow(projector).to receive(:apply).and_wrap_original do |original, envelope|
        attempts += 1
        raise Sequel::DatabaseConnectionError, 'gone' if attempts < 3

        original.call(envelope)
      end

      runner.run

      expect(slept).to eq([0.25, 0.5])
      expect(source.committed).to eq([10])
    end
  end

  context 'when projecting keeps failing' do
    let(:messages) { [initialized, started] }

    # go/docs/adr/0003: a message that cannot be processed stops the consumer.
    # Skipping it would leave the read model silently wrong; not committing it
    # means a restart picks up exactly where this left off.
    it 'halts after the attempt limit without committing' do
      allow(projector).to receive(:apply).and_raise(Sequel::DatabaseConnectionError, 'gone')

      expect { runner.run }.to raise_error(described_class::Halted, /gone/)
      expect(projector).to have_received(:apply).exactly(described_class::MAX_ATTEMPTS).times
      expect(source.committed).to be_empty
    end
  end
end
