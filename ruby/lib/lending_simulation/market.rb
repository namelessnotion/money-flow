# frozen_string_literal: true
# typed: strict

require 'logger'
require_relative 'investors'
require_relative 'lending'
require_relative 'money'
require_relative 'names'
require_relative 'world'

module LendingSimulation
  # A Groundfloor-like market played out one simulated day at a time.
  #
  # Each day, in order: Investors top up; new loans are offered; Investors
  # auto-invest into what is open; fully subscribed Securities are drawn to
  # their Borrowers, who withdraw the money to spend on the property; loans
  # due pay off from a deposit of the sale's proceeds and are disbursed to
  # their holders; and now and then an Investor withdraws.
  class Market
    Role = Types::Enums::EntityRole

    sig { returns(World) }
    attr_reader :world

    sig { params(world: World, logger: ::Logger).void }
    def initialize(world:, logger:)
      @world = world
      @logger = logger
      @investors = T.let(Investors.new(world), Investors)
      @lending = T.let(Lending.new(world), Lending)
    end

    sig { void }
    def run
      open_market
      until finished?
        paced { tick }
        @world.day += 1
      end
    end

    # Every Investor and Borrower in the run: whose cash the Book mirrors.
    sig { returns(T::Array[Integer]) }
    def entity_ids = @world.investor_ids + @world.borrower_ids

    private

    sig { returns(T::Boolean) }
    def finished?
      return true if @world.day >= @world.settings.max_days

      @world.day >= @world.settings.issue_days && @world.loans.values.all?(&:repaid)
    end

    # Everyone joins, and every Investor makes a first deposit.
    sig { void }
    def open_market
      settings = @world.settings
      tag = settings.tag
      @world.issuer_id = @world.platform.onboard(Names.issuer(tag), Role::Issuer)
      @world.borrower_ids = onboard(settings.borrowers, Role::Borrower) { |n| Names.borrower(tag, n) }
      @world.investor_ids = onboard(settings.investors, Role::Investor) { |n| Names.investor(tag, n) }
      first_deposits
    end

    sig do
      params(count: Integer, role: Role, name: T.proc.params(number: Integer).returns(String))
        .returns(T::Array[Integer])
    end
    def onboard(count, role, &name)
      (1..count).map { |number| @world.platform.onboard(name.call(number), role) }
    end

    sig { void }
    def first_deposits
      initial = @world.profile.investor.initial_deposit
      amounts = @world.investor_ids.to_h { |id| [id, initial.sample(@world.random)] }
      @world.deposit(amounts)
      @logger.info("#{amounts.size} investors and #{@world.borrower_ids.size} borrowers, " \
                   "#{Money.format(amounts.values.sum)} deposited")
    end

    sig { void }
    def tick
      @investors.top_up
      @lending.offer
      @investors.invest
      @lending.draw
      @lending.pay_off
      @investors.withdraw
      progress if (@world.day % 30).zero?
    end

    sig { void }
    def progress
      loans = @world.loans.values
      @logger.info(format('day %<day>4d %<date>s: %<open>d open, %<drawn>d drawn, %<repaid>d repaid',
                          day: @world.day, date: @world.today, open: loans.count { |l| l.drawn_day.nil? },
                          drawn: loans.count { |l| l.drawn_day && !l.repaid }, repaid: loans.count(&:repaid)))
    end

    sig { params(block: T.proc.void).void }
    def paced(&block)
      started = Process.clock_gettime(Process::CLOCK_MONOTONIC)
      block.call
      spare = @world.settings.seconds_per_day - (Process.clock_gettime(Process::CLOCK_MONOTONIC) - started)
      sleep(spare) if spare.positive?
    end
  end
end
