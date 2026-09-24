# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::MoneyFlow do
  let(:world) do
    securities_world(name: 'sim-7 Oak Street',
                     draw_transaction_id: SecureRandom.uuid_v7, draw_transfer_id: SecureRandom.uuid_v7)
  end
  let(:security) { world.security }
  let(:investor) { world.investor }
  let(:borrower) { world.borrower }

  def completed(aggregate_id, at: Time.utc(2026, 9, 1, 12))
    create(:transaction_projection, aggregate_id: aggregate_id, state: 'completed', state_changed_at: at)
  end

  def party(id) = "entity:#{id}"
  def security_party = "security:#{security.id}"

  def kind = Services::MoneyFlow::Movement::Kind
  def party_kind = Services::MoneyFlow::Party::Kind
  def movements(**filters) = described_class.of(**filters).movements

  describe 'the path money takes' do
    it 'moves a deposit in from the bank and a withdrawal back out to it' do
      deposit = create(:ach_transaction, entity: investor, direction: 'deposit', amount_minor_units: 5_000)
      withdrawal = create(:ach_transaction, entity: investor, direction: 'withdrawal', amount_minor_units: 2_000)
      completed(deposit.id)
      completed(withdrawal.id)

      expect(movements.map { |m| [m.kind, m.source, m.target, m.amount_minor_units] }).to contain_exactly(
        [kind::Deposit, 'bank', party(investor.id), 5_000],
        [kind::Withdrawal, party(investor.id), 'bank', 2_000]
      )
    end

    it 'moves a Subscription from the Investor into the Security' do
      subscription = create(:subscription, security: security, investor: investor, amount_minor_units: 30_000)
      completed(subscription.id)

      expect(movements.map { |m| [m.kind, m.source, m.target, m.amount_minor_units] })
        .to eq([[kind::Subscription, party(investor.id), security_party, 30_000]])
    end

    it 'moves the Draw from the Security to its Borrower, for the whole offering' do
      completed(security.draw_transaction_id)

      expect(movements.map { |m| [m.kind, m.source, m.target, m.amount_minor_units] })
        .to eq([[kind::Draw, security_party, party(borrower.id), security.principal_minor_units]])
    end

    it 'moves a Repayment, principal and interest together, from the Borrower into the Security' do
      repayment = create(:repayment, security: security, principal_minor_units: 100_000, interest_minor_units: 9_000)
      completed(repayment.id)

      expect(movements.map { |m| [m.kind, m.source, m.target, m.amount_minor_units] })
        .to eq([[kind::Repayment, party(borrower.id), security_party, 109_000]])
    end

    it 'splits a Disbursement into the principal and the interest it pays the Investor' do
      repayment = create(:repayment, security: security)
      disbursement = create(:disbursement, repayment: repayment, investor: investor,
                                           principal_minor_units: 50_000, interest_minor_units: 4_000)
      completed(disbursement.id)

      expect(movements.map { |m| [m.kind, m.source, m.target, m.amount_minor_units] }).to contain_exactly(
        [kind::DisbursementPrincipal, security_party, party(investor.id), 50_000],
        [kind::DisbursementInterest, security_party, party(investor.id), 4_000]
      )
    end

    it 'leaves out a part of a Disbursement that moved nothing' do
      # An interest-only Repayment retires no principal; a zero edge is noise.
      repayment = create(:repayment, security: security)
      disbursement = create(:disbursement, :interest_only, repayment: repayment, investor: investor,
                                                           interest_minor_units: 4_000)
      completed(disbursement.id)

      expect(movements.map(&:kind)).to eq([kind::DisbursementInterest])
    end
  end

  it 'counts only what the read model has seen complete' do
    # In flight, rejected or rolled back never moved any money for good.
    create(:subscription, security: security, investor: investor)
    rejected = create(:subscription, security: security, investor: investor)
    create(:transaction_projection, aggregate_id: rejected.id, state: 'rejected')
    rolled_back = create(:ach_transaction, entity: investor)
    create(:transaction_projection, aggregate_id: rolled_back.id, state: 'rolled_back')

    expect(movements).to be_empty
  end

  it 'dates each Movement by when Go says its Transaction completed, oldest first' do
    later = create(:subscription, security: security, investor: investor)
    earlier = create(:ach_transaction, entity: investor)
    completed(later.id, at: Time.utc(2026, 9, 3))
    completed(earlier.id, at: Time.utc(2026, 9, 2))

    expect(movements.map(&:occurred_at)).to eq([Time.utc(2026, 9, 2), Time.utc(2026, 9, 3)])
    expect(movements.map(&:transaction_id)).to eq([earlier.id, later.id])
  end

  describe 'the parties' do
    it 'names every party a Movement touches, and no other' do
      subscription = create(:subscription, security: security, investor: investor)
      deposit = create(:ach_transaction, entity: investor)
      completed(subscription.id)
      completed(deposit.id)

      parties = described_class.of.parties.map { |p| [p.id, p.kind, p.label] }

      expect(parties).to contain_exactly(
        ['bank', party_kind::Bank, 'Bank'],
        [party(investor.id), party_kind::Investor, investor.name],
        [security_party, party_kind::Security, security.name]
      )
    end

    it 'gives a Borrower its role' do
      completed(security.draw_transaction_id)

      kinds = described_class.of.parties.to_h { |p| [p.id, p.kind] }

      expect(kinds.fetch(party(borrower.id))).to eq(party_kind::Borrower)
    end
  end

  describe 'narrowing' do
    it 'keeps only Movements whose entity is named with the prefix' do
      # Every Movement has exactly one entity on one end; a simulation run
      # tags its entities' names, and this is how a reader picks one run out.
      tagged = create(:entity, :investor, name: 'sim-7 investor 1')
      other = create(:entity, :investor, name: 'sim-70 investor 1')
      [tagged, other].each { |entity| completed(create(:ach_transaction, entity: entity).id) }

      expect(movements(name_prefix: 'sim-7 ').map(&:target)).to eq([party(tagged.id)])
    end

    it 'treats the prefix literally, not as a pattern' do
      completed(create(:ach_transaction, entity: create(:entity, name: 'sim-7 a')).id)

      expect(movements(name_prefix: 'sim-_')).to be_empty
    end

    it 'keeps only Movements that completed at or after `since`' do
      old = create(:ach_transaction, entity: investor)
      recent = create(:ach_transaction, entity: investor)
      completed(old.id, at: Time.utc(2026, 9, 1))
      completed(recent.id, at: Time.utc(2026, 9, 5))

      expect(movements(since: Time.utc(2026, 9, 5)).map(&:transaction_id)).to eq([recent.id])
    end
  end

  it 'reads every kind in one query rather than one per kind' do
    subscription = create(:subscription, security: security, investor: investor)
    completed(subscription.id)
    completed(create(:ach_transaction, entity: investor).id)
    completed(security.draw_transaction_id)

    statements = capture_sql { described_class.of }

    expect(statements.count { |s| s.include?('transaction_projections') }).to eq(1)
  end
end
