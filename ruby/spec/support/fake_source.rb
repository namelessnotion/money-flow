# frozen_string_literal: true

# Stands in for Kafka in Runner specs: hands out its messages in order and
# records which offsets were committed.
class FakeSource
  include Consumer::Source

  attr_reader :committed

  def initialize(messages)
    @messages = messages.dup
    @committed = []
  end

  def next_message = @messages.shift

  def commit(message) = @committed << message.offset
end
