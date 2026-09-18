# frozen_string_literal: true
# typed: strict

require_relative 'entry'

module Services
  module Ach
    # The port to whichever ACH provider originates entries on the network.
    #
    # Submission is the only call Ruby makes to the provider. What happens
    # next — the entry settling, or coming back as a return — the provider
    # reports asynchronously, and reaches Ruby as a call to Settle or Return.
    module Provider
      extend T::Helpers

      interface!

      class SubmissionFailed < StandardError; end

      # Submits `entry` for origination and returns the provider's reference
      # for it. Must be idempotent on entry.transaction_id. Raises
      # SubmissionFailed when the provider refuses the entry.
      sig { abstract.params(entry: Entry).returns(String) }
      def submit(entry); end
    end
  end
end
