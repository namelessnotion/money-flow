# frozen_string_literal: true
# typed: strict

require_relative '../../../gen/proto/transaction/v1/transaction_pb'
require_relative '../det_id'

module Services
  module Securities
    # Securities move US dollars only, the same as everything else here: Go's
    # ledger map is hardcoded to {"USD": 1}, so no other denomination is
    # representable. Claims are denominated in the same minor units as the
    # money that buys them, which is what makes a pro-rata split exact and a
    # retirement leg mean something.
    CURRENCY = 'USD'

    # One Transfer of a Securities shape.
    #
    # Nothing a Security does crosses the bank boundary, so no leg here ever
    # stages: a Transaction runs to completion inside the call that starts it
    # rather than waiting days on a network. Only the offering mints, and only
    # because minting supply is the one way claims enter the ledger at all.
    #
    # Money is kept on two sides, and a Securities Transaction moves both
    # (ruby/docs/adr/0009). The cleared side — cleared_cash, escrow, repayment
    # — is what a party may spend; the cash side — `cash`, `security_cash` —
    # is the real money behind it, and is what an ACH withdrawal's real leg
    # draws on. So every **money leg** here has a **cash leg** beside it:
    # the same amount, between the same two parties, on the cash side.
    # `money` builds the pair, and nothing builds half of one.
    module Leg
      # Where one party holds money: its wallet on each side.
      class MoneyWallets < T::Struct
        const :cleared, String
        const :cash, String
      end

      # A money leg and its cash leg, keyed by id. The cash leg's id is derived
      # from the money leg's, so a row that records the one records both, and
      # a re-send converges on Go's idempotency for both.
      sig do
        params(id: String, amount_minor_units: Integer, from: MoneyWallets, to: MoneyWallets)
          .returns(T::Hash[String, T.untyped])
      end
      def self.money(id:, amount_minor_units:, from:, to:)
        cash_id = cash_id_for(id)
        {
          id => transfer(id: id, amount_minor_units: amount_minor_units, from: from.cleared, to: to.cleared),
          cash_id => transfer(id: cash_id, amount_minor_units: amount_minor_units, from: from.cash, to: to.cash)
        }
      end

      # A money leg and its cash leg run one after the other, never side by
      # side, so that an entity's cash always covers its cleared cash, even
      # while a Transaction is in flight. An ACH withdrawal's Funding checks
      # only cleared cash, and its real leg then needs the cash behind it
      # (ruby/docs/adr/0010).
      #
      # The two legs move both parties at once, so one order can't cover
      # both. It covers the entity: every Securities shape runs between an
      # entity and a Security, and only an entity can withdraw.

      # When the entity pays: its cleared cash leaves first, then its cash.
      sig { params(id: String).returns(T::Array[String]) }
      def self.entity_pays(id) = [id, cash_id_for(id)]

      # When the entity is paid: its cash arrives first, then its cleared cash.
      sig { params(id: String).returns(T::Array[String]) }
      def self.entity_is_paid(id) = [cash_id_for(id), id]

      sig { params(id: String).returns(String) }
      def self.cash_id_for(id) = DetId.for("#{id}:cash")

      # Untyped for the same reason as every generated message: the class is a
      # constant assigned from the descriptor pool, so Sorbet sees a value
      # rather than a type.
      sig do
        params(id: String, amount_minor_units: Integer, from: String, to: String, mint_source: T::Boolean)
          .returns(T.untyped)
      end
      def self.transfer(id:, amount_minor_units:, from:, to:, mint_source: false)
        Transaction::V1::Transfer.new(
          id: id,
          amount: Shared::V1::Money.new(minor_units: amount_minor_units, currency: CURRENCY),
          from_wallet_id: from,
          to_wallet_id: to,
          stage: false,
          mint_source: mint_source
        )
      end

      # The dependency map that runs `ids` in order: each waits for the one
      # before it. The first has no entry, so it's the DAG root, which is the
      # leg Go pre-flights and the one that gates the rest.
      sig { params(ids: T::Array[String]).returns(T::Hash[String, T.untyped]) }
      def self.chain(ids)
        ids.drop(1).each_with_index.to_h do |child, index|
          [child, Transaction::V1::TransferIdList.new(transfer_id: [ids.fetch(index)])]
        end
      end
    end
  end
end
