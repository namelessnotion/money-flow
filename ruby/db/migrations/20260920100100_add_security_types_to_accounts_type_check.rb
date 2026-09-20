# frozen_string_literal: true
# typed: ignore

# The account types a Security's money flow needs, added the way
# 20260905171057_add_bank_control_to_accounts_type_check added bank_control:
# both lists written out literally, because a migration has to keep meaning
# what it meant when it ran.
#
#   investment          an Investor's claims, denominated in minor units
#   issuer_control      where an Issuer mints claim supply from, and retires it to
#   security_supply     a Security's unsold claims — the oversubscription control
#   security_escrow     Investor money a Security holds until its Draw
#   security_repayment  Borrower money a Security holds until it is disbursed
#
# The three security_* types are scoped to a Security rather than to an entity;
# 20260920100300_add_security_id_to_accounts is what enforces that.
OLD_ACCOUNT_TYPES = %w[
  bank
  bank_control
  debit_card
  uncleared_cash
  cleared_cash
  cash
  gain
  loss
].freeze

NEW_ACCOUNT_TYPES = %w[
  bank
  bank_control
  debit_card
  uncleared_cash
  cleared_cash
  cash
  gain
  loss
  investment
  issuer_control
  security_supply
  security_escrow
  security_repayment
].freeze

Sequel.migration do
  up do
    alter_table(:accounts) do
      drop_constraint(:accounts_type_check)
      add_constraint(:accounts_type_check, type: NEW_ACCOUNT_TYPES)
    end
  end

  down do
    alter_table(:accounts) do
      drop_constraint(:accounts_type_check)
      add_constraint(:accounts_type_check, type: OLD_ACCOUNT_TYPES)
    end
  end
end
