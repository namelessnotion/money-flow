# frozen_string_literal: true
# typed: ignore

# One Recovery: collecting some of what an entity owes from its cleared cash
# (ruby/docs/adr/0011, decision 5).
#
# The primary key is a derived id — detid("<entity id>:recovery:<sequence>") —
# recorded before Go is asked, so the sweep re-sending one is a no-op in Go.
# The unique index on (entity_id, sequence) is what keeps two sweeps from both
# starting a Recovery for the same entity: the second insert loses, and only
# one Recovery per entity is ever in flight.
#
# cleared_global_seq is the read model's position in the event log for the
# entity's cleared cash when the Recovery was sized. After one is refused, the
# next is tried only once that has moved on, so a stale view of cleared cash
# can't retry for ever.
Sequel.migration do
  change do
    create_table(:receivable_recoveries) do
      uuid :id, primary_key: true
      foreign_key :entity_id, :entities, null: false
      Integer :sequence, null: false
      Bignum :amount_minor_units, null: false
      String :currency, null: false
      Bignum :cleared_global_seq, null: false
      DateTime :created_at, null: false, default: Sequel::SQL::Constants::CURRENT_TIMESTAMP

      constraint(:receivable_recoveries_amount_positive, Sequel.lit('amount_minor_units > 0'))
      constraint(:receivable_recoveries_sequence_positive, Sequel.lit('sequence > 0'))
      index %i[entity_id sequence], unique: true
    end
  end
end
