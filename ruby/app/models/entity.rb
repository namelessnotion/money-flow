# frozen_string_literal: true
# typed: strict

module Models
  # persisted database model for an entity
  class Entity < Sequel::Model
    one_to_many :accounts
    one_to_many :ach_transactions
  end
end
