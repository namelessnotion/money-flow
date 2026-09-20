# frozen_string_literal: true
# typed: strict

module Types
  module Enums
    # What part an entity plays in this market. Exactly one per entity, and it
    # decides which accounts onboarding opens for it.
    #
    # An Issuer is one of many: nothing here, and nothing that reads it, treats
    # any entity as the house.
    #
    # Serialized values are explicit: they are persisted in entities.role and
    # T::Enum would otherwise derive them by downcasing the constant.
    class EntityRole < T::Enum
      enums do
        Investor = new('investor')  # buys fractions of a Security
        Borrower = new('borrower')  # owes the debt obligation behind one
        Issuer = new('issuer')      # offers Securities in a Borrower's obligation
      end
    end
  end
end
