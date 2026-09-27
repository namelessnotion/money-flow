# frozen_string_literal: true
# typed: strict

require 'logger'
require_relative 'return'

module Services
  module Ach
    # The sweep behind the scheduled job: sees every return notice through
    # (ruby/docs/adr/0011).
    #
    # Return acts on a notice the moment it arrives, but it can't always
    # finish. A notice that lands just as the entry settles finds the real
    # leg posted and the Transaction not yet completed, so there is nothing to
    # cancel and no late return to record yet. And a late return Ruby recorded
    # may never have reached Go. Either way the notice is on the row, so this
    # picks it up again:
    #
    # - a notice with no late return, while the ACH Transaction may still move
    #   money: not rolled back, rejected or failed, and its real leg not
    #   cancelled or failed by the return itself;
    # - a late return recorded but not yet seen by the read model. One it has
    #   seen, in any state, is left alone: a rolled-back one needs a person.
    #
    # One late return per ACH Transaction, fixed by its derived id, and
    # nothing reacts to one completing, so the chain ends there.
    class ReturnDue
      # What a sweep did.
      class Result < T::Struct
        const :acted_on, T::Array[String]
        # ACH Transaction id => the failure, for each one that could not be acted on.
        const :failed, T::Hash[String, String]
      end

      ACH = T.let(Sequel[:ach_transactions], Sequel::SQL::Identifier)
      TXN = T.let(Sequel[:txn], Sequel::SQL::Identifier)
      REAL = T.let(Sequel[:real], Sequel::SQL::Identifier)
      LATE_RETURN = T.let(Sequel[:late_return], Sequel::SQL::Identifier)

      # An ACH Transaction in one of these moved no money, or needs a person.
      SETTLED_WITHOUT_MONEY = T.let(
        [Types::Enums::TransactionState::RolledBack, Types::Enums::TransactionState::Rejected,
         Types::Enums::TransactionState::RollbackFailed].map(&:serialize).freeze,
        T::Array[String]
      )
      # A real leg in one of these will never post: the return was seen through
      # before settlement.
      NEVER_POSTS = T.let(
        [Types::Enums::TransferState::Cancelled, Types::Enums::TransferState::Failed].map(&:serialize).freeze,
        T::Array[String]
      )

      sig { params(logger: ::Logger, return_service: Return).void }
      def initialize(logger:, return_service: Return.new)
        @logger = logger
        @return = return_service
      end

      sig { returns(Result) }
      def call
        due = candidates
        acted_on = T.let([], T::Array[String])
        failed = T.let({}, T::Hash[String, String])
        due.each { |ach| attempt(ach, acted_on, failed) }
        @logger.info("ACH returns: #{acted_on.size} acted on, #{failed.size} failed, of #{due.size} due")
        Result.new(acted_on: acted_on, failed: failed)
      end

      private

      T::Sig::WithoutRuntime.sig { returns(T::Array[Models::AchTransaction]) }
      def candidates
        noticed.where(Sequel.|(awaiting_late_return, late_return_unseen))
               .select_all(:ach_transactions).order(ACH[:returned_at]).all
      end

      # Every ACH Transaction with a notice, beside the projections of itself,
      # its real leg and its late return.
      T::Sig::WithoutRuntime.sig { returns(Models::AchTransaction::PrivateDataset) }
      def noticed
        Models::AchTransaction.dataset
                              .left_join(Sequel[:transaction_projections].as(:txn), aggregate_id: ACH[:id])
                              .left_join(Sequel[:transfer_projections].as(:real), aggregate_id: ACH[:real_transfer_id])
                              .left_join(Sequel[:transaction_projections].as(:late_return),
                                         aggregate_id: ACH[:return_transaction_id])
                              .exclude(ACH[:returned_at] => nil)
      end

      T::Sig::WithoutRuntime.sig { returns(Sequel::SQL::BooleanExpression) }
      def awaiting_late_return
        Sequel.&(
          { ACH[:return_transaction_id] => nil },
          Sequel.|({ TXN[:state] => nil }, Sequel.~(TXN[:state] => SETTLED_WITHOUT_MONEY)),
          Sequel.|({ REAL[:state] => nil }, Sequel.~(REAL[:state] => NEVER_POSTS))
        )
      end

      T::Sig::WithoutRuntime.sig { returns(Sequel::SQL::BooleanExpression) }
      def late_return_unseen
        Sequel.&(Sequel.~(ACH[:return_transaction_id] => nil), { LATE_RETURN[:aggregate_id] => nil })
      end

      sig { params(ach: Models::AchTransaction, acted_on: T::Array[String], failed: T::Hash[String, String]).void }
      def attempt(ach, acted_on, failed)
        @return.act_on_notice(ach)
        acted_on << ach.id
      rescue StandardError => e
        failed[ach.id] = "#{e.class}: #{e.message}"
        @logger.error("ACH return failed for #{ach.id}: #{failed[ach.id]}")
      end
    end
  end
end
