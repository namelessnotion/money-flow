# frozen_string_literal: true
# typed: strict

require_relative '../../../gen/proto/transaction/v1/transaction_pb'
require_relative 'entity_wallets'
require_relative 'transaction_shape'

module Services
  module Ach
    # The Transaction that clears a settled ACH deposit: one Transfer moving
    # its amount from uncleared cash to cleared cash. Not staged — both wallets
    # are inside the platform — and minting nothing, since the deposit's shadow
    # leg already minted the uncleared balance it moves.
    class ClearingShape < T::Struct
      FACTORY_NAME = 'ach_clearing'
      FACTORY_VERSION = '1'

      AccountType = Types::Enums::AccountType

      const :transfer_id, String
      const :from_wallet_id, String
      const :to_wallet_id, String

      sig { params(accounts: T::Array[Models::Account], transfer_id: String).returns(ClearingShape) }
      def self.for(accounts:, transfer_id:)
        wallets = EntityWallets.for([AccountType::UnclearedCash, AccountType::ClearedCash], accounts)
        new(transfer_id: transfer_id,
            from_wallet_id: wallets.fetch(AccountType::UnclearedCash),
            to_wallet_id: wallets.fetch(AccountType::ClearedCash))
      end

      # The Transaction::V1::StartInitializingTransactionRequest for it —
      # untyped for the same reason as every generated message (see
      # TransactionShape#start_request).
      sig { params(transaction_id: String, amount_minor_units: Integer).returns(T.untyped) }
      def start_request(transaction_id:, amount_minor_units:)
        Transaction::V1::StartInitializingTransactionRequest.new(
          id: transaction_id,
          factory_name: FACTORY_NAME,
          factory_version: FACTORY_VERSION,
          transfers: { transfer_id => transfer(amount_minor_units) }
        )
      end

      private

      sig { params(amount_minor_units: Integer).returns(T.untyped) }
      def transfer(amount_minor_units)
        Transaction::V1::Transfer.new(
          id: transfer_id,
          amount: Shared::V1::Money.new(minor_units: amount_minor_units, currency: TransactionShape::CURRENCY),
          from_wallet_id: from_wallet_id,
          to_wallet_id: to_wallet_id,
          stage: false,
          mint_source: false
        )
      end
    end
  end
end
