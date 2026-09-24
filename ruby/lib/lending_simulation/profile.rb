# frozen_string_literal: true
# typed: strict

require 'yaml'
require_relative 'quantiles'

module LendingSimulation
  # The market a run imitates, from config/simulation/*.yml: what loans look
  # like, how they pay off, and how Investors behave. Sizes are scaled as the
  # file says when it is loaded, so everything downstream sees simulation-sized
  # amounts only.
  class Profile < T::Struct
    DEFAULT_PATH = T.let(File.expand_path('../../config/simulation/groundfloor_like.yml', __dir__), String)
    DAYS_PER_YEAR = 365
    MONTHS_PER_YEAR = 12

    # A credit grade: how often it is offered, and what it pays.
    class Grade < T::Struct
      const :name, String
      const :weight, Integer
      const :annual_rate_bps, Quantiles
    end

    # What one loan will be, drawn before it is offered.
    class LoanTerms < T::Struct
      const :principal_minor_units, Integer
      const :grade, String
      const :annual_rate_bps, Integer
      const :term_days, Integer
      # Days after maturity it pays off: negative is early, positive is an
      # extension.
      const :payoff_offset_days, Integer

      # How long after the Draw the Borrower pays it off: never sooner than
      # `minimum`, however early the offset.
      sig { params(minimum: Integer).returns(Integer) }
      def days_to_payoff(minimum:) = [term_days + payoff_offset_days, minimum].max
    end

    # How Investors come and go.
    class InvestorBehaviour < T::Struct
      const :initial_deposit, Quantiles
      const :top_up_probability_per_day, Float
      const :top_up, Quantiles
      const :withdrawal_probability_per_day, Float
      const :withdrawal_share_percent, Quantiles
    end

    # How the market as a whole moves.
    class MarketBehaviour < T::Struct
      const :offerings_per_day, Float
      const :buyers_per_offering_per_day, Integer
      const :minimum_days_drawn, Integer
    end

    const :loan_amount, Quantiles
    const :grades, T::Array[Grade]
    const :term_months, T::Hash[Integer, Integer]
    const :payoff_offset_days, Quantiles
    const :position, Quantiles
    const :investor, InvestorBehaviour
    const :market, MarketBehaviour

    sig { params(path: String).returns(Profile) }
    def self.load(path)
      from_config(ProfileConfig.new(YAML.safe_load_file(path)))
    end

    sig { params(config: ProfileConfig).returns(Profile) }
    def self.from_config(config)
      new(loan_amount: config.quantiles('loan_amount_minor_units').scaled(config.scale('loan_amount')),
          grades: config.grades, term_months: config.weights('term_months'),
          payoff_offset_days: config.quantiles('payoff_offset_days'),
          position: config.quantiles('position_minor_units').scaled(config.scale('position')),
          investor: config.investor, market: config.market)
    end

    sig { params(random: Random).returns(LoanTerms) }
    def loan(random)
      grade = weighted(grades.to_h { |g| [g, g.weight] }, random)
      LoanTerms.new(principal_minor_units: principal(random), grade: grade.name,
                    annual_rate_bps: grade.annual_rate_bps.sample(random), term_days: term_days(random),
                    payoff_offset_days: payoff_offset_days.sample(random))
    end

    private

    # In whole dollars, the way a loan is priced.
    sig { params(random: Random).returns(Integer) }
    def principal(random) = (loan_amount.sample(random) / 100.0).round * 100

    sig { params(random: Random).returns(Integer) }
    def term_days(random) = (weighted(term_months, random) * DAYS_PER_YEAR / Float(MONTHS_PER_YEAR)).round

    sig do
      type_parameters(:T)
        .params(weights: T::Hash[T.type_parameter(:T), Integer], random: Random).returns(T.type_parameter(:T))
    end
    def weighted(weights, random)
      point = random.rand(weights.values.sum)
      weights.each do |choice, weight|
        return choice if point < weight

        point -= weight
      end
      raise ArgumentError, 'weights must sum to more than zero'
    end
  end
end

require_relative 'profile_config'
