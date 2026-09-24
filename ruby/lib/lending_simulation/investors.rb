# frozen_string_literal: true
# typed: strict

require_relative 'auto_invest'
require_relative 'world'

module LendingSimulation
  # What Investors do each day: now and then top up, auto-invest their idle
  # cash into open offerings, and now and then take some of it out.
  class Investors
    sig { params(world: World).void }
    def initialize(world)
      @world = world
    end

    sig { void }
    def top_up
      topping = chosen(behaviour.top_up_probability_per_day)
      @world.deposit(topping.to_h { |id| [id, behaviour.top_up.sample(random)] })
    end

    sig { void }
    def invest
      offerings = open_offerings
      return if offerings.empty?

      profile = @world.profile
      subscribe(AutoInvest.plan(offerings: offerings, cash: book.cleared_balances(@world.investor_ids),
                                position: profile.position, random: random,
                                buyers_per_offering: profile.market.buyers_per_offering_per_day))
    end

    # A share of what they could take out: never more than their cash wallet
    # backs (see Book).
    sig { void }
    def withdraw
      amounts = chosen(behaviour.withdrawal_probability_per_day).to_h do |id|
        [id, book.withdrawable(id) * behaviour.withdrawal_share_percent.sample(random) / 100]
      end
      @world.withdraw(amounts.select { |_id, amount| amount.positive? })
    end

    private

    # Money leaves the Book when a Subscription is sent, and comes back if it
    # did not complete.
    sig { params(planned: T::Array[AutoInvest::PlannedPurchase]).void }
    def subscribe(planned)
      return if planned.empty?

      planned.each { |p| book.spend(p.investor_entity_id, p.amount_minor_units) }
      @world.platform.purchase(planned).each { |purchase, completed| settle(purchase, completed) }
    end

    sig { params(purchase: AutoInvest::PlannedPurchase, completed: T::Boolean).void }
    def settle(purchase, completed)
      if completed
        @world.loans.fetch(purchase.security_id).remaining_minor_units -= purchase.amount_minor_units
      else
        book.receive(purchase.investor_entity_id, purchase.amount_minor_units)
      end
    end

    sig { returns(T::Array[AutoInvest::Offering]) }
    def open_offerings
      @world.loans.values.reject(&:funded?).map do |loan|
        AutoInvest::Offering.new(security_id: loan.security_id, remaining_minor_units: loan.remaining_minor_units)
      end
    end

    # The Investors who act today, each with this chance.
    sig { params(chance: Float).returns(T::Array[Integer]) }
    def chosen(chance) = @world.investor_ids.select { random.rand < chance }

    sig { returns(Profile::InvestorBehaviour) }
    def behaviour = @world.profile.investor

    sig { returns(Book) }
    def book = @world.book

    sig { returns(Random) }
    def random = @world.random
  end
end
