# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Resolvers::MoneyFlow do
  let(:world) { securities_world(name: 'sim-3 Elm Street') }
  let(:investor) { world.investor }
  let(:security) { world.security }
  let(:query) do
    <<~GRAPHQL
      query($namePrefix: String, $since: ISO8601DateTime) {
        moneyFlow(namePrefix: $namePrefix, since: $since) {
          parties { id kind label }
          movements { kind source target amountMinorUnits occurredAt transactionId }
        }
      }
    GRAPHQL
  end

  def execute(query, variables = {})
    MoneyFlowSchema.execute(query, variables: variables).to_h
  end

  def completed(aggregate_id, at: Time.utc(2026, 9, 1, 12))
    create(:transaction_projection, aggregate_id: aggregate_id, state: 'completed', state_changed_at: at)
  end

  it 'answers with the Parties and the Movements between them' do
    subscription = create(:subscription, security: security, investor: investor, amount_minor_units: 12_345)
    completed(subscription.id)

    result = execute(query)

    expect(result['errors']).to be_nil
    expect(result.dig('data', 'moneyFlow', 'parties')).to contain_exactly(
      { 'id' => "entity:#{investor.id}", 'kind' => 'INVESTOR', 'label' => investor.name },
      { 'id' => "security:#{security.id}", 'kind' => 'SECURITY', 'label' => 'sim-3 Elm Street' }
    )
    expect(result.dig('data', 'moneyFlow', 'movements')).to eq(
      [{ 'kind' => 'SUBSCRIPTION', 'source' => "entity:#{investor.id}", 'target' => "security:#{security.id}",
         'amountMinorUnits' => '12345', 'occurredAt' => '2026-09-01T12:00:00+00:00',
         'transactionId' => subscription.id }]
    )
  end

  it 'narrows by name prefix and completion time' do
    tagged = create(:entity, name: 'sim-3 investor')
    old = create(:ach_transaction, entity: tagged)
    recent = create(:ach_transaction, entity: tagged)
    completed(old.id, at: Time.utc(2026, 9, 1))
    completed(recent.id, at: Time.utc(2026, 9, 4))
    completed(create(:ach_transaction, entity: create(:entity, name: 'someone else')).id, at: Time.utc(2026, 9, 4))

    result = execute(query, { 'namePrefix' => 'sim-3 ', 'since' => '2026-09-02T00:00:00Z' })

    expect(result.dig('data', 'moneyFlow', 'movements').map { |m| m['transactionId'] }).to eq([recent.id])
  end

  it 'reads the whole flow in one query' do
    completed(create(:subscription, security: security, investor: investor).id)
    completed(create(:ach_transaction, entity: investor).id)

    statements = capture_sql { execute(query) }

    expect(statements.count { |s| s.include?('SELECT') }).to eq(1)
  end
end
