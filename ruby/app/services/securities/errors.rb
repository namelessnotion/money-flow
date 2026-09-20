# frozen_string_literal: true
# typed: strict

module Services
  module Securities
    # Go declined a well-formed command; the message is its reason. Sending it
    # again cannot help.
    class Refused < StandardError; end

    # Go could not be reached, or failed, after every retry. Every Go command
    # here is idempotent per id, so the same call is safe to make again later.
    class Unavailable < StandardError; end

    class NotFound < StandardError; end

    class InvalidAmount < StandardError; end

    # An entity or a Security lacks an account a Securities shape moves money
    # through.
    class MissingAccount < StandardError; end

    # More of a Security was asked for than its Supply has left, as the read
    # model last saw it. Advisory only: the ledger is what actually refuses an
    # oversubscription, and it may refuse one this never saw coming.
    class Oversubscribed < StandardError; end

    # The Security is not fully subscribed, or its money has already been
    # drawn.
    class NotDrawable < StandardError; end

    # The Security has not been drawn, or the repayment exceeds what is still
    # owed on it.
    class NotRepayable < StandardError; end

    # The entity plays a different part in this market than the command needs —
    # a borrower asked to issue, say.
    class WrongRole < StandardError; end

    # A split did not add up to what was split. Raised by ProRata and
    # Allocation against their own results, because a rounding rule that
    # quietly loses a minor unit is the failure they exist to prevent.
    class AllocationError < StandardError; end
  end
end
