# frozen_string_literal: true
# typed: strict

require_relative 'interest'
require_relative 'positions'

module Services
  module Securities
    # What a Security owes as of a given day: the principal still outstanding,
    # and the simple interest accrued on it since the money was drawn.
    #
    # The draw date comes from the draw Transaction's `state_changed_at` — Go's
    # clock, not Ruby's. The day a Borrower started owing interest is the day
    # the ledger says they got the money, and nothing else is defensible.
    module Schedule
      # What is due, and the reckoning behind it.
      class Due < T::Struct
        const :principal_minor_units, Integer
        const :interest_minor_units, Integer
        # Days of accrual behind the interest figure, so a caller can explain it.
        const :days, Integer

        sig { returns(Integer) }
        def total_minor_units = principal_minor_units + interest_minor_units
      end

      # Everything owed on `security` as of `as_of`. Zero interest before the
      # money is drawn, because nothing has been borrowed yet.
      sig { params(security: Models::Security, as_of: Date).returns(Due) }
      def self.due(security:, as_of:)
        principal = Positions.outstanding_principal(security)
        days = days_since_draw(security, as_of)

        Due.new(
          principal_minor_units: principal,
          interest_minor_units: Interest.accrued(principal_minor_units: principal,
                                                 annual_rate_bps: security.annual_rate_bps, days: days),
          days: days
        )
      end

      # Nil until the read model has seen the draw complete.
      sig { params(security: Models::Security).returns(T.nilable(Date)) }
      def self.drawn_on(security)
        transaction_id = security.draw_transaction_id
        return nil if transaction_id.nil?

        projection = Models::TransactionProjection[transaction_id]
        return nil if projection.nil?
        return nil unless projection.state == Types::Enums::TransactionState::Completed.serialize

        # Go's time where the projection carries it, else when Ruby last saw
        # the row change — the same coalesce ClearDue uses for a completion.
        (projection.state_changed_at || projection.updated_at).to_date
      end

      sig { params(security: Models::Security, as_of: Date).returns(Integer) }
      def self.days_since_draw(security, as_of)
        drawn = drawn_on(security)
        return 0 if drawn.nil?

        [(as_of - drawn).to_i, 0].max
      end
      private_class_method :days_since_draw
    end
  end
end
