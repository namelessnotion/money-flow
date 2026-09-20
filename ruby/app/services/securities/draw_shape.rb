# frozen_string_literal: true
# typed: strict

require_relative 'leg'
require_relative 'wallets'

module Services
  module Securities
    # How a fully-subscribed Security's money reaches its Borrower: one
    # Transfer from the Security's Escrow into the Borrower's cleared cash.
    #
    # Root and non-minting, so Go pre-flights it: a Security whose escrow is
    # short — not actually fully subscribed, or a Draw sent twice under
    # different ids — is refused before anything is written.
    #
    # It does not cross the bank boundary and so never stages. Getting that
    # money out to a real bank is an ordinary ACH withdrawal from the
    # Borrower's own cleared cash, on rails that already exist.
    class DrawShape < T::Struct
      FACTORY_NAME = 'security_draw'
      # Bumped whenever the legs or their order change, so Go's record of each
      # Transaction says which shape ran.
      FACTORY_VERSION = '1'

      AccountType = Types::Enums::AccountType

      const :transfer_id, String
      const :escrow_wallet_id, String
      const :borrower_wallet_id, String

      # `accounts` is the Security's and the Borrower's, concatenated.
      sig do
        params(security: Models::Security, accounts: T::Array[Models::Account], transfer_id: String)
          .returns(DrawShape)
      end
      def self.for(security:, accounts:, transfer_id:)
        of_security = Wallets.of_security(security.id, [AccountType::SecurityEscrow], accounts)
        of_borrower = Wallets.of_entity(security.borrower_entity_id, [AccountType::ClearedCash], accounts)

        new(transfer_id: transfer_id,
            escrow_wallet_id: of_security.fetch(AccountType::SecurityEscrow),
            borrower_wallet_id: of_borrower.fetch(AccountType::ClearedCash))
      end

      sig { params(transaction_id: String, amount_minor_units: Integer).returns(T.untyped) }
      def start_request(transaction_id:, amount_minor_units:)
        Transaction::V1::StartInitializingTransactionRequest.new(
          id: transaction_id,
          factory_name: FACTORY_NAME,
          factory_version: FACTORY_VERSION,
          transfers: {
            transfer_id => Leg.transfer(id: transfer_id, amount_minor_units: amount_minor_units,
                                        from: escrow_wallet_id, to: borrower_wallet_id)
          }
        )
      end
    end
  end
end
