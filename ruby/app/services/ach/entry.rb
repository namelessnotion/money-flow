# frozen_string_literal: true
# typed: strict

module Services
  module Ach
    # What is handed to the ACH provider to originate: one debit (deposit) or
    # credit (withdrawal) against the entity's linked bank account.
    class Entry < T::Struct
      # Also the idempotency key the provider sees: resubmitting the same
      # Transaction must never originate a second entry.
      const :transaction_id, String
      const :direction, Types::Enums::AchDirection
      const :amount_minor_units, Integer
      const :currency, String
      const :bank_wallet_id, String
    end
  end
end
