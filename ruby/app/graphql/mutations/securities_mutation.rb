# frozen_string_literal: true
# typed: strict

require_relative 'base_mutation'

module Mutations
  # What every Securities mutation shares: a refusal the caller can act on —
  # Go declining, an oversubscription, a wrong role, an unknown id — comes back
  # as a GraphQL error rather than a crash. Unavailable is not caught: it is an
  # outage, not an answer.
  #
  # The payload differs per mutation, so the three thin bases below declare it;
  # this holds only what they have in common.
  class SecuritiesMutation < BaseMutation
    EXPECTED = T.let(
      [Services::Securities::Refused, Services::Securities::NotFound,
       Services::Securities::InvalidAmount, Services::Securities::MissingAccount,
       Services::Securities::Oversubscribed, Services::Securities::NotDrawable,
       Services::Securities::NotRepayable, Services::Securities::WrongRole,
       Services::Securities::AllocationError].freeze,
      T::Array[T.class_of(StandardError)]
    )

    private

    sig { params(blk: T.proc.returns(T::Hash[Symbol, T.untyped])).returns(T::Hash[Symbol, T.untyped]) }
    def answering(&blk)
      blk.call
    rescue *EXPECTED => e
      raise GraphQL::ExecutionError, e.message
    end
  end
end
