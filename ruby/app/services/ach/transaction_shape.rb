# frozen_string_literal: true
# typed: strict

require_relative '../../../gen/proto/transaction/v1/transaction_pb'
require_relative 'entity_wallets'

module Services
  module Ach
    # How an ACH Transaction is built from an entity's accounts: the two
    # Transfers Go runs for it and the order it runs them in.
    #
    # The **real leg** moves money across the bank boundary and is staged: it
    # waits, pending, while the ACH network settles over days. The **shadow
    # leg** records the same movement in the clearing accounts. Which runs
    # first depends on the direction (ruby/docs/adr/0004):
    #
    # - a deposit's shadow leg waits for the real leg to post, so uncleared
    #   cash is only minted for money the ACH network actually delivered;
    # - a withdrawal's real leg waits for the shadow leg, so the cleared cash
    #   is moved to bank control — and a shortfall refused — before any money
    #   leaves over ACH.
    class TransactionShape < T::Struct
      # ACH moves US dollars only.
      CURRENCY = 'USD'

      # Bumped whenever a direction's legs or their order change, so Go's
      # record of each Transaction says which shape it ran. Withdrawal 1 ran
      # the real leg first.
      FACTORY_VERSIONS = T.let(
        { Types::Enums::AchDirection::Deposit => '1', Types::Enums::AchDirection::Withdrawal => '2' }.freeze,
        T::Hash[Types::Enums::AchDirection, String]
      )

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
            wallets: EntityWallets.for(account_types(direction), accounts))
      end

      # Every account type a `direction` Transaction moves money between.
      sig { params(direction: Types::Enums::AchDirection).returns(T::Array[AccountType]) }
      def self.account_types(direction)
        LEGS.fetch(direction).values.flat_map { |leg| [leg.from, leg.to] }.uniq
      end
      private_class_method :account_types

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
          factory_version: FACTORY_VERSIONS.fetch(direction),
          transfers: {
            real_transfer_id => transfer(real_transfer_id, :real, amount_minor_units),
            shadow_transfer_id => transfer(shadow_transfer_id, :shadow, amount_minor_units)
          },
          transfer_dependency: dependency
        )
      end

      private

      # Which leg waits for which: see the class comment.
      sig { returns(T::Hash[String, T.untyped]) }
      def dependency
        first, second =
          if direction == Types::Enums::AchDirection::Withdrawal
            [shadow_transfer_id, real_transfer_id]
          else
            [real_transfer_id, shadow_transfer_id]
          end
        { second => Transaction::V1::TransferIdList.new(transfer_id: [first]) }
      end

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
