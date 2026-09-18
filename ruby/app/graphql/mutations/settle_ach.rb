# frozen_string_literal: true
# typed: strict

require_relative 'ach_mutation'

module Mutations
  # Stands in for the provider's settlement notice until a real provider
  # delivers one.
  class SettleAch < AchMutation
    description 'Record that the ACH network posted the entry.'

    argument :ach_transaction_id, ID, required: true

    sig { params(ach_transaction_id: String).returns(T::Hash[Symbol, T.untyped]) }
    def resolve(ach_transaction_id:)
      answering { Services::Ach::Settle.new.call(ach_transaction_id: ach_transaction_id) }
    end
  end
end
