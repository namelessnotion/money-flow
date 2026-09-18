# frozen_string_literal: true
# typed: strict

require 'base64'
require 'json'
require 'time'
require_relative 'event_body'

module Consumer
  # One published event as Debezium's outbox EventRouter puts it on
  # transfer-events / transaction-events (docs/cdc-tracer-bullet.md): the
  # aggregate id is the message key, and the value is schemaless JSON carrying
  # the routing fields and the event's protobuf bytes, base64-encoded.
  class Envelope < T::Struct
    # Anything that is not a well-formed published event. Never retryable: the
    # same bytes will fail the same way however often they are read.
    class MalformedMessage < StandardError; end

    const :aggregate_id, String
    const :aggregate_type, String
    const :event_type, String
    # Position within the aggregate's own stream — the monotonic guard's key.
    const :sequence, Integer
    # Position in Go's whole event log; kept for diagnostics.
    const :global_seq, Integer
    # The event's protobuf bytes, already base64-decoded.
    const :payload, String
    # When Go recorded the event (events.occurred_at). Nil for messages
    # published before the envelope carried it.
    const :occurred_at, T.nilable(Time), default: nil

    STRING_FIELDS = T.let(%w[payload aggregate_type event_type].freeze, T::Array[String])
    INTEGER_FIELDS = T.let(%w[sequence global_seq].freeze, T::Array[String])

    sig { params(key: T.nilable(String), value: T.nilable(String)).returns(Envelope) }
    def self.parse(key:, value:)
      raise MalformedMessage, 'message has no key; the key is the aggregate id' if key.nil? || key.empty?

      fields = routing_fields(value)
      new(
        aggregate_id: key,
        aggregate_type: fields.fetch('aggregate_type'),
        event_type: fields.fetch('event_type'),
        sequence: fields.fetch('sequence'),
        global_seq: fields.fetch('global_seq'),
        **decoded_fields(fields)
      )
    end

    # The fields that need decoding rather than a type check: the base64
    # payload and the ISO-8601 event time.
    sig { params(fields: T::Hash[String, T.untyped]).returns({ payload: String, occurred_at: T.nilable(Time) }) }
    def self.decoded_fields(fields)
      { payload: decode_payload(fields.fetch('payload')), occurred_at: parse_time(fields['occurred_at']) }
    end
    private_class_method :decoded_fields

    # The payload decoded as the generated class event_type names.
    sig { returns(EventBody) }
    def body
      descriptor = Google::Protobuf::DescriptorPool.generated_pool.lookup(event_type)
      unless descriptor.is_a?(Google::Protobuf::Descriptor)
        raise MalformedMessage, "no generated protobuf class for #{event_type}"
      end

      EventBody.new(descriptor.msgclass.decode(payload), descriptor)
    rescue Google::Protobuf::ParseError => e
      raise MalformedMessage, "payload is not a valid #{event_type}: #{e.message}"
    end

    # The JSON value's fields, each checked for presence and type. JSON.parse
    # returns whatever the document holds, hence the untyped values.
    sig { params(value: T.nilable(String)).returns(T::Hash[String, T.untyped]) }
    def self.routing_fields(value)
      fields = json_object(value)
      STRING_FIELDS.each { |name| require_field!(fields, name, String) }
      INTEGER_FIELDS.each { |name| require_field!(fields, name, Integer) }
      fields
    end
    private_class_method :routing_fields

    # Debezium publishes a timestamptz as an ISO-8601 string.
    sig { params(value: T.untyped).returns(T.nilable(Time)) }
    def self.parse_time(value)
      return nil if value.nil?
      raise MalformedMessage, "occurred_at must be an ISO-8601 string, got #{value.inspect}" unless value.is_a?(String)

      Time.iso8601(value)
    rescue ArgumentError
      raise MalformedMessage, "occurred_at is not ISO-8601: #{value.inspect}"
    end
    private_class_method :parse_time

    sig { params(encoded: String).returns(String) }
    def self.decode_payload(encoded)
      Base64.strict_decode64(encoded)
    rescue ArgumentError => e
      raise MalformedMessage, "payload is not base64: #{e.message}"
    end
    private_class_method :decode_payload

    sig { params(value: T.nilable(String)).returns(T::Hash[String, T.untyped]) }
    def self.json_object(value)
      parsed = JSON.parse(value.to_s)
      raise MalformedMessage, 'value is not a JSON object' unless parsed.is_a?(Hash)

      parsed
    rescue JSON::ParserError => e
      raise MalformedMessage, "value is not JSON: #{e.message}"
    end
    private_class_method :json_object

    sig { params(fields: T::Hash[String, T.untyped], name: String, type: T::Class[T.anything]).void }
    def self.require_field!(fields, name, type)
      return if fields[name].is_a?(type)

      raise MalformedMessage, "#{name} must be a #{type}, got #{fields[name].inspect}"
    end
    private_class_method :require_field!
  end
end
