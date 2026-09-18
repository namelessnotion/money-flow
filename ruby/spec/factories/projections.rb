# frozen_string_literal: true

FactoryBot.define do
  factory :transaction_projection, class: 'Models::TransactionProjection' do
    aggregate_id { SecureRandom.uuid_v7 }
    state { 'initialized' }
    last_sequence { 1 }
    last_global_seq { 1 }
    state_changed_at { Time.now }

    to_create(&:save)
  end

  factory :transfer_projection, class: 'Models::TransferProjection' do
    aggregate_id { SecureRandom.uuid_v7 }
    state { 'accepted' }
    last_sequence { 1 }
    last_global_seq { 1 }

    to_create(&:save)
  end

  factory :token_balance_projection, class: 'Models::TokenBalanceProjection' do
    token_id { SecureRandom.uuid_v7 }
    wallet_uuid { SecureRandom.uuid_v7 }
    currency { 'USD' }
    posted_minor_units { 0 }
    pending_outgoing_minor_units { 0 }
    pending_incoming_minor_units { 0 }
    last_sequence { 2 }
    last_global_seq { 2 }

    to_create(&:save)
  end
end
