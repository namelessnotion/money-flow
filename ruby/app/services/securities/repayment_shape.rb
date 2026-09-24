# frozen_string_literal: true
# typed: strict

require_relative 'leg'
require_relative 'wallets'

module Services
  module Securities
    # How a Borrower's payment reaches the Security: a money leg from the
    # Borrower's cleared cash into the Security's `security_repayment` wallet,
    # and its cash leg from the Borrower's cash into the Security's
    # (ruby/docs/adr/0009), each for principal plus interest together.
    #
    # One amount rather than a principal leg and an interest leg because the
    # ledger moves dollars, and which part of them is principal is a business
    # fact — recorded on the `repayments` row, where the allocation reads it.
    # Splitting the on-ramp would only matter if interest ever had to route
    # somewhere else, such as a platform `gain` account, and that is a later
    # refinement.
    #
    # The Draw's mirror: both roots and non-minting, so an underfunded Borrower
    # is refused at accept time before anything is written. The Borrower gets
    # the money the ordinary way — an ACH deposit that has cleared, which
    # credits both sides.
    class RepaymentShape < T::Struct
      FACTORY_NAME = 'security_repayment'
      # Bumped whenever the legs or their order change, so Go's record of each
      # Transaction says which shape ran.
      # 1 moved cleared cash only, leaving the Borrower's `cash` holding money
      # they had repaid.
      FACTORY_VERSION = '2'

      AccountType = Types::Enums::AccountType

      const :transfer_id, String
      const :borrower_money, Leg::MoneyWallets
      const :repayment_money, Leg::MoneyWallets

      # `accounts` is the Borrower's and the Security's, concatenated.
      sig do
        params(security: Models::Security, accounts: T::Array[Models::Account], transfer_id: String)
          .returns(RepaymentShape)
      end
      def self.for(security:, accounts:, transfer_id:)
        new(transfer_id: transfer_id,
            borrower_money: Wallets.money_of_entity(security.borrower_entity_id, accounts),
            repayment_money: Wallets.money_of_security(security.id, AccountType::SecurityRepayment, accounts))
      end

      sig { params(transaction_id: String, amount_minor_units: Integer).returns(T.untyped) }
      def start_request(transaction_id:, amount_minor_units:)
        Transaction::V1::StartInitializingTransactionRequest.new(
          id: transaction_id,
          factory_name: FACTORY_NAME,
          factory_version: FACTORY_VERSION,
          transfers: Leg.money(id: transfer_id, amount_minor_units: amount_minor_units,
                               from: borrower_money, to: repayment_money)
        )
      end
    end
  end
end
