# frozen_string_literal: true
# typed: strict

require_relative 'enums/ach_direction'

# type to be passed to initiate an ACH deposit or withdrawal for an entity
class InitiateAchRequest < T::Struct
  const :entity_id, Integer
  const :direction, Types::Enums::AchDirection
  const :amount_minor_units, Integer
end
