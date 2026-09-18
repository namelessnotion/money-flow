# frozen_string_literal: true
# typed: strict

require_relative 'message'

module Consumer
  # Where the Runner reads messages from and records its progress. Kafka in
  # production (KafkaSource); an in-memory list in specs.
  module Source
    extend T::Helpers

    interface!

    # The next message in order, waiting for one to arrive. Nil once the
    # source is closed: there is nothing more to consume.
    sig { abstract.returns(T.nilable(Message)) }
    def next_message; end

    # Records `message` as processed: after a restart, reading resumes after it.
    sig { abstract.params(message: Message).void }
    def commit(message); end
  end
end
