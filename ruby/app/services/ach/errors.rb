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
  end
end
