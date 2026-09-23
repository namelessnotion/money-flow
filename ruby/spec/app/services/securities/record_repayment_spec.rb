# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::RecordRepayment do
  subject(:service) { described_class.new(gateway: securities_gateway) }

  let(:world) { securities_world(principal_minor_units: 100_000) }
  let(:security) { world.security }

  before { stub_go_securities_happy_path }

  # A drawn Security with `held` of principal outstanding.
  def drawn(held: 100_000, drawn_at: Time.now)
    subscription = create(:subscription, security: security, investor: world.investor,
                                         amount_minor_units: held)
    create(:transaction_projection, aggregate_id: subscription.id, state: 'completed')
    security.update(draw_transaction_id: SecureRandom.uuid_v7)
    create(:transaction_projection, aggregate_id: security.draw_transaction_id, state: 'completed',
                                    state_changed_at: drawn_at)
    security.refresh
  end

  def repay(principal: 40_000, interest: 4_000)
    service.call(security_id: security.id, principal_minor_units: principal,
                 interest_minor_units: interest, as_of: Date.new(2026, 9, 20))
  end

  def sent_request
    request = nil
    expect(transaction_client).to have_received(:start_initializing_transaction) { |req| request = req }
    request
  end

  context 'when the Security has been drawn' do
    before { drawn }

    it 'records what was paid, split into principal and interest' do
      repayment = repay

      expect(repayment.principal_minor_units).to eq(40_000)
      expect(repayment.interest_minor_units).to eq(4_000)
      expect(repayment.as_of).to eq(Date.new(2026, 9, 20))
      expect(repayment.security_id).to eq(security.id)
    end

    it 'collects both parts as one amount, because the ledger moves dollars' do
      repayment = repay
      leg = sent_request.transfers[repayment.collection_transfer_id]

      expect(sent_request.id).to eq(repayment.id)
      expect(sent_request.factory_name).to eq('security_repayment')
      expect(leg.amount.minor_units).to eq(44_000)
      expect(leg.from_wallet_id).to eq(world.wallet_of(world.borrower, 'cleared_cash'))
      expect(leg.to_wallet_id).to eq(world.security_wallet('security_repayment'))
    end

    it 'records the intent before calling Go, so a crash cannot lose the ids' do
      rows_when_started = nil
      allow(transaction_client).to receive(:start_initializing_transaction) do |req|
        rows_when_started = Models::Repayment.count
        transaction_initialized(req.id)
      end

      repay

      expect(rows_when_started).to eq(Models::Repayment.count)
    end

    it 'accepts an interest-only payment' do
      expect(repay(principal: 0, interest: 1_000).principal_minor_units).to eq(0)
    end

    it 'refuses more principal than is still owed' do
      expect { repay(principal: 100_001, interest: 0) }
        .to raise_error(Services::Securities::NotRepayable, /has 100000 principal outstanding/)
    end

    it 'refuses a payment of nothing, which has no leg to send' do
      expect { repay(principal: 0, interest: 0) }
        .to raise_error(Services::Securities::InvalidAmount, /nothing has no leg/)
    end

    it "raises Go's reason when the Borrower is short, which the pre-flight catches" do
      # The collection leg is the DAG root, so it is pre-flighted and refused
      # before anything is written. That is the whole of what this service can
      # learn: the rest arrives through the projection.
      allow(transaction_client).to receive(:start_initializing_transaction) do |req|
        transaction_rejected(req.id, 'wallet "cleared_cash" has insufficient Token capacity: 4000 USD short')
      end

      expect { repay }.to raise_error(Services::Securities::Refused, /insufficient Token capacity/)
    end
  end

  describe 'what it refuses before the money is drawn' do
    it 'refuses a Security that has not been drawn: nothing is owed yet' do
      expect { repay }.to raise_error(Services::Securities::NotRepayable, /nothing is owed until it is drawn/)
    end

    it 'never reaches Go' do
      expect { repay }.to raise_error(Services::Securities::NotRepayable)
      expect(transaction_client).not_to have_received(:start_initializing_transaction)
    end

    it 'refuses an unknown security' do
      expect do
        service.call(security_id: SecureRandom.uuid_v7, principal_minor_units: 1, interest_minor_units: 0)
      end.to raise_error(Services::Securities::NotFound, /no security/)
    end
  end

  describe Services::Securities::Schedule do
    it 'accrues nothing before the money is drawn' do
      due = described_class.due(security: security, as_of: Date.new(2026, 9, 20))

      expect(due.days).to eq(0)
      expect(due.interest_minor_units).to eq(0)
    end

    it "counts days from the day Go says the money moved, not Ruby's clock" do
      drawn(drawn_at: Time.utc(2026, 8, 21, 12, 0))

      due = described_class.due(security: security, as_of: Date.new(2026, 9, 20))

      expect(due.days).to eq(30)
      expect(due.principal_minor_units).to eq(100_000)
      # 100000 * 1000 * 30 / (10000 * 365) = 821.9...
      expect(due.interest_minor_units).to eq(822)
    end

    it 'owes nothing once every holder has had their principal back' do
      drawn(held: 100_000, drawn_at: Time.utc(2026, 8, 21))
      paid = create(:repayment, security: security, principal_minor_units: 100_000, interest_minor_units: 1)
      settled = create(:disbursement, repayment: paid, investor: world.investor,
                                      principal_minor_units: 100_000, interest_minor_units: 1)
      create(:transaction_projection, aggregate_id: settled.id, state: 'completed')

      due = described_class.due(security: security, as_of: Date.new(2026, 9, 20))

      # No principal outstanding, so no interest accrues on it either.
      expect(due.total_minor_units).to eq(0)
      expect(due.days).to eq(30)
    end
  end
end
