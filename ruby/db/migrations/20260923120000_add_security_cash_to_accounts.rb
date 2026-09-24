# frozen_string_literal: true
# typed: ignore

# A Security's fourth Wallet, `security_cash`: the real money it holds, beside
# the escrow and repayment wallets that hold its cleared cash
# (ruby/docs/adr/0009). Security-scoped like the other three, so both CHECKs
# that name the scoped types learn it.
#
# Both lists written out literally, as everywhere else in db/migrations: a
# migration has to keep meaning what it meant when it ran. Locals rather than
# top-level constants, which earlier migrations define under the same names.
Sequel.migration do
  entity_types = %w[bank bank_control debit_card uncleared_cash cleared_cash cash gain loss investment
                    issuer_control]
  old_security_types = %w[security_supply security_escrow security_repayment]
  new_security_types = [*old_security_types, 'security_cash']

  # A literal rather than `lambda`: the migration DSL is a BasicObject.
  scoped = ->(types) { Sequel.lit('(type IN ?) = (security_id IS NOT NULL)', types) }

  up do
    alter_table(:accounts) do
      drop_constraint(:accounts_type_check)
      add_constraint(:accounts_type_check, type: entity_types + new_security_types)
      drop_constraint(:accounts_security_scoped_types)
      add_constraint(:accounts_security_scoped_types, scoped.call(new_security_types))
    end
  end

  down do
    alter_table(:accounts) do
      drop_constraint(:accounts_security_scoped_types)
      add_constraint(:accounts_security_scoped_types, scoped.call(old_security_types))
      drop_constraint(:accounts_type_check)
      add_constraint(:accounts_type_check, type: entity_types + old_security_types)
    end
  end
end
