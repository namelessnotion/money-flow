# frozen_string_literal: true
# typed: strict

module Consumer
  # A decoded event payload, read by proto field name.
  #
  # The projection needs only a couple of body fields (a reason, the owning
  # transaction_id) and reads them the same way across many event types, so
  # it asks by field name through the message's descriptor rather than
  # switching over generated classes.
  class EventBody
    # `message` is an instance of a generated protobuf class. Those classes are
    # built at load time by constant assignment from the descriptor pool, so
    # Sorbet sees a value, not a class, and there is no type to name here.
    sig { params(message: T.untyped, descriptor: Google::Protobuf::Descriptor).void }
    def initialize(message, descriptor)
      @message = message
      @descriptor = descriptor
    end

    # The value of the string field `name`, or nil when the event has no such
    # field or leaves it empty — proto3 cannot tell an unset string from "".
    sig { params(name: String).returns(T.nilable(String)) }
    def string(name)
      field = T.let(@descriptor.lookup(name), T.nilable(Google::Protobuf::FieldDescriptor))
      return nil if field.nil?

      value = field.get(@message)
      value.is_a?(String) && !value.empty? ? value : nil
    end
  end
end
