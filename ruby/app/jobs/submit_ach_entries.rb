# frozen_string_literal: true
# typed: strict

require 'logger'

module Jobs
  # Resque job run by resque-scheduler (config/resque_schedule.yml): hands every
  # ready ACH entry to the provider. All the logic is Services::Ach::SubmitDue;
  # this only adapts it to Resque.
  class SubmitAchEntries
    # Some entries could not be submitted. Raised after the rest were, so the
    # run shows up in Resque's failed queue; the next run retries them.
    class Incomplete < StandardError; end

    sig { returns(Symbol) }
    def self.queue = :ach

    sig { void }
    def self.perform
      result = Services::Ach::SubmitDue.new(logger: Logger.new($stdout, progname: name)).call
      return if result.failed.empty?

      raise Incomplete, result.failed.map { |id, failure| "#{id}: #{failure}" }.join('; ')
    end
  end
end
