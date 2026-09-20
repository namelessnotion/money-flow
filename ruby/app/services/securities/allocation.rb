# frozen_string_literal: true
# typed: strict

require_relative 'errors'
require_relative 'positions'
require_relative 'pro_rata'

module Services
  module Securities
    # How one Repayment is split across the holders of its Security: pro rata
    # by outstanding principal, principal and interest split separately.
    #
    # Separately is the part that matters. Allocating the combined total and
    # then splitting it back into the two would let a holder's *principal*
    # share come out above their outstanding principal on a rounding boundary,
    # and their retirement leg would then be refused by the `investment`
    # wallet's debits_must_not_exceed_credits — turning a rounding choice into
    # a Disbursement stuck forever.
    module Allocation
      # What one holder is owed of one Repayment.
      class Share < T::Struct
        const :investor_entity_id, Integer
        const :principal_minor_units, Integer
        const :interest_minor_units, Integer

        sig { returns(Integer) }
        def total_minor_units = principal_minor_units + interest_minor_units
      end

      # Every holder's share, ordered by entity id. Holders owed nothing are
      # dropped: there would be no leg to send them, and Go refuses a
      # zero-amount Transfer.
      sig { params(repayment: Models::Repayment).returns(T::Array[Share]) }
      def self.for(repayment)
        positions = Positions.of(security_of(repayment))
        weights = positions.to_h { |p| [p.investor_entity_id, p.outstanding_principal_minor_units] }

        shares = split(repayment, weights).reject { |share| share.total_minor_units.zero? }
        verify!(repayment, positions, shares)
        shares
      end

      # Looked up rather than reached through `repayment.security`: the tapioca
      # compiler generates column accessors for a Sequel model but not its
      # associations, so the association would be untyped here. The models
      # still declare it — GraphQL eager-loads through it.
      sig { params(repayment: Models::Repayment).returns(Models::Security) }
      def self.security_of(repayment)
        Models::Security[repayment.security_id] ||
          raise(NotFound, "repayment #{repayment.id} names no security #{repayment.security_id}")
      end

      sig do
        params(repayment: Models::Repayment, weights: T::Hash[Integer, Integer]).returns(T::Array[Share])
      end
      def self.split(repayment, weights)
        return [] if weights.empty?

        principal = ProRata.allocate(total: repayment.principal_minor_units, weights: weights)
        interest = ProRata.allocate(total: repayment.interest_minor_units, weights: weights)

        weights.keys.sort.map do |entity_id|
          Share.new(investor_entity_id: entity_id,
                    principal_minor_units: principal.fetch(entity_id),
                    interest_minor_units: interest.fetch(entity_id))
        end
      end
      private_class_method :split

      # Asserted rather than merely tested. A split that does not add up is a
      # Disbursement that can never be reconciled against its Repayment, and a
      # principal share above what a holder holds is one that can never be
      # sent at all — both worth failing at the call site rather than at a
      # ledger reconciliation months later.
      sig do
        params(repayment: Models::Repayment, positions: T::Array[Positions::Position], shares: T::Array[Share])
          .void
      end
      def self.verify!(repayment, positions, shares)
        # A Repayment against a Security with no holders has nowhere to go: the
        # Borrower's money is sitting in the repayment wallet and nobody is
        # owed it. Unreachable through the services — a Repayment needs a drawn
        # Security, which needs a fully subscribed one — so if it happens, the
        # right thing is to say so rather than quietly disburse nothing.
        check_sum!(shares.sum(&:principal_minor_units), repayment.principal_minor_units, 'principal')
        check_sum!(shares.sum(&:interest_minor_units), repayment.interest_minor_units, 'interest')
        check_holdings!(positions, shares)
      end
      private_class_method :verify!

      sig { params(allocated: Integer, total: Integer, bucket: String).void }
      def self.check_sum!(allocated, total, bucket)
        return if allocated == total

        raise AllocationError, "allocated #{allocated} of #{total} #{bucket}"
      end
      private_class_method :check_sum!

      sig { params(positions: T::Array[Positions::Position], shares: T::Array[Share]).void }
      def self.check_holdings!(positions, shares)
        outstanding = positions.to_h { |p| [p.investor_entity_id, p.outstanding_principal_minor_units] }

        shares.each do |share|
          held = outstanding.fetch(share.investor_entity_id, 0)
          next if share.principal_minor_units <= held

          raise AllocationError,
                "entity #{share.investor_entity_id} would be repaid #{share.principal_minor_units} " \
                "principal but holds #{held}"
        end
      end
      private_class_method :check_holdings!
    end
  end
end
