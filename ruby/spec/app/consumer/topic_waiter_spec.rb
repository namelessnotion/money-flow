# frozen_string_literal: true

require 'spec_helper'
require 'logger'
require 'stringio'

RSpec.describe Consumer::TopicWaiter do
  let(:now) { [0.0] }
  let(:clock) { -> { now.first } }
  let(:sleeper) { ->(seconds) { now[0] += seconds } }

  def waiter(answers)
    described_class.new(
      list_topics: -> { answers.shift.then { |answer| answer.is_a?(Exception) ? raise(answer) : answer } },
      logger: Logger.new(StringIO.new), clock: clock, sleeper: sleeper
    )
  end

  it 'returns as soon as every topic exists' do
    answers = [%w[transfer-events transaction-events other]]

    waiter(answers).wait_for(%w[transfer-events transaction-events])

    expect(now.first).to eq(0.0)
  end

  # Nothing publishes a topic until the connector has an event for it, so a
  # missing topic is normal at startup and is waited on indefinitely.
  it 'keeps polling while a topic is missing' do
    answers = [%w[transfer-events], %w[transfer-events], %w[transfer-events transaction-events]]

    waiter(answers).wait_for(%w[transfer-events transaction-events])

    expect(now.first).to eq(2 * described_class::POLL_INTERVAL_SECONDS)
  end

  it 'rides out brokers that are briefly unreachable' do
    answers = [StandardError.new('down'), StandardError.new('down'), %w[transfer-events]]

    expect { waiter(answers).wait_for(%w[transfer-events]) }.not_to raise_error
  end

  it 'gives up once no broker has answered for the unreachable budget' do
    attempts = (described_class::MAX_UNREACHABLE_SECONDS / described_class::POLL_INTERVAL_SECONDS).to_i + 2
    answers = Array.new(attempts) { StandardError.new('connection refused') }

    expect { waiter(answers).wait_for(%w[transfer-events]) }
      .to raise_error(described_class::Unreachable, /connection refused/)
  end
end
