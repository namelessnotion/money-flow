# frozen_string_literal: true

require 'spec_helper'
require 'base64'
require 'json'

RSpec.describe Consumer::Envelope do
  let(:transfer_id) { SecureRandom.uuid_v7 }
  let(:transaction_id) { SecureRandom.uuid_v7 }
  let(:accepted) { Transfer::V1::TransferRequestAccepted.new(id: transfer_id, transaction_id: transaction_id) }

  # The shape Debezium's outbox EventRouter publishes (docs/cdc-tracer-bullet.md):
  # the aggregate id as the key, and a schemaless JSON value whose payload is
  # the base64 of the event's protobuf bytes.
  def value_for(message, aggregate_type: 'transfer', sequence: 1, global_seq: 7)
    {
      payload: Base64.strict_encode64(message.class.encode(message)),
      aggregate_type: aggregate_type,
      event_type: message.class.descriptor.name,
      sequence: sequence,
      global_seq: global_seq
    }.to_json
  end

  describe '.parse' do
    it 'reads the routing fields and takes the aggregate id from the key' do
      envelope = described_class.parse(key: transfer_id, value: value_for(accepted, sequence: 3, global_seq: 9))

      expect(envelope.aggregate_id).to eq(transfer_id)
      expect(envelope.aggregate_type).to eq('transfer')
      expect(envelope.event_type).to eq('transfer.v1.TransferRequestAccepted')
      expect(envelope.sequence).to eq(3)
      expect(envelope.global_seq).to eq(9)
    end

    it 'parses the exact message recorded by the CDC tracer bullet' do
      value = '{"payload":"CiQwMWEwN2VmOC0zNDMxLTcxZGUtYWMyOS1lNmYwMzQ3MzJkYjY=","aggregate_type":"transfer",' \
              '"event_type":"transfer.v1.TransferRequestAccepted","sequence":1,"global_seq":1}'

      envelope = described_class.parse(key: '01a07ef8-3431-71de-ac29-e6f034732db6', value: value)

      expect(envelope.body.string('id')).to eq('01a07ef8-3431-71de-ac29-e6f034732db6')
    end

    it 'rejects a message with no key' do
      expect { described_class.parse(key: nil, value: value_for(accepted)) }
        .to raise_error(described_class::MalformedMessage, /key/)
    end

    it 'rejects a value that is not JSON' do
      expect { described_class.parse(key: transfer_id, value: 'not json') }
        .to raise_error(described_class::MalformedMessage)
    end

    it 'rejects a value missing a routing field' do
      value = JSON.parse(value_for(accepted)).except('sequence').to_json

      expect { described_class.parse(key: transfer_id, value: value) }
        .to raise_error(described_class::MalformedMessage, /sequence/)
    end

    # Debezium publishes events.occurred_at (timestamptz) as an ISO-8601 string.
    it 'reads the event time Go recorded' do
      value = JSON.parse(value_for(accepted)).merge('occurred_at' => '2026-09-18T14:52:36.123456Z').to_json

      envelope = described_class.parse(key: transfer_id, value: value)

      expect(envelope.occurred_at).to eq(Time.utc(2026, 9, 18, 14, 52, 36.123456r))
    end

    it 'has no event time for a message published before the envelope carried one' do
      expect(described_class.parse(key: transfer_id, value: value_for(accepted)).occurred_at).to be_nil
    end

    it 'rejects an event time that is not ISO-8601' do
      value = JSON.parse(value_for(accepted)).merge('occurred_at' => 'yesterday').to_json

      expect { described_class.parse(key: transfer_id, value: value) }
        .to raise_error(described_class::MalformedMessage, /occurred_at/)
    end

    it 'rejects a sequence that is not an integer' do
      value = JSON.parse(value_for(accepted)).merge('sequence' => '1').to_json

      expect { described_class.parse(key: transfer_id, value: value) }
        .to raise_error(described_class::MalformedMessage, /sequence/)
    end
  end

  describe '#body' do
    it 'decodes the payload as the class named by event_type' do
      envelope = described_class.parse(key: transfer_id, value: value_for(accepted))

      expect(envelope.body.string('transaction_id')).to eq(transaction_id)
    end

    it 'reads a field the event does not have as nil' do
      envelope = described_class.parse(key: transfer_id, value: value_for(accepted))

      expect(envelope.body.string('reason')).to be_nil
    end

    it 'reads an empty proto string as nil' do
      unowned = Transfer::V1::TransferRequestAccepted.new(id: transfer_id)
      envelope = described_class.parse(key: transfer_id, value: value_for(unowned))

      expect(envelope.body.string('transaction_id')).to be_nil
    end

    it 'fails on an event type with no generated class' do
      value = JSON.parse(value_for(accepted)).merge('event_type' => 'transfer.v1.Nope').to_json
      envelope = described_class.parse(key: transfer_id, value: value)

      expect { envelope.body }.to raise_error(described_class::MalformedMessage, /transfer\.v1\.Nope/)
    end
  end
end
