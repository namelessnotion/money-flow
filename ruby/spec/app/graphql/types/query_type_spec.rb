# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Types::QueryType do
  def execute(query, variables: {})
    MoneyFlowSchema.execute(query, variables: variables).to_h
  end

  def create_entity(name:)
    create(:entity, name: name)
  end

  describe 'entities' do
    it 'returns onboarded entities' do
      entity = create_entity(name: 'Acme Corp')

      result = execute('{ entities { nodes { id name holderUuid } } }')

      expect(result['errors']).to be_nil
      expect(result.dig('data', 'entities', 'nodes')).to eq(
        [{ 'id' => entity.id.to_s, 'name' => 'Acme Corp', 'holderUuid' => entity.holder_uuid }]
      )
    end

    it 'paginates at 100 per page by default' do
      101.times { |n| create_entity(name: "Entity #{n}") }

      result = execute('{ entities { nodes { id } pageInfo { hasNextPage } } }')

      expect(result.dig('data', 'entities', 'nodes').length).to eq(100)
      expect(result.dig('data', 'entities', 'pageInfo', 'hasNextPage')).to be true
    end

    it 'honors an explicit first argument' do
      3.times { |n| create_entity(name: "Entity #{n}") }

      result = execute('{ entities(first: 2) { nodes { id } pageInfo { hasNextPage } } }')

      expect(result.dig('data', 'entities', 'nodes').length).to eq(2)
      expect(result.dig('data', 'entities', 'pageInfo', 'hasNextPage')).to be true
    end

    it 'selects only the requested entity columns' do
      create_entity(name: 'Acme Corp')

      sqls = capture_sql { execute('{ entities { nodes { id name } } }') }

      expect(sqls).to include(a_string_including('SELECT "id", "name" FROM "entities"'))
    end

    it 'eager loads accounts in a single query, without N+1s' do
      first = create_entity(name: 'Acme Corp')
      second = create_entity(name: 'Beta LLC')
      create(:account, name: 'Checking', type: 'bank', entity: first)
      create(:account, name: 'Savings', type: 'cash', entity: second)

      result = nil
      sqls = capture_sql do
        result = execute('{ entities { nodes { id accounts { name type } } } }')
      end

      expect(result['errors']).to be_nil
      expect(sqls.count { |sql| sql.include?('FROM "accounts"') }).to eq(1)

      accounts_by_entity = result.dig('data', 'entities', 'nodes').to_h do |node|
        [node['id'], node['accounts'].map { |account| account['name'] }]
      end
      expect(accounts_by_entity).to eq(
        first.id.to_s => ['Checking'],
        second.id.to_s => ['Savings']
      )
    end

    it "loads every page's Account balances in a single query" do
      2.times do |n|
        account = create(:account, entity: create_entity(name: "Entity #{n}"))
        create(:token_balance_projection, wallet_uuid: account.wallet_uuid, posted_minor_units: 100)
      end

      result = nil
      sqls = capture_sql do
        result = execute('{ entities { nodes { accounts { balances { postedMinorUnits } } } } }')
      end

      expect(result['errors']).to be_nil
      expect(result.dig('data', 'entities', 'nodes').flat_map { |n| n['accounts'] })
        .to all(eq('balances' => [{ 'postedMinorUnits' => '100' }]))
      expect(sqls.count { |sql| sql.include?('FROM "token_balance_projections"') }).to eq(1)
    end

    it 'does not eager load accounts when they are not requested' do
      create_entity(name: 'Acme Corp')

      sqls = capture_sql { execute('{ entities { nodes { id } } }') }

      expect(sqls.none? { |sql| sql.include?('FROM "accounts"') }).to be true
    end
  end

  describe 'entity' do
    def entity_query(id)
      execute('query($id: ID!) { entity(id: $id) { id name accounts { type } } }', variables: { id: id })
    end

    it 'returns one entity with its accounts' do
      entity = create_entity(name: 'Acme Corp')
      create(:account, type: 'cash', entity: entity)

      result = entity_query(entity.id.to_s)

      expect(result['errors']).to be_nil
      expect(result.dig('data', 'entity')).to eq(
        'id' => entity.id.to_s, 'name' => 'Acme Corp', 'accounts' => [{ 'type' => 'cash' }]
      )
    end

    def balance_json(posted, outgoing: 0, incoming: 0)
      { 'currency' => 'USD', 'postedMinorUnits' => posted.to_s,
        'pendingOutgoingMinorUnits' => outgoing.to_s, 'pendingIncomingMinorUnits' => incoming.to_s }
    end

    def balances_query(entity)
      execute(<<~GRAPHQL, variables: { id: entity.id.to_s })
        query($id: ID!) {
          entity(id: $id) {
            accounts { type balances { currency postedMinorUnits pendingOutgoingMinorUnits pendingIncomingMinorUnits } }
          }
        }
      GRAPHQL
    end

    it "shows each Account's balance, summed over its Tokens" do
      entity = create_entity(name: 'Acme Corp')
      bank = create(:account, type: 'bank', entity: entity)
      cash = create(:account, type: 'cash', entity: entity)
      create(:token_balance_projection, wallet_uuid: bank.wallet_uuid, posted_minor_units: -500)
      create(:token_balance_projection, wallet_uuid: cash.wallet_uuid, posted_minor_units: 300)
      create(:token_balance_projection, wallet_uuid: cash.wallet_uuid, posted_minor_units: 200,
                                        pending_outgoing_minor_units: 50, pending_incoming_minor_units: 20)

      result = nil
      sqls = capture_sql { result = balances_query(entity) }

      expect(result.dig('data', 'entity', 'accounts')).to contain_exactly(
        { 'type' => 'bank', 'balances' => [balance_json(-500)] },
        { 'type' => 'cash', 'balances' => [balance_json(500, outgoing: 50, incoming: 20)] }
      )
      expect(sqls.count { |sql| sql.include?('FROM "token_balance_projections"') }).to eq(1)
    end

    it 'shows no balances for an Account with no ledger activity yet' do
      entity = create_entity(name: 'Acme Corp')
      create(:account, entity: entity)

      result = execute('query($id: ID!) { entity(id: $id) { accounts { balances { currency } } } }',
                       variables: { id: entity.id.to_s })

      expect(result.dig('data', 'entity', 'accounts')).to eq([{ 'balances' => [] }])
    end

    it 'is null for an id that names no entity' do
      expect([entity_query('0'), entity_query('not-an-id')]).to all(eq('data' => { 'entity' => nil }))
    end
  end

  describe 'achTransaction' do
    def ach_query(id)
      execute('query($id: ID!) { achTransaction(id: $id) { id direction state } }', variables: { id: id })
    end

    it 'returns one ACH Transaction with its projected state' do
      ach = create(:ach_transaction)
      create(:transaction_projection, aggregate_id: ach.id, state: 'started')

      result = ach_query(ach.id)

      expect(result['errors']).to be_nil
      expect(result.dig('data', 'achTransaction')).to eq('id' => ach.id, 'direction' => 'DEPOSIT', 'state' => 'STARTED')
    end

    it 'is null for an id that names no ACH Transaction' do
      expect([ach_query(SecureRandom.uuid_v7), ach_query('not-a-uuid')])
        .to all(eq('data' => { 'achTransaction' => nil }))
    end
  end
end
