# frozen_string_literal: true
# typed: strict

module Models
  # Lagging read model of one Go Transfer or Reversal aggregate, keyed by its
  # id. Written only by Consumer::Projector.
  class TransferProjection < Sequel::Model
    unrestrict_primary_key
  end
end
