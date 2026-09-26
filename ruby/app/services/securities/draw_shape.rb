# frozen_string_literal: true
# typed: strict

require_relative 'leg'
require_relative 'wallets'

module Services
  module Securities
    # How a fully-subscribed Security's money reaches its Borrower: a money leg
    # from the Security's Escrow into the Borrower's cleared cash, and its cash
    # leg from the Security's cash into the Borrower's (ruby/docs/adr/0009).
    #
    # The cash leg runs first, and the money leg follows it. The Borrower's
    # cash arrives before their cleared cash, so their cash always covers
    # their cleared cash. Otherwise they could fund a withdrawal from drawn
    # cleared cash whose cash hadn't landed (ruby/docs/adr/0010).
    #
    # Neither leg mints, so Go pre-flights the root. A Draw before any
    # Repayment finds the Security's cash equal to its escrow, so a Security
    # not actually fully subscribed, or a Draw sent twice under different
    # ids, is refused before anything is written.
    #
    # It does not cross the bank boundary and so never stages. Getting that
    # money out to a real bank is an ordinary ACH withdrawal, on rails that
    # already exist — and the cash leg is what gives that withdrawal's real
    # leg something to draw on.
    class DrawShape < T::Struct
      FACTORY_NAME = 'security_draw'
      # Bumped whenever the legs or their order change, so Go's record of each
      # Transaction says which shape ran.
      # 1 moved cleared cash only, so a Borrower could never withdraw it. 2
      # ran the cash leg beside the money leg, unordered.
      FACTORY_VERSION = '3'

      AccountType = Types::Enums::AccountType

      const :transfer_id, String
      const :escrow_money, Leg::MoneyWallets
      const :borrower_money, Leg::MoneyWallets

      # `accounts` is the Security's and the Borrower's, concatenated.
      sig do
        params(security: Models::Security, accounts: T::Array[Models::Account], transfer_id: String)
          .returns(DrawShape)
      end
      def self.for(security:, accounts:, transfer_id:)
        new(transfer_id: transfer_id,
            escrow_money: Wallets.money_of_security(security.id, AccountType::SecurityEscrow, accounts),
            borrower_money: Wallets.money_of_entity(security.borrower_entity_id, accounts))
      end

      sig { params(transaction_id: String, amount_minor_units: Integer).returns(T.untyped) }
      def start_request(transaction_id:, amount_minor_units:)
        Transaction::V1::StartInitializingTransactionRequest.new(
          id: transaction_id,
          factory_name: FACTORY_NAME,
          factory_version: FACTORY_VERSION,
          transfers: Leg.money(id: transfer_id, amount_minor_units: amount_minor_units,
                               from: escrow_money, to: borrower_money),
          transfer_dependency: Leg.chain(Leg.entity_is_paid(transfer_id))
        )
      end
    end
  end
end
