# frozen_string_literal: true
# typed: ignore

# An entity's `receivable`: what it owes the platform when a late ACH deposit
# return takes back money it no longer holds (ruby/docs/adr/0011). Entity-
# scoped, so only accounts_type_check learns it; the security-scoped CHECK
# is unchanged.
#
# The list written out literally, as everywhere else in db/migrations: a
# migration has to keep meaning what it meant when it ran.
Sequel.migration do
  old_entity_types = %w[bank bank_control debit_card uncleared_cash cleared_cash cash gain loss investment
                        issuer_control]
  new_entity_types = [*old_entity_types, 'receivable']
  security_types = %w[security_supply security_escrow security_repayment security_cash]

  up do
    alter_table(:accounts) do
      drop_constraint(:accounts_type_check)
      add_constraint(:accounts_type_check, type: new_entity_types + security_types)
    end
  end

  down do
    alter_table(:accounts) do
      drop_constraint(:accounts_type_check)
      add_constraint(:accounts_type_check, type: old_entity_types + security_types)
    end
  end
end
