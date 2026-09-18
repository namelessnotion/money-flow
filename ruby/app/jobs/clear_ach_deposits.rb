# frozen_string_literal: true
# typed: strict

require 'logger'

module Jobs
  # Resque job run by resque-scheduler (config/resque_schedule.yml): clears
  # every ACH deposit that is due. All the logic is Services::Ach::ClearDue;
  # this only adapts it to Resque.
  class ClearAchDeposits
    # Some deposits could not be cleared. Raised after the rest were, so the
    # run shows up in Resque's failed queue; the next run retries them.
    class Incomplete < StandardError; end

    sig { returns(Symbol) }
    def self.queue = :ach

    sig { void }
    def self.perform
      result = Services::Ach::ClearDue.new(logger: Logger.new($stdout, progname: name)).call
      return if result.failed.empty?

      raise Incomplete, result.failed.map { |id, failure| "#{id}: #{failure}" }.join('; ')
    end
  end
end
