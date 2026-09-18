# frozen_string_literal: true
# typed: strict

module Types
  # GraphQL face of Services::Ach::Progress::Status, whose values it is built from.
  class AchStepStatusEnum < GraphQL::Schema::Enum
    graphql_name 'AchStepStatus'
    description 'Where an ACH lifecycle step stands. SKIPPED means an earlier step failed, so it will never run.'

    Services::Ach::Progress::Status.each_value { |status| value(status.serialize.upcase, value: status) }
  end
end
