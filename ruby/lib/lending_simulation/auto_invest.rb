# frozen_string_literal: true
# typed: strict

require_relative 'quantiles'

module LendingSimulation
  # One day's auto-invest: Investors with idle cash spread it across the open
  # offerings in positions of the profile's size, oldest offering first — the
  # way Groundfloor's auto-investor spreads small, even pieces across many
  # loans.
  #
  # A pure plan over the cash the Book says each Investor has. Nothing here
  # touches the ledger; the Market sends what it returns.
  module AutoInvest
    # An offering still open, and what of it has not been sold.
    class Offering < T::Struct
      const :security_id, String
      const :remaining_minor_units, Integer
    end

    # One Subscription to send.
    class PlannedPurchase < T::Struct
      const :security_id, String
      const :investor_entity_id, Integer
      const :amount_minor_units, Integer
    end

    # At most `buyers_per_offering` Investors buy into each offering. A buyer
    # whose position would overrun what remains takes exactly what remains,
    # so an offering with enough interest fills to the cent.
    sig do
      params(offerings: T::Array[Offering], cash: T::Hash[Integer, Integer], position: Quantiles,
             buyers_per_offering: Integer, random: Random).returns(T::Array[PlannedPurchase])
    end
    def self.plan(offerings:, cash:, position:, buyers_per_offering:, random:)
      idle = cash.dup
      offerings.flat_map do |offering|
        buyers = idle.keys.sort.shuffle(random: random).select { |id| idle.fetch(id).positive? }
        fill(offering, buyers.first(buyers_per_offering), idle, position, random)
      end
    end

    sig do
      params(offering: Offering, buyers: T::Array[Integer], idle: T::Hash[Integer, Integer], position: Quantiles,
             random: Random).returns(T::Array[PlannedPurchase])
    end
    def self.fill(offering, buyers, idle, position, random)
      remaining = offering.remaining_minor_units
      buyers.filter_map do |investor_id|
        amount = [position.sample(random), idle.fetch(investor_id), remaining].min
        next unless amount.positive?

        remaining -= amount
        idle[investor_id] = idle.fetch(investor_id) - amount
        PlannedPurchase.new(security_id: offering.security_id, investor_entity_id: investor_id,
                            amount_minor_units: amount)
      end
    end
    private_class_method :fill
  end
end
