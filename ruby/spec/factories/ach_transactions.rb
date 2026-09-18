# frozen_string_literal: true

FactoryBot.define do
  factory :ach_transaction, class: 'Models::AchTransaction' do
    id { SecureRandom.uuid_v7 }
    entity
    direction { 'deposit' }
    amount_minor_units { 10_000 }
    currency { 'USD' }
    real_transfer_id { SecureRandom.uuid_v7 }
    shadow_transfer_id { SecureRandom.uuid_v7 }

    to_create(&:save)
  end
end
