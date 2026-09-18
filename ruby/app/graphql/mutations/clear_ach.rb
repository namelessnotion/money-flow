# frozen_string_literal: true
# typed: strict

require_relative 'ach_mutation'

module Mutations
  # Stands in for the scheduled clearing sweep in demonstrations, so a settled
  # deposit need not wait out its return window.
  class ClearAch < AchMutation
    description 'Clear a completed deposit now, without waiting for its return window (for demonstrations).'

    argument :ach_transaction_id, ID, required: true

    sig { params(ach_transaction_id: String).returns(T::Hash[Symbol, T.untyped]) }
    def resolve(ach_transaction_id:)
      answering { Services::Ach::ClearNow.new.call(ach_transaction_id: ach_transaction_id) }
    end
  end
end
