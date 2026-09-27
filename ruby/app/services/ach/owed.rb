# frozen_string_literal: true
# typed: strict

module Services
  module Ach
    # What an entity owes the platform: its late deposit returns recorded as
    # debt, less what Recovery has collected (ruby/docs/adr/0011).
    #
    # Read from Ruby's rows and the Transaction projection only, never from the
    # Receivable's balance. Token balances reach Ruby on another topic and can
    # lag the Transaction states, and a Recovery sized from a balance that
    # hadn't caught up with the last one would collect it twice.
    #
    # Two amounts, for two readers:
    #
    # - for_gate counts every debt whose notice is recorded, whether or not
    #   the projection has seen its late return. It refuses the ways money
    #   leaves where Recovery can't reach it, and has to close the moment the
    #   notice arrives;
    # - for_recovery counts only debts whose late return completed. Recovery
    #   must never collect a debt the ledger hasn't recorded yet.
    class Owed < T::Struct
      ACH = T.let(Sequel[:ach_transactions], Sequel::SQL::Identifier)
      RECOVERY = T.let(Sequel[:receivable_recoveries], Sequel::SQL::Identifier)
      PROJECTION = T.let(Sequel[:outcome], Sequel::SQL::Identifier)
      COMPLETED = T.let(Types::Enums::TransactionState::Completed.serialize, String)

      const :debt_recorded, Integer
      const :debt_completed, Integer
      const :recovered, Integer

      sig { params(entity_id: Integer).returns(Owed) }
      def self.for(entity_id) = for_each([entity_id]).fetch(entity_id)

      # What each of `entity_ids` owes, in three queries however many there are.
      sig { params(entity_ids: T::Array[Integer]).returns(T::Hash[Integer, Owed]) }
      def self.for_each(entity_ids)
        recorded = totals(debts, ACH[:entity_id], entity_ids)
        returned = totals(completed_debts, ACH[:entity_id], entity_ids)
        recovered = totals(recoveries, RECOVERY[:entity_id], entity_ids)
        entity_ids.to_h do |id|
          [id, new(debt_recorded: recorded.fetch(id, 0), debt_completed: returned.fetch(id, 0),
                   recovered: recovered.fetch(id, 0))]
        end
      end

      # Every entity that owes something Recovery may collect now.
      sig { returns(T::Array[Integer]) }
      def self.recoverable_entity_ids
        ids = completed_debts.distinct.select_map(ACH[:entity_id])
        ids.map { |id| Integer(id) }.select { |id| self.for(id).for_recovery.positive? }.sort
      end

      # A deposit with a notice and a Clearing recorded: its late return is a
      # debt return, whatever it has reached.
      T::Sig::WithoutRuntime.sig { returns(Models::AchTransaction::PrivateDataset) }
      def self.debts
        Models::AchTransaction.where(ACH[:direction] => Types::Enums::AchDirection::Deposit.serialize)
                              .exclude(ACH[:returned_at] => nil)
                              .exclude(ACH[:clearing_transaction_id] => nil)
      end
      private_class_method :debts

      # Debts whose late return the projection has seen complete.
      T::Sig::WithoutRuntime.sig { returns(Sequel::Dataset) }
      def self.completed_debts = completed(debts, ACH[:return_transaction_id])
      private_class_method :completed_debts

      # Recoveries the projection has seen complete.
      T::Sig::WithoutRuntime.sig { returns(Sequel::Dataset) }
      def self.recoveries = completed(Models::ReceivableRecovery.dataset, RECOVERY[:id])
      private_class_method :recoveries

      # `dataset`'s rows whose Transaction, by the id in `column`, the
      # projection has seen complete.
      T::Sig::WithoutRuntime.sig do
        params(dataset: Sequel::Dataset, column: Sequel::SQL::QualifiedIdentifier).returns(Sequel::Dataset)
      end
      def self.completed(dataset, column)
        dataset.join(Sequel[:transaction_projections].as(:outcome), aggregate_id: column)
               .where(PROJECTION[:state] => COMPLETED)
      end
      private_class_method :completed

      # Each of `entity_ids`' summed amount, by the entity column `by`.
      # Postgres sums bigints as numeric, which Sequel hands back as BigDecimal.
      T::Sig::WithoutRuntime.sig do
        params(dataset: Sequel::Dataset, by: Sequel::SQL::QualifiedIdentifier, entity_ids: T::Array[Integer])
          .returns(T::Hash[Integer, Integer])
      end
      def self.totals(dataset, by, entity_ids)
        dataset.where(by => entity_ids).select_group(by.as(:entity_id))
               .select_append(Sequel.function(:sum, :amount_minor_units).as(:total))
               .naked.all.to_h { |row| [Integer(row.fetch(:entity_id)), Integer(row.fetch(:total))] }
      end
      private_class_method :totals

      sig { returns(Integer) }
      def for_gate = [debt_recorded - recovered, 0].max

      sig { returns(Integer) }
      def for_recovery = [debt_completed - recovered, 0].max
    end
  end
end
