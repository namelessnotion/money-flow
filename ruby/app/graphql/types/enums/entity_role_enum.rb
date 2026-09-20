# frozen_string_literal: true
# typed: strict

module Types
  # GraphQL face of Types::Enums::EntityRole, whose values it is built from.
  class EntityRoleEnum < GraphQL::Schema::Enum
    graphql_name 'EntityRole'
    description 'What part an entity plays in this market. It decides which accounts it holds.'

    Types::Enums::EntityRole.each_value { |role| value(role.serialize.upcase, value: role.serialize) }
  end
end
