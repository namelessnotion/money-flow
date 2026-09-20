# frozen_string_literal: true
# typed: strict

require_relative 'securities_mutation'
require_relative '../types/objects/security'

module Mutations
  # A Securities mutation answering with the Security it acted on, re-read with
  # its projected state.
  class SecurityMutation < SecuritiesMutation
    field :security, Types::Security, null: true

    private

    sig { params(blk: T.proc.returns(Models::Security)).returns(T::Hash[Symbol, T.untyped]) }
    def answering_with_security(&blk)
      answering { { security: Types::Security.find(blk.call.id) } }
    end
  end
end
