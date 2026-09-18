# frozen_string_literal: true
# typed: strict

require_relative 'base_mutation'
require_relative '../types/objects/ach_transaction'

module Mutations
  # What every ACH mutation shares: it answers with the ACH Transaction, and a
  # refusal the caller can act on — Go or the provider declining, an unknown
  # id, a bad amount — comes back as a GraphQL error rather than a crash.
  # Unavailable is not caught: it is an outage, not an answer.
  class AchMutation < BaseMutation
    field :ach_transaction, Types::AchTransaction, null: true

    EXPECTED = T.let(
      [Services::Ach::Refused, Services::Ach::NotFound, Services::Ach::InvalidAmount,
       Services::Ach::MissingAccount, Services::Ach::NotClearable].freeze,
      T::Array[T.class_of(StandardError)]
    )

    private

    # Runs the service call, then answers with the ACH Transaction it acted on,
    # re-read with its projected state.
    sig { params(blk: T.proc.returns(Models::AchTransaction)).returns(T::Hash[Symbol, T.nilable(Models::AchTransaction)]) }
    def answering(&blk)
      ach = blk.call
      { ach_transaction: Types::AchTransaction.find(ach.id) }
    rescue *EXPECTED => e
      raise GraphQL::ExecutionError, e.message
    end
  end
end
