# frozen_string_literal: true
# typed: strict

require_relative '../../../gen/proto/transaction/v1/transaction_pb'

module Services
  module Ach
    # How an ACH Transaction is built from an entity's accounts: the two
    # Transfers Go runs for it and the order it runs them in.
    #
    # The **real leg** moves money across the bank boundary and is staged: it
    # waits, pending, while the ACH network settles over days. The **shadow
    # leg** records the same movement in the clearing accounts, and depends on
    # the real leg, so it runs only once the real leg has posted. Both shapes
    # are the ones go/internal/transaction/saga_test.go works through.
    class TransactionShape < T::Struct
      class MissingAccount < StandardError; end

      # ACH moves US dollars only.
      CURRENCY = 'USD'
      FACTORY_VERSION = '1'

      # One Transfer of the shape, with its accounts named by type.
      class Leg < T::Struct
        const :from, Types::Enums::AccountType
        const :to, Types::Enums::AccountType
        const :stage, T::Boolean
        const :mint_source, T::Boolean
      end

      AccountType = Types::Enums::AccountType

      # Deposit: money arrives from outside, so both legs mint at their source.
      # Withdrawal: money already inside the platform leaves, so neither does.
      LEGS = T.let(
        {
          Types::Enums::AchDirection::Deposit => {
            real: Leg.new(from: AccountType::Bank, to: AccountType::Cash, stage: true, mint_source: true),
            shadow: Leg.new(from: AccountType::BankControl, to: AccountType::UnclearedCash,
                            stage: false, mint_source: true)
          },
          Types::Enums::AchDirection::Withdrawal => {
            real: Leg.new(from: AccountType::Cash, to: AccountType::Bank, stage: true, mint_source: false),
            shadow: Leg.new(from: AccountType::ClearedCash, to: AccountType::BankControl,
                            stage: false, mint_source: false)
          }
        }.freeze,
        T::Hash[Types::Enums::AchDirection, T::Hash[Symbol, Leg]]
      )

      const :direction, Types::Enums::AchDirection
      const :real_transfer_id, String
      const :shadow_transfer_id, String
      # Wallet uuid per account type, for the accounts the legs use.
      const :wallets, T::Hash[Types::Enums::AccountType, String]

      sig do
        params(
          direction: Types::Enums::AchDirection,
          accounts: T::Array[Models::Account],
          real_transfer_id: String,
          shadow_transfer_id: String
        ).returns(TransactionShape)
      end
      def self.for(direction:, accounts:, real_transfer_id:, shadow_transfer_id:)
        new(direction: direction, real_transfer_id: real_transfer_id, shadow_transfer_id: shadow_transfer_id,
            wallets: wallets_for(account_types(direction), accounts))
      end

      # Every account type a `direction` Transaction moves money between.
      sig { params(direction: Types::Enums::AchDirection).returns(T::Array[AccountType]) }
      def self.account_types(direction)
        LEGS.fetch(direction).values.flat_map { |leg| [leg.from, leg.to] }.uniq
      end
      private_class_method :account_types

      # The wallet backing each of the `needed` account types.
      sig do
        params(needed: T::Array[AccountType], accounts: T::Array[Models::Account])
          .returns(T::Hash[AccountType, String])
      end
      def self.wallets_for(needed, accounts)
        by_type = accounts.to_h { |account| [AccountType.deserialize(account.type), account.wallet_uuid] }
        missing = needed.reject { |type| by_type.key?(type) }
        raise MissingAccount, "entity has no #{missing.map(&:serialize).join(', ')} account" unless missing.empty?

        needed.to_h { |type| [type, by_type.fetch(type)] }
      end
      private_class_method :wallets_for

      # e.g. "ach_deposit" — opaque to Go, which records it on the Transaction.
      sig { returns(String) }
      def factory_name
        "ach_#{direction.serialize}"
      end

      # The Transaction::V1::StartInitializingTransactionRequest that asks Go to
      # run this shape. Untyped for the same reason as every generated message:
      # the class is a constant assigned from the descriptor pool, so Sorbet
      # sees a value rather than a type.
      sig { params(transaction_id: String, amount_minor_units: Integer).returns(T.untyped) }
      def start_request(transaction_id:, amount_minor_units:)
        Transaction::V1::StartInitializingTransactionRequest.new(
          id: transaction_id,
          factory_name: factory_name,
          factory_version: FACTORY_VERSION,
          transfers: {
            real_transfer_id => transfer(real_transfer_id, :real, amount_minor_units),
            shadow_transfer_id => transfer(shadow_transfer_id, :shadow, amount_minor_units)
          },
          # The shadow leg waits for the real leg to post.
          transfer_dependency: { shadow_transfer_id => Transaction::V1::TransferIdList.new(transfer_id: [real_transfer_id]) }
        )
      end

      private

      sig { params(id: String, role: Symbol, amount_minor_units: Integer).returns(T.untyped) }
      def transfer(id, role, amount_minor_units)
        leg = LEGS.fetch(direction).fetch(role)
        Transaction::V1::Transfer.new(
          id: id,
          amount: Shared::V1::Money.new(minor_units: amount_minor_units, currency: CURRENCY),
          from_wallet_id: wallets.fetch(leg.from),
          to_wallet_id: wallets.fetch(leg.to),
          auto_process: true,
          stage: leg.stage,
          mint_source: leg.mint_source
        )
      end
    end
  end
end
