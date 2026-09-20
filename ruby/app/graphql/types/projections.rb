# frozen_string_literal: true
# typed: strict

module Types
  # Joining a business table to the projections of the Transactions and
  # Transfers it originated, so a row arrives with its lifecycle already on it
  # rather than fetched again per row.
  #
  # Every type here needs the same join: left-join each projection table on
  # `aggregate_id = <this table>.<the column holding that aggregate's id>`,
  # then select the business columns plus the projected ones under their field
  # names. A row the projection has not reached yet comes back with nulls,
  # which is what the null state fields mean.
  #
  # Extended into each type rather than inherited: they already inherit
  # BaseObject, and this is one method rather than a kind of thing to be.
  module Projections
    # `joins` maps an alias to [projection table, the column on `table` holding
    # the id of the aggregate it projects].
    #
    # Untyped because the dataset is a per-model `PrivateDataset` — the
    # static-only fiction the tapioca compiler generates — so there is no one
    # type every model's dataset shares. Each caller's own `dataset` method
    # declares the concrete one, and the chain stays checked from there.
    sig do
      params(
        model: T.untyped,
        table: Symbol,
        joins: T::Hash[Symbol, T::Array[Symbol]],
        columns: T::Array[Sequel::SQL::AliasedExpression]
      ).returns(T.untyped)
    end
    def projected(model:, table:, joins:, columns:)
      joined = joins.reduce(model.dataset) do |dataset, (as, (projection_table, key))|
        dataset.left_join(Sequel[projection_table].as(as), aggregate_id: Sequel[table][key])
      end
      joined.select_all(table).select_append(*columns)
    end
  end
end
