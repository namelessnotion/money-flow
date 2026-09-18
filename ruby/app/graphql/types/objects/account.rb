# frozen_string_literal: true
# typed: strict

require_relative 'base_object'
require_relative 'account_balance'
require_relative '../../sources/account_balances'

module Types
  # GraphQL type for the Account model
  class Account < BaseObject
    field :id, ID, null: false
    field :name, String, null: false
    field :wallet_uuid, ID, null: false
    field :type, String, null: false
    field :created_at, GraphQL::Types::ISO8601DateTime, null: false
    field :updated_at, GraphQL::Types::ISO8601DateTime, null: false
    field :balances, [Types::AccountBalance],
          null: false,
          description: "What the Account holds, one entry per currency; empty until the Account's first ledger " \
                       'activity has been consumed.'

    # DB column backing each scalar field, keyed by the field's Ruby name.
    COLUMNS_BY_FIELD = T.let({
      id: :id,
      name: :name,
      wallet_uuid: :wallet_uuid,
      type: :type,
      created_at: :created_at,
      updated_at: :updated_at
    }.freeze, T::Hash[Symbol, Symbol])

    class << self
      # Columns to select for the accounts requested under `lookahead`, restricted to
      # whatever fields were actually asked for so eager-loaded accounts don't pull
      # more than the query needs. `id` is always included: it's the record's
      # identity and Sequel needs it to match rows back to their entity.
      sig { params(lookahead: GraphQL::Execution::Lookahead).returns(T::Array[Symbol]) }
      def selected_columns(lookahead)
        columns = COLUMNS_BY_FIELD.each_with_object([:id]) do |(field, column), selected|
          selected << column if column != :id && lookahead.selects?(field)
        end
        # Balances are looked up by the Account's Wallet.
        lookahead.selects?(:balances) ? columns | [:wallet_uuid] : columns
      end
    end

    sig { returns(T::Array[Models::AccountBalance]) }
    def balances
      dataloader.with(Sources::AccountBalances).load(object.wallet_uuid)
    end
  end
end
