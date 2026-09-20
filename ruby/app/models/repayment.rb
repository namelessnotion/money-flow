# frozen_string_literal: true
# typed: strict

module Models
  # One payment from the Borrower into a Security, split into the part that
  # repays principal and the part that pays interest. The ledger moves the two
  # as one amount; the split is the business fact the allocation reads.
  class Repayment < Sequel::Model
    # The id is the Go Transaction's id, chosen by Ruby before Go is called.
    unrestrict_primary_key

    many_to_one :security
    one_to_many :disbursements
  end
end
