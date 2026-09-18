# frozen_string_literal: true

FactoryBot.define do
  factory :transaction_projection, class: 'Models::TransactionProjection' do
    aggregate_id { SecureRandom.uuid_v7 }
    state { 'initialized' }
    last_sequence { 1 }
    last_global_seq { 1 }

    to_create(&:save)
  end

  factory :transfer_projection, class: 'Models::TransferProjection' do
    aggregate_id { SecureRandom.uuid_v7 }
    state { 'accepted' }
    last_sequence { 1 }
    last_global_seq { 1 }

    to_create(&:save)
  end
end
