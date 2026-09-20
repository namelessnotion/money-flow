# frozen_string_literal: true
# typed: strict

# type to be passed to onboard an entity
class OnboardEntityRequest < T::Struct
  const :name, String
  # What the entity will do in this market. Required rather than defaulted:
  # the role decides which accounts get opened, and guessing it would give an
  # entity wallets it can never use — or leave out one it needs.
  const :role, Types::Enums::EntityRole
end
