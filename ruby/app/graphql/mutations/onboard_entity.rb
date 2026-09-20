# frozen_string_literal: true
# typed: strict

require_relative 'base_mutation'
require_relative '../types/objects/entity'
require_relative '../types/enums/entity_role_enum'

module Mutations
  # Onboards an entity by calling the onboarding service
  class OnboardEntity < BaseMutation
    argument :name, String, required: true
    argument :role, Types::EntityRoleEnum, required: true,
                                           description: 'Which accounts to open for it.'

    field :entity, Types::Entity, null: true

    sig { params(name: String, role: String).returns(T::Hash[Symbol, Models::Entity]) }
    def resolve(name:, role:)
      response = Services::OnboardEntity.new.call(
        request: OnboardEntityRequest.new(name: name, role: Types::Enums::EntityRole.deserialize(role))
      )

      # perform runs in a transaction that either commits with the entity
      # fully persisted or raises, so a response here means it exists.
      entity = Models::Entity[response.entity_id]
      raise "entity #{response.entity_id} not found after onboarding" if entity.nil?

      { entity: entity }
    end
  end
end
