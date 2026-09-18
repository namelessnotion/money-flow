# frozen_string_literal: true
# typed: ignore

# Ruby's record of an ACH Transaction it originated: what it asked Go to do,
# under which ids. It never holds lifecycle state — that is Go's, and reaches
# Ruby only through the projections (see 20260918140023_create_projections).
#
# The ids are written here before Go is ever called, so a retry or a re-run
# after a crash resends the same ids and converges on Go's idempotency rather
# than originating a second Transaction.
#
# Directions written out literally, not read from Types::Enums::AchDirection:
# a migration has to keep meaning what it meant when it ran.
Sequel.migration do
  change do
    create_table(:ach_transactions) do
      uuid :id, primary_key: true
      foreign_key :entity_id, :entities, null: false, index: true
      String :direction, null: false
      Bignum :amount_minor_units, null: false
      String :currency, null: false
      uuid :real_transfer_id, null: false, unique: true
      uuid :shadow_transfer_id, null: false, unique: true
      String :provider_reference
      DateTime :created_at, null: false, default: Sequel::SQL::Constants::CURRENT_TIMESTAMP
      DateTime :updated_at, null: false, default: Sequel::SQL::Constants::CURRENT_TIMESTAMP

      constraint(:ach_transactions_direction_check, direction: %w[deposit withdrawal])
      constraint(:ach_transactions_amount_positive, Sequel.lit('amount_minor_units > 0'))
    end
  end
end
