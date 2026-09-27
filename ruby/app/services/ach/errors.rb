# frozen_string_literal: true
# typed: strict

module Services
  module Ach
    # Go, or the ACH provider, declined a well-formed command; the message is
    # its reason. Sending it again cannot help.
    class Refused < StandardError; end

    # Go could not be reached, or failed, after every retry. Every Go command
    # here is idempotent per id, so the same call is safe to make again later.
    class Unavailable < StandardError; end

    class NotFound < StandardError; end

    class InvalidAmount < StandardError; end

    # Only a deposit mints uncleared cash; there is nothing else to clear.
    class NotClearable < StandardError; end

    # The entry may not reach the provider yet, or ever: its real leg is not
    # staged, or Go says the Transaction is no longer running. Raised rather
    # than waited on, because the caller — a sweep — will look again, and a
    # Transaction that has gone terminal needs a person rather than a retry
    # (ruby/docs/adr/0004).
    class NotSubmittable < StandardError; end

    # The entity lacks an account an ACH shape moves money through.
    class MissingAccount < StandardError; end

    # The entity owes the platform for a late deposit return, so it may not
    # send money where Recovery can't reach it: out by ACH, or into a
    # Security's escrow (ruby/docs/adr/0011, decision 6).
    class Owes < StandardError
      extend T::Sig

      sig { params(entity_id: Integer, owed: Integer, what: String).returns(Owes) }
      def self.refusing(entity_id, owed, what)
        new("entity #{entity_id} owes #{owed} for a late ACH return; no #{what} until it is recovered")
      end
    end
  end
end
