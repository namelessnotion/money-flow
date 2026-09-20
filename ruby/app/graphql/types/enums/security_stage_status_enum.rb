# frozen_string_literal: true
# typed: strict

module Types
  # GraphQL face of Services::Securities::Stage::Status, whose values it is
  # built from.
  #
  # Its own enum rather than a reuse of AchStepStatusEnum: the two capabilities
  # happen to describe a step the same way today, and coupling their GraphQL
  # surfaces through one name would make that an accident nobody could change.
  class SecurityStageStatusEnum < GraphQL::Schema::Enum
    graphql_name 'SecurityStageStatus'
    description 'Where a phase of a Security’s life stands, as the read model last saw it.'

    Services::Securities::Stage::Status.each_value { |status| value(status.serialize.upcase, value: status) }
  end
end
