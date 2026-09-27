# frozen_string_literal: true
# typed: ignore

# The provider's return notice for an entry, and the late return originated
# for it (ruby/docs/adr/0011). The notice is a fact the provider reported, like
# provider_reference, not lifecycle state: Ruby records it before anything
# acts on it, and it decides which form a late return takes. The late return's
# id is derived from the ACH Transaction's (Services::DetId), so it is a record
# of what was sent, not the idempotency guard — Go is that.
Sequel.migration do
  change do
    alter_table(:ach_transactions) do
      add_column :returned_at, :timestamptz
      add_column :return_reason, :text
      add_column :return_transaction_id, :uuid, unique: true

      add_constraint(:ach_transactions_return_notice_whole,
                     Sequel.lit('(returned_at IS NULL) = (return_reason IS NULL)'))
      add_constraint(:ach_transactions_return_follows_notice,
                     Sequel.lit('return_transaction_id IS NULL OR returned_at IS NOT NULL'))
    end
  end
end
