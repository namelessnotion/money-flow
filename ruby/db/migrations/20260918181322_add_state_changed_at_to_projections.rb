# frozen_string_literal: true
# typed: ignore

# When the aggregate entered its current state, by Go's clock: the occurred_at
# of the event that moved it there, published in the CDC envelope. Nullable —
# rows projected before the envelope carried occurred_at have no source time.
# ACH clearing counts business days from a Transaction's completion.
Sequel.migration do
  change do
    add_column :transaction_projections, :state_changed_at, 'timestamptz'
    add_column :transfer_projections, :state_changed_at, 'timestamptz'
  end
end
