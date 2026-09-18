# frozen_string_literal: true

# Builds Consumer::Envelopes the way Debezium publishes them, from real
# generated protobuf messages, so projector and runner specs exercise the same
# decoding path as a message off the topic.
module PublishedEvents
  def envelope_for(message, aggregate_id:, sequence:, global_seq: sequence, occurred_at: nil)
    event_type = message.class.descriptor.name
    Consumer::Envelope.new(
      aggregate_id: aggregate_id,
      aggregate_type: event_type.split('.').first,
      event_type: event_type,
      sequence: sequence,
      global_seq: global_seq,
      payload: message.class.encode(message),
      occurred_at: occurred_at
    )
  end

  # The raw Kafka key and value for the same event.
  def kafka_message_for(message, aggregate_id:, sequence:, global_seq: sequence)
    envelope = envelope_for(message, aggregate_id: aggregate_id, sequence: sequence, global_seq: global_seq)
    value = {
      payload: Base64.strict_encode64(envelope.payload),
      aggregate_type: envelope.aggregate_type,
      event_type: envelope.event_type,
      sequence: sequence,
      global_seq: global_seq
    }.to_json
    { key: aggregate_id, value: value }
  end
end
