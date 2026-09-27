# frozen_string_literal: true

require 'spec_helper'
require 'logger'
require 'stringio'

RSpec.describe Services::Ach::ReturnDue do
  subject(:sweep) { described_class.new(return_service: return_service, logger: Logger.new(StringIO.new)) }

  let(:return_service) { Services::Ach::Return.new(gateway: go_gateway) }

  before { allow(return_service).to receive(:act_on_notice) }

  def noticed(state: 'completed', real: 'committed', **attributes)
    ach = create(:ach_transaction, returned_at: Time.now, return_reason: 'R10', **attributes)
    create(:transaction_projection, aggregate_id: ach.id, state: state) if state
    create(:transfer_projection, aggregate_id: ach.real_transfer_id, state: real) if real
    ach
  end

  def acted_on
    ids = []
    allow(return_service).to receive(:act_on_notice) { |ach| ids << ach.id }
    sweep.call
    ids
  end

  it 'acts on a notice for a completed Transaction that has no late return yet' do
    ach = noticed

    expect(acted_on).to eq([ach.id])
  end

  # The cancel was refused because the leg had just posted: the sweep keeps
  # trying until the Transaction completes and the notice becomes a late return.
  it 'keeps acting on a notice whose Transaction is still running' do
    ach = noticed(state: 'started')

    expect(acted_on).to eq([ach.id])
  end

  it 'acts on a notice whose Transaction the read model has not seen yet' do
    ach = noticed(state: nil, real: nil)

    expect(acted_on).to eq([ach.id])
  end

  it 'leaves a notice whose real leg was cancelled: the return was seen through before settlement' do
    noticed(state: 'rollback_started', real: 'cancelled')

    expect(acted_on).to be_empty
  end

  it 'leaves a notice for a Transaction that never moved money, or needs a person' do
    %w[rolled_back rejected rollback_failed].each { |state| noticed(state: state) }

    expect(acted_on).to be_empty
  end

  it 'leaves an ACH Transaction with no notice' do
    create(:ach_transaction)

    expect(acted_on).to be_empty
  end

  # Sent, but not yet projected: Go may never have received it, and sending the
  # same id again is a no-op if it did.
  it 'sends again a late return the read model has not seen' do
    ach = noticed
    ach.update(return_transaction_id: Services::DetId.for("#{ach.id}:return"))

    expect(acted_on).to eq([ach.id])
  end

  it 'leaves a late return the read model has seen, in any state' do
    ach = noticed
    ach.update(return_transaction_id: Services::DetId.for("#{ach.id}:return"))
    create(:transaction_projection, aggregate_id: ach.return_transaction_id, state: 'rolled_back')

    expect(acted_on).to be_empty
  end

  it 'reports a failure, and carries on with the rest' do
    first = noticed
    second = noticed
    allow(return_service).to receive(:act_on_notice) do |ach|
      raise Services::Ach::Unavailable, 'go is down' if ach.id == first.id
    end

    result = sweep.call

    expect(result.acted_on).to eq([second.id])
    expect(result.failed.fetch(first.id)).to include('go is down')
  end
end
