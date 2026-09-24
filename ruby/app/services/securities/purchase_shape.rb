# frozen_string_literal: true
# typed: strict

require_relative 'leg'
require_relative 'wallets'

module Services
  module Securities
    # How one Investor's fractional purchase is built: the three Transfers Go
    # runs for it, and the order it runs them in.
    #
    # The **claim leg** moves claims out of the Security's Supply into the
    # Investor's `investment` account. The **money leg** moves the Investor's
    # cleared cash into the Security's Escrow, and its **cash leg** moves the
    # real money behind it from the Investor's `cash` into the Security's
    # (ruby/docs/adr/0009). Both wait for the claim leg.
    #
    # That order is the whole design (ruby/docs/adr/0006). `security_supply` is
    # the hot wallet: every concurrent purchase of this Security draws on it,
    # and it is exactly where Go's accept-time pre-flight can over-accept,
    # since it evaluates every child ready at time zero against one unconsumed
    # balance snapshot. Leaving the claim leg alone at the root buys two
    # things:
    #
    # - normally the claim leg *is* pre-flighted, so an oversubscription is a
    #   clean TransactionRejected before Go writes anything at all;
    # - when the race does slip past, the claim leg fails at real dispatch with
    #   the money leg never having run. There is nothing to reverse and no
    #   Investor money has moved.
    #
    # Had the money leg been a root, every lost race would reverse a committed
    # Investor payment instead.
    #
    # The cash leg is the money leg's sibling rather than its child: with the
    # two sides kept in step, an Investor with the cleared cash has the cash,
    # so chaining them would only add a dispatch round trip.
    #
    # The cost, stated rather than hidden: a child behind a dependency edge
    # gets no pre-flight at all, so an Investor short of cleared cash gets a
    # Transaction that initializes and then rolls back rather than one refused
    # outright. Nothing can tell them at the time — Purchase used to ask Go how
    # it had actually gone, and since the async cutover there is nothing to ask
    # about yet (go/docs/adr/0006). It surfaces as a rolled-back Subscription in
    # the projection, through Services::Securities::Stage.
    class PurchaseShape < T::Struct
      FACTORY_NAME = 'security_purchase'
      # Bumped whenever the legs or their order change, so Go's record of each
      # Transaction says which shape ran.
      # 1 moved cleared cash only, leaving the Investor's `cash` holding money
      # they had spent.
      FACTORY_VERSION = '2'

      AccountType = Types::Enums::AccountType

      const :claim_transfer_id, String
      const :money_transfer_id, String
      const :supply_wallet_id, String
      const :investment_wallet_id, String
      const :investor_money, Leg::MoneyWallets
      const :escrow_money, Leg::MoneyWallets

      # `accounts` is the Security's and the Investor's, concatenated — each
      # scope is resolved separately, so neither can answer for the other.
      sig do
        params(
          security: Models::Security,
          investor_entity_id: Integer,
          accounts: T::Array[Models::Account],
          claim_transfer_id: String,
          money_transfer_id: String
        ).returns(PurchaseShape)
      end
      def self.for(security:, investor_entity_id:, accounts:, claim_transfer_id:, money_transfer_id:)
        new(claim_transfer_id: claim_transfer_id, money_transfer_id: money_transfer_id,
            supply_wallet_id: Wallets.of_security(security.id, [AccountType::SecuritySupply], accounts)
                                     .fetch(AccountType::SecuritySupply),
            investment_wallet_id: Wallets.of_entity(investor_entity_id, [AccountType::Investment], accounts)
                                         .fetch(AccountType::Investment),
            investor_money: Wallets.money_of_entity(investor_entity_id, accounts),
            escrow_money: Wallets.money_of_security(security.id, AccountType::SecurityEscrow, accounts))
      end

      sig { params(transaction_id: String, amount_minor_units: Integer).returns(T.untyped) }
      def start_request(transaction_id:, amount_minor_units:)
        Transaction::V1::StartInitializingTransactionRequest.new(
          id: transaction_id,
          factory_name: FACTORY_NAME,
          factory_version: FACTORY_VERSION,
          transfers: transfers(amount_minor_units),
          transfer_dependency: Leg.after(Leg.money_ids(money_transfer_id), claim_transfer_id)
        )
      end

      private

      sig { params(amount_minor_units: Integer).returns(T::Hash[String, T.untyped]) }
      def transfers(amount_minor_units)
        { claim_transfer_id => claim_leg(amount_minor_units) }.merge(
          Leg.money(id: money_transfer_id, amount_minor_units: amount_minor_units,
                    from: investor_money, to: escrow_money)
        )
      end

      sig { params(amount_minor_units: Integer).returns(T.untyped) }
      def claim_leg(amount_minor_units)
        Leg.transfer(id: claim_transfer_id, amount_minor_units: amount_minor_units,
                     from: supply_wallet_id, to: investment_wallet_id)
      end
    end
  end
end
