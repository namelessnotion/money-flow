# frozen_string_literal: true
# typed: strict

require_relative 'errors'

module Services
  module Securities
    # Splits an amount across weighted holders, in whole minor units.
    #
    # Largest remainder: every holder first takes the floor of their exact
    # share, then the units left over are handed out one each to the largest
    # fractional remainders. Ties go to the lower key.
    #
    # The tie-break is not about fairness, it is about determinism. A
    # Disbursement's Transaction id is derived from the Repayment and the
    # Investor (ruby/docs/adr/0007) while its *amount* is not, so a sweep that
    # re-runs has to compute the same amount it computed the first time —
    # otherwise Go's idempotency returns the Transaction that moved the first
    # amount while Ruby records the second, and the two disagree forever.
    #
    # Remainders are compared as exact integers (the numerator over the common
    # denominator `total_weight`), never as floats: a float comparison here
    # would make the tie-break depend on rounding error.
    module ProRata
      # Each key's share of `total`, in proportion to its weight. The parts sum
      # to exactly `total` — asserted below rather than merely tested, because
      # a rounding rule that quietly loses a minor unit is the failure this
      # method exists to prevent, and it would surface months later as a
      # Repayment that will not reconcile.
      sig { params(total: Integer, weights: T::Hash[Integer, Integer]).returns(T::Hash[Integer, Integer]) }
      def self.allocate(total:, weights:)
        validate!(total, weights)

        total_weight = weights.each_value.sum
        return nothing_for(total, weights) if total_weight.zero?

        parts = floors(total, weights, total_weight)
        distribute(parts, remainders(total, weights, total_weight), total - parts.each_value.sum)

        raise AllocationError, "allocated #{parts.each_value.sum} of #{total}" unless parts.each_value.sum == total

        parts
      end

      sig { params(total: Integer, weights: T::Hash[Integer, Integer]).void }
      def self.validate!(total, weights)
        raise AllocationError, "total must not be negative, got #{total}" if total.negative?

        negative = weights.select { |_, weight| weight.negative? }
        raise AllocationError, "weight must not be negative: #{negative.inspect}" unless negative.empty?
      end
      private_class_method :validate!

      # Nothing to take a proportion of. Splitting nothing across holders who
      # hold nothing is every holder getting nothing; splitting *something*
      # that way has no answer at all.
      sig { params(total: Integer, weights: T::Hash[Integer, Integer]).returns(T::Hash[Integer, Integer]) }
      def self.nothing_for(total, weights)
        raise AllocationError, "no proportion to split #{total} by: every weight is zero" unless total.zero?

        weights.transform_values { 0 }
      end
      private_class_method :nothing_for

      sig do
        params(total: Integer, weights: T::Hash[Integer, Integer], total_weight: Integer)
          .returns(T::Hash[Integer, Integer])
      end
      def self.floors(total, weights, total_weight)
        weights.transform_values { |weight| total * weight / total_weight }
      end
      private_class_method :floors

      # Keys ordered by the share they were short-changed, largest first, then
      # by key so a tie always resolves the same way.
      sig do
        params(total: Integer, weights: T::Hash[Integer, Integer], total_weight: Integer)
          .returns(T::Array[Integer])
      end
      def self.remainders(total, weights, total_weight)
        weights.keys.sort_by { |key| [-((total * weights.fetch(key)) % total_weight), key] }
      end
      private_class_method :remainders

      sig { params(parts: T::Hash[Integer, Integer], order: T::Array[Integer], leftover: Integer).void }
      def self.distribute(parts, order, leftover)
        order.first(leftover).each { |key| parts[key] = parts.fetch(key) + 1 }
      end
      private_class_method :distribute
    end
  end
end
