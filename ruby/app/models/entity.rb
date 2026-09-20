# frozen_string_literal: true
# typed: strict

module Models
  # persisted database model for an entity
  class Entity < Sequel::Model
    one_to_many :accounts
    one_to_many :ach_transactions

    # What the entity does in the securities market, by role. An entity holds
    # exactly one of these three sets — its role decides which.
    one_to_many :subscriptions, key: :investor_entity_id
    one_to_many :issued_securities, class: 'Models::Security', key: :issuer_entity_id
    one_to_many :borrowed_securities, class: 'Models::Security', key: :borrower_entity_id
  end
end
