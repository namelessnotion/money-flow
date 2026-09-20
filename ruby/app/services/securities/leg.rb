# frozen_string_literal: true
# typed: strict

require_relative '../../../gen/proto/transaction/v1/transaction_pb'

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
    module Leg
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
          auto_process: true,
          stage: false,
          mint_source: mint_source
        )
      end

      # The dependency map saying `child` waits for `parent`. A child with no
      # entry is a DAG root, which is how a shape says "this one gates".
      sig { params(child: String, parent: String).returns(T::Hash[String, T.untyped]) }
      def self.after(child, parent)
        { child => Transaction::V1::TransferIdList.new(transfer_id: [parent]) }
      end
    end
  end
end
