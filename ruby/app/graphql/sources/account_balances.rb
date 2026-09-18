# frozen_string_literal: true
# typed: strict

module Sources
  # Batches Account balance lookups by wallet_uuid, so a page of Accounts
  # costs one query rather than one per Account.
  class AccountBalances < GraphQL::Dataloader::Source
    sig { params(wallet_uuids: T::Array[String]).returns(T::Array[T::Array[Models::AccountBalance]]) }
    def fetch(wallet_uuids)
      totals = Models::TokenBalanceProjection.totals_for(wallet_uuids)
      wallet_uuids.map { |wallet_uuid| totals.fetch(wallet_uuid, []) }
    end
  end
end
