# frozen_string_literal: true
# typed: strict

require 'logger'

module Consumer
  # Waits until the topics a consumer is about to subscribe to exist.
  #
  # A topic exists only once the connector has published an event to it, so a
  # missing topic is normal at startup and is waited on indefinitely. A broker
  # that stops answering is not: once none has answered for
  # MAX_UNREACHABLE_SECONDS, the wait gives up rather than hang forever.
  #
  # `list_topics` must only *read* cluster metadata. A metadata request for a
  # named topic can create it when auto-creation is on — the bug e52ceec fixed
  # in the Go reader — so KafkaSource asks for all topics instead, which never
  # creates one, and bounds each ask with its own timeout (0144abc).
  class TopicWaiter
    class Unreachable < StandardError; end

    POLL_INTERVAL_SECONDS = 2.0
    MAX_UNREACHABLE_SECONDS = 60.0

    sig do
      params(
        list_topics: T.proc.returns(T::Array[String]),
        logger: ::Logger,
        clock: T.proc.returns(Float),
        sleeper: T.proc.params(seconds: Float).void
      ).void
    end
    def initialize(list_topics:, logger:, clock: -> { Process.clock_gettime(Process::CLOCK_MONOTONIC).to_f },
                   sleeper: ->(seconds) { sleep(seconds) })
      @list_topics = list_topics
      @logger = logger
      @clock = clock
      @sleeper = sleeper
    end

    sig { params(topics: T::Array[String]).void }
    def wait_for(topics)
      last_answer = @clock.call
      until (missing = missing_topics(topics))&.empty?
        last_answer = note(missing, last_answer)
        @sleeper.call(POLL_INTERVAL_SECONDS)
      end
    end

    private

    # The topics not yet in the cluster, or nil when no broker answered.
    sig { params(topics: T::Array[String]).returns(T.nilable(T::Array[String])) }
    def missing_topics(topics)
      topics - @list_topics.call
    rescue StandardError => e
      @last_error = T.let(e, T.nilable(StandardError))
      @logger.warn("could not list topics: #{e.message}")
      nil
    end

    # Logs what a check found, and returns when a broker last answered. Gives
    # up once none has for MAX_UNREACHABLE_SECONDS.
    sig { params(missing: T.nilable(T::Array[String]), last_answer: Float).returns(Float) }
    def note(missing, last_answer)
      if missing.nil?
        give_up! if @clock.call - last_answer >= MAX_UNREACHABLE_SECONDS
        return last_answer
      end

      @logger.info("waiting for topics: #{missing.join(', ')}")
      @clock.call
    end

    sig { returns(T.noreturn) }
    def give_up!
      raise Unreachable, "no broker answered for #{MAX_UNREACHABLE_SECONDS}s: #{@last_error&.message}"
    end
  end
end
