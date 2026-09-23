# frozen_string_literal: true
# typed: strict

require 'logger'
require_relative 'submit'

module Services
  module Ach
    # The sweep behind the scheduled job: hands every ACH entry that is ready to
    # the provider.
    #
    # This is where submission lives since the async cutover (go/docs/adr/0006).
    # Services::Ach::Initiate used to do it, because it could: it ran the
    # Transaction itself and so knew, in the same call, that a withdrawal's
    # funding leg had committed. Now Go accepts and returns, the orchestrator
    # runs the saga, and nothing is known at origination time. Waiting for the
    # ledger to move is therefore not a scheduling choice — it is the only place
    # the ADR 0004 guarantee can still be made.
    #
    # An entry is a candidate when the read model has seen its real leg reach
    # **staged** and the row carries no provider reference yet. Staged is the
    # load-bearing half: for a withdrawal the real leg sits behind the
    # dependency edge on the funding leg, so it cannot be staged unless funding
    # committed. Submit then confirms with Go before anything leaves. See its
    # own comment for why that pairing is stronger than the single check it
    # replaced.
    #
    # A row with a provider reference is never a candidate again — that column
    # is the record of what was sent, the way `disbursements` is, and not a flag
    # anything sets deliberately.
    #
    # Being a sweep, it catches up on its own after any outage: nothing is
    # scheduled per entry that could be lost, and an entry it could not submit
    # is simply tried again on the next run. One that can never be submitted —
    # a rolled-back or rejected Transaction — falls out of the candidates by
    # itself and needs a person, not a retry loop (ruby/docs/adr/0003 and 0007
    # take the same line).
    class SubmitDue
      # What a sweep did.
      class Result < T::Struct
        const :submitted, T::Array[String]
        # ACH Transaction id => the failure, for each one that could not be sent.
        const :failed, T::Hash[String, String]
      end

      # The candidate query's tables: the ACH Transaction and the projection of
      # its real leg.
      ACH = T.let(Sequel[:ach_transactions], Sequel::SQL::Identifier)
      REAL = T.let(Sequel[:real], Sequel::SQL::Identifier)

      sig { params(logger: ::Logger, submit: Submit).void }
      def initialize(logger:, submit: Submit.new)
        @logger = logger
        @submit = submit
      end

      sig { params(ach_transaction_id: T.nilable(String)).returns(Result) }
      def call(ach_transaction_id: nil)
        due = candidates(ach_transaction_id)
        submitted = T.let([], T::Array[String])
        failed = T.let({}, T::Hash[String, String])
        due.each { |ach| attempt(ach.id, submitted, failed) }
        @logger.info("ACH submission: #{submitted.size} submitted, #{failed.size} failed, of #{due.size} due")
        Result.new(submitted: submitted, failed: failed)
      end

      private

      # Unsubmitted entries whose real leg the read model has seen staged.
      T::Sig::WithoutRuntime.sig { params(id: T.nilable(String)).returns(T::Array[Models::AchTransaction]) }
      def candidates(id)
        scope = staged_and_unsubmitted
        scope = scope.where(ACH[:id] => id) if id
        scope.select_all(:ach_transactions).order(ACH[:created_at]).all
      end

      T::Sig::WithoutRuntime.sig { returns(Models::AchTransaction::PrivateDataset) }
      def staged_and_unsubmitted
        Models::AchTransaction.dataset
                              .join(Sequel[:transfer_projections].as(:real), aggregate_id: ACH[:real_transfer_id])
                              .where(ACH[:provider_reference] => nil,
                                     REAL[:state] => Types::Enums::TransferState::Staged.serialize)
      end

      sig { params(id: String, submitted: T::Array[String], failed: T::Hash[String, String]).void }
      def attempt(id, submitted, failed)
        @submit.call(ach_transaction_id: id)
        submitted << id
      rescue StandardError => e
        failed[id] = "#{e.class}: #{e.message}"
        @logger.error("ACH submission failed for #{id}: #{failed[id]}")
      end
    end
  end
end
