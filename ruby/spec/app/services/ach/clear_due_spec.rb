# frozen_string_literal: true

require 'spec_helper'
require 'logger'
require 'stringio'

RSpec.describe Services::Ach::ClearDue do
  subject(:sweep) { described_class.new(clear: clear, logger: Logger.new(StringIO.new)) }

  let(:clear) { Services::Ach::Clear.new(gateway: go_gateway) }
  # Thursday 2026-09-17, 12:00 Eastern: deposits completed Monday or earlier are due.
  let(:now) { Time.utc(2026, 9, 17, 16, 0) }
  let(:monday) { Time.utc(2026, 9, 14, 15, 0) }
  let(:tuesday) { Time.utc(2026, 9, 15, 15, 0) }

  before { allow(clear).to receive(:call) { |ach_transaction_id:| Models::AchTransaction[ach_transaction_id] } }

  def deposit(state: 'completed', completed_at: monday, direction: 'deposit')
    ach = create(:ach_transaction, direction: direction)
    create(:transaction_projection, aggregate_id: ach.id, state: state, state_changed_at: completed_at)
    ach
  end

  it 'clears completed deposits whose third business day has come' do
    due = deposit

    expect(sweep.call(now: now).cleared).to eq([due.id])
    expect(clear).to have_received(:call).with(ach_transaction_id: due.id)
  end

  it 'leaves deposits that are not due yet' do
    deposit(completed_at: tuesday)

    expect(sweep.call(now: now).cleared).to be_empty
  end

  it 'leaves deposits that have not completed' do
    deposit(state: 'started')
    create(:ach_transaction) # not projected at all yet

    expect(sweep.call(now: now).cleared).to be_empty
  end

  it 'leaves withdrawals, which mint nothing to clear' do
    deposit(direction: 'withdrawal')

    expect(sweep.call(now: now).cleared).to be_empty
  end

  it 'leaves a deposit whose clearing the read model has already seen' do
    ach = deposit
    clearing_id = Services::DetId.for("#{ach.id}:clearing")
    ach.update(clearing_transaction_id: clearing_id)
    create(:transaction_projection, aggregate_id: clearing_id, state: 'started')

    expect(sweep.call(now: now).cleared).to be_empty
  end

  # Sent, but not yet projected: Go may never have received it, and sending
  # the same id again is a no-op if it did.
  it 'sends again a clearing the read model has not seen yet' do
    ach = deposit
    ach.update(clearing_transaction_id: Services::DetId.for("#{ach.id}:clearing"))

    expect(sweep.call(now: now).cleared).to eq([ach.id])
  end

  it 'keeps clearing the others when one fails, and reports the failure' do
    failing = deposit
    ok = deposit
    allow(clear).to receive(:call).with(ach_transaction_id: failing.id).and_raise(Services::Ach::Refused, 'no')

    result = sweep.call(now: now)

    expect(result.cleared).to eq([ok.id])
    expect(result.failed).to eq(failing.id => 'Services::Ach::Refused: no')
  end
end
