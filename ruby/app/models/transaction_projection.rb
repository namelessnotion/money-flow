# frozen_string_literal: true
# typed: strict

module Models
  # Lagging read model of one Go Transaction aggregate, keyed by its id. Written
  # only by Consumer::Projector.
  class TransactionProjection < Sequel::Model
    unrestrict_primary_key
  end
end
