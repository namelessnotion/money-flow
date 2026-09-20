# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::DisburseDue do
  subject(:sweep) { described_class.new(logger: logger, disburse: disburse) }

  let(:logger) { Logger.new(StringIO.new) }
  let(:disburse) { Services::Securities::Disburse.new(gateway: securities_gateway) }
  let(:world) { securities_world(principal_minor_units: 100_000) }

  before { stub_go_securities_happy_path }

  def holds(amount, investor: create_provisioned_entity)
    subscription = create(:subscription, security: world.security, investor: investor,
                                         amount_minor_units: amount)
    create(:transaction_projection, aggregate_id: subscription.id, state: 'completed')
    investor
  end

  # A Repayment the read model has, or has not, seen complete.
  def repayment(principal: 40_000, interest: 4_000, state: 'completed')
    paid = create(:repayment, security: world.security,
                              principal_minor_units: principal, interest_minor_units: interest)
    create(:transaction_projection, aggregate_id: paid.id, state: state) if state
    paid
  end

  it 'sends one Transaction per holder, not one fan-out across all of them' do
    alice = holds(75_000)
    bob = holds(25_000)
    paid = repayment

    result = sweep.call

    expect(result.disbursed.size).to eq(2)
    expect(result.failed).to be_empty
    expect(Models::Disbursement.where(repayment_id: paid.id).all.map(&:investor_entity_id))
      .to contain_exactly(alice.id, bob.id)
  end

  it 'pays each holder their pro-rata share, principal and interest kept apart' do
    alice = holds(75_000)
    holds(25_000)
    paid = repayment

    sweep.call

    share = Models::Disbursement.where(repayment_id: paid.id, investor_entity_id: alice.id).first
    expect(share.principal_minor_units).to eq(30_000)
    expect(share.interest_minor_units).to eq(3_000)
  end

  it 'derives each Transaction id from the Repayment and the holder' do
    alice = holds(100_000)
    paid = repayment

    sweep.call

    expect(Models::Disbursement.where(repayment_id: paid.id).first.id)
      .to eq(Services::DetId.for("#{paid.id}:disbursement:#{alice.id}"))
  end

  it 'leaves a Repayment the read model has not seen complete' do
    holds(100_000)
    repayment(state: 'started')

    expect(sweep.call.disbursed).to be_empty
  end

  it 'leaves a Repayment the read model has not seen at all' do
    holds(100_000)
    repayment(state: nil)

    expect(sweep.call.disbursed).to be_empty
  end

  it 'leaves a holder it has already seen paid' do
    alice = holds(100_000)
    paid = repayment
    create(:transaction_projection, state: 'completed',
                                    aggregate_id: Services::Securities::Disburse.transaction_id(paid.id, alice.id))

    expect(sweep.call.disbursed).to be_empty
  end

  it 'leaves a holder whose Disbursement rolled back, because that needs a person' do
    alice = holds(100_000)
    paid = repayment
    create(:transaction_projection, state: 'rolled_back',
                                    aggregate_id: Services::Securities::Disburse.transaction_id(paid.id, alice.id))

    expect(sweep.call.disbursed).to be_empty
  end

  it 'sends again one it sent but has not seen, which Go dedupes by its derived id' do
    holds(100_000)
    repayment

    first = sweep.call
    second = sweep.call

    expect(second.disbursed).to eq(first.disbursed)
    expect(Models::Disbursement.count).to eq(1)
  end

  it 'keeps going when one holder fails, and reports which' do
    alice = holds(75_000)
    holds(25_000)
    repayment
    allow(disburse).to receive(:call).and_wrap_original do |original, **kwargs|
      raise Services::Securities::Refused, 'wallet not found' if kwargs[:share].investor_entity_id == alice.id

      original.call(**kwargs)
    end

    result = sweep.call

    expect(result.disbursed.size).to eq(1)
    expect(result.failed.values.first).to include('Refused: wallet not found')
  end

  it 'confines an allocation that will not add up to its own Repayment' do
    # A Repayment on a Security nobody bought into: the interest has nowhere
    # to go, and the allocation says so. The other Security's holder still
    # gets paid.
    unheld = create(:security, issuer: world.issuer, borrower: world.borrower)
    orphan = create(:repayment, security: unheld, principal_minor_units: 0, interest_minor_units: 500)
    create(:transaction_projection, aggregate_id: orphan.id, state: 'completed')
    holds(100_000)
    good = repayment

    result = sweep.call

    expect(result.failed.keys).to eq([orphan.id])
    expect(result.failed.fetch(orphan.id)).to include('AllocationError')
    expect(Models::Disbursement.where(repayment_id: good.id).count).to eq(1)
  end

  it 'sweeps only the Repayment it is asked about' do
    holds(100_000)
    first = repayment
    repayment

    sweep.call(repayment_id: first.id)

    expect(Models::Disbursement.all.map(&:repayment_id)).to eq([first.id])
  end

  describe 'an interest-only repayment' do
    it 'sends a payout with no retirement leg, because Go refuses a zero-amount Transfer' do
      holds(100_000)
      paid = repayment(principal: 0, interest: 5_000)

      sweep.call

      disbursement = Models::Disbursement.where(repayment_id: paid.id).first
      expect(disbursement.retirement_transfer_id).to be_nil
      expect(disbursement.interest_minor_units).to eq(5_000)
    end
  end
end
