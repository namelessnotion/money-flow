# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Types::Entity do
  # A deposit that cleared and then came back, recorded as owed
  # (ruby/docs/adr/0011).
  def owe(entity, amount)
    create(:ach_transaction, entity: entity, amount_minor_units: amount, returned_at: Time.now, return_reason: 'R10',
                             clearing_transaction_id: SecureRandom.uuid_v7)
  end

  def owed_by_id
    MoneyFlowSchema.execute('{ entities { nodes { id owedMinorUnits } } }').to_h
                   .dig('data', 'entities', 'nodes').to_h { |node| [node['id'], node['owedMinorUnits']] }
  end

  it 'shows what each entity owes for late deposit returns, and nothing for one that owes nothing' do
    debtor = create(:entity)
    square = create(:entity)
    owe(debtor, 5_000)
    owe(debtor, 2_500)

    expect(owed_by_id).to include(debtor.id.to_s => '7500', square.id.to_s => '0')
  end

  it 'reads what a page of entities owe in a fixed number of queries' do
    3.times { owe(create(:entity), 1_000) }

    sqls = capture_sql { owed_by_id }

    expect(sqls.count { |sql| sql.include?('FROM "ach_transactions"') }).to eq(2)
  end
end
