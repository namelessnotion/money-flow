# frozen_string_literal: true
# typed: strict

require_relative 'book'
require_relative 'platform'
require_relative 'profile'

module LendingSimulation
  # How big a market, and for how long.
  class Settings < T::Struct
    const :tag, String
    const :seed, Integer
    const :investors, Integer
    const :borrowers, Integer
    # New loans are offered on these first days only; the run then continues
    # until every loan has paid off, or `max_days`.
    const :issue_days, Integer
    const :max_days, Integer
    const :start_date, Date
    # The least real time one simulated day takes, so the completion times
    # the money flow is dated by stay proportional to simulated time.
    const :seconds_per_day, Float
  end

  # One loan, from offering to payoff.
  class Loan < T::Struct
    const :security_id, String
    const :borrower_id, Integer
    const :terms, Profile::LoanTerms
    prop :remaining_minor_units, Integer
    prop :drawn_day, T.nilable(Integer)
    prop :payoff_day, T.nilable(Integer)
    prop :repaid, T::Boolean, default: false

    sig { returns(T::Boolean) }
    def funded? = remaining_minor_units.zero?
  end

  # Everything a run's actors share: the day, the parties, the loans, the
  # Book, and the platform they all move money through. Money crossing the
  # bank boundary goes through here, so the Book can never miss a movement.
  class World
    sig { returns(Settings) }
    attr_reader :settings

    sig { returns(Profile) }
    attr_reader :profile

    sig { returns(Platform) }
    attr_reader :platform

    sig { returns(Random) }
    attr_reader :random

    sig { returns(Book) }
    attr_reader :book

    sig { returns(T::Hash[String, Loan]) }
    attr_reader :loans

    sig { returns(Integer) }
    attr_accessor :day, :issuer_id

    sig { returns(T::Array[Integer]) }
    attr_accessor :investor_ids, :borrower_ids

    sig { params(settings: Settings, profile: Profile, platform: Platform).void }
    def initialize(settings:, profile:, platform:)
      @settings = settings
      @profile = profile
      @platform = platform
      @random = T.let(Random.new(settings.seed), Random)
      @book = T.let(Book.new, Book)
      @loans = T.let({}, T::Hash[String, Loan])
      @day = T.let(0, Integer)
      @issuer_id = T.let(0, Integer)
      @investor_ids = T.let([], T::Array[Integer])
      @borrower_ids = T.let([], T::Array[Integer])
    end

    sig { returns(Date) }
    def today = settings.start_date + day

    # Deposits, cleared and spendable once this returns.
    sig { params(amounts: T::Hash[Integer, Integer]).void }
    def deposit(amounts)
      return if amounts.empty?

      platform.deposit(amounts)
      amounts.each { |id, amount| book.deposit(id, amount) }
    end

    # Withdrawals, gone once this returns.
    sig { params(amounts: T::Hash[Integer, Integer]).void }
    def withdraw(amounts)
      return if amounts.empty?

      amounts.each { |id, amount| book.withdraw(id, amount) }
      platform.withdraw(amounts)
    end
  end
end
