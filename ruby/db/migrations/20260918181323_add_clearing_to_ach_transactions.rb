# frozen_string_literal: true
# typed: ignore

# The clearing Transaction originated for an ACH deposit, and its single
# Transfer. Both ids are derived from the deposit's id (Services::DetId), so
# they are a record of what was sent, not the idempotency guard — Go is that.
Sequel.migration do
  change do
    alter_table(:ach_transactions) do
      add_column :clearing_transaction_id, :uuid, unique: true
      add_column :clearing_transfer_id, :uuid, unique: true
    end
  end
end
