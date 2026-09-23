# frozen_string_literal: true
# typed: strict

require_relative 'ach_mutation'

module Mutations
  # Stands in for the scheduled submission sweep in demonstrations, so an entry
  # whose funding leg has gone through need not wait for the next run.
  class SubmitAchNow < AchMutation
    description 'Submit a ready ACH entry to the provider now, without waiting for the sweep ' \
                '(for demonstrations).'

    argument :ach_transaction_id, ID, required: true

    sig { params(ach_transaction_id: String).returns(T::Hash[Symbol, T.untyped]) }
    def resolve(ach_transaction_id:)
      answering { Services::Ach::SubmitNow.new.call(ach_transaction_id: ach_transaction_id) }
    end
  end
end
