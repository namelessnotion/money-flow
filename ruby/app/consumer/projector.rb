# frozen_string_literal: true
# typed: strict

require_relative 'envelope'
require_relative 'event_state_map'

module Consumer
  # Folds one published event into Ruby's read model of Go's Transactions and
  # Transfers (ruby/docs/adr/0001).
  #
  # Applies the monotonic guard: an event is applied only if its sequence is
  # above the aggregate's high-water mark, so a duplicate or reordered delivery
  # can never walk the read model backwards. The mark is advanced in the same
  # row, in the same database transaction, as the state it guards.
  class Projector
    Row = T.type_alias { T.any(Models::TransactionProjection, Models::TransferProjection) }

    # Applies `envelope`, and returns whether it changed the read model. Raises
    # EventStateMap::UnmappedEvent, before writing anything, for an event the
    # read model has no mapping for.
    sig { params(envelope: Envelope).returns(T::Boolean) }
    def apply(envelope)
      state = EventStateMap.transition(aggregate_type: envelope.aggregate_type, event_type: envelope.event_type)

      DB.transaction do
        row = target_row(envelope, state)
        next false if row.nil?

        record(row, envelope, state)
        true
      end
    end

    private

    # The row `envelope` should be written to, locked — or nil when the guard
    # says it must not be applied.
    #
    # Only an event that names a state can open a row. A step event on an
    # aggregate never seen before (possible when the connector started
    # mid-stream) leaves nothing honest to record.
    sig { params(envelope: Envelope, state: T.nilable(T::Enum)).returns(T.nilable(Row)) }
    def target_row(envelope, state)
      row = locked_row(envelope)
      return state.nil? ? nil : new_row(envelope) if row.nil?

      envelope.sequence > row.last_sequence ? row : nil
    end

    sig { params(envelope: Envelope).returns(T.nilable(Row)) }
    def locked_row(envelope)
      if transaction?(envelope)
        Models::TransactionProjection.where(aggregate_id: envelope.aggregate_id).for_update.first
      else
        Models::TransferProjection.where(aggregate_id: envelope.aggregate_id).for_update.first
      end
    end

    sig { params(envelope: Envelope).returns(Row) }
    def new_row(envelope)
      if transaction?(envelope)
        Models::TransactionProjection.new(aggregate_id: envelope.aggregate_id)
      else
        Models::TransferProjection.new(aggregate_id: envelope.aggregate_id)
      end
    end

    sig { params(row: Row, envelope: Envelope, state: T.nilable(T::Enum)).void }
    def record(row, envelope, state)
      describe(row, envelope.body, state)
      row.last_sequence = envelope.sequence
      row.last_global_seq = envelope.global_seq
      row.updated_at = Time.now
      row.save
    end

    sig { params(row: Row, body: EventBody, state: T.nilable(T::Enum)).void }
    def describe(row, body, state)
      unless state.nil?
        row.state = state.serialize
        # The reason explains the state it arrived with, so it is replaced
        # (or cleared) on every transition and kept across step events.
        row.reason = body.string('reason')
      end
      # The owning Transaction is stated once, on a Transfer's opening event.
      row.transaction_id ||= body.string('transaction_id') if row.is_a?(Models::TransferProjection)
    end

    sig { params(envelope: Envelope).returns(T::Boolean) }
    def transaction?(envelope)
      envelope.aggregate_type == 'transaction'
    end
  end
end
