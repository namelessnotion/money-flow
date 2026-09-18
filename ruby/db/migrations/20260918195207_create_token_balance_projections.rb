# frozen_string_literal: true
# typed: ignore

# Ruby's lagging read model of each Go Token's ledger balance, folded from the
# TokenBalanceRecorded events on token-events (ruby/docs/adr/0005). An
# Account's balance is the sum of these rows over its wallet_uuid.
#
# last_sequence is the Token stream's sequence of the balance held here: Go
# appends each observation with optimistic concurrency, so the highest
# sequence is the most recent balance, and a lower one is never applied.
#
# No foreign key to accounts: a Token's Wallet may belong to no Account Ruby
# knows about, and events arrive in whatever order the topic delivers them.
Sequel.migration do
  change do
    create_table(:token_balance_projections) do
      uuid :token_id, primary_key: true
      uuid :wallet_uuid, null: false, index: true
      String :currency, null: false
      Bignum :posted_minor_units, null: false
      Bignum :pending_outgoing_minor_units, null: false
      Bignum :pending_incoming_minor_units, null: false
      Bignum :last_sequence, null: false
      Bignum :last_global_seq, null: false
      DateTime :created_at, null: false, default: Sequel::SQL::Constants::CURRENT_TIMESTAMP
      DateTime :updated_at, null: false, default: Sequel::SQL::Constants::CURRENT_TIMESTAMP
    end
  end
end
