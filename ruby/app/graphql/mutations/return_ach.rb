# frozen_string_literal: true
# typed: strict

require_relative 'ach_mutation'

module Mutations
  # Stands in for the provider's return notice until a real provider delivers
  # one.
  class ReturnAch < AchMutation
    description 'Record that the ACH network returned the entry; the Transaction is rolled back.'

    argument :ach_transaction_id, ID, required: true
    argument :reason, String, required: true, description: 'The return reason, e.g. "R01 insufficient funds".'

    sig { params(ach_transaction_id: String, reason: String).returns(T::Hash[Symbol, T.untyped]) }
    def resolve(ach_transaction_id:, reason:)
      answering { Services::Ach::Return.new.call(ach_transaction_id: ach_transaction_id, reason: reason) }
    end
  end
end
