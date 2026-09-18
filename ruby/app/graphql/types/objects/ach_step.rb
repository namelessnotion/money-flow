# frozen_string_literal: true
# typed: strict

require_relative 'base_object'
require_relative '../enums/ach_step_name_enum'
require_relative '../enums/ach_step_status_enum'

module Types
  # GraphQL type for one Services::Ach::Progress::Step.
  class AchStep < BaseObject
    description 'One step of an ACH Transaction’s lifecycle and where it stands, as the read model last saw it.'

    field :name, AchStepNameEnum, null: false
    field :status, AchStepStatusEnum, null: false
  end
end
