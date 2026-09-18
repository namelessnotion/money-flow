# frozen_string_literal: true
# typed: ignore

# Ruby's lagging read model of Go's Transfer and Transaction aggregates, folded
# from transfer-events / transaction-events (ruby/docs/adr/0001).
#
# last_sequence is the monotonic guard's high-water mark: an event is applied
# only if its per-aggregate sequence is above it. It lives in the same row it
# guards so the two can never be committed apart.
#
# No foreign keys to ach_transactions or between the tables: rows are created
# the first time an aggregate id is seen, in whatever order the topics deliver
# them, and Reversals belong to no Transaction Ruby originated.
Sequel.migration do
  change do
    create_table(:transaction_projections) do
      uuid :aggregate_id, primary_key: true
      String :state, null: false
      String :reason, text: true
      Bignum :last_sequence, null: false
      Bignum :last_global_seq, null: false
      DateTime :created_at, null: false, default: Sequel::SQL::Constants::CURRENT_TIMESTAMP
      DateTime :updated_at, null: false, default: Sequel::SQL::Constants::CURRENT_TIMESTAMP
    end

    create_table(:transfer_projections) do
      uuid :aggregate_id, primary_key: true
      uuid :transaction_id, index: true
      String :state, null: false
      String :reason, text: true
      Bignum :last_sequence, null: false
      Bignum :last_global_seq, null: false
      DateTime :created_at, null: false, default: Sequel::SQL::Constants::CURRENT_TIMESTAMP
      DateTime :updated_at, null: false, default: Sequel::SQL::Constants::CURRENT_TIMESTAMP
    end
  end
end
