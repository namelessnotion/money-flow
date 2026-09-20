# frozen_string_literal: true
# typed: ignore

# One payment from the Borrower into the Security, split into the part that
# repays principal and the part that pays interest.
#
# The split is a business fact rather than a ledger one: the collection leg
# moves principal + interest as a single amount into the Security's repayment
# wallet, and it is this row the allocation reads to decide how much of each a
# holder is owed.
#
# The primary key IS the Go Transaction's id, as everywhere else here.
Sequel.migration do
  change do
    create_table(:repayments) do
      uuid :id, primary_key: true
      foreign_key :security_id, :securities, type: :uuid, null: false, index: true
      Bignum :principal_minor_units, null: false
      Bignum :interest_minor_units, null: false
      # The day interest was accrued to, so re-reading the row explains the
      # figure without re-deriving it from a clock that has since moved.
      Date :as_of, null: false
      String :currency, null: false
      uuid :collection_transfer_id, null: false, unique: true
      DateTime :created_at, null: false, default: Sequel::SQL::Constants::CURRENT_TIMESTAMP
      DateTime :updated_at, null: false, default: Sequel::SQL::Constants::CURRENT_TIMESTAMP

      # Either part may be zero on its own — an interest-only payment, or a
      # principal paydown with no interest due.
      constraint(:repayments_parts_non_negative,
                 Sequel.lit('principal_minor_units >= 0 AND interest_minor_units >= 0'))
      # But not both. Go refuses a zero-amount Transfer, so a Repayment of
      # nothing has no collection leg to send.
      constraint(:repayments_total_positive,
                 Sequel.lit('principal_minor_units + interest_minor_units > 0'))
    end
  end
end
