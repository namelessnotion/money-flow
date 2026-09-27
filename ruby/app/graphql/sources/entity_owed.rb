# frozen_string_literal: true
# typed: strict

module Sources
  # Batches what entities owe for late ACH deposit returns, so a page of
  # entities costs three queries rather than three per entity.
  class EntityOwed < GraphQL::Dataloader::Source
    sig { params(entity_ids: T::Array[Integer]).returns(T::Array[Integer]) }
    def fetch(entity_ids)
      owed = Services::Ach::Owed.for_each(entity_ids)
      entity_ids.map { |id| owed.fetch(id).for_gate }
    end
  end
end
