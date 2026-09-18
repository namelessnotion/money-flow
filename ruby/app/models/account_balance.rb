# frozen_string_literal: true
# typed: strict

module Models
  # What an Account holds in one currency: the sum over its Wallet's Tokens of
  # the balances Go publishes for each. Posted money has settled on the
  # ledger; pending money is reserved by a staged Transfer that has yet to be
  # posted (it moves) or voided (it stays).
  class AccountBalance < T::Struct
    const :currency, String
    # Signed: an Account whose Wallet allows both onramp and offramp can go
    # negative.
    const :posted, Integer
    # Reserved to leave the Account.
    const :pending_outgoing, Integer
    # Reserved to arrive in the Account.
    const :pending_incoming, Integer

    # A value: two balances holding the same amounts are the same balance.
    sig { params(other: BasicObject).returns(T::Boolean) }
    def ==(other)
      T.unsafe(other).is_a?(AccountBalance) && T.cast(other, AccountBalance).serialize == serialize
    end
    alias eql? ==

    sig { returns(Integer) }
    def hash = serialize.hash
  end
end
