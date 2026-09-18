# frozen_string_literal: true
# typed: strict

require_relative 'provider'

module Services
  module Ach
    # Stands in for a real ACH provider until one is integrated. Accepts every
    # entry, and derives its reference from the Transaction id so that
    # resubmitting is idempotent, as the port requires. Settlement and returns
    # are driven by hand through the settleAch / returnAch mutations.
    class FakeProvider
      include Provider

      sig { override.params(entry: Entry).returns(String) }
      def submit(entry)
        "fake-ach-#{entry.transaction_id}"
      end
    end
  end
end
