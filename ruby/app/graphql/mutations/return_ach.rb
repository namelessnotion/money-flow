# frozen_string_literal: true
# typed: strict

require_relative 'ach_mutation'

module Mutations
  # Stands in for the provider's return notice until a real provider delivers
  # one. The notice is recorded and acted on at once where it can be; one it
  # can't finish yet is seen through by Services::Ach::ReturnDue, and the
  # answer is the ACH Transaction either way.
  class ReturnAch < AchMutation
    description 'Record that the ACH network returned the entry. Before settlement the Transaction is rolled back; ' \
                'after it, a late return is recorded (a withdrawal is put back, a deposit is clawed back or owed).'

    argument :ach_transaction_id, ID, required: true
    argument :reason, String, required: true, description: 'The return reason, e.g. "R01 insufficient funds".'

    sig { params(ach_transaction_id: String, reason: String).returns(T::Hash[Symbol, T.untyped]) }
    def resolve(ach_transaction_id:, reason:)
      answering { Services::Ach::Return.new.call(ach_transaction_id: ach_transaction_id, reason: reason) }
    end
  end
end
