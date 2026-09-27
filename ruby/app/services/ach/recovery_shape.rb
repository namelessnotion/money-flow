# frozen_string_literal: true
# typed: strict

require_relative '../../../gen/proto/transaction/v1/transaction_pb'
require_relative '../det_id'
require_relative 'entity_wallets'
require_relative 'transaction_shape'

module Services
  module Ach
    # The Transaction that collects some of what an entity owes
    # (ruby/docs/adr/0011, decision 5): its cleared cash to bank control, then
    # its cash to its Receivable, for the same amount. The entity pays, so its
    # cleared cash leaves first (ruby/docs/adr/0010), like a Repayment. Nothing
    # is minted or staged: the money is already the entity's, inside the
    # platform.
    #
    # Ids are derived from the entity and the Recovery's sequence number, which
    # RecoverDue records before it asks Go.
    class RecoveryShape < T::Struct
      FACTORY_NAME = 'receivable_recovery'
      FACTORY_VERSION = '1'

      AccountType = Types::Enums::AccountType

      const :transaction_id, String
      const :cleared_transfer_id, String
      const :cash_transfer_id, String
      # Wallet uuid per account type, for the accounts the legs use.
      const :wallets, T::Hash[AccountType, String]

      sig do
        params(entity_id: Integer, sequence: Integer, accounts: T::Array[Models::Account]).returns(RecoveryShape)
      end
      def self.for(entity_id:, sequence:, accounts:)
        id = DetId.for("#{entity_id}:recovery:#{sequence}")
        new(transaction_id: id,
            cleared_transfer_id: DetId.for("#{entity_id}:recovery:#{sequence}:cleared"),
            cash_transfer_id: DetId.for("#{entity_id}:recovery:#{sequence}:cash"),
            wallets: EntityWallets.for(
              [AccountType::ClearedCash, AccountType::BankControl, AccountType::Cash, AccountType::Receivable], accounts
            ))
      end

      # The Transaction::V1::StartInitializingTransactionRequest for it —
      # untyped for the same reason as every generated message (see
      # TransactionShape#start_request).
      sig { params(amount_minor_units: Integer).returns(T.untyped) }
      def start_request(amount_minor_units:)
        Transaction::V1::StartInitializingTransactionRequest.new(
          id: transaction_id,
          factory_name: FACTORY_NAME,
          factory_version: FACTORY_VERSION,
          transfers: transfers(amount_minor_units),
          transfer_dependency: {
            cash_transfer_id => Transaction::V1::TransferIdList.new(transfer_id: [cleared_transfer_id])
          }
        )
      end

      private

      sig { params(amount_minor_units: Integer).returns(T::Hash[String, T.untyped]) }
      def transfers(amount_minor_units)
        {
          cleared_transfer_id => transfer(cleared_transfer_id, AccountType::ClearedCash, AccountType::BankControl,
                                          amount_minor_units),
          cash_transfer_id => transfer(cash_transfer_id, AccountType::Cash, AccountType::Receivable, amount_minor_units)
        }
      end

      sig { params(id: String, from: AccountType, to: AccountType, amount_minor_units: Integer).returns(T.untyped) }
      def transfer(id, from, to, amount_minor_units)
        Transaction::V1::Transfer.new(
          id: id,
          amount: Shared::V1::Money.new(minor_units: amount_minor_units, currency: TransactionShape::CURRENCY),
          from_wallet_id: wallets.fetch(from),
          to_wallet_id: wallets.fetch(to),
          stage: false,
          mint_source: false
        )
      end
    end
  end
end
