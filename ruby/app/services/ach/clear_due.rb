# frozen_string_literal: true
# typed: strict

require 'logger'
require_relative 'clear'
require_relative 'clearing_policy'

module Services
  module Ach
    # The sweep behind the scheduled job: clears every ACH deposit that is due.
    #
    # A deposit is a candidate once the read model has seen its Transaction
    # complete and has not yet seen a clearing Transaction for it. A clearing
    # the read model has seen, in any state, is left alone: in flight it needs
    # nothing, and a rejected or rolled-back one needs a person, not a retry.
    # One sent but not yet seen is sent again, which Go dedupes by id.
    #
    # Being a sweep, it catches up on its own after any outage: nothing is
    # scheduled per deposit that could be lost.
    class ClearDue
      # What a sweep did.
      class Result < T::Struct
        const :cleared, T::Array[String]
        # ACH Transaction id => the failure, for each one that could not be cleared.
        const :failed, T::Hash[String, String]
      end

      # The candidate query's tables: the deposit, the projection of its
      # Transaction, and the projection of its clearing Transaction.
      ACH = T.let(Sequel[:ach_transactions], Sequel::SQL::Identifier)
      COMPLETION = T.let(Sequel[:completion], Sequel::SQL::Identifier)
      CLEARING = T.let(Sequel[:clearing], Sequel::SQL::Identifier)
      # When the deposit's Transaction completed: Go's time where the
      # projection has it, else when the projection last changed.
      COMPLETED_AT = T.let(
        Sequel.function(:coalesce, COMPLETION[:state_changed_at], COMPLETION[:updated_at]).as(:completed_at),
        Sequel::SQL::AliasedExpression
      )

      sig { params(logger: ::Logger, clear: Clear).void }
      def initialize(logger:, clear: Clear.new)
        @logger = logger
        @clear = clear
      end

      sig { params(now: Time).returns(Result) }
      def call(now: Time.now)
        due = candidates.select { |ach| ClearingPolicy.due?(ach[:completed_at], now: now) }
        cleared = T.let([], T::Array[String])
        failed = T.let({}, T::Hash[String, String])
        due.each { |ach| attempt(ach.id, cleared, failed) }
        @logger.info("ACH clearing: #{cleared.size} cleared, #{failed.size} failed, of #{due.size} due")
        Result.new(cleared: cleared, failed: failed)
      end

      private

      # Completed deposits with no clearing in the read model yet, each with
      # its completed_at.
      T::Sig::WithoutRuntime.sig { returns(T::Array[Models::AchTransaction]) }
      def candidates
        without_seen_clearing(completed_deposits).select_all(:ach_transactions).select_append(COMPLETED_AT)
                                                 .order(ACH[:created_at]).all
      end

      T::Sig::WithoutRuntime.sig { returns(Models::AchTransaction::PrivateDataset) }
      def completed_deposits
        Models::AchTransaction.dataset
                              .join(Sequel[:transaction_projections].as(:completion), aggregate_id: ACH[:id])
                              .where(ACH[:direction] => Types::Enums::AchDirection::Deposit.serialize,
                                     COMPLETION[:state] => Types::Enums::TransactionState::Completed.serialize)
      end

      T::Sig::WithoutRuntime.sig do
        params(dataset: Models::AchTransaction::PrivateDataset).returns(Models::AchTransaction::PrivateDataset)
      end
      def without_seen_clearing(dataset)
        dataset.left_join(Sequel[:transaction_projections].as(:clearing), aggregate_id: ACH[:clearing_transaction_id])
               .where(CLEARING[:aggregate_id] => nil)
      end

      sig { params(id: String, cleared: T::Array[String], failed: T::Hash[String, String]).void }
      def attempt(id, cleared, failed)
        @clear.call(ach_transaction_id: id)
        cleared << id
      rescue StandardError => e
        failed[id] = "#{e.class}: #{e.message}"
        @logger.error("ACH clearing failed for #{id}: #{failed[id]}")
      end
    end
  end
end
