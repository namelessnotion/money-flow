# frozen_string_literal: true
# typed: strict

module Types
  # GraphQL face of Services::MoneyFlow::Movement::Kind, whose values it is
  # built from.
  class MovementKindEnum < GraphQL::Schema::Enum
    graphql_name 'MovementKind'
    description 'What moved the money in a Movement, which decides where it ran from and to.'

    Services::MoneyFlow::Movement::Kind.each_value { |kind| value(kind.serialize.upcase, value: kind) }
  end
end
