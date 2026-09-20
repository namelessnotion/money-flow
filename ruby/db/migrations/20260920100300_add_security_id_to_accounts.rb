# frozen_string_literal: true
# typed: ignore

# A Security's three wallets, hung off the accounts table rather than given a
# table of their own.
#
# They are opened on the *Issuer's* Holder — Go has no Holder for a Security —
# so the row needs entity_id regardless, and a separate table would carry
# entity_id, wallet_uuid, type, name and timestamps too: all of accounts, plus
# one column. More to the point, `accounts.wallet_uuid UNIQUE` is that table's
# real invariant ("one account per Wallet") and it has to hold across both
# kinds; split in two, it would need a cross-table exclusion Postgres will not
# give you. Every read path — Types::Account, Sources::AccountBalances,
# Models::TokenBalanceProjection.totals_for — is already keyed on wallet_uuid.
#
# Types written out literally, as everywhere else in db/migrations.
Sequel.migration do
  up do
    alter_table(:accounts) do
      add_foreign_key :security_id, :securities, type: :uuid, null: true, index: true

      # The load-bearing one, in both directions: a security_escrow with no
      # Security is unreachable, and a cleared_cash hanging off a Security
      # would be money nobody owns.
      add_constraint(
        :accounts_security_scoped_types,
        Sequel.lit("(type IN ('security_supply', 'security_escrow', 'security_repayment')) " \
                   '= (security_id IS NOT NULL)')
      )
    end

    # Partial on purpose. (entity_id, type) stays deliberately non-unique — an
    # entity may hold several bank accounts, see
    # 20260822211706_add_accounts_type_check — but a Security has exactly one
    # wallet of each of its three types, and Services::Securities::Wallets
    # resolves by type on the strength of that.
    run <<~SQL.strip
      CREATE UNIQUE INDEX accounts_security_type_unique
        ON accounts (security_id, type) WHERE security_id IS NOT NULL
    SQL
  end

  down do
    run 'DROP INDEX IF EXISTS accounts_security_type_unique'

    alter_table(:accounts) do
      drop_constraint(:accounts_security_scoped_types)
      drop_column :security_id
    end
  end
end
