# frozen_string_literal: true
# typed: strict

module LendingSimulation
  # Names for a run's parties, so the graph reads like a market rather than a
  # list of ids. Every entity name starts with the run's tag, which is how
  # `moneyFlow(namePrefix:)` picks one run out from the rest.
  module Names
    STREETS = T.let(%w[Oak Maple Cedar Elm Pine Willow Birch Magnolia Peachtree Hickory Juniper Laurel Chestnut
                       Sycamore Dogwood Cypress Aspen Hawthorn Linden Poplar].freeze, T::Array[String])
    SUFFIXES = T.let(%w[Street Avenue Drive Lane Road Court Way Place Circle Trail].freeze, T::Array[String])
    BUILDERS = T.let(%w[Keystone Cornerstone Brickyard Ridgeline Northgate Fairview Stonebridge Harbor Summit
                        Riverbend].freeze, T::Array[String])

    sig { params(tag: String, number: Integer).returns(String) }
    def self.investor(tag, number) = format('%<tag>s investor %<n>02d', tag: tag, n: number)

    sig { params(tag: String, number: Integer).returns(String) }
    def self.borrower(tag, number)
      "#{tag} #{BUILDERS.fetch((number - 1) % BUILDERS.size)} Homes #{number}"
    end

    sig { params(tag: String).returns(String) }
    def self.issuer(tag) = "#{tag} platform"

    # A property address, labelled with the loan's grade and rate the way a
    # listing shows them.
    sig { params(random: Random, grade: String, annual_rate_bps: Integer).returns(String) }
    def self.property(random, grade:, annual_rate_bps:)
      address = "#{random.rand(100..9_999)} #{STREETS.sample(random: random)} #{SUFFIXES.sample(random: random)}"
      format('%<address>s (%<grade>s, %<rate>.2f%%)', address: address, grade: grade, rate: annual_rate_bps / 100.0)
    end
  end
end
