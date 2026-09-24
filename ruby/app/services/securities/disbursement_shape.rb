# frozen_string_literal: true
# typed: strict

require_relative 'leg'
require_relative 'wallets'

module Services
  module Securities
    # How one holder's share of one Repayment is paid: their claim is retired,
    # then they are paid.
    #
    # The **retirement leg** returns the repaid claim from the Investor's
    # `investment` account to the Issuer's `issuer_control` wallet — the mint's
    # mirror, which is what brings that control wallet's standing negative back
    # towards zero as a Security is repaid. The **payout leg** moves principal
    # plus interest from the Security's repayment wallet into the Investor's
    # cleared cash, and its **cash leg** moves the real money behind it from
    # the Security's cash into the Investor's (ruby/docs/adr/0009) — without
    # which an Investor could never withdraw what they earned. Both wait for
    # the retirement leg.
    #
    # The exact mirror of a purchase, for the same reason: the claim is the
    # scarce, ledger-enforced thing. `investment` permits neither direction, so
    # its Token carries debits_must_not_exceed_credits — retiring first means a
    # holder can never be paid principal they do not hold, and the refusal
    # lands before any money leaves the repayment wallet. It is also the only
    # leg Go's accept-time pre-flight can see, since the other is gated.
    #
    # **An interest-only Repayment retires nothing, so it has no retirement leg
    # at all** and the payout leg and its cash leg become roots. The leg is
    # absent rather than zero, and that was once the difference between
    # survivable and not:
    # Go turned a zero-amount Transfer into a transport error the saga
    # swallowed, stranding the Transaction in Started with nothing to explain
    # it. Go now refuses such a leg at accept time instead, as one recorded
    # rejection (go/docs/adr/0007), so this is belt-and-braces — it gives a
    # better message here, at the call site, and the disbursements CHECK stops
    # a bad row being written at all.
    class DisbursementShape < T::Struct
      FACTORY_NAME = 'security_disbursement'
      # Bumped whenever the legs or their order change, so Go's record of each
      # Transaction says which shape ran.
      # 1 paid cleared cash only, so an Investor could never withdraw it.
      FACTORY_VERSION = '2'

      AccountType = Types::Enums::AccountType

      const :payout_transfer_id, String
      const :investment_wallet_id, String
      const :control_wallet_id, String
      const :repayment_money, Leg::MoneyWallets
      const :investor_money, Leg::MoneyWallets
      # Absent when no principal is repaid; see the class comment.
      const :retirement_transfer_id, T.nilable(String)

      # `accounts` is the Issuer's, the Security's and the Investor's,
      # concatenated.
      sig do
        params(
          security: Models::Security,
          investor_entity_id: Integer,
          accounts: T::Array[Models::Account],
          payout_transfer_id: String,
          retirement_transfer_id: T.nilable(String)
        ).returns(DisbursementShape)
      end
      def self.for(security:, investor_entity_id:, accounts:, payout_transfer_id:, retirement_transfer_id:)
        of_issuer = Wallets.of_entity(security.issuer_entity_id, [AccountType::IssuerControl], accounts)
        of_investor = Wallets.of_entity(investor_entity_id, [AccountType::Investment], accounts)

        new(payout_transfer_id: payout_transfer_id, retirement_transfer_id: retirement_transfer_id,
            investment_wallet_id: of_investor.fetch(AccountType::Investment),
            control_wallet_id: of_issuer.fetch(AccountType::IssuerControl),
            repayment_money: Wallets.money_of_security(security.id, AccountType::SecurityRepayment, accounts),
            investor_money: Wallets.money_of_entity(investor_entity_id, accounts))
      end

      # `principal_minor_units` is what this holder gets back of what they lent;
      # `interest_minor_units` is what they earned on it. The payout leg and its
      # cash leg each move both together — the split is recorded on the
      # `disbursements` row rather than in the ledger.
      sig do
        params(transaction_id: String, principal_minor_units: Integer, interest_minor_units: Integer)
          .returns(T.untyped)
      end
      def start_request(transaction_id:, principal_minor_units:, interest_minor_units:)
        payout = principal_minor_units + interest_minor_units
        raise InvalidAmount, 'a disbursement of nothing has no leg to send' unless payout.positive?

        retirement = retirement_leg_for(principal_minor_units)

        Transaction::V1::StartInitializingTransactionRequest.new(
          id: transaction_id,
          factory_name: FACTORY_NAME,
          factory_version: FACTORY_VERSION,
          transfers: transfers(payout, principal_minor_units, retirement),
          # With no principal to retire there is nothing to wait for, so the
          # payout leg and its cash leg are roots and get the pre-flight
          # themselves.
          transfer_dependency: retirement ? Leg.after(Leg.money_ids(payout_transfer_id), retirement) : {}
        )
      end

      private

      # A retirement leg exists exactly when there is principal to retire —
      # the same biconditional as the disbursements_retirement_leg_iff_principal
      # CHECK, enforced here too because the two halves are read by different
      # methods below and a disagreement would build a dependency on a leg that
      # was never sent. Go's validateDAG would reject that outright, which is
      # at least loud; the reason to catch it here is that the caller derived
      # both from the same allocation and one of them is simply wrong.
      sig { params(principal_minor_units: Integer).returns(T.nilable(String)) }
      def retirement_leg_for(principal_minor_units)
        retirement = retirement_transfer_id
        return retirement if principal_minor_units.positive? == !retirement.nil?

        raise InvalidAmount,
              "a disbursement repaying #{principal_minor_units} principal " \
              "#{retirement ? 'must not carry' : 'needs'} a retirement leg"
      end

      sig do
        params(payout: Integer, principal_minor_units: Integer, retirement: T.nilable(String))
          .returns(T::Hash[String, T.untyped])
      end
      def transfers(payout, principal_minor_units, retirement)
        legs = Leg.money(id: payout_transfer_id, amount_minor_units: payout,
                         from: repayment_money, to: investor_money)
        return legs unless retirement

        legs.merge(
          retirement => Leg.transfer(id: retirement, amount_minor_units: principal_minor_units,
                                     from: investment_wallet_id, to: control_wallet_id)
        )
      end
    end
  end
end
