# frozen_string_literal: true
# typed: strict

module Sources
  # Batches Entity lookups by id, so a page of rows that each name a party —
  # a Security's issuer and borrower, a Position's or Disbursement's investor —
  # costs one query rather than one per row.
  #
  # Dataloader rather than Sequel's `.eager`: eager-loading silently degrades
  # under graphql-ruby's connection wrapper for anything reached from a
  # connection's nodes, which is exactly where these are read from.
  class EntitiesById < GraphQL::Dataloader::Source
    sig { params(ids: T::Array[Integer]).returns(T::Array[T.nilable(Models::Entity)]) }
    def fetch(ids)
      by_id = Models::Entity.where(id: ids).all.to_h { |entity| [entity.id, entity] }
      ids.map { |id| by_id[id] }
    end
  end
end
