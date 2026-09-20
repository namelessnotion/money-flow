# frozen_string_literal: true
# typed: strict

require_relative 'security_mutation'

module Mutations
  # Moves a fully-subscribed Security's escrow to its Borrower. Operator-
  # initiated on purpose: nothing reacts to the last Subscription completing,
  # which is what keeps the follow-on chain one step long.
  class DrawSecurity < SecurityMutation
    description 'Move a fully subscribed Security’s escrowed money to its Borrower’s cleared cash.'

    argument :security_id, ID, required: true

    sig { params(security_id: String).returns(T::Hash[Symbol, T.untyped]) }
    def resolve(security_id:)
      answering_with_security { Services::Securities::Draw.new.call(security_id: security_id) }
    end
  end
end
