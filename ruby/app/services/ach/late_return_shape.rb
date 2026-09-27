# frozen_string_literal: true
# typed: strict

require_relative '../../../gen/proto/transaction/v1/transaction_pb'
require_relative '../det_id'
require_relative 'entity_wallets'
require_relative 'transaction_shape'

module Services
  module Ach
    # The Transaction that records a late return: a return notice for an ACH
    # Transaction that already completed (ruby/docs/adr/0011). It never
    # reverses the original Transfers — a deposit's cash Token may have been
    # spent — and moves the returned amount afresh, in one of three forms:
    #
    # - a withdrawal return puts the money back, cash first;
    # - a clawback takes back a deposit whose Clearing was never recorded,
    #   out of uncleared cash first. Nothing else ever took that money out;
    # - a debt return records a deposit whose Clearing was recorded as owed,
    #   by minting the whole amount out of the entity's Receivable, so the
    #   ledger can't refuse it whatever the entity still holds.
    #
    # The form is read off Ruby's own record. Once a notice is recorded, Clear
    # can no longer record a Clearing that wasn't already, so the form can't
    # change between asking and asking again.
    class LateReturnShape < T::Struct
      # Each form's factory name, which Go records and never reads.
      class Form < T::Enum
        enums do
          WithdrawalReturn = new('ach_withdrawal_return')
          Clawback = new('ach_deposit_clawback')
          DebtReturn = new('ach_deposit_return')
        end
      end

      FACTORY_VERSION = '1'

      Leg = TransactionShape::Leg
      AccountType = Types::Enums::AccountType

      # Each form's legs by role, in the order they run: every leg waits for
      # the one before it. The order is ruby/docs/adr/0010's: an entity's
      # cleared side leaves before its cash, and its cash arrives before its
      # cleared side.
      LEGS = T.let(
        {
          Form::WithdrawalReturn => {
            cash: Leg.new(from: AccountType::Bank, to: AccountType::Cash, stage: false, mint_source: true),
            cleared: Leg.new(from: AccountType::BankControl, to: AccountType::ClearedCash,
                             stage: false, mint_source: true)
          },
          Form::Clawback => {
            cleared: Leg.new(from: AccountType::UnclearedCash, to: AccountType::BankControl,
                             stage: false, mint_source: false),
            cash: Leg.new(from: AccountType::Cash, to: AccountType::Bank, stage: false, mint_source: false)
          },
          Form::DebtReturn => {
            receivable: Leg.new(from: AccountType::Receivable, to: AccountType::Bank, stage: false, mint_source: true)
          }
        }.freeze,
        T::Hash[Form, T::Hash[Symbol, Leg]]
      )

      const :form, Form
      const :transaction_id, String
      # Transfer id by leg role, in the order the legs run.
      const :transfer_ids, T::Hash[Symbol, String]
      # Wallet uuid per account type, for the accounts the legs use.
      const :wallets, T::Hash[AccountType, String]

      sig { params(ach: Models::AchTransaction, accounts: T::Array[Models::Account]).returns(LateReturnShape) }
      def self.for(ach:, accounts:)
        form = form_for(ach)
        legs = LEGS.fetch(form)
        new(
          form: form,
          transaction_id: DetId.for("#{ach.id}:return"),
          transfer_ids: legs.keys.to_h { |role| [role, DetId.for("#{ach.id}:return:#{role}")] },
          wallets: EntityWallets.for(legs.values.flat_map { |leg| [leg.from, leg.to] }.uniq, accounts)
        )
      end

      sig { params(ach: Models::AchTransaction).returns(Form) }
      def self.form_for(ach)
        if ach.direction == Types::Enums::AchDirection::Withdrawal.serialize
          Form::WithdrawalReturn
        elsif ach.clearing_transaction_id.nil?
          Form::Clawback
        else
          Form::DebtReturn
        end
      end
      private_class_method :form_for

      # The Transaction::V1::StartInitializingTransactionRequest for it —
      # untyped for the same reason as every generated message (see
      # TransactionShape#start_request).
      sig { params(amount_minor_units: Integer).returns(T.untyped) }
      def start_request(amount_minor_units:)
        Transaction::V1::StartInitializingTransactionRequest.new(
          id: transaction_id,
          factory_name: form.serialize,
          factory_version: FACTORY_VERSION,
          transfers: LEGS.fetch(form).to_h { |role, leg| transfer(role, leg, amount_minor_units) },
          transfer_dependency: dependency
        )
      end

      private

      # Each leg after the first waits for the one before it.
      sig { returns(T::Hash[String, T.untyped]) }
      def dependency
        ids = transfer_ids.values
        ids.drop(1).each_with_index.to_h do |second, index|
          [second, Transaction::V1::TransferIdList.new(transfer_id: [ids.fetch(index)])]
        end
      end

      sig { params(role: Symbol, leg: Leg, amount_minor_units: Integer).returns([String, T.untyped]) }
      def transfer(role, leg, amount_minor_units)
        id = transfer_ids.fetch(role)
        [id, Transaction::V1::Transfer.new(
          id: id,
          amount: Shared::V1::Money.new(minor_units: amount_minor_units, currency: TransactionShape::CURRENCY),
          from_wallet_id: wallets.fetch(leg.from),
          to_wallet_id: wallets.fetch(leg.to),
          stage: leg.stage,
          mint_source: leg.mint_source
        )]
      end
    end
  end
end
