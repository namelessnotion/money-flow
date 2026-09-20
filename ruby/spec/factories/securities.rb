# frozen_string_literal: true

FactoryBot.define do
  factory :security, class: 'Models::Security' do
    id { SecureRandom.uuid_v7 }
    sequence(:name) { |n| "Security #{n}" }
    issuer factory: %i[entity issuer]
    borrower factory: %i[entity borrower]
    principal_minor_units { 1_000_000 } # $10,000.00
    annual_rate_bps { 1000 }            # 10.00%
    term_days { 365 }
    currency { 'USD' }

    # An offering nobody has minted yet. Traits add the ids as the Security
    # moves on, so a spec says which point of its life it is standing at.
    trait :offered do
      offering_transaction_id { SecureRandom.uuid_v7 }
      offering_transfer_id { SecureRandom.uuid_v7 }
    end

    trait :drawn do
      offered
      draw_transaction_id { SecureRandom.uuid_v7 }
      draw_transfer_id { SecureRandom.uuid_v7 }
    end

    to_create(&:save)
  end

  factory :subscription, class: 'Models::Subscription' do
    id { SecureRandom.uuid_v7 }
    security
    investor factory: %i[entity investor]
    amount_minor_units { 100_000 } # $1,000.00
    currency { 'USD' }
    claim_transfer_id { SecureRandom.uuid_v7 }
    money_transfer_id { SecureRandom.uuid_v7 }

    to_create(&:save)
  end

  factory :repayment, class: 'Models::Repayment' do
    id { SecureRandom.uuid_v7 }
    security
    principal_minor_units { 100_000 }
    interest_minor_units { 10_000 }
    as_of { Date.new(2026, 9, 20) }
    currency { 'USD' }
    collection_transfer_id { SecureRandom.uuid_v7 }

    to_create(&:save)
  end

  factory :disbursement, class: 'Models::Disbursement' do
    id { SecureRandom.uuid_v7 }
    repayment
    investor factory: %i[entity investor]
    principal_minor_units { 50_000 }
    interest_minor_units { 5_000 }
    currency { 'USD' }
    retirement_transfer_id { SecureRandom.uuid_v7 }
    payout_transfer_id { SecureRandom.uuid_v7 }

    # An interest-only Repayment retires no principal, and Go refuses a
    # zero-amount Transfer — so the leg is absent rather than zero.
    trait :interest_only do
      principal_minor_units { 0 }
      retirement_transfer_id { nil }
    end

    to_create(&:save)
  end
end
