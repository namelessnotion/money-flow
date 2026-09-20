# frozen_string_literal: true
# typed: strict

module Models
  # One holder's share of one Repayment: the Transaction that retires that much
  # of their claim and pays them the money.
  #
  # The row is a record of what was sent, not an "already disbursed" flag — the
  # id is derived from the Repayment and the Investor, so Go is the guard and
  # the sweep re-sending one is a no-op there.
  class Disbursement < Sequel::Model
    unrestrict_primary_key

    many_to_one :repayment
    many_to_one :investor, class: 'Models::Entity', key: :investor_entity_id
  end
end
