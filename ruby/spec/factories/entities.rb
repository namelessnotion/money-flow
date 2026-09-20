# frozen_string_literal: true

FactoryBot.define do
  factory :entity, class: 'Models::Entity' do
    sequence(:name) { |n| "Entity #{n}" }
    holder_uuid { SecureRandom.uuid_v7 }
    role { 'investor' }

    trait :investor do
      role { 'investor' }
    end

    trait :borrower do
      role { 'borrower' }
    end

    trait :issuer do
      role { 'issuer' }
    end

    to_create(&:save)
  end
end
