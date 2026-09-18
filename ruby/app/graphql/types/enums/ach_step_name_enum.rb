# frozen_string_literal: true
# typed: strict

module Types
  # GraphQL face of Services::Ach::Progress::Name, whose values it is built from.
  class AchStepNameEnum < GraphQL::Schema::Enum
    graphql_name 'AchStepName'
    description 'A step in an ACH Transaction’s lifecycle, in the order it is reached.'

    Services::Ach::Progress::Name.each_value { |name| value(name.serialize.upcase, value: name) }
  end
end
