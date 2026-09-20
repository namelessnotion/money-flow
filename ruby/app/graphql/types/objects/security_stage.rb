# frozen_string_literal: true
# typed: strict

require_relative 'base_object'
require_relative '../enums/security_stage_name_enum'
require_relative '../enums/security_stage_status_enum'

module Types
  # GraphQL type for one Services::Securities::Stage::Step.
  class SecurityStage < BaseObject
    description 'One phase of a Security’s life and where it stands, as the read model last saw it.'

    field :name, SecurityStageNameEnum, null: false
    field :status, SecurityStageStatusEnum, null: false
  end
end
