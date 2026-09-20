# frozen_string_literal: true
# typed: ignore

# A claim on one Borrower's debt obligation, offered by an Issuer and bought in
# fractions by Investors. Each Security names its own Issuer — the platform
# hosts many, and nothing here assumes a house issuer.
#
# Terms and the ids it was sent to Go under, and nothing else. There is no
# state column, no drawn_on, no outstanding_principal: lifecycle is Go's and
# reaches Ruby only through the projections, joined on the *_transaction_id
# columns below, exactly as ach_transactions does it. Even the date the money
# was drawn — which interest accrual needs — is the draw Transaction's
# state_changed_at, so the clock that decides it is the same one that decided
# the transfer.
#
# The ids are written before Go is ever called, so a retry or a re-run after a
# crash resends the same ids and converges on Go's idempotency rather than
# minting a second supply.
Sequel.migration do
  change do
    create_table(:securities) do
      uuid :id, primary_key: true
      String :name, null: false
      foreign_key :issuer_entity_id, :entities, null: false, index: true
      foreign_key :borrower_entity_id, :entities, null: false, index: true
      Bignum :principal_minor_units, null: false
      Integer :annual_rate_bps, null: false
      Integer :term_days, null: false
      String :currency, null: false

      uuid :offering_transaction_id, unique: true
      uuid :offering_transfer_id, unique: true
      uuid :draw_transaction_id, unique: true
      uuid :draw_transfer_id, unique: true

      DateTime :created_at, null: false, default: Sequel::SQL::Constants::CURRENT_TIMESTAMP
      DateTime :updated_at, null: false, default: Sequel::SQL::Constants::CURRENT_TIMESTAMP

      # The offering size. Go refuses a zero-amount Transfer, so a Security
      # with nothing to sell could never have its supply minted.
      constraint(:securities_principal_positive, Sequel.lit('principal_minor_units > 0'))
      constraint(:securities_rate_non_negative, Sequel.lit('annual_rate_bps >= 0'))
      constraint(:securities_term_positive, Sequel.lit('term_days > 0'))
      # An Issuer selling a claim on its own debt would have the supply wallet
      # and the borrower wallet on the same Holder, which is not a market.
      constraint(:securities_distinct_parties, Sequel.lit('issuer_entity_id <> borrower_entity_id'))
    end
  end
end
