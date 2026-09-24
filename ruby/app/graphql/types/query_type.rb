# frozen_string_literal: true
# typed: strict

require_relative 'objects/base_object'
require_relative 'objects/entity'
require_relative 'objects/ach_transaction'
require_relative 'objects/security'
require_relative 'objects/subscription'
require_relative 'objects/repayment'
require_relative '../resolvers/money_flow'

module Types
  # Root Query type.
  class QueryType < BaseObject
    field :ok, Boolean, null: false, resolver_method: :ok?,
                        description: 'Health check placeholder until real queries exist.'

    field :entities, Types::Entity.connection_type, null: false,
                                                    default_page_size: 100, max_page_size: 100,
                                                    extras: [:lookahead],
                                                    description: 'Onboarded entities, paginated at 100 per page.'

    field :entity, Types::Entity, null: true, description: 'One entity by id, or null when there is none.' do |field|
      field.argument :id, ID, required: true
    end

    field :ach_transaction, Types::AchTransaction,
          null: true, description: 'One ACH Transaction by id, or null when there is none.' do |field|
      field.argument :id, ID, required: true
    end

    field :ach_transactions, Types::AchTransaction.connection_type,
          null: false, default_page_size: 100, max_page_size: 100,
          description: "An entity's ACH Transactions, oldest first, paginated at 100 per page." do |field|
      field.argument :entity_id, ID, required: true
    end

    field :security, Types::Security,
          null: true, description: 'One Security by id, or null when there is none.' do |field|
      field.argument :id, ID, required: true
    end

    field :securities, Types::Security.connection_type,
          null: false, default_page_size: 100, max_page_size: 100,
          description: 'Securities, newest first, paginated at 100 per page.' do |field|
      field.argument :issuer_entity_id, ID, required: false
      field.argument :borrower_entity_id, ID, required: false
    end

    field :subscriptions, Types::Subscription.connection_type,
          null: false, default_page_size: 100, max_page_size: 100,
          description: 'A Security’s Subscriptions, oldest first, paginated at 100 per page.' do |field|
      field.argument :security_id, ID, required: true
    end

    field :repayments, Types::Repayment.connection_type,
          null: false, default_page_size: 100, max_page_size: 100,
          description: 'A Security’s Repayments, oldest first, paginated at 100 per page.' do |field|
      field.argument :security_id, ID, required: true
    end

    field :money_flow, resolver: Resolvers::MoneyFlow

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

    # An id that is not a bigint names no entity, rather than being an error.
    sig { params(id: String).returns(T.nilable(Models::Entity)) }
    def entity(id:)
      return nil unless /\A\d{1,18}\z/.match?(id)

      Models::Entity[Integer(id, 10)]
    end

    sig { params(id: String).returns(T.nilable(Models::AchTransaction)) }
    def ach_transaction(id:)
      Types::AchTransaction.find(id)
    end

    # Oldest first. The id breaks ties, so pages stay stable when rows share a
    # created_at (it is the insert transaction's start time).
    T::Sig::WithoutRuntime.sig { params(entity_id: String).returns(Models::AchTransaction::PrivateDataset) }
    def ach_transactions(entity_id:)
      Types::AchTransaction.dataset
                           .where(Sequel[:ach_transactions][:entity_id] => Integer(entity_id, 10))
                           .order(Sequel[:ach_transactions][:created_at], Sequel[:ach_transactions][:id])
    end

    sig { params(id: String).returns(T.nilable(Models::Security)) }
    def security(id:)
      Types::Security.find(id)
    end

    # Newest first: an offering still open is what a caller is usually after.
    # The id breaks ties, so pages stay stable when rows share a created_at.
    T::Sig::WithoutRuntime.sig do
      params(issuer_entity_id: T.nilable(String), borrower_entity_id: T.nilable(String))
        .returns(Models::Security::PrivateDataset)
    end
    def securities(issuer_entity_id: nil, borrower_entity_id: nil)
      scoped = Types::Security.dataset
      scoped = by_party(scoped, :issuer_entity_id, issuer_entity_id)
      scoped = by_party(scoped, :borrower_entity_id, borrower_entity_id)
      scoped.order(Sequel.desc(Sequel[:securities][:created_at]), Sequel.desc(Sequel[:securities][:id]))
    end

    T::Sig::WithoutRuntime.sig { params(security_id: String).returns(Models::Subscription::PrivateDataset) }
    def subscriptions(security_id:)
      Types::Subscription.dataset
                         .where(Types::Security.matching(Sequel[:subscriptions][:security_id], security_id))
                         .order(Sequel[:subscriptions][:created_at], Sequel[:subscriptions][:id])
    end

    T::Sig::WithoutRuntime.sig { params(security_id: String).returns(Models::Repayment::PrivateDataset) }
    def repayments(security_id:)
      Types::Repayment.dataset
                      .where(Types::Security.matching(Sequel[:repayments][:security_id], security_id))
                      .order(Sequel[:repayments][:created_at], Sequel[:repayments][:id])
    end

    private

    # An entity id that is not a bigint matches nothing, rather than being an
    # error — the same rule `entity` follows.
    T::Sig::WithoutRuntime.sig do
      params(dataset: Models::Security::PrivateDataset, column: Symbol, entity_id: T.nilable(String))
        .returns(Models::Security::PrivateDataset)
    end
    def by_party(dataset, column, entity_id)
      return dataset if entity_id.nil?
      return dataset.where(false) unless /\A\d{1,18}\z/.match?(entity_id)

      dataset.where(Sequel[:securities][column] => Integer(entity_id, 10))
    end
  end
end
