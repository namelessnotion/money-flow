# frozen_string_literal: true
# typed: strict

module LendingSimulation
  # Minor units as dollars, for the run's log.
  module Money
    sig { params(minor_units: Integer).returns(String) }
    def self.format(minor_units)
      dollars, cents = minor_units.abs.divmod(100)
      grouped = dollars.to_s.reverse.scan(/\d{1,3}/).join(',').reverse
      "#{'-' if minor_units.negative?}$#{grouped}.#{cents.to_s.rjust(2, '0')}"
    end
  end
end
