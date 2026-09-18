# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Types::AchTransaction do
  let(:entity) { create(:entity) }

  def query(entity_id)
    MoneyFlowSchema.execute(<<~GRAPHQL, variables: { entityId: entity_id.to_s }).to_h
      query($entityId: ID!) {
        achTransactions(entityId: $entityId) { nodes { id state reason realLegState } }
      }
    GRAPHQL
  end

  def capture_sql
    statements = []
    recorder = Object.new
    %i[info warn error].each { |level| recorder.define_singleton_method(level) { |message| statements << message } }
    DB.loggers << recorder
    yield
    statements
  ensure
    DB.loggers.delete(recorder)
  end

  it "joins in each Transaction's projected state and its real leg's" do
    ach = create(:ach_transaction, entity: entity)
    create(:transaction_projection, aggregate_id: ach.id, state: 'rollback_started', reason: 'R01')
    create(:transfer_projection, aggregate_id: ach.real_transfer_id, state: 'cancelled')

    nodes = query(entity.id).dig('data', 'achTransactions', 'nodes')

    expect(nodes).to eq(
      [{ 'id' => ach.id, 'state' => 'ROLLBACK_STARTED', 'reason' => 'R01', 'realLegState' => 'CANCELLED' }]
    )
  end

  it 'shows a Transaction the projection has not reached yet with null state' do
    ach = create(:ach_transaction, entity: entity)

    nodes = query(entity.id).dig('data', 'achTransactions', 'nodes')

    expect(nodes).to eq([{ 'id' => ach.id, 'state' => nil, 'reason' => nil, 'realLegState' => nil }])
  end

  it "lists only the entity's own, oldest first" do
    newer = create(:ach_transaction, entity: entity, created_at: Time.now)
    older = create(:ach_transaction, entity: entity, created_at: Time.now - 60)
    create(:ach_transaction)

    ids = query(entity.id).dig('data', 'achTransactions', 'nodes').map { |node| node['id'] }

    expect(ids).to eq([older.id, newer.id])
  end

  it 'shows a completed deposit\'s clearing due date and its clearing Transaction\'s state' do
    ach = create(:ach_transaction, entity: entity)
    create(:transaction_projection, aggregate_id: ach.id, state: 'completed',
                                    state_changed_at: Time.utc(2026, 9, 14, 15, 0))
    clearing_id = Services::DetId.for("#{ach.id}:clearing")
    ach.update(clearing_transaction_id: clearing_id)
    create(:transaction_projection, aggregate_id: clearing_id, state: 'completed')

    node = MoneyFlowSchema.execute(
      'query($e: ID!) { achTransactions(entityId: $e) { nodes { clearingDueOn clearingState } } }',
      variables: { e: entity.id.to_s }
    ).to_h.dig('data', 'achTransactions', 'nodes', 0)

    expect(node).to eq('clearingDueOn' => '2026-09-17', 'clearingState' => 'COMPLETED')
  end

  it 'has no clearing due date before the deposit completes' do
    ach = create(:ach_transaction, entity: entity)
    create(:transaction_projection, aggregate_id: ach.id, state: 'started')

    node = MoneyFlowSchema.execute('query($e: ID!) { achTransactions(entityId: $e) { nodes { clearingDueOn } } }',
                                   variables: { e: entity.id.to_s }).to_h.dig('data', 'achTransactions', 'nodes', 0)

    expect(node).to eq('clearingDueOn' => nil)
  end

  it 'reads every row and its projections in a single query' do
    3.times do
      ach = create(:ach_transaction, entity: entity)
      create(:transaction_projection, aggregate_id: ach.id)
    end

    sqls = capture_sql { query(entity.id) }

    expect(sqls.count { |sql| sql.include?('SELECT') }).to eq(1)
  end

  describe 'steps' do
    def steps_of(ach)
      MoneyFlowSchema.execute('query($id: ID!) { achTransaction(id: $id) { steps { name status } } }',
                              variables: { id: ach.id }).to_h.dig('data', 'achTransaction', 'steps')
    end

    it 'shows a pending deposit as submitted and waiting to settle' do
      ach = create(:ach_transaction, entity: entity, provider_reference: 'fake-1')
      create(:transaction_projection, aggregate_id: ach.id, state: 'started')
      create(:transfer_projection, aggregate_id: ach.real_transfer_id, state: 'pending')

      expect(steps_of(ach)).to eq(
        [{ 'name' => 'INITIATION', 'status' => 'DONE' }, { 'name' => 'SUBMISSION', 'status' => 'DONE' },
         { 'name' => 'SETTLEMENT', 'status' => 'WAITING' }, { 'name' => 'COMPLETION', 'status' => 'WAITING' },
         { 'name' => 'CLEARING', 'status' => 'WAITING' }]
      )
    end

    it 'shows a returned withdrawal as funded, failing settlement, and rolling back' do
      ach = create(:ach_transaction, entity: entity, direction: 'withdrawal', provider_reference: 'fake-1')
      create(:transaction_projection, aggregate_id: ach.id, state: 'rollback_started', reason: 'R01')
      create(:transfer_projection, aggregate_id: ach.shadow_transfer_id, state: 'committed')
      create(:transfer_projection, aggregate_id: ach.real_transfer_id, state: 'cancelled')

      expect(steps_of(ach)).to eq(
        [{ 'name' => 'INITIATION', 'status' => 'DONE' }, { 'name' => 'FUNDING', 'status' => 'DONE' },
         { 'name' => 'SUBMISSION', 'status' => 'DONE' }, { 'name' => 'SETTLEMENT', 'status' => 'FAILED' },
         { 'name' => 'COMPLETION', 'status' => 'SKIPPED' }, { 'name' => 'ROLLBACK', 'status' => 'WAITING' }]
      )
    end

    it 'shows a withdrawal short of cleared cash as failing funding, never submitted' do
      ach = create(:ach_transaction, entity: entity, direction: 'withdrawal')
      create(:transaction_projection, aggregate_id: ach.id, state: 'rolled_back')
      create(:transfer_projection, aggregate_id: ach.shadow_transfer_id, state: 'rejected')

      expect(steps_of(ach).to_h { |step| [step['name'], step['status']] }).to eq(
        'INITIATION' => 'DONE', 'FUNDING' => 'FAILED', 'SUBMISSION' => 'SKIPPED', 'SETTLEMENT' => 'SKIPPED',
        'COMPLETION' => 'SKIPPED', 'ROLLBACK' => 'DONE'
      )
    end
  end
end
