# frozen_string_literal: true
# typed: strict

module Models
  # Repsents a financials account held by an entity. This is a base class for all account types.
  class Account < Sequel::Model
    many_to_one :entity

    # Set only on a Security's own three wallets, which are opened on their
    # Issuer's Holder and so carry that entity_id too. Null on everything else.
    many_to_one :security
  end
end
