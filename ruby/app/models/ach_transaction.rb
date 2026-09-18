# frozen_string_literal: true
# typed: strict

module Models
  # Ruby's record of an ACH Transaction it originated: the intent and the ids it
  # was sent under. Lifecycle state is Go's and is read from the projections,
  # never stored here.
  class AchTransaction < Sequel::Model
    # The id is the Go Transaction's id, chosen by Ruby before Go is called.
    unrestrict_primary_key

    many_to_one :entity
  end
end
