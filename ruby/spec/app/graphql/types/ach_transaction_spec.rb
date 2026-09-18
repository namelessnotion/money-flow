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

  it 'reads every row and its projections in a single query' do
    3.times do
      ach = create(:ach_transaction, entity: entity)
      create(:transaction_projection, aggregate_id: ach.id)
    end

    sqls = capture_sql { query(entity.id) }

    expect(sqls.count { |sql| sql.include?('SELECT') }).to eq(1)
  end
end
