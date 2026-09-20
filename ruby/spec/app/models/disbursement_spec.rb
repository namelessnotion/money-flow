# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Models::Disbursement do
  # Go turns a zero-amount Transfer into a transport error the Transaction saga
  # logs and swallows, stranding the Transaction in Started with no event to
  # explain it. These constraints are that rule written where it can be relied
  # on rather than remembered.
  describe 'the zero-amount rule' do
    it 'carries a retirement leg when it repays principal' do
      disbursement = create(:disbursement)

      expect(disbursement.principal_minor_units).to be_positive
      expect(disbursement.retirement_transfer_id).not_to be_nil
    end

    it 'omits the retirement leg when it repays no principal' do
      disbursement = create(:disbursement, :interest_only)

      expect(disbursement.principal_minor_units).to be_zero
      expect(disbursement.retirement_transfer_id).to be_nil
    end

    it 'refuses a retirement leg that would move nothing' do
      expect { create(:disbursement, principal_minor_units: 0) }
        .to raise_error(Sequel::CheckConstraintViolation, /retirement_leg_iff_principal/)
    end

    it 'refuses repaid principal with no leg to retire it' do
      expect { create(:disbursement, retirement_transfer_id: nil) }
        .to raise_error(Sequel::CheckConstraintViolation, /retirement_leg_iff_principal/)
    end

    it 'refuses a disbursement of nothing at all' do
      expect { create(:disbursement, :interest_only, interest_minor_units: 0) }
        .to raise_error(Sequel::CheckConstraintViolation, /total_positive/)
    end
  end

  it 'disburses a repayment to a holder at most once' do
    # The id is derived from the pair, so Go already dedupes a re-send; this
    # says the same thing where Ruby can rely on it.
    first = create(:disbursement)

    expect { create(:disbursement, repayment: first.repayment, investor: first.investor) }
      .to raise_error(Sequel::UniqueConstraintViolation, /repayment_id_investor_entity_id/)
  end

  describe Models::Repayment do
    it 'allows an interest-only payment' do
      expect { create(:repayment, principal_minor_units: 0) }.not_to raise_error
    end

    it 'allows a principal paydown with no interest due' do
      expect { create(:repayment, interest_minor_units: 0) }.not_to raise_error
    end

    it 'refuses a payment of nothing, which has no collection leg to send' do
      expect { create(:repayment, principal_minor_units: 0, interest_minor_units: 0) }
        .to raise_error(Sequel::CheckConstraintViolation, /total_positive/)
    end
  end
end
