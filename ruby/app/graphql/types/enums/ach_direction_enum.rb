# frozen_string_literal: true
# typed: strict

module Types
  # GraphQL face of Types::Enums::AchDirection, whose values it is built from.
  class AchDirectionEnum < GraphQL::Schema::Enum
    graphql_name 'AchDirection'
    description 'Which way an ACH Transaction moves money across the bank boundary.'

    Types::Enums::AchDirection.each_value { |direction| value(direction.serialize.upcase, value: direction.serialize) }
  end
end
