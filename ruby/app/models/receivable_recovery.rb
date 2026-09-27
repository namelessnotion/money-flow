# frozen_string_literal: true
# typed: strict

module Models
  # One Recovery of what an entity owes (ruby/docs/adr/0011). The row is a
  # record of what was sent, written before Go is asked; its outcome is Go's,
  # read from the projection by its id.
  class ReceivableRecovery < Sequel::Model
    unrestrict_primary_key

    many_to_one :entity
  end
end
