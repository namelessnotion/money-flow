# frozen_string_literal: true
# typed: strict

require_relative 'errors'

module Services
  module Securities
    # Simple interest on a Security's outstanding principal: actual days over a
    # 365-day year, at the Security's rate in basis points.
    #
    # Integer arithmetic end to end. The ledger counts minor units, and a rate
    # carried as a float is exactly what drifts away from them — 0.1 is not a
    # tenth, and thirty days of that is a cent nobody can account for.
    #
    # One Rational rounded once, at the end, rather than a per-day figure
    # multiplied out: rounding each day first truncates the same error thirty
    # times over. See the "rounds once at the end" example in the spec.
    module Interest
      DAYS_PER_YEAR = 365
      BPS_PER_UNIT = 10_000

      # Interest accrued over `days`, rounded half away from zero — the way a
      # servicer rounds a minor unit, rather than the way a computer truncates
      # one.
      sig do
        params(principal_minor_units: Integer, annual_rate_bps: Integer, days: Integer).returns(Integer)
      end
      def self.accrued(principal_minor_units:, annual_rate_bps:, days:)
        raise InvalidAmount, "principal must not be negative, got #{principal_minor_units}" if
          principal_minor_units.negative?
        raise InvalidAmount, "rate must not be negative, got #{annual_rate_bps} bps" if annual_rate_bps.negative?
        raise InvalidAmount, "days must not be negative, got #{days}" if days.negative?

        Rational(principal_minor_units * annual_rate_bps * days, BPS_PER_UNIT * DAYS_PER_YEAR).round
      end
    end
  end
end
