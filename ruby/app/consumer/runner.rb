# frozen_string_literal: true
# typed: strict

require 'logger'
require_relative 'envelope'
require_relative 'projector'
require_relative 'source'

module Consumer
  # Drives the projection: for each message, parse it, project it, and only
  # then commit its offset — at-least-once, which the Projector's monotonic
  # guard makes safe to repeat.
  #
  # The failure policy is go/docs/adr/0003's: a transient failure is retried
  # with backoff; a message that still cannot be projected halts the consumer
  # without committing it. Skipping it would leave the read model quietly
  # wrong, and a halted consumer resumes from exactly that message on restart.
  # A message that can never succeed — malformed, or an event with no mapping
  # — halts at once.
  class Runner
    # Raised to stop consuming. The underlying failure is its `cause`.
    class Halted < StandardError; end

    MAX_ATTEMPTS = 3
    INITIAL_BACKOFF_SECONDS = 0.25

    PERMANENT = T.let([Envelope::MalformedMessage, EventStateMap::UnmappedEvent].freeze, T::Array[T.class_of(StandardError)])

    sig do
      params(
        source: Source,
        logger: ::Logger,
        projector: Projector,
        sleeper: T.proc.params(seconds: Float).void
      ).void
    end
    def initialize(source:, logger:, projector: Projector.new, sleeper: ->(seconds) { sleep(seconds) })
      @source = source
      @logger = logger
      @projector = projector
      @sleeper = sleeper
    end

    # Consumes until the source is closed. Raises Halted on a message it
    # cannot project.
    sig { void }
    def run
      while (message = @source.next_message)
        handle(message)
      end
    end

    private

    sig { params(message: Message).void }
    def handle(message)
      envelope = Envelope.parse(key: message.key, value: message.value)
      applied = project(envelope)
      @source.commit(message)
      @logger.info("#{message.position} #{envelope.event_type} #{envelope.aggregate_id} " \
                   "seq=#{envelope.sequence} #{applied ? 'applied' : 'already applied'}")
    rescue StandardError => e
      raise Halted, "halting at #{message.position}: #{e.class}: #{e.message}"
    end

    sig { params(envelope: Envelope, attempt: Integer).returns(T::Boolean) }
    def project(envelope, attempt = 1)
      @projector.apply(envelope)
    rescue *PERMANENT
      raise
    rescue StandardError => e
      raise if attempt >= MAX_ATTEMPTS

      backoff = INITIAL_BACKOFF_SECONDS * (1 << (attempt - 1))
      @logger.warn("#{envelope.event_type} #{envelope.aggregate_id}: #{e.message}; retrying in #{backoff}s")
      @sleeper.call(backoff)
      project(envelope, attempt + 1)
    end
  end
end
