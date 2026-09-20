# frozen_string_literal: true
# typed: strict

module Models
  # One Investor's fractional purchase of a Security: the intent and the ids it
  # was sent under. Whether it went through comes from the projection on its id.
  class Subscription < Sequel::Model
    # The id is the Go Transaction's id, chosen by Ruby before Go is called.
    unrestrict_primary_key

    many_to_one :security
    many_to_one :investor, class: 'Models::Entity', key: :investor_entity_id
  end
end
