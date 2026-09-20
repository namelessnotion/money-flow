# frozen_string_literal: true
# typed: strict

require_relative 'securities_mutation'
require_relative '../types/objects/repayment'

module Mutations
  # A Securities mutation answering with the Repayment it acted on, re-read
  # with its projected state.
  class RepaymentMutation < SecuritiesMutation
    field :repayment, Types::Repayment, null: true

    private

    sig { params(id: String).returns(T::Hash[Symbol, T.untyped]) }
    def repayment_payload(id) = { repayment: Types::Repayment.find(id) }
  end
end
