# frozen_string_literal: true
# typed: strict

require 'logger'

module Jobs
  # Resque job run by resque-scheduler (config/resque_schedule.yml): disburses
  # every completed Repayment to the holders of its Security. All the logic is
  # Services::Securities::DisburseDue; this only adapts it to Resque.
  class DisburseRepayments
    # Some holders could not be paid. Raised after the rest were, so the run
    # shows up in Resque's failed queue; the next run retries them.
    class Incomplete < StandardError; end

    sig { returns(Symbol) }
    def self.queue = :securities

    sig { void }
    def self.perform
      result = Services::Securities::DisburseDue.new(logger: Logger.new($stdout, progname: name)).call
      return if result.failed.empty?

      raise Incomplete, result.failed.map { |id, failure| "#{id}: #{failure}" }.join('; ')
    end
  end
end
