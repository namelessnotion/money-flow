# frozen_string_literal: true
# typed: strict

module Sources
  # Batches Position lookups by Security, so a page of Securities costs a
  # fixed number of queries rather than two per row.
  #
  # Dataloader rather than Sequel's `.eager`: a Position is not an association
  # but a fold over Subscriptions, Disbursements and their projections, so
  # eager-loading would not reach it at all.
  class SecurityPositions < GraphQL::Dataloader::Source
    sig do
      params(security_ids: T::Array[String])
        .returns(T::Array[T::Array[Services::Securities::Positions::Position]])
    end
    def fetch(security_ids)
      by_security = Services::Securities::Positions.for_securities(security_ids)
      security_ids.map { |id| by_security.fetch(id, []) }
    end
  end
end
