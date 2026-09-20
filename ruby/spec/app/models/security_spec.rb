# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Models::Security do
  it 'belongs to an issuer and a borrower' do
    security = create(:security)

    expect(security.issuer.role).to eq('issuer')
    expect(security.borrower.role).to eq('borrower')
  end

  it 'holds no lifecycle state of its own' do
    # Whether the supply minted, and when the money was drawn, are Go's and
    # reach Ruby only through the projections joined on these ids.
    expect(described_class.columns).not_to include(:state, :status, :drawn_on, :outstanding_principal)
  end

  describe 'what the database refuses' do
    it 'refuses an offering of nothing, which could have no supply leg' do
      expect { create(:security, principal_minor_units: 0) }
        .to raise_error(Sequel::CheckConstraintViolation, /principal_positive/)
    end

    it 'refuses a negative rate' do
      expect { create(:security, annual_rate_bps: -1) }
        .to raise_error(Sequel::CheckConstraintViolation, /rate_non_negative/)
    end

    it 'refuses a term of no days' do
      expect { create(:security, term_days: 0) }
        .to raise_error(Sequel::CheckConstraintViolation, /term_positive/)
    end

    it 'refuses an issuer selling a claim on its own debt' do
      entity = create(:entity, :issuer)

      expect { create(:security, issuer: entity, borrower: entity) }
        .to raise_error(Sequel::CheckConstraintViolation, /distinct_parties/)
    end
  end

  describe 'its wallets' do
    let(:security) { create(:security) }

    it "opens a Security's wallets on its issuer's holder" do
      account = create(:account, entity: security.issuer, security: security, type: 'security_supply')

      expect(security.accounts.map(&:id)).to eq([account.id])
      expect(account.entity_id).to eq(security.issuer_entity_id)
    end

    it 'refuses a security-scoped account with no security' do
      expect { create(:account, type: 'security_escrow', security: nil) }
        .to raise_error(Sequel::CheckConstraintViolation, /security_scoped_types/)
    end

    it 'refuses an entity-scoped account hung off a security' do
      expect { create(:account, type: 'cleared_cash', security: security) }
        .to raise_error(Sequel::CheckConstraintViolation, /security_scoped_types/)
    end

    it 'refuses a second wallet of the same type on one security' do
      create(:account, entity: security.issuer, security: security, type: 'security_escrow')

      expect { create(:account, entity: security.issuer, security: security, type: 'security_escrow') }
        .to raise_error(Sequel::UniqueConstraintViolation, /security_type_unique/)
    end

    it 'still lets one entity hold several accounts of the same type' do
      # The partial index is scoped to security_id; (entity_id, type) stays
      # deliberately non-unique, because an entity may hold several banks.
      entity = create(:entity)
      create(:account, entity: entity, type: 'bank')

      expect { create(:account, entity: entity, type: 'bank') }.not_to raise_error
    end
  end
end
