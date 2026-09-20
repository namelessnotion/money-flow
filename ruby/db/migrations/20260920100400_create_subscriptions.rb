# frozen_string_literal: true
# typed: ignore

# One Investor's fractional purchase of a Security: what Ruby asked Go to do,
# under which ids, and nothing about how it went.
#
# The primary key IS the Go Transaction's id, chosen here before Go is called —
# the same contract as ach_transactions — so the projections join straight onto
# it and a retry converges instead of buying twice.
#
# Two legs, named by their role rather than numbered: the claim leg moves
# claims out of the Security's supply, the money leg moves the Investor's
# cleared cash into its escrow.
Sequel.migration do
  change do
    create_table(:subscriptions) do
      uuid :id, primary_key: true
      foreign_key :security_id, :securities, type: :uuid, null: false, index: true
      foreign_key :investor_entity_id, :entities, null: false
      Bignum :amount_minor_units, null: false
      String :currency, null: false
      uuid :claim_transfer_id, null: false, unique: true
      uuid :money_transfer_id, null: false, unique: true
      DateTime :created_at, null: false, default: Sequel::SQL::Constants::CURRENT_TIMESTAMP
      DateTime :updated_at, null: false, default: Sequel::SQL::Constants::CURRENT_TIMESTAMP

      # Go refuses a zero-amount Transfer, so a Subscription for nothing could
      # never have been sent.
      constraint(:subscriptions_amount_positive, Sequel.lit('amount_minor_units > 0'))

      # Deliberately not unique: an Investor may buy into the same Security
      # more than once, and a Position is the sum of those purchases.
      index %i[security_id investor_entity_id]
    end
  end
end
