# frozen_string_literal: true
# typed: strict

require_relative '../types/query_type'
require_relative '../types/mutation_type'
require_relative '../connections/sequel_dataset_connection'

# GraphQL schema for the MoneyFlow application
class MoneyFlowSchema < GraphQL::Schema
  query Types::QueryType
  mutation Types::MutationType

  connections.add(Sequel::Dataset, Connections::SequelDatasetConnection)

  # Batches per-object lookups (Sources::*) across a whole query.
  use GraphQL::Dataloader
end
