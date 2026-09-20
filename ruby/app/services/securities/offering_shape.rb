# frozen_string_literal: true
# typed: strict

require_relative 'leg'
require_relative 'wallets'

module Services
  module Securities
    # How a Security's Supply is minted: one Transfer from the Issuer's
    # `issuer_control` wallet into the Security's `security_supply`, for
    # exactly the offering size.
    #
    # This is the only place claims enter the ledger, and it is structurally
    # the same as an ACH deposit's shadow leg — a control wallet with no
    # standing balance minting exactly the amount asked for. Hence
    # `mint_source: true`, which is also why `issuer_control` has to allow
    # onramp: Go's validateMintSource refuses to mint from anything narrower.
    #
    # The supply wallet permits neither direction, so its Token carries
    # debits_must_not_exceed_credits and can never be drawn past what was
    # minted here. That is the oversubscription control, and it is the ledger's
    # rather than Ruby's.
    #
    # Go skips mint_source children in its accept-time pre-flight, which is
    # right: there is no balance to check against.
    class OfferingShape < T::Struct
      FACTORY_NAME = 'security_offering'
      # Bumped whenever the legs or their order change, so Go's record of each
      # Transaction says which shape ran.
      FACTORY_VERSION = '1'

      AccountType = Types::Enums::AccountType

      const :supply_transfer_id, String
      const :control_wallet_id, String
      const :supply_wallet_id, String

      # `accounts` is the Issuer's and the Security's, concatenated.
      sig do
        params(security: Models::Security, accounts: T::Array[Models::Account], supply_transfer_id: String)
          .returns(OfferingShape)
      end
      def self.for(security:, accounts:, supply_transfer_id:)
        of_issuer = Wallets.of_entity(security.issuer_entity_id, [AccountType::IssuerControl], accounts)
        of_security = Wallets.of_security(security.id, [AccountType::SecuritySupply], accounts)

        new(supply_transfer_id: supply_transfer_id,
            control_wallet_id: of_issuer.fetch(AccountType::IssuerControl),
            supply_wallet_id: of_security.fetch(AccountType::SecuritySupply))
      end

      sig { params(transaction_id: String, amount_minor_units: Integer).returns(T.untyped) }
      def start_request(transaction_id:, amount_minor_units:)
        Transaction::V1::StartInitializingTransactionRequest.new(
          id: transaction_id,
          factory_name: FACTORY_NAME,
          factory_version: FACTORY_VERSION,
          transfers: { supply_transfer_id => supply_leg(amount_minor_units) }
        )
      end

      private

      sig { params(amount_minor_units: Integer).returns(T.untyped) }
      def supply_leg(amount_minor_units)
        Leg.transfer(id: supply_transfer_id, amount_minor_units: amount_minor_units,
                     from: control_wallet_id, to: supply_wallet_id, mint_source: true)
      end
    end
  end
end
