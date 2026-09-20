# frozen_string_literal: true
# typed: strict

require_relative 'leg'
require_relative 'wallets'

module Services
  module Securities
    # How a Borrower's payment reaches the Security: one Transfer from the
    # Borrower's cleared cash into the Security's `security_repayment` wallet,
    # for principal plus interest together.
    #
    # One leg rather than two because the ledger moves dollars, and which part
    # of them is principal is a business fact — recorded on the `repayments`
    # row, where the allocation reads it. Splitting the on-ramp would only
    # matter if interest ever had to route somewhere else, such as a platform
    # `gain` account, and that is a later refinement.
    #
    # The Draw's mirror: root and non-minting, so an underfunded Borrower is
    # refused at accept time before anything is written. The Borrower gets the
    # cleared cash the ordinary way — an ACH deposit that has cleared.
    class RepaymentShape < T::Struct
      FACTORY_NAME = 'security_repayment'
      # Bumped whenever the legs or their order change, so Go's record of each
      # Transaction says which shape ran.
      FACTORY_VERSION = '1'

      AccountType = Types::Enums::AccountType

      const :transfer_id, String
      const :borrower_wallet_id, String
      const :repayment_wallet_id, String

      # `accounts` is the Borrower's and the Security's, concatenated.
      sig do
        params(security: Models::Security, accounts: T::Array[Models::Account], transfer_id: String)
          .returns(RepaymentShape)
      end
      def self.for(security:, accounts:, transfer_id:)
        of_borrower = Wallets.of_entity(security.borrower_entity_id, [AccountType::ClearedCash], accounts)
        of_security = Wallets.of_security(security.id, [AccountType::SecurityRepayment], accounts)

        new(transfer_id: transfer_id,
            borrower_wallet_id: of_borrower.fetch(AccountType::ClearedCash),
            repayment_wallet_id: of_security.fetch(AccountType::SecurityRepayment))
      end

      sig { params(transaction_id: String, amount_minor_units: Integer).returns(T.untyped) }
      def start_request(transaction_id:, amount_minor_units:)
        Transaction::V1::StartInitializingTransactionRequest.new(
          id: transaction_id,
          factory_name: FACTORY_NAME,
          factory_version: FACTORY_VERSION,
          transfers: {
            transfer_id => Leg.transfer(id: transfer_id, amount_minor_units: amount_minor_units,
                                        from: borrower_wallet_id, to: repayment_wallet_id)
          }
        )
      end
    end
  end
end
