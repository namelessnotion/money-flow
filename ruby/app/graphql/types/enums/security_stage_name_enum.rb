# frozen_string_literal: true
# typed: strict

module Types
  # GraphQL face of Services::Securities::Stage::Name, whose values it is
  # built from.
  class SecurityStageNameEnum < GraphQL::Schema::Enum
    graphql_name 'SecurityStageName'
    description 'A phase in a Security’s life, in the order it is reached.'

    Services::Securities::Stage::Name.each_value { |name| value(name.serialize.upcase, value: name) }
  end
end
