# frozen_string_literal: true

require 'spec_helper'
require 'logger'
require 'stringio'

RSpec.describe Services::Ach::RecoverDue do
  subject(:sweep) { described_class.new(gateway: go_gateway, logger: Logger.new(StringIO.new)) }

  let(:entity) { create_provisioned_entity }
  let(:wallets) { wallet_uuids_of(entity) }

  before { stub_go_happy_path }

  def owes(amount)
    ach = create(:ach_transaction, entity: entity, amount_minor_units: amount, returned_at: Time.now,
                                   return_reason: 'R10', clearing_transaction_id: SecureRandom.uuid_v7)
    ach.update(return_transaction_id: Services::DetId.for("#{ach.id}:return"))
    create(:transaction_projection, aggregate_id: ach.return_transaction_id, state: 'completed')
  end

  def cleared_cash(posted, pending_outgoing: 0, global_seq: 10)
    create(:token_balance_projection, wallet_uuid: wallets['cleared_cash'], posted_minor_units: posted,
                                      pending_outgoing_minor_units: pending_outgoing, last_global_seq: global_seq)
  end

  def recovery(sequence:, amount:, state:, cleared_global_seq: 10)
    row = Models::ReceivableRecovery.create(
      id: Services::DetId.for("#{entity.id}:recovery:#{sequence}"), entity_id: entity.id, sequence: sequence,
      amount_minor_units: amount, currency: 'USD', cleared_global_seq: cleared_global_seq
    )
    create(:transaction_projection, aggregate_id: row.id, state: state) if state
    row
  end

  def sent
    requests = []
    allow(transaction_client).to receive(:start_initializing_transaction) do |req|
      requests << req
      transaction_initialized(req.id)
    end
    sweep.call
    requests
  end

  def amount_of(request) = request.transfers.values.first.amount.minor_units

  it 'collects what the entity owes from its cleared cash' do
    owes(10_000)
    cleared_cash(25_000)

    requests = sent

    expect(requests.map(&:factory_name)).to eq(['receivable_recovery'])
    expect(amount_of(requests.first)).to eq(10_000)
  end

  it "collects no more than the cleared cash that isn't reserved" do
    owes(10_000)
    cleared_cash(5_000, pending_outgoing: 1_500)

    expect(sent.map { |request| amount_of(request) }).to eq([3_500])
  end

  it 'records the Recovery before asking Go, with the cleared-cash position it was sized from' do
    owes(10_000)
    cleared_cash(5_000, global_seq: 42)
    recorded = nil
    allow(transaction_client).to receive(:start_initializing_transaction) do |req|
      recorded = Models::ReceivableRecovery[req.id]
      transaction_initialized(req.id)
    end

    sweep.call

    expect([recorded.sequence, recorded.amount_minor_units, recorded.cleared_global_seq]).to eq([1, 5_000, 42])
  end

  it 'leaves an entity with no cleared cash to collect from' do
    owes(10_000)

    expect(sent).to be_empty
  end

  it 'leaves an entity that owes nothing recoverable' do
    cleared_cash(25_000)

    expect(sent).to be_empty
  end

  it 'takes the next sequence once the last Recovery has completed, for what is still owed' do
    owes(10_000)
    cleared_cash(25_000)
    recovery(sequence: 1, amount: 4_000, state: 'completed')

    requests = sent

    expect(requests.map(&:id)).to eq([Services::DetId.for("#{entity.id}:recovery:2")])
    expect(amount_of(requests.first)).to eq(6_000)
  end

  # One in flight at a time: what it collects isn't known until it completes,
  # and a second sized before then could collect the same debt twice.
  it 'waits while a Recovery is in flight' do
    owes(10_000)
    cleared_cash(25_000)
    recovery(sequence: 1, amount: 4_000, state: 'started')

    expect(sent).to be_empty
  end

  it 'sends again a Recovery the read model has not seen, rather than starting another' do
    owes(10_000)
    cleared_cash(25_000)
    recovery(sequence: 1, amount: 4_000, state: nil)

    requests = sent

    expect(requests.map(&:id)).to eq([Services::DetId.for("#{entity.id}:recovery:1")])
    expect(amount_of(requests.first)).to eq(4_000)
  end

  it 'leaves a Recovery whose rollback failed to a person, and starts no other' do
    owes(10_000)
    cleared_cash(25_000, global_seq: 99)
    recovery(sequence: 1, amount: 4_000, state: 'rollback_failed')

    expect(sent).to be_empty
  end

  context 'when the last Recovery was refused' do
    before do
      owes(10_000)
      recovery(sequence: 1, amount: 4_000, state: 'rejected', cleared_global_seq: 10)
    end

    it 'waits until cleared cash has changed since' do
      cleared_cash(25_000, global_seq: 10)

      expect(sent).to be_empty
    end

    it 'tries again once it has' do
      cleared_cash(25_000, global_seq: 11)

      expect(sent.map(&:id)).to eq([Services::DetId.for("#{entity.id}:recovery:2")])
    end
  end

  # Another sweep recorded the same sequence first. The unique index is what
  # keeps two Recoveries from being in flight at once.
  it 'skips an entity whose next Recovery another sweep recorded first' do
    owes(10_000)
    cleared_cash(25_000)
    allow(Models::ReceivableRecovery).to receive(:create).and_raise(Sequel::UniqueConstraintViolation)

    expect(sent).to be_empty
  end

  it 'reports a refusal without failing the run: the next one retries on a fresher view' do
    owes(10_000)
    cleared_cash(25_000)
    allow(transaction_client).to receive(:start_initializing_transaction) do |req|
      transaction_rejected(req.id, 'insufficient Token capacity')
    end

    result = sweep.call

    expect(result.refused.keys).to eq([entity.id])
    expect(result.failed).to be_empty
  end

  it 'reports a failure, and carries on with the rest' do
    owes(10_000)
    cleared_cash(25_000)
    other = create_provisioned_entity
    other_ach = create(:ach_transaction, entity: other, amount_minor_units: 1_000, returned_at: Time.now,
                                         return_reason: 'R10', clearing_transaction_id: SecureRandom.uuid_v7,
                                         return_transaction_id: SecureRandom.uuid_v7)
    create(:transaction_projection, aggregate_id: other_ach.return_transaction_id, state: 'completed')
    create(:token_balance_projection, wallet_uuid: wallet_uuids_of(other)['cleared_cash'], posted_minor_units: 1_000)
    allow(transaction_client).to receive(:start_initializing_transaction) do |req|
      if req.id.start_with?(Services::DetId.for("#{entity.id}:recovery:1"))
        raise Services::Ach::Unavailable, 'go is down'
      end

      transaction_initialized(req.id)
    end

    result = sweep.call

    expect(result.failed.fetch(entity.id)).to include('go is down')
    expect(result.sent).to eq([Services::DetId.for("#{other.id}:recovery:1")])
  end
end
