# frozen_string_literal: true
# typed: strict

require_relative '../../gen/proto/token/v1/token_pb'
require_relative 'envelope'
require_relative 'event_state_map'

module Consumer
  # Folds the balances Go publishes for each Token (TokenBalanceRecorded) into
  # token_balance_projections, from which an Account's balance is summed
  # (ruby/docs/adr/0005).
  #
  # Go appends each observation with optimistic concurrency, re-reading the
  # ledger when it loses a race, so the highest stream sequence holds the
  # most recent balance. The row keeps that sequence and is replaced only by
  # a higher one: a duplicate or reordered delivery can never walk it back.
  class BalanceProjector
    RECORDED = 'token.v1.TokenBalanceRecorded'

    # Token events that say nothing about a balance. Listed, like
    # EventStateMap's nil entries, so an unknown event is still refused.
    WITHOUT_BALANCE = T.let(%w[token.v1.TokenMinted].freeze, T::Array[String])

    # Applies `envelope`, and returns whether it changed the read model.
    sig { params(envelope: Envelope).returns(T::Boolean) }
    def apply(envelope)
      return !upsert(envelope).empty? if envelope.event_type == RECORDED
      return false if WITHOUT_BALANCE.include?(envelope.event_type)

      raise EventStateMap::UnmappedEvent, "no projection for token event #{envelope.event_type.inspect}"
    end

    private

    # One statement, so the guard and the write can't be separated by a
    # concurrent delivery: the row is inserted, or updated only when this
    # event's sequence is higher than the one it holds. Returns the rows
    # written — none when the guard refused. Sequel types a returned row's
    # values by the column's database type, known only at runtime.
    sig { params(envelope: Envelope).returns(T::Array[T::Hash[Symbol, T.untyped]]) }
    def upsert(envelope)
      body = envelope.body
      columns = balance_columns(envelope, body)
      updated = columns.except(:token_id).merge(updated_at: Sequel::SQL::Constants::CURRENT_TIMESTAMP)
      Models::TokenBalanceProjection.dataset.returning(:token_id).insert_conflict(
        target: :token_id, update: updated,
        update_where: Sequel[:token_balance_projections][:last_sequence] < Sequel[:excluded][:last_sequence]
      ).insert(columns)
    end

    sig { params(envelope: Envelope, body: EventBody).returns(T::Hash[Symbol, T.any(String, Integer)]) }
    def balance_columns(envelope, body)
      {
        token_id: envelope.aggregate_id,
        wallet_uuid: body.string('wallet_id') || raise(Envelope::MalformedMessage, "#{RECORDED} has no wallet_id"),
        currency: body.string('currency') || raise(Envelope::MalformedMessage, "#{RECORDED} has no currency"),
        posted_minor_units: body.integer('posted_minor_units'),
        pending_outgoing_minor_units: body.integer('pending_outgoing_minor_units'),
        pending_incoming_minor_units: body.integer('pending_incoming_minor_units'),
        last_sequence: envelope.sequence,
        last_global_seq: envelope.global_seq
      }
    end
  end
end
