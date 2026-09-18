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
  end
end
