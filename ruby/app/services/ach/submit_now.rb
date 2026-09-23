# frozen_string_literal: true
# typed: strict

require 'logger'
require_relative 'submit_due'

module Services
  module Ach
    # Submits one ACH entry immediately, instead of waiting for the next run of
    # the scheduled sweep. It exists for demonstrations, the way ClearNow does.
    #
    # Unlike ClearNow it skips nothing. The sweep's only reason for waiting is
    # the schedule itself, so running its work for one entry is the same work,
    # with the same two checks: the real leg staged, and Go asked before
    # anything reaches the provider. There is no shortcut here to take, and one
    # would be a way to pay out an unfunded withdrawal.
    #
    # An entry that is not ready is not an error: the sweep will get it when it
    # is. So this answers with the row either way, and a caller reads
    # AchTransaction.steps to see where it actually stands.
    class SubmitNow
      sig { params(sweep: SubmitDue).void }
      def initialize(sweep: SubmitDue.new(logger: Logger.new($stdout, progname: 'SubmitAchNow')))
        @sweep = sweep
      end

      sig { params(ach_transaction_id: String).returns(Models::AchTransaction) }
      def call(ach_transaction_id:)
        ach = Models::AchTransaction[ach_transaction_id] ||
              raise(NotFound, "no ACH transaction #{ach_transaction_id}")
        @sweep.call(ach_transaction_id: ach.id)
        ach
      end
    end
  end
end
