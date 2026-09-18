# frozen_string_literal: true
# typed: strict

require_relative 'objects/base_object'
require_relative 'objects/entity'
require_relative 'objects/ach_transaction'

module Types
  # Root Query type.
  class QueryType < BaseObject
    field :ok, Boolean, null: false, resolver_method: :ok?,
                        description: 'Health check placeholder until real queries exist.'

    field :entities, Types::Entity.connection_type, null: false,
                                                    default_page_size: 100, max_page_size: 100,
                                                    extras: [:lookahead],
                                                    description: 'Onboarded entities, paginated at 100 per page.'

    field :ach_transactions, Types::AchTransaction.connection_type,
          null: false, default_page_size: 100, max_page_size: 100,
          description: "An entity's ACH Transactions, oldest first, paginated at 100 per page." do |field|
      field.argument :entity_id, ID, required: true
    end

    sig { returns(T::Boolean) }
    def ok?
      true
    end

    # `Models::Entity::PrivateDataset` is a Sorbet-only fiction (see
    # `Types::Entity.scope`) so this uses `T::Sig::WithoutRuntime.sig` rather
    # than a normal `sig`, which would try to resolve the constant at runtime.
    T::Sig::WithoutRuntime.sig do
      params(lookahead: GraphQL::Execution::Lookahead).returns(Models::Entity::PrivateDataset)
    end
    def entities(lookahead:)
      Types::Entity.scope(lookahead, Models::Entity.dataset.order(:id))
    end

    # Oldest first. The id breaks ties, so pages stay stable when rows share a
    # created_at (it is the insert transaction's start time).
    T::Sig::WithoutRuntime.sig { params(entity_id: String).returns(Models::AchTransaction::PrivateDataset) }
    def ach_transactions(entity_id:)
      Types::AchTransaction.dataset
                           .where(Sequel[:ach_transactions][:entity_id] => Integer(entity_id, 10))
                           .order(Sequel[:ach_transactions][:created_at], Sequel[:ach_transactions][:id])
    end
  end
end
