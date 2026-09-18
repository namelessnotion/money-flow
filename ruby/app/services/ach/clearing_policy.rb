# frozen_string_literal: true
# typed: strict

require_relative 'business_calendar'

module Services
  module Ach
    # When an ACH deposit's money stops being uncleared: from the start of the
    # third Federal Reserve business day after its Transaction completed, until
    # which an ACH return could still claw it back.
    module ClearingPolicy
      BUSINESS_DAYS = 3

      # The Eastern-time date the deposit becomes due for clearing.
      sig { params(completed_at: Time).returns(Date) }
      def self.due_on(completed_at)
        BusinessCalendar.add_business_days(BusinessCalendar.date_of(completed_at), BUSINESS_DAYS)
      end

      # Whether it is due as of `now`. Stays true after the due date, so a
      # sweep that missed a day still clears it.
      sig { params(completed_at: Time, now: Time).returns(T::Boolean) }
      def self.due?(completed_at, now:)
        BusinessCalendar.date_of(now) >= due_on(completed_at)
      end
    end
  end
end
