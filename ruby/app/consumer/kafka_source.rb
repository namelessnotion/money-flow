# frozen_string_literal: true
# typed: strict

require 'logger'
require 'rdkafka'
require_relative 'message'
require_relative 'source'
require_relative 'topic_waiter'

module Consumer
  # Reads transfer-events and transaction-events through librdkafka, as the
  # consumer group money-flow-ruby-projection — its own group, so its offsets
  # are independent of the Go orchestrator's money-flow-saga-* groups.
  #
  # Offsets are committed only by #commit, synchronously, after the Runner has
  # projected a message: nothing is committed by a timer, so a crash never
  # loses a message it had not finished with.
  class KafkaSource
    include Source

    TRANSFER_TOPIC = 'transfer-events'
    TRANSACTION_TOPIC = 'transaction-events'
    TOPICS = T.let([TRANSFER_TOPIC, TRANSACTION_TOPIC].freeze, T::Array[String])
    GROUP_ID = 'money-flow-ruby-projection'
    POLL_TIMEOUT_MS = 1_000
    METADATA_TIMEOUT_MS = 5_000

    sig { params(brokers: String, logger: ::Logger).returns(KafkaSource) }
    def self.connect(brokers:, logger:)
      config = Rdkafka::Config.new(
        'bootstrap.servers': brokers,
        'group.id': GROUP_ID,
        'enable.auto.commit': false,
        'enable.auto.offset.store': false,
        # A new group starts at the beginning: the read model must see every
        # event, not only those published after it first ran.
        'auto.offset.reset': 'earliest',
        'allow.auto.create.topics': false
      )
      new(consumer: config.consumer, logger: logger)
    end

    sig { params(consumer: Rdkafka::Consumer, logger: ::Logger).void }
    def initialize(consumer:, logger:)
      @consumer = consumer
      @logger = logger
      @closing = T.let(false, T::Boolean)
    end

    # Waits for the topics to exist, then joins the group. Subscribing to a
    # topic that doesn't exist yet would only fail each poll until it did.
    sig { void }
    def start
      TopicWaiter.new(list_topics: -> { topic_names }, logger: @logger).wait_for(TOPICS)
      @consumer.subscribe(TRANSFER_TOPIC, TRANSACTION_TOPIC)
      @logger.info("subscribed to #{TOPICS.join(', ')} as #{GROUP_ID}")
    end

    sig { override.returns(T.nilable(Message)) }
    def next_message
      until @closing
        raw = @consumer.poll(POLL_TIMEOUT_MS)
        return to_message(raw) if raw
      end
      nil
    end

    # Commits the offset after `message`, so a restart resumes with the next
    # one. Synchronous: when this returns, the progress is durable.
    sig { override.params(message: Message).void }
    def commit(message)
      list = Rdkafka::Consumer::TopicPartitionList.new
      list.add_topic_and_partitions_with_offsets(message.topic, { message.partition => message.offset + 1 })
      @consumer.commit(list, false)
    end

    # Asks #next_message to return nil at its next poll timeout. Only sets a
    # flag, so it is safe to call from a signal handler.
    sig { void }
    def stop
      @closing = true
    end

    # Leaves the group. Call once the Runner has returned.
    sig { void }
    def close
      @consumer.close
    end

    private

    # Every topic in the cluster. Deliberately not a per-topic request, which
    # can create the topic it asks about.
    sig { returns(T::Array[String]) }
    def topic_names
      topics = T.let(@consumer.metadata(nil, METADATA_TIMEOUT_MS).topics, T::Array[T::Hash[Symbol, T.untyped]])
      topics.map { |topic| topic.fetch(:topic_name).to_s }
    end

    sig { params(raw: Rdkafka::Consumer::Message).returns(Message) }
    def to_message(raw)
      Message.new(topic: raw.topic, partition: raw.partition, offset: raw.offset, key: raw.key, value: raw.payload)
    end
  end
end
