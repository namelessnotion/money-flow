# frozen_string_literal: true
# typed: strict

module LendingSimulation
  # The simulation's own mirror of what each entity holds, kept so it only
  # ever asks the ledger for what the ledger would allow. An Investor short of
  # cleared cash gets a Subscription that rolls back, and a healthy market
  # should never cause one.
  #
  # Two balances, because the ledger keeps two. **Cleared** is the cleared_cash
  # wallet that every securities Transaction moves. **Cash** is the real-money
  # wallet: only ACH moves it, and a withdrawal's real leg draws on it. So what
  # can leave is the smaller of the two. Money received on the platform (a
  # Draw, a Disbursement) is spendable there but cannot be withdrawn beyond
  # what was deposited.
  #
  # A mirror, not an authority: it changes only once the Transaction that
  # moved the money is seen complete, and the Report reconciles it against
  # the projected balances when the run ends.
  class Book
    # The simulation planned to move money it did not have. That is its own
    # bug, not the market's.
    class Overdrawn < StandardError; end

    sig { void }
    def initialize
      @cleared = T.let({}, T::Hash[Integer, Integer])
      @cash = T.let({}, T::Hash[Integer, Integer])
    end

    sig { params(entity_id: Integer).returns(Integer) }
    def cleared(entity_id) = @cleared.fetch(entity_id, 0)

    sig { params(entity_id: Integer).returns(Integer) }
    def cash(entity_id) = @cash.fetch(entity_id, 0)

    sig { params(entity_id: Integer).returns(Integer) }
    def withdrawable(entity_id) = [cleared(entity_id), cash(entity_id)].min

    sig { params(entity_ids: T::Array[Integer]).returns(T::Hash[Integer, Integer]) }
    def cleared_balances(entity_ids) = entity_ids.to_h { |id| [id, cleared(id)] }

    # An ACH deposit that has cleared.
    sig { params(entity_id: Integer, amount_minor_units: Integer).void }
    def deposit(entity_id, amount_minor_units)
      @cleared[entity_id] = cleared(entity_id) + amount_minor_units
      @cash[entity_id] = cash(entity_id) + amount_minor_units
    end

    # An ACH withdrawal: out of both.
    sig { params(entity_id: Integer, amount_minor_units: Integer).void }
    def withdraw(entity_id, amount_minor_units)
      ensure_covered!(entity_id, amount_minor_units, withdrawable(entity_id))
      @cleared[entity_id] = cleared(entity_id) - amount_minor_units
      @cash[entity_id] = cash(entity_id) - amount_minor_units
    end

    # Money paid out on the platform: a Subscription, a Repayment.
    sig { params(entity_id: Integer, amount_minor_units: Integer).void }
    def spend(entity_id, amount_minor_units)
      ensure_covered!(entity_id, amount_minor_units, cleared(entity_id))
      @cleared[entity_id] = cleared(entity_id) - amount_minor_units
    end

    # Money received on the platform: a Draw, a Disbursement, a Subscription
    # credited back because it did not complete.
    sig { params(entity_id: Integer, amount_minor_units: Integer).void }
    def receive(entity_id, amount_minor_units)
      @cleared[entity_id] = cleared(entity_id) + amount_minor_units
    end

    private

    sig { params(entity_id: Integer, amount: Integer, available: Integer).void }
    def ensure_covered!(entity_id, amount, available)
      return if amount <= available

      raise Overdrawn, "entity #{entity_id} has #{available} to move, not #{amount}"
    end
  end
end
