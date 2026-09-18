# frozen_string_literal: true
# typed: strict

require 'date'
require 'holidays'
require 'tzinfo'

module Services
  module Ach
    # Federal Reserve business days: the calendar ACH settles on.
    #
    # Weekends and Fed holidays are not business days. The :federalreservebanks
    # region with observance applied matches the Fed's own rule: a holiday on a
    # Sunday is observed the Monday after, but one on a Saturday is not moved to
    # the Friday — the Reserve Banks stay open. Dates are Eastern time, the
    # Fed's.
    module BusinessCalendar
      REGION = :federalreservebanks
      ZONE = T.let(TZInfo::Timezone.get('America/New_York'), TZInfo::Timezone)

      sig { params(date: Date).returns(T::Boolean) }
      def self.business_day?(date)
        !date.saturday? && !date.sunday? && Holidays.on(date, REGION, :observed).empty?
      end

      # The business day `count` business days after `date`. From a date that
      # is not itself a business day, counting starts from the next one — an
      # ACH posted on a Saturday is treated as posted Monday.
      sig { params(date: Date, count: Integer).returns(Date) }
      def self.add_business_days(date, count)
        day = next_business_day_from(date)
        count.times { day = next_business_day_from(day + 1) }
        day
      end

      # `date` itself when it is a business day, else the next one.
      sig { params(date: Date).returns(Date) }
      def self.next_business_day_from(date)
        day = date
        day += 1 until business_day?(day)
        day
      end
      private_class_method :next_business_day_from

      # The Eastern-time calendar date `time` falls on.
      sig { params(time: Time).returns(Date) }
      def self.date_of(time)
        ZONE.to_local(time).to_date
      end
    end
  end
end
