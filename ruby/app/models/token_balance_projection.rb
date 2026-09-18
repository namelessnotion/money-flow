# frozen_string_literal: true
# typed: strict

require_relative 'account_balance'

module Models
  # Lagging read model of one Go Token's ledger balance, keyed by its id.
  # Written only by Consumer::BalanceProjector.
  class TokenBalanceProjection < Sequel::Model
    unrestrict_primary_key

    # Each of `wallet_uuids`' balances, one per currency, summed over its
    # Tokens in a single query. A Wallet with no Token balance yet is absent.
    sig { params(wallet_uuids: T::Array[String]).returns(T::Hash[String, T::Array[AccountBalance]]) }
    def self.totals_for(wallet_uuids)
      sums(wallet_uuids).group_by { |row| String(row.fetch(:wallet_uuid)) }.transform_values do |totals|
        totals.map { |row| balance(row) }
      end
    end

    # The summing column for each AccountBalance amount.
    SUMMED = T.let(
      { posted: :posted_minor_units, pending_outgoing: :pending_outgoing_minor_units,
        pending_incoming: :pending_incoming_minor_units }.freeze,
      T::Hash[Symbol, Symbol]
    )

    # One row per wallet_uuid and currency, with each SUMMED amount totalled.
    # A naked row's values are whatever the database returns for each column,
    # known only at runtime; #balance narrows them into an AccountBalance.
    sig { params(wallet_uuids: T::Array[String]).returns(T::Array[T::Hash[Symbol, T.untyped]]) }
    def self.sums(wallet_uuids)
      totals = SUMMED.map { |amount, column| Sequel.function(:sum, column).as(amount) }
      # T.unsafe: Sorbet cannot splat an array of unknown length (https://srb.help/7019).
      where(wallet_uuid: wallet_uuids).select_group(:wallet_uuid, :currency).select_append(*T.unsafe(totals))
                                      .order(:wallet_uuid, :currency).naked.all
    end
    private_class_method :sums

    # Postgres sums bigints as numeric, which Sequel hands back as BigDecimal.
    sig { params(row: T::Hash[Symbol, T.untyped]).returns(AccountBalance) }
    def self.balance(row)
      AccountBalance.new(
        currency: String(row.fetch(:currency)),
        posted: Integer(row.fetch(:posted)),
        pending_outgoing: Integer(row.fetch(:pending_outgoing)),
        pending_incoming: Integer(row.fetch(:pending_incoming))
      )
    end
    private_class_method :balance
  end
end
