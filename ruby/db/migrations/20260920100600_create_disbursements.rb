# frozen_string_literal: true
# typed: ignore

# One holder's share of one Repayment: the Transaction that retires that much
# of their claim and pays them the money.
#
# One Transaction per holder rather than one fan-out across all of them, so a
# holder whose leg fails is a holder whose leg failed, not a reversal of
# everyone already paid (ruby/docs/adr/0007).
#
# The primary key is a derived id — detid("<repayment id>:disbursement:<investor
# id>") — so the sweep re-sending one is a no-op in Go and Ruby keeps no
# "already disbursed" flag. The unique index below says the same thing in
# Postgres, where it can be relied on rather than remembered.
#
# retirement_transfer_id is nullable because an interest-only Repayment retires
# no principal, and Go refuses a zero-amount Transfer: the leg is absent rather
# than zero. Worse than refused, in fact — a zero amount is a transport error
# the Transaction saga logs and swallows, stranding the Transaction in Started
# with no event to explain it. The CHECK is that rule written down.
Sequel.migration do
  change do
    create_table(:disbursements) do
      uuid :id, primary_key: true
      foreign_key :repayment_id, :repayments, type: :uuid, null: false, index: true
      foreign_key :investor_entity_id, :entities, null: false
      Bignum :principal_minor_units, null: false
      Bignum :interest_minor_units, null: false
      String :currency, null: false
      uuid :retirement_transfer_id, unique: true
      uuid :payout_transfer_id, null: false, unique: true
      DateTime :created_at, null: false, default: Sequel::SQL::Constants::CURRENT_TIMESTAMP
      DateTime :updated_at, null: false, default: Sequel::SQL::Constants::CURRENT_TIMESTAMP

      constraint(:disbursements_parts_non_negative,
                 Sequel.lit('principal_minor_units >= 0 AND interest_minor_units >= 0'))
      constraint(:disbursements_total_positive,
                 Sequel.lit('principal_minor_units + interest_minor_units > 0'))
      constraint(:disbursements_retirement_leg_iff_principal,
                 Sequel.lit('(principal_minor_units > 0) = (retirement_transfer_id IS NOT NULL)'))

      index %i[repayment_id investor_entity_id], unique: true
    end
  end
end
